/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package client

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/valyala/bytebufferpool"
	"github.com/zhaojh329/rtty-go/proto"
)

const (
	rttyProtoVer         = byte(6)
	rttyTermLimit        = 10
	rttyHeartbeatTimeout = 3 * time.Second
)

type Session interface {
	stop() bool
	writeInput([]byte)
	ack(uint16)
}

type RttyClient struct {
	sessions sync.Map
	httpCons sync.Map

	conn             net.Conn
	cfg              Config
	ntty             int
	nserial          int
	heartbeatTimer   *time.Timer
	lastHeartbeat    time.Time
	waitingHeartbeat bool
	mu               sync.Mutex
	writeMu          sync.Mutex

	msg *proto.MsgReaderWriter
}

func New(cfg Config) *RttyClient {
	return &RttyClient{cfg: cfg}
}

var msgHandlers = map[byte]func(*RttyClient, []byte) error{
	proto.MsgTypeHeartbeat:   handleHeartbeatMsg,
	proto.MsgTypeLogin:       handleTermLoginMsg,
	proto.MsgTypeTermData:    handleTermDataMsg,
	proto.MsgTypeWinsize:     handleTermWinsizeMsg,
	proto.MsgTypeAck:         handleAckMsg,
	proto.MsgTypeFile:        handleFileMsg,
	proto.MsgTypeCmd:         handleCmdMsg,
	proto.MsgTypeHttp:        handleHttpMsg,
	proto.MsgTypeSerialPorts: handleSerialPortsMsg,
	proto.MsgTypeSerialOpen:  handleSerialOpenMsg,
	proto.MsgTypeLogout:      handleLogoutMsg,
}

func (cli *RttyClient) Run() {
	for {
		cli.run()

		if !cli.cfg.Reconnect {
			break
		}

		delay := rand.IntN(10) + 5
		log.Error().Msgf("Reconnecting in %d seconds...", delay)
		time.Sleep(time.Duration(delay) * time.Second)
	}
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

		cli.mu.Lock()
		cli.waitingHeartbeat = false
		cli.mu.Unlock()
	}
}

func (cli *RttyClient) Connect() error {
	cfg := cli.cfg
	var conn net.Conn
	var err error

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))

	if cfg.SSL {
		dialer := &net.Dialer{
			Timeout: 5 * time.Second,
		}

		tlsConfig := &tls.Config{
			InsecureSkipVerify: cfg.Insecure,
		}

		if cfg.CACert != "" {
			caCert, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return fmt.Errorf("load cacert fail: %w", err)
			}

			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)

			tlsConfig.RootCAs = caCertPool

		}

		if cfg.SSLCert != "" && cfg.SSLKey != "" {
			cert, err := tls.LoadX509KeyPair(cfg.SSLCert, cfg.SSLKey)
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

	log.Info().Msgf("Connected to %s:%d", cfg.Host, cfg.Port)

	return nil
}

func (cli *RttyClient) ReadMsg() (byte, []byte, error) {
	return cli.msg.Read()
}

func (cli *RttyClient) WriteMsg(typ byte, data ...any) error {
	cli.writeMu.Lock()
	defer cli.writeMu.Unlock()

	return cli.msg.Write(typ, data...)
}

func (cli *RttyClient) Register() error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	cfg := cli.cfg

	bb.WriteByte(rttyProtoVer)

	putMsgAttr(bb, proto.MsgRegAttrHeartbeat, cfg.Heartbeat)
	putMsgAttr(bb, proto.MsgRegAttrDevid, cfg.ID)

	if cfg.Group != "" {
		putMsgAttr(bb, proto.MsgRegAttrGroup, cfg.Group)
	}

	if cfg.Description != "" {
		putMsgAttr(bb, proto.MsgRegAttrDescription, cfg.Description)
	}

	if cfg.Token != "" {
		putMsgAttr(bb, proto.MsgRegAttrToken, cfg.Token)
	}

	return cli.WriteMsg(proto.MsgTypeRegister, bb)
}

func (cli *RttyClient) Close() {
	cli.mu.Lock()
	cli.waitingHeartbeat = false
	if cli.heartbeatTimer != nil {
		cli.heartbeatTimer.Stop()
		cli.heartbeatTimer = nil
	}
	cli.mu.Unlock()

	if cli.conn != nil {
		cli.conn.Close()
	}

	cli.sessions.Range(func(_, value any) bool {
		value.(Session).stop()
		return true
	})

	cli.httpCons.Range(func(key, value any) bool {
		con := value.(*RttyHttpConn)
		con.cancel()
		return true
	})
}

func (cli *RttyClient) startHeartbeat() {
	cli.mu.Lock()
	defer cli.mu.Unlock()

	cli.lastHeartbeat = time.Time{}

	heartbeatInterval := time.Duration(cli.cfg.Heartbeat) * time.Second
	conn := cli.conn

	var timer *time.Timer
	timer = time.AfterFunc(heartbeatInterval, func() {
		cli.mu.Lock()
		if cli.heartbeatTimer != timer {
			cli.mu.Unlock()
			return
		}

		if cli.waitingHeartbeat {
			cli.mu.Unlock()
			log.Error().Msg("heartbeat timeout")
			conn.Close()
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

		cli.mu.Lock()
		if cli.heartbeatTimer != timer {
			cli.mu.Unlock()
			return
		}
		cli.lastHeartbeat = time.Now()
		cli.waitingHeartbeat = true
		timer.Reset(rttyHeartbeatTimeout)
		cli.mu.Unlock()

		cli.WriteMsg(proto.MsgTypeHeartbeat, bb)
		log.Debug().Msg("send msg: heartbeat")
	})
	cli.heartbeatTimer = timer
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

func handleLogoutMsg(cli *RttyClient, data []byte) error {
	sid := string(data)
	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("session %s not found", sid)
		return nil
	}

	val.(Session).stop()

	return nil
}

func handleTermDataMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])
	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("session %s not found", sid)
		return nil
	}

	val.(Session).writeInput(data[32:])

	return nil
}

func handleAckMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])
	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("session %s not found", sid)
		return nil
	}

	val.(Session).ack(binary.BigEndian.Uint16(data[32:34]))

	return nil
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
