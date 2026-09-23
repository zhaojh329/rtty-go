package client

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/valyala/bytebufferpool"
	"github.com/zhaojh329/rtty-go/proto"
)

func TestHttpDialFailureRemovesConnection(t *testing.T) {
	cli := New(Config{})
	clientConn, serverConn := net.Pipe()
	clientConn.Close()
	defer serverConn.Close()
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)

	var addr [18]byte
	conn := &RttyHttpConn{data: make(chan *bytebufferpool.ByteBuffer, 1)}
	conn.ctx, conn.cancel = context.WithCancel(context.Background())
	cli.httpCons.Store(addr, conn)

	conn.run(cli, false, addr, "127.0.0.1", 0)

	if _, ok := cli.httpCons.Load(addr); ok {
		t.Fatal("failed HTTP connection remained registered")
	}
	select {
	case <-conn.ctx.Done():
	default:
		t.Fatal("failed HTTP connection was not canceled")
	}
}

func TestHttpQueueUnblocksOnCancel(t *testing.T) {
	cli := New(Config{})
	var addr [18]byte
	conn := &RttyHttpConn{data: make(chan *bytebufferpool.ByteBuffer, 1)}
	conn.ctx, conn.cancel = context.WithCancel(context.Background())
	conn.data <- bytebufferpool.Get()
	cli.httpCons.Store(addr, conn)

	data := make([]byte, 26)
	copy(data[1:19], addr[:])
	data[25] = 1

	done := make(chan error, 1)
	go func() { done <- handleHttpMsg(cli, data) }()
	conn.cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP message remained blocked after cancellation")
	}
	bytebufferpool.Put(<-conn.data)
}

func TestHttpLoopClosesConnectionOnCancel(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	conn := &RttyHttpConn{conn: clientConn, data: make(chan *bytebufferpool.ByteBuffer, 1)}
	conn.ctx, conn.cancel = context.WithCancel(context.Background())
	conn.active.Store(time.Now().Add(time.Minute).Unix())

	done := make(chan struct{})
	go func() {
		conn.loop()
		close(done)
	}()
	conn.cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP loop did not finish after cancellation")
	}
	if _, err := serverConn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("HTTP connection remained open: %v", err)
	}
}
