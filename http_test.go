/* SPDX-License-Identifier: MIT */

package main

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/valyala/bytebufferpool"
	"github.com/zhaojh329/rtty-go/proto"
)

func buildHttpMsgData(saddr [18]byte, ip net.IP, port uint16, payload []byte) []byte {
	ip4 := ip.To4()
	data := make([]byte, 1+18+4+2+len(payload))
	copy(data[1:19], saddr[:])
	copy(data[19:23], ip4)
	binary.BigEndian.PutUint16(data[23:25], port)
	copy(data[25:], payload)
	return data
}

func TestHandleHttpMsgDoesNotBlockWhenBacklogFull(t *testing.T) {
	cli := &RttyClient{}
	saddr := [18]byte{0x7f, 0, 0, 1, 0, 80}

	conn := newRttyHttpConn()
	for i := 0; i < cap(conn.data); i++ {
		bb := bytebufferpool.Get()
		bb.WriteString("x")
		conn.data <- bb
	}
	cli.httpCons.Store(saddr, conn)

	payload := buildHttpMsgData(saddr, net.IPv4(127, 0, 0, 1), 80, []byte("more"))

	done := make(chan error, 1)
	go func() {
		done <- handleHttpMsg(cli, payload)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleHttpMsg: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleHttpMsg blocked on full http backlog")
	}

	select {
	case <-conn.ctx.Done():
	default:
		t.Fatal("expected http conn to be cancelled when backlog is full")
	}
}

func TestHandleHttpMsgDialFailCleansMap(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	go io.Copy(io.Discard, server)

	cli := &RttyClient{
		msg: proto.NewMsgReaderWriter(proto.RoleRtty, client),
	}

	saddr := [18]byte{9, 9, 9, 9}
	payload := buildHttpMsgData(saddr, net.IPv4(127, 0, 0, 1), 1, []byte("GET / HTTP/1.0\r\n\r\n"))
	if err := handleHttpMsg(cli, payload); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		cli.httpCons.Range(func(k, v any) bool {
			found = true
			return false
		})
		if !found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("http connection was not removed after dial failure")
}
