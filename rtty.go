/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/valyala/bytebufferpool"
	"github.com/zhaojh329/rtty-go/proto"
)

const (
	rttyProtoVer         = byte(5)
	rttyTermLimit        = 10
	rttyTermTimeout      = 600 * time.Second
	rttyHeartbeatTimeout = 3 * time.Second
)

type RttyClient struct {
	sessions sync.Map
	httpCons sync.Map

	conn             net.Conn
	cfg              Config
	ntty             int
	heartbeatTimer   *time.Timer
	lastHeartbeat    time.Time
	waitingHeartbeat bool
	mu               sync.Mutex

	msg  *proto.MsgReaderWriter
	stop atomic.Bool
}

var reconnectWait = func() time.Duration {
	return time.Duration(rand.IntN(10)+5) * time.Second
}

var msgHandlers = map[byte]func(*RttyClient, []byte) error{
	proto.MsgTypeHeartbeat: handleHeartbeatMsg,
	proto.MsgTypeLogin:     handleLoginMsg,
	proto.MsgTypeLogout:    handleLogoutMsg,
	proto.MsgTypeTermData:  handleTermDataMsg,
	proto.MsgTypeWinsize:   handleTermWinsizeMsg,
	proto.MsgTypeAck:       handleAckMsg,
	proto.MsgTypeFile:      handleFileMsg,
	proto.MsgTypeCmd:       handleCmdMsg,
	proto.MsgTypeHttp:      handleHttpMsg,
}

func (cli *RttyClient) Run() {
	for {
		if cli.stop.Load() {
			return
		}

		cli.run()

		if cli.stop.Load() || !cli.cfg.reconnect {
			break
		}

		delay := reconnectWait()
		log.Error().Msgf("Reconnecting in %v...", delay)
		time.Sleep(delay)
	}
}

func (cli *RttyClient) Stop() {
	cli.stop.Store(true)
	cli.Close()
}

func (cli *RttyClient) run() {
	defer cli.Close()

	err := cli.Connect()
	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to server")
		return
	}

	err = cli.Register()
	if err != nil {
		log.Error().Err(err).Msg("Failed to register with server")
		return
	}

	cli.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	typ, data, err := cli.ReadMsg()
	if err != nil {
		log.Error().Err(err).Msg("Failed to read register msg")
		return
	}

	if typ != proto.MsgTypeRegister {
		log.Error().Msgf("register msg expected first, got %s", proto.MsgTypeName(typ))
		return
	}

	regCode := data[0]
	if regCode != 0 {
		log.Error().Msgf("register failed: %s", string(data[1:]))
		return
	}

	log.Info().Msg("registered successfully")

	cli.conn.SetReadDeadline(time.Time{})

	cli.startHeartbeat()

	for {
		typ, data, err = cli.ReadMsg()
		if err != nil {
			log.Error().Err(err).Msg("Failed to read message")
			return
		}

		cli.mu.Lock()
		cli.waitingHeartbeat = false
		cli.mu.Unlock()

		log.Debug().Msgf("recv msg: %s", proto.MsgTypeName(typ))

		handler, ok := msgHandlers[typ]
		if !ok {
			log.Error().Msgf("unexpected message '%s'", proto.MsgTypeName(typ))
			return
		}

		err = handler(cli, data)
		if err != nil {
			log.Error().Err(err).Msgf("failed to handle message '%s'", proto.MsgTypeName(typ))
			return
		}
	}
}

func (cli *RttyClient) Connect() error {
	cfg := cli.cfg
	var conn net.Conn
	var err error

	addr := net.JoinHostPort(cfg.host, fmt.Sprintf("%d", cfg.port))

	if cfg.ssl {
		dialer := &net.Dialer{
			Timeout: 5 * time.Second,
		}

		tlsConfig := &tls.Config{
			InsecureSkipVerify: cfg.insecure,
		}

		if cfg.cacert != "" {
			caCert, err := os.ReadFile(cfg.cacert)
			if err != nil {
				return fmt.Errorf("load cacert fail: %w", err)
			}

			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)

			tlsConfig.RootCAs = caCertPool

		}

		if cfg.sslcert != "" && cfg.sslkey != "" {
			cert, err := tls.LoadX509KeyPair(cfg.sslcert, cfg.sslkey)
			if err != nil {
				return fmt.Errorf("load cert and key fail: %w", err)
			}

			tlsConfig.Certificates = []tls.Certificate{cert}
		}

		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = net.DialTimeout("tcp", addr, 5*time.Second)
	}

	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, conn)
	cli.conn = conn

	log.Info().Msgf("Connected to %s:%d", cfg.host, cfg.port)

	return nil
}

