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
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/valyala/bytebufferpool"
)

type RttyHttpConn struct {
	active  atomic.Int64
	conn    rttyConn
	data    chan *bytebufferpool.ByteBuffer
	closeCh chan struct{}
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

	if len(data) == 0 {
		log.Debug().Msg("Received empty HTTP message")
		return nil
	}

	conn := &RttyHttpConn{
		closeCh: make(chan struct{}),
		data:    make(chan *bytebufferpool.ByteBuffer, 100),
	}

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
	var conn rttyConn
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
		close(c.closeCh)
		conn.Close()
	}()

	go c.loop()

	buf := make([]byte, 1024*63)

	for {
		n, _ := conn.Read(buf)
		cli.SendHttpMsg(saddr, buf[:n])
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

		for bb := range c.data {
			bytebufferpool.Put(bb)
		}
	}()

	for {
		select {
		case bb := <-c.data:
			c.Write(bb.B)
			bytebufferpool.Put(bb)
		case <-tick.C:
			if time.Now().Unix() > c.active.Load() {
				c.conn.Close()
				return
			}
		case <-c.closeCh:
			return
		}
	}
}
