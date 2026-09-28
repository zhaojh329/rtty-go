/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"context"
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
	rttyProtoVer    = byte(5)
	rttyTermLimit   = 10
	rttyTermTimeout = 600 * time.Second
)

type RttyClient struct {
	sessions sync.Map
	httpCons sync.Map

	conn           net.Conn
	cfg            Config
	ntty           int
	heartbeatTimer *time.Timer
	lastHeartbeat  time.Time
	// Send time and recvSeq snapshot of the oldest unanswered heartbeat.
	pendingSince time.Time
	pendingSeq   uint64
	recvSeq      atomic.Uint64
	mu           sync.Mutex

	msg       *proto.MsgReaderWriter
	stop      atomic.Bool
	closed    atomic.Bool
	closeOnce sync.Once
	writeMu   sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	// Run owns a fresh client per TCP connection. Background work must never
	// acquire the next connection through a reused client pointer.
	active      *RttyClient
	stopCh      chan struct{}
	lastReceive atomic.Int64
	lastSend    atomic.Int64
	handlerType atomic.Int32
}

var reconnectWait = func() time.Duration {
	return time.Duration(rand.IntN(10)+5) * time.Second
}

// The link is shared by terminal, file and HTTP proxy traffic, so a heartbeat
// reply may queue behind bulk data; keep the timeout well above one RTT.
var heartbeatTimeoutMin = 10 * time.Second

func heartbeatTimeout(interval time.Duration) time.Duration {
	// Leave time for a second probe before declaring the server unresponsive.
	return max(2*interval, heartbeatTimeoutMin)
}

const rttyWriteTimeout = 10 * time.Second

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
	cli.mu.Lock()
	if cli.stopCh == nil {
		cli.stopCh = make(chan struct{})
	}
	stopCh := cli.stopCh
	cli.mu.Unlock()
	for {
		cli.mu.Lock()
		if cli.stop.Load() {
			cli.mu.Unlock()
			return
		}
		attempt := &RttyClient{cfg: cli.cfg}
		cli.active = attempt
		cli.mu.Unlock()
		attempt.run()
		cli.mu.Lock()
		cli.active = nil
		cli.mu.Unlock()

		if cli.stop.Load() || !cli.cfg.reconnect {
			break
		}

		delay := reconnectWait()
		log.Error().Msgf("Reconnecting in %v...", delay)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-stopCh:
			timer.Stop()
			return
		}
	}
}

func (cli *RttyClient) Stop() {
	cli.mu.Lock()
	if !cli.stop.Swap(true) && cli.stopCh != nil {
		close(cli.stopCh)
	}
	active := cli.active
	cli.mu.Unlock()
	if active != nil {
		active.Stop()
	}
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

		cli.recvSeq.Add(1)
		cli.lastReceive.Store(time.Now().UnixNano())

		log.Debug().Msgf("recv msg: %s", proto.MsgTypeName(typ))

		handler, ok := msgHandlers[typ]
		if !ok {
			log.Error().Msgf("unexpected message '%s'", proto.MsgTypeName(typ))
			return
		}

		cli.handlerType.Store(int32(typ) + 1)
		err = handler(cli, data)
		cli.handlerType.Store(0)
		if err != nil {
			log.Error().Err(err).Msgf("failed to handle message '%s'", proto.MsgTypeName(typ))
			return
		}
	}
}

func (cli *RttyClient) Connect() error {
	cli.mu.Lock()
	if cli.closed.Load() || cli.stop.Load() {
		cli.mu.Unlock()
		return net.ErrClosed
	}
	cli.ctx, cli.cancel = context.WithCancel(context.Background())
	ctx := cli.ctx
	cli.mu.Unlock()
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

		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
		conn, err = tlsDialer.DialContext(ctx, "tcp", addr)
	} else {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}

	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	cli.mu.Lock()
	if cli.closed.Load() || cli.stop.Load() {
		cli.mu.Unlock()
		conn.Close()
		return net.ErrClosed
	}
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, conn)
	cli.conn = conn
	cli.mu.Unlock()

	log.Info().Msgf("Connected to %s:%d", cfg.host, cfg.port)

	return nil
}

func (cli *RttyClient) ReadMsg() (byte, []byte, error) {
	return cli.msg.Read()
}

func (cli *RttyClient) WriteMsg(typ byte, data ...any) error {
	cli.writeMu.Lock()
	defer cli.writeMu.Unlock()
	cli.mu.Lock()
	conn, msg := cli.conn, cli.msg
	cli.mu.Unlock()
	if cli.closed.Load() || conn == nil || msg == nil {
		return net.ErrClosed
	}
	if err := conn.SetWriteDeadline(time.Now().Add(rttyWriteTimeout)); err != nil {
		conn.Close()
		return err
	}
	if err := msg.Write(typ, data...); err != nil {
		log.Error().Err(err).Str("msg_type", proto.MsgTypeName(typ)).Msg("server write failed, closing connection")
		conn.Close()
		return err
	}
	cli.lastSend.Store(time.Now().UnixNano())
	return conn.SetWriteDeadline(time.Time{})
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
	cli.closeOnce.Do(cli.closeConnection)
}