func (cli *RttyClient) ReadMsg() (byte, []byte, error) {
	return cli.msg.Read()
}

func (cli *RttyClient) WriteMsg(typ byte, data ...any) error {
	return cli.msg.Write(typ, data...)
}

func (cli *RttyClient) Register() error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	cfg := cli.cfg

	bb.WriteByte(rttyProtoVer)

	putMsgAttr(bb, proto.MsgRegAttrHeartbeat, cfg.heartbeat)
	putMsgAttr(bb, proto.MsgRegAttrDevid, cfg.id)

	if cfg.group != "" {
		putMsgAttr(bb, proto.MsgRegAttrGroup, cfg.group)
	}

	if cfg.description != "" {
		putMsgAttr(bb, proto.MsgRegAttrDescription, cfg.description)
	}

	if cfg.token != "" {
		putMsgAttr(bb, proto.MsgRegAttrToken, cfg.token)
	}

	return cli.WriteMsg(proto.MsgTypeRegister, bb)
}

func (cli *RttyClient) Close() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error().Interface("panic", rec).Msg("close panicked")
		}
	}()

	cli.mu.Lock()
	cli.waitingHeartbeat = false
	cli.ntty = 0
	if cli.heartbeatTimer != nil {
		cli.heartbeatTimer.Stop()
		cli.heartbeatTimer = nil
	}
	conn := cli.conn
	cli.mu.Unlock()

	if conn != nil {
		conn.Close()
	}

	cli.sessions.Range(func(key, value any) bool {
		s, ok := value.(*TermSession)
		if ok && s != nil {
			s.mu.Lock()
			if s.timer != nil {
				s.timer.Stop()
				s.timer = nil
			}
			s.mu.Unlock()

			if s.term != nil {
				s.term.Close()
			}
			if s.fc != nil {
				s.fc.reset()
			}
		}
		cli.sessions.Delete(key)
		return true
	})

	cli.httpCons.Range(func(key, value any) bool {
		if con, ok := value.(*RttyHttpConn); ok {
			con.closeLocal()
		}
		cli.httpCons.Delete(key)
		return true
	})
}

func (cli *RttyClient) startHeartbeat() {
	cli.mu.Lock()
	defer cli.mu.Unlock()

	cli.lastHeartbeat = time.Time{}
	cli.waitingHeartbeat = false

	heartbeatInterval := time.Duration(cli.cfg.heartbeat) * time.Second

	var timer *time.Timer
	timer = time.AfterFunc(heartbeatInterval, func() {
		cli.onHeartbeatTimer(timer, heartbeatInterval)
	})
	cli.heartbeatTimer = timer
}

func (cli *RttyClient) onHeartbeatTimer(timer *time.Timer, heartbeatInterval time.Duration) {
	cli.mu.Lock()
	if cli.heartbeatTimer != timer {
		cli.mu.Unlock()
		return
	}

	if cli.waitingHeartbeat {
		conn := cli.conn
		cli.mu.Unlock()
		log.Error().Msg("heartbeat timeout")
		if conn != nil {
			conn.Close()
		}
		return
	}

	elapsed := time.Since(cli.lastHeartbeat)
	if elapsed < heartbeatInterval {
		timer.Reset(heartbeatInterval - elapsed)
		cli.mu.Unlock()
		return
	}
	cli.mu.Unlock()

	uptime, _ := host.Uptime()

	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	putMsgAttr(bb, proto.MsgHeartbeatAttrUptime, uint32(uptime))
	err := cli.WriteMsg(proto.MsgTypeHeartbeat, bb)

	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.heartbeatTimer != timer {
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("send heartbeat fail")
		if cli.conn != nil {
			cli.conn.Close()
		}
		return
	}

	cli.lastHeartbeat = time.Now()
	cli.waitingHeartbeat = true
	timer.Reset(rttyHeartbeatTimeout)
	log.Debug().Msg("send msg: heartbeat")
}

func (cli *RttyClient) SendFileMsg(sid string, typ byte, data []byte) error {
	return cli.WriteMsg(proto.MsgTypeFile, sid, typ, data)
}

func (cli *RttyClient) SendHttpMsg(saddr [18]byte, data []byte) error {
	return cli.WriteMsg(proto.MsgTypeHttp, saddr[:], data)
}

func handleHeartbeatMsg(cli *RttyClient, data []byte) error {
	return nil
}

