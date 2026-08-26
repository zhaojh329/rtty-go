/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/valyala/bytebufferpool"
)

type RttyHttpConn struct {
	active atomic.Int64
	data   chan *bytebufferpool.ByteBuffer
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	conn net.Conn
}

var httpBufPool = sync.Pool{
	New: func() any {
		return &HttpBuf{
			buf: make([]byte, 1024*32),
		}
	},
}

type HttpBuf struct {
	buf []byte
}

const (
	httpTimeOut     = 30 * time.Second
	httpDataBacklog = 100
)

func newRttyHttpConn() *RttyHttpConn {
	c := &RttyHttpConn{
		data: make(chan *bytebufferpool.ByteBuffer, httpDataBacklog),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.active.Store(time.Now().Add(httpTimeOut).Unix())
	return c
}

func handleHttpMsg(cli *RttyClient, data []byte) error {
	var saddr [18]byte

	isHttps := data[0] == 1

	copy(saddr[:], data[1:19])

	data = data[19:]

	daddr := net.IPv4(data[0], data[1], data[2], data[3]).String()
	dport := binary.BigEndian.Uint16(data[4:])
	data = data[6:]

	var bb *bytebufferpool.ByteBuffer

	if len(data) > 0 {
		bb = bytebufferpool.Get()
		bb.Write(data)
	}

	if v, loaded := cli.httpCons.Load(saddr); loaded {
		conn := v.(*RttyHttpConn)
		if bb == nil {
			conn.closeLocal()
			return nil
		}
		conn.enqueue(bb)
		return nil
	}

	if bb == nil {
		return nil
	}

	conn := newRttyHttpConn()
	if v, loaded := cli.httpCons.LoadOrStore(saddr, conn); loaded {
		conn.cancel()
		existing := v.(*RttyHttpConn)
		existing.enqueue(bb)
		return nil
	}

	if !conn.enqueue(bb) {
		cli.httpCons.Delete(saddr)
		conn.cancel()
		return nil
	}

	go conn.run(cli, isHttps, saddr, daddr, dport)

	return nil
}

func (c *RttyHttpConn) enqueue(bb *bytebufferpool.ByteBuffer) bool {
	select {
	case <-c.ctx.Done():
		bytebufferpool.Put(bb)
		return false
	default:
	}

	select {
	case c.data <- bb:
		return true
	case <-c.ctx.Done():
		bytebufferpool.Put(bb)
		return false
	default:
		bytebufferpool.Put(bb)
		log.Warn().Msg("http proxy backlog full, drop connection")
		c.closeLocal()
		return false
	}
}

func (c *RttyHttpConn) closeLocal() {
	if c == nil {
		return
	}
	if cancel := c.cancel; cancel != nil {
		cancel()
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (c *RttyHttpConn) setConn(conn net.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *RttyHttpConn) drain() {
	for {
		select {
		case bb := <-c.data:
			if bb != nil {
				bytebufferpool.Put(bb)
			}
		default:
			return
		}
	}
}

func (c *RttyHttpConn) run(cli *RttyClient, isHttps bool, saddr [18]byte, daddr string, dport uint16) {
	defer func() {
		cli.httpCons.Delete(saddr)
		c.closeLocal()
		c.drain()
	}()

	addr := net.JoinHostPort(daddr, fmt.Sprintf("%d", dport))

	dialer := &net.Dialer{
		Timeout: 3 * time.Second,
	}

	var conn net.Conn
	var err error

	if isHttps {
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{InsecureSkipVerify: true},
		}
		conn, err = tlsDialer.DialContext(c.ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(c.ctx, "tcp", addr)
	}

	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to target address")
		cli.SendHttpMsg(saddr, nil)
		return
	}

	select {
	case <-c.ctx.Done():
		conn.Close()
		return
	default:
	}

	c.setConn(conn)

	go c.loop()

	hb := httpBufPool.Get().(*HttpBuf)
	defer httpBufPool.Put(hb)

	for {
		n, _ := conn.Read(hb.buf)
		err := cli.SendHttpMsg(saddr, hb.buf[:n])
		if err != nil {
			log.Error().Err(err).Msg("send http msg fail")
			return
		}
		if n == 0 {
			return
		}
		c.active.Store(time.Now().Add(httpTimeOut).Unix())
	}
}

func (c *RttyHttpConn) Write(data []byte) (int, error) {
	c.active.Store(time.Now().Add(httpTimeOut).Unix())

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return 0, net.ErrClosed
	}

	_ = conn.SetWriteDeadline(time.Now().Add(httpTimeOut))
	return conn.Write(data)
}

func (c *RttyHttpConn) loop() {
	tick := time.NewTicker(5 * time.Second)
	defer func() {
		tick.Stop()
		c.closeLocal()
		c.drain()
	}()

	for {
		select {
		case bb := <-c.data:
			_, err := c.Write(bb.B)
			bytebufferpool.Put(bb)
			if err != nil {
				return
			}
		case <-tick.C:
			if time.Now().Unix() > c.active.Load() {
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}