func (cli *RttyClient) closeConnection() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error().Interface("panic", rec).Msg("close panicked")
		}
	}()

	cli.mu.Lock()
	cli.closed.Store(true)
	cli.pendingSince = time.Time{}
	cli.ntty = 0
	if cli.heartbeatTimer != nil {
		cli.heartbeatTimer.Stop()
		cli.heartbeatTimer = nil
	}
	conn := cli.conn
	cancel := cli.cancel
	cli.mu.Unlock()
	if cancel != nil {
		cancel()
	}

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
	if cli.closed.Load() {
		return
	}

	cli.lastHeartbeat = time.Now()
	cli.lastReceive.Store(cli.lastHeartbeat.UnixNano())
	cli.pendingSince = time.Time{}

	heartbeatInterval := time.Duration(cli.cfg.heartbeat) * time.Second

	var timer *time.Timer
	timer = time.AfterFunc(heartbeatInterval, func() {
		cli.onHeartbeatTimer(timer, heartbeatInterval)
	})
	cli.heartbeatTimer = timer
}

func (cli *RttyClient) onHeartbeatTimer(timer *time.Timer, heartbeatInterval time.Duration) {
	timeout := heartbeatTimeout(heartbeatInterval)

	cli.mu.Lock()
	if cli.heartbeatTimer != timer {
		cli.mu.Unlock()
		return
	}

	now := time.Now()

	if !cli.pendingSince.IsZero() {
		if cli.recvSeq.Load() != cli.pendingSeq {
			cli.pendingSince = time.Time{}
		} else if now.Sub(cli.pendingSince) >= timeout {
			conn := cli.conn
			cli.mu.Unlock()
			event := log.Error().Dur("timeout", timeout).
				Dur("since_last_receive", time.Since(time.Unix(0, cli.lastReceive.Load())))
			if sent := cli.lastSend.Load(); sent != 0 {
				event = event.Dur("since_last_send", time.Since(time.Unix(0, sent)))
			}
			if handling := cli.handlerType.Load(); handling != 0 {
				event = event.Str("handling", proto.MsgTypeName(byte(handling-1)))
			}
			event.Msg("heartbeat timeout, no server message consumed after probes")
			if conn != nil {
				conn.Close()
			}
			return
		}
	}

	send := now.Sub(cli.lastHeartbeat) >= heartbeatInterval
	if send {
		cli.lastHeartbeat = now
		// Must be recorded before writing, the reply may be read before WriteMsg returns.
		if cli.pendingSince.IsZero() {
			cli.pendingSince = now
			cli.pendingSeq = cli.recvSeq.Load()
		}
	}

	next := cli.lastHeartbeat.Add(heartbeatInterval)
	if !cli.pendingSince.IsZero() {
		if deadline := cli.pendingSince.Add(timeout); deadline.Before(next) {
			next = deadline
		}
	}
	timer.Reset(time.Until(next))
	cli.mu.Unlock()

	if !send {
		return
	}

	uptime, _ := host.Uptime()

	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	putMsgAttr(bb, proto.MsgHeartbeatAttrUptime, uint32(uptime))
	err := cli.WriteMsg(proto.MsgTypeHeartbeat, bb)
	if err != nil {
		cli.mu.Lock()
		stale := cli.heartbeatTimer != timer
		conn := cli.conn
		cli.mu.Unlock()
		if !stale {
			log.Error().Err(err).Msg("send heartbeat fail")
			if conn != nil {
				conn.Close()
			}
		}
		return
	}

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
	if cli.ntty >= rttyTermLimit || cli.closed.Load() {
		log.Error().Msgf("maximum number of TTYs reached: %d", cli.ntty)
		retCode = 1
		cli.mu.Unlock()
	} else {
		cli.ntty++
		cli.mu.Unlock()
		// Starting login may involve slow local resources. Do not hold the
		// heartbeat/connection mutex while doing it.
		term, err := NewTerminal(cli.cfg.username)
		if err != nil {
			log.Error().Err(err).Msg("failed to create terminal")
			retCode = 1
			cli.mu.Lock()
			if cli.ntty > 0 {
				cli.ntty--
			}
			cli.mu.Unlock()
		} else {
			s := &TermSession{
				cli:  cli,
				sid:  sid,
				term: term,
			}

			s.fc = &RttyFileContext{ses: s}
			cli.mu.Lock()
			if cli.closed.Load() {
				retCode = 1
				cli.mu.Unlock()
				term.Close()
			} else {
				cli.sessions.Store(sid, s)
				log.Info().Msgf("new tty: %d/%d %s", cli.ntty, rttyTermLimit, sid)
				cli.mu.Unlock()
				go s.Run(cli)
			}
		}
	}

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
		s.mu.Unlock()
		cli.mu.Lock()
		if cli.ntty > 0 {
			cli.ntty--
		}
		cli.mu.Unlock()
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

	if err := s.cli.WriteMsg(proto.MsgTypeTermData, s.sid, buf); err != nil {
		return 0, err
	}

	s.term.WaitAck(length)

	return length, nil
}

func (s *TermSession) Run(cli *RttyClient) {
	s.mu.Lock()
	if cli.closed.Load() {
		s.mu.Unlock()
		return
	}
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
	if !cli.sessions.CompareAndDelete(s.sid, s) {
		return
	}

	cli.WriteMsg(proto.MsgTypeLogout, s.sid)

	s.term.Close()

	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	cli.mu.Lock()
	if cli.ntty > 0 {
		cli.ntty--
	}
	cli.mu.Unlock()

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