func handleLoginMsg(cli *RttyClient, data []byte) error {

	sid := string(data)

	var retCode byte

	cli.mu.Lock()
	if cli.ntty == rttyTermLimit {
		log.Error().Msgf("maximum number of TTYs reached: %d", cli.ntty)
		retCode = 1
	} else {
		term, err := NewTerminal(cli.cfg.username)
		if err != nil {
			log.Error().Err(err).Msg("failed to create terminal")
			retCode = 1
		} else {
			log.Info().Msgf("new tty: %d/%d %s", cli.ntty, rttyTermLimit, sid)

			s := &TermSession{
				cli:  cli,
				sid:  sid,
				term: term,
			}

			s.fc = &RttyFileContext{ses: s}

			cli.sessions.Store(sid, s)

			cli.ntty++

			go s.Run(cli)
		}
	}
	cli.mu.Unlock()

	cli.WriteMsg(proto.MsgTypeLogin, sid, retCode)

	return nil
}

func handleLogoutMsg(cli *RttyClient, data []byte) error {
	sid := string(data)

	if val, loaded := cli.sessions.LoadAndDelete(sid); loaded {
		log.Info().Msgf("delete tty %s", sid)
		s := val.(*TermSession)

		s.term.Close()

		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		cli.ntty--
		s.mu.Unlock()
	} else {
		log.Error().Msgf("tty session %s not found", sid)
		return nil
	}

	return nil
}

func handleTermDataMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])

	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("terminal session %s not found", sid)
		return nil
	}

	s := val.(*TermSession)
	s.term.Write(data[32:])
	s.active()

	return nil
}

func handleTermWinsizeMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])

	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("terminal session %s not found", sid)
		return nil
	}

	col := binary.BigEndian.Uint16(data[32:34])
	row := binary.BigEndian.Uint16(data[34:36])

	err := val.(*TermSession).term.SetWinSize(col, row)
	if err != nil {
		log.Error().Err(err).Msgf("failed to set terminal size for %s", sid)
		return err
	}

	log.Debug().Msgf("setting terminal %s size to %dx%d", sid, col, row)

	return nil
}

func handleAckMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])

	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("terminal session %s not found", sid)
		return nil
	}

	val.(*TermSession).term.Ack(binary.BigEndian.Uint16(data[32:34]))

	return nil
}

type TermSession struct {
	cli   *RttyClient
	sid   string
	term  *Terminal
	timer *time.Timer
	mu    sync.Mutex
	fc    *RttyFileContext
}

func (s *TermSession) Write(buf []byte) (int, error) {
	length := len(buf)

	s.active()

	if s.fc.detect(buf) {
		return length, nil
	}

	s.cli.WriteMsg(proto.MsgTypeTermData, s.sid, buf)

	s.term.WaitAck(length)

	return length, nil
}

func (s *TermSession) Run(cli *RttyClient) {
	s.mu.Lock()
	s.timer = time.AfterFunc(rttyTermTimeout, func() {
		log.Info().Msgf("tty %s inactive over %v, now kill it", s.sid, rttyTermTimeout)
		s.term.Close()
	})
	s.mu.Unlock()

	if _, err := io.Copy(s, s.term); err != nil {
		log.Error().Err(err).Msgf("error while copying terminal data for %s", s.sid)
	}
	s.close(cli)
}

func (s *TermSession) active() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.timer != nil {
		s.timer.Reset(rttyTermTimeout)
	}
}

func (s *TermSession) close(cli *RttyClient) {
	if _, loaded := cli.sessions.LoadAndDelete(s.sid); !loaded {
		return
	}

	cli.WriteMsg(proto.MsgTypeLogout, s.sid)

	s.term.Close()

	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	cli.ntty--
	s.mu.Unlock()

	log.Info().Msgf("delete tty %s", s.sid)
}

func putMsgAttr(bb *bytebufferpool.ByteBuffer, attrType byte, val any) {
	bb.WriteByte(attrType)

	lengthPos := bb.Len()
	length := 0

	bb.Write([]byte{0, 0}) // Placeholder for length

	switch v := val.(type) {
	case []byte:
		length, _ = bb.Write(v)
	case string:
		length, _ = bb.WriteString(v)
	case uint8:
		bb.WriteByte(v)
		length = 1
	case uint16:
		bb.WriteByte(0)
		bb.WriteByte(0)
		length = 2
		binary.BigEndian.PutUint16(bb.B[bb.Len()-2:], v)
	case uint32:
		bb.WriteByte(0)
		bb.WriteByte(0)
		bb.WriteByte(0)
		bb.WriteByte(0)
		length = 4
		binary.BigEndian.PutUint32(bb.B[bb.Len()-4:], v)
	default:
		panic(fmt.Sprintf("unsupported attribute type: %T", v))
	}

	binary.BigEndian.PutUint16(bb.B[lengthPos:], uint16(length))
}
