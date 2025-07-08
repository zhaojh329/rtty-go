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
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

var rttyHttpCons sync.Map

type RttyHttpConn struct {
	active  atomic.Int64
	conn    net.Conn
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

	if v, ok := rttyHttpCons.Load(saddr); ok {
		conn := v.(*RttyHttpConn)
		conn.Write(data)
		return nil
	}

	go runHttpProxy(cli, isHttps, saddr, daddr, dport, data)

	return nil
}

func runHttpProxy(cli *RttyClient, isHttps bool, saddr [18]byte, daddr string, dport uint16, data []byte) {
	if isHttps {
	} else {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", daddr, dport), 3*time.Second)
		if err != nil {
			log.Error().Err(err).Msg("Failed to connect to target address")
			cli.SendHttpMsg(saddr, nil)
			return
		}

		c := &RttyHttpConn{
			closeCh: make(chan struct{}),
			conn:    conn,
		}

		c.active.Store(time.Now().Add(httpTimeOut).Unix())

		rttyHttpCons.Store(saddr, c)

		defer func() {
			rttyHttpCons.Delete(saddr)
			close(c.closeCh)
			conn.Close()
		}()

		conn.Write(data)

		buf := make([]byte, 1024*63)

		go c.activeCheck()

		for {
			n, _ := conn.Read(buf)
			cli.SendHttpMsg(saddr, buf[:n])
			if n == 0 {
				return
			}
			c.active.Store(time.Now().Add(httpTimeOut).Unix())
		}
	}
}

func (c *RttyHttpConn) Write(data []byte) (int, error) {
	c.active.Store(time.Now().Add(httpTimeOut).Unix())
	return c.conn.Write(data)
}

func (c *RttyHttpConn) activeCheck() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	for {
		select {
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
