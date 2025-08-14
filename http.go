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
	conn   net.Conn
	data   chan *bytebufferpool.ByteBuffer
	ctx    context.Context
	cancel context.CancelFunc
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
	httpTimeOut = 30 * time.Second
)

func handleHttpMsg(cli *RttyClient, data []byte) error {
	var saddr [18]byte

	isHttps := data[0] == 1

	copy(saddr[:], data[1:19])

	data = data[19:]

	daddr := net.IPv4(data[0], data[1], data[2], data[3]).String()
	dport := binary.BigEndian.Uint16(data[4:])
	data = data[6:]

	conn := &RttyHttpConn{
		data: make(chan *bytebufferpool.ByteBuffer, 100),
	}

	conn.ctx, conn.cancel = context.WithCancel(context.Background())

	bb := bytebufferpool.Get()
	bb.Write(data)

	if v, loaded := cli.httpCons.LoadOrStore(saddr, conn); loaded {
		conn := v.(*RttyHttpConn)
		conn.data <- bb
		return nil
	}

	conn.data <- bb

	go conn.run(cli, isHttps, saddr, daddr, dport)

	return nil
}

func (c *RttyHttpConn) run(cli *RttyClient, isHttps bool, saddr [18]byte, daddr string, dport uint16) {
	var conn net.Conn
	var err error

	addr := net.JoinHostPort(daddr, fmt.Sprintf("%d", dport))

	if isHttps {
		dialer := &net.Dialer{
			Timeout: 3 * time.Second,
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 3*time.Second)
	}

	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to target address")
		cli.SendHttpMsg(saddr, nil)
		return
	}

	c.conn = conn

	defer func() {
		cli.httpCons.Delete(saddr)
		c.cancel()
	}()

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
	return c.conn.Write(data)
}

func (c *RttyHttpConn) loop() {
	tick := time.NewTicker(5 * time.Second)
	defer func() {
		tick.Stop()
		c.conn.Close()

		for bb := range c.data {
			bytebufferpool.Put(bb)
		}
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
