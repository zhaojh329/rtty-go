/*
 * MIT License
 *
 * Copyright (c) 2025 Jianhui Zhao <zhaojh329@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/valyala/bytebufferpool"
)

const (
	MsgTypeRegister = byte(iota)
	MsgTypeLogin
	MsgTypeLogout
	MsgTypeTermData
	MsgTypeWinsize
	MsgTypeCmd
	MsgTypeHeartbeat
	MsgTypeFile
	MsgTypeHttp
	MsgTypeAck
)

const (
	MsgTypeFileSend = byte(iota)
	MsgTypeFileRecv
	MsgTypeFileInfo
	MsgTypeFileData
	MsgTypeFileAck
	MsgTypeFileAbort
)

const (
	MsgRegAttrHeartbeat = byte(iota)
	MsgRegAttrDevid
	MsgRegAttrDescription
	MsgRegAttrToken
	MsgRegAttrGroup
)

const (
	MsgHeartbeatAttrUptime = byte(iota)
)

const (
	rttyProtoVer         = byte(5)
	rttyTermLimit        = 10
	rttyTermTimeout      = 600 * time.Second
	rttyHeartbeatTimeout = 3 * time.Second
)

type RttyClient struct {
	sessions         sync.Map
	conn             rttyConn
	cfg              Config
	ntty             int
	heartbeatTimer   *time.Timer
	lastHeartbeat    time.Time
	waitingHeartbeat bool
	mu               sync.Mutex
	br               *bufio.Reader
	head             [3]byte
	buf              []byte
}

type rttyConn interface {
	Read(b []byte) (n int, err error)
	Write(b []byte) (n int, err error)
	Close() error
	SetReadDeadline(t time.Time) error
}

var fixedMsgLens = map[byte]int{
	MsgTypeRegister: 1,
	MsgTypeLogin:    32,
	MsgTypeLogout:   32,
	MsgTypeTermData: 32,
	MsgTypeWinsize:  36,
	MsgTypeFile:     33,
	MsgTypeAck:      34,
	MsgTypeHttp:     25,
}

var msgHandlers = map[byte]func(*RttyClient, []byte) error{
	MsgTypeHeartbeat: handleHeartbeatMsg,
	MsgTypeLogin:     handleLoginMsg,
	MsgTypeLogout:    handleLogoutMsg,
	MsgTypeTermData:  handleTermDataMsg,
	MsgTypeWinsize:   handleTermWinsizeMsg,
	MsgTypeAck:       handleAckMsg,
	MsgTypeFile:      handleFileMsg,
	MsgTypeCmd:       handleCmdMsg,
	MsgTypeHttp:      handleHttpMsg,
}

func (cli *RttyClient) Run() {
	defer func() {
		cli.Close()

		if cli.cfg.reconnect {
			delay := rand.IntN(10) + 5
			log.Error().Msgf("Reconnecting in %d seconds...", delay)
			time.Sleep(time.Duration(delay) * time.Second)
			cli.Run()
		}
	}()

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

	if typ != MsgTypeRegister {
		log.Error().Msgf("register msg expected first, got %s", msgTypeName(typ))
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

		log.Debug().Msgf("recv msg: %s", msgTypeName(typ))

		handler, ok := msgHandlers[typ]
		if !ok {
			log.Error().Msgf("unexpected message '%s'", msgTypeName(typ))
			return
		}

		err = handler(cli, data)
		if err != nil {
			log.Error().Err(err).Msgf("failed to handle message '%s'", msgTypeName(typ))
			return
		}

		cli.waitingHeartbeat = false
	}
}

func (cli *RttyClient) Connect() error {
	var conn rttyConn
	var err error

	addr := cli.cfg.host + ":" + fmt.Sprint(cli.cfg.port)

	if cli.cfg.ssl {
		dialer := &net.Dialer{
			Timeout: 5 * time.Second,
		}

		tlsConfig := &tls.Config{
			InsecureSkipVerify: cli.cfg.insecure,
		}

		if cli.cfg.cacert != "" {
			caCert, err := os.ReadFile(cli.cfg.cacert)
			if err != nil {
				return fmt.Errorf("load cacert fail: %w", err)
			}

			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)

			tlsConfig.RootCAs = caCertPool

		}

		if cli.cfg.sslcert != "" && cli.cfg.sslkey != "" {
			cert, err := tls.LoadX509KeyPair(cli.cfg.sslcert, cli.cfg.sslkey)
			if err != nil {
				return fmt.Errorf("load cert and key fail: %w", err)
			}

			tlsConfig.Certificates = []tls.Certificate{cert}
		}

		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
		if err != nil {
			return fmt.Errorf("failed to connect to %s:%d: %w", cli.cfg.host, cli.cfg.port, err)
		}
	} else {
		conn, err = net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to %s:%d: %w", cli.cfg.host, cli.cfg.port, err)
		}
	}

	cli.br = bufio.NewReader(conn)
	cli.buf = make([]byte, 0, 4096)
	cli.conn = conn

	log.Info().Msgf("Connected to %s:%d", cli.cfg.host, cli.cfg.port)

	return nil
}

func (cli *RttyClient) Register() error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	cfg := cli.cfg

	bb.WriteByte(rttyProtoVer)

	putMsgAttr(bb, MsgRegAttrHeartbeat, cfg.heartbeat)
	putMsgAttr(bb, MsgRegAttrDevid, cfg.id)

	if cfg.group != "" {
		putMsgAttr(bb, MsgRegAttrGroup, cfg.group)
	}

	if cfg.description != "" {
		putMsgAttr(bb, MsgRegAttrDescription, cfg.description)
	}

	if cfg.token != "" {
		putMsgAttr(bb, MsgRegAttrToken, cfg.token)
	}

	return cli.SendMsg(MsgTypeRegister, bb)
}

func (cli *RttyClient) ReadMsg() (byte, []byte, error) {
	_, err := io.ReadFull(cli.br, cli.head[:])
	if err != nil {
		return 0, nil, err
	}

	typ := cli.head[0]
	msgLen := binary.BigEndian.Uint16(cli.head[1:])

	if fixedMsgLen, ok := fixedMsgLens[typ]; ok {
		if msgLen < uint16(fixedMsgLen) {
			return 0, nil, fmt.Errorf("invalid message length for %s: at least %d, got %d",
				msgTypeName(typ), fixedMsgLen, msgLen)
		}
	}

	if cap(cli.buf) < int(msgLen) {
		cli.buf = make([]byte, msgLen)
	} else {
		cli.buf = cli.buf[:msgLen]
	}

	_, err = io.ReadFull(cli.br, cli.buf)
	if err != nil {
		return 0, nil, err
	}

	return typ, cli.buf, nil
}

func (cli *RttyClient) Close() {
	cli.mu.Lock()
	if cli.heartbeatTimer != nil {
		cli.heartbeatTimer.Stop()
		cli.heartbeatTimer = nil
	}
	cli.mu.Unlock()

	cli.sessions.Range(func(key, value any) bool {
		s := value.(*TermSession)

		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		s.mu.Unlock()

		s.term.Close()
		s.fc.reset()
		cli.sessions.Delete(key)
		return true
	})
}

func (cli *RttyClient) startHeartbeat() {
	cli.mu.Lock()
	defer cli.mu.Unlock()

	cli.lastHeartbeat = time.Time{}

	heartbeatInterval := time.Duration(cli.cfg.heartbeat) * time.Second

	cli.heartbeatTimer = time.AfterFunc(heartbeatInterval, func() {
		if cli.waitingHeartbeat {
			log.Error().Msg("heartbeat timeout")
			cli.conn.Close()
			return
		}

		elapsed := time.Since(cli.lastHeartbeat)

		if elapsed < heartbeatInterval {
			cli.heartbeatTimer.Reset(heartbeatInterval - elapsed)
		} else {
			uptime, _ := host.Uptime()

			bb := bytebufferpool.Get()
			defer bytebufferpool.Put(bb)

			putMsgAttr(bb, MsgHeartbeatAttrUptime, uint32(uptime))
			cli.SendMsg(MsgTypeHeartbeat, bb)

			cli.lastHeartbeat = time.Now()
			cli.waitingHeartbeat = true
			cli.heartbeatTimer.Reset(rttyHeartbeatTimeout)
			log.Debug().Msg("send msg: heartbeat")
		}
	})
}

func (cli *RttyClient) SendFileMsg(sid string, typ byte, data []byte) error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	bb.WriteString(sid)
	bb.WriteByte(typ)

	if data != nil {
		bb.Write(data)
	}

	return cli.SendMsg(MsgTypeFile, bb)
}

func (cli *RttyClient) SendHttpMsg(saddr [18]byte, data []byte) error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	bb.Write(saddr[:])

	if data != nil {
		bb.Write(data)
	}

	return cli.SendMsg(MsgTypeHttp, bb)
}

func (cli *RttyClient) SendMsg(typ byte, data any) error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	var length int

	bb.WriteByte(typ)
	bb.Write([]byte{0, 0})

	switch v := data.(type) {
	case []byte:
		length, _ = bb.Write(v)
	case string:
		length, _ = bb.WriteString(v)
	case *bytebufferpool.ByteBuffer:
		length, _ = bb.Write(v.B)
	default:
		return fmt.Errorf("unsupported data type: %T", v)
	}

	binary.BigEndian.PutUint16(bb.B[1:], uint16(length))

	_, err := bb.WriteTo(cli.conn)

	return err
}

func handleHeartbeatMsg(cli *RttyClient, data []byte) error {
	return nil
}

func handleLoginMsg(cli *RttyClient, data []byte) error {

	sid := string(data)

	var retCode byte

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

			go s.Run(cli)
		}
	}

	cli.ntty++

	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	bb.WriteString(sid)
	bb.WriteByte(retCode)

	cli.SendMsg(MsgTypeLogin, bb)

	return nil
}

func handleLogoutMsg(cli *RttyClient, data []byte) error {
	sid := string(data)

	if val, loaded := cli.sessions.LoadAndDelete(sid); loaded {
		log.Info().Msgf("delete tty %s", sid)
		s := val.(*TermSession)

		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		s.mu.Unlock()

		s.term.Close()
		cli.ntty--
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

	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	bb.WriteString(s.sid)
	bb.Write(buf)

	s.cli.SendMsg(MsgTypeTermData, bb)

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

	io.Copy(s, s.term)
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

	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()

	cli.SendMsg(MsgTypeLogout, s.sid)

	s.term.Close()

	cli.ntty--

	log.Info().Msgf("delete tty %s", s.sid)
}

func msgTypeName(typ byte) string {
	switch typ {
	case MsgTypeRegister:
		return "register"
	case MsgTypeLogin:
		return "login"
	case MsgTypeLogout:
		return "logout"
	case MsgTypeTermData:
		return "termdata"
	case MsgTypeWinsize:
		return "winsize"
	case MsgTypeCmd:
		return "cmd"
	case MsgTypeHeartbeat:
		return "heartbeat"
	case MsgTypeFile:
		return "file"
	case MsgTypeHttp:
		return "http"
	case MsgTypeAck:
		return "ack"
	default:
		return fmt.Sprintf("unknown(%d)", typ)
	}
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
		data := make([]byte, 2)
		binary.BigEndian.PutUint16(data, v)
		length, _ = bb.Write(data)
	case uint32:
		data := make([]byte, 4)
		binary.BigEndian.PutUint32(data, v)
		length, _ = bb.Write(data)
	default:
		panic(fmt.Sprintf("unsupported attribute type: %T", v))
	}

	binary.BigEndian.PutUint16(bb.B[lengthPos:], uint16(length))
}
