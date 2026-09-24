/* SPDX-License-Identifier: MIT */
package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
	"github.com/zhaojh329/rtty-go/tcpforward"
)

type tcpSession struct {
	cli     *RttyClient
	id      string
	ctx     context.Context
	cancel  context.CancelFunc
	forward *tcpforward.Forwarder
	mu      sync.Mutex
	once    sync.Once
}

func (cli *RttyClient) sendTCP(id string, op byte, body ...any) error {
	args := append([]any{id, op}, body...)
	return cli.WriteMsg(proto.MsgTypeTCP, args...)
}

func handleTCPMsg(cli *RttyClient, data []byte) error {
	if len(data) < 33 {
		return fmt.Errorf("short TCP message")
	}

	id, op := string(data[:32]), data[32]
	if op == proto.TCPTypeOpen {
		if len(data) != 39 {
			return fmt.Errorf("invalid TCP open message")
		}
		ctx, cancel := context.WithCancel(context.Background())
		s := &tcpSession{cli: cli, id: id, ctx: ctx, cancel: cancel}
		if _, loaded := cli.tcpCons.LoadOrStore(id, s); loaded {
			cancel()
			return fmt.Errorf("duplicate TCP connection ID")
		}
		ip := net.IP(data[33:37]).String()
		port := binary.BigEndian.Uint16(data[37:39])
		go s.run(net.JoinHostPort(ip, fmt.Sprint(port)))
		return nil
	}
	v, ok := cli.tcpCons.Load(id)
	if !ok {
		return nil
	}

	s := v.(*tcpSession)
	if op == proto.TCPTypeClose {
		if len(data) != 33 {
			return fmt.Errorf("invalid TCP close")
		}

		s.close(false)
		return nil
	}

	s.mu.Lock()
	forward := s.forward
	s.mu.Unlock()

	if forward == nil {
		s.close(true)
		return nil
	}

	if err := forward.Handle(op, data[33:]); err != nil {
		s.close(true)
	}

	return nil
}

func (s *tcpSession) close(notify bool) {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		if s.forward != nil {
			s.forward.Close()
		}
		s.mu.Unlock()
		s.cli.tcpCons.CompareAndDelete(s.id, s)
		if notify {
			s.cli.sendTCP(s.id, proto.TCPTypeClose)
		}
	})
}

func (s *tcpSession) run(addr string) {
	notify := true
	defer func() { s.close(notify) }()
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(s.ctx, "tcp", addr)
	if err != nil {
		if s.ctx.Err() == nil {
			s.cli.sendTCP(s.id, proto.TCPTypeOpenResult, proto.TCPOpenFailed)
			notify = false
		}
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		conn.Close()
		return
	}

	s.forward = tcpforward.New(conn, func(op byte, data []byte) error {
		return s.cli.sendTCP(s.id, op, data)
	})
	s.mu.Unlock()

	if s.cli.sendTCP(s.id, proto.TCPTypeOpenResult, proto.TCPOpenOK) != nil {
		return
	}

	notify = s.forward.Run() != nil
}
