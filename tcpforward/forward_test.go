package tcpforward

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
)

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	peer, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })

	conn, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	peer.SetDeadline(time.Now().Add(10 * time.Second))
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	return peer, conn
}

type pausedWriter struct {
	*net.TCPConn
	resume chan struct{}
	done   chan struct{}
	once   sync.Once
}

func (c *pausedWriter) Write(data []byte) (int, error) {
	select {
	case <-c.resume:
		return c.TCPConn.Write(data)
	case <-c.done:
		return 0, net.ErrClosed
	}
}

func (c *pausedWriter) Close() error {
	c.once.Do(func() { close(c.done) })

	return c.TCPConn.Close()
}

func waitResult(t *testing.T, result <-chan error) {
	t.Helper()

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("forward did not finish")
	}
}

func TestBidirectionalBackpressure(t *testing.T) {
	leftApp, leftConn := tcpPair(t)
	rightApp, rightConn := tcpPair(t)
	leftSlow := &pausedWriter{TCPConn: leftConn, resume: make(chan struct{}), done: make(chan struct{})}
	rightSlow := &pausedWriter{TCPConn: rightConn, resume: make(chan struct{}), done: make(chan struct{})}
	leftFull, rightFull := make(chan struct{}), make(chan struct{})
	var left, right *Forwarder
	sentLeft, sentRight := 0, 0

	left = New(leftSlow, func(op byte, data []byte) error {
		err := right.Handle(op, data)
		if op == proto.TCPTypeData {
			sentLeft += len(data)
			if sentLeft == proto.TCPWindowSize {
				close(leftFull)
			}
		}

		return err
	})
	right = New(rightSlow, func(op byte, data []byte) error {
		err := left.Handle(op, data)
		if op == proto.TCPTypeData {
			sentRight += len(data)
			if sentRight == proto.TCPWindowSize {
				close(rightFull)
			}
		}

		return err
	})
	defer left.Close()
	defer right.Close()

	results := make(chan error, 4)
	go func() { results <- left.Run() }()
	go func() { results <- right.Run() }()
	upload := bytes.Repeat([]byte("upload!"), 600000)
	download := bytes.Repeat([]byte("download?"), 500000)

	for _, transfer := range []struct {
		conn *net.TCPConn
		data []byte
	}{{leftApp, upload}, {rightApp, download}} {
		go func() {
			_, err := transfer.conn.Write(transfer.data)
			if err == nil {
				err = transfer.conn.CloseWrite()
			}

			results <- err
		}()
	}

	for _, full := range []chan struct{}{leftFull, rightFull} {
		select {
		case <-full:
		case <-time.After(3 * time.Second):
			t.Fatal("sender did not fill its window")
		}
	}

	// Both destinations are stalled. Protocol dispatch must still accept small
	// frames for another connection, even when their count exceeds the old queue.
	otherApp, otherConn := tcpPair(t)
	other := New(otherConn, func(byte, []byte) error { return nil })
	defer other.Close()
	for range 4096 {
		if err := other.Handle(proto.TCPTypeData, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}

	if err := other.Handle(proto.TCPTypeCloseWrite, nil); err != nil {
		t.Fatal(err)
	}

	otherApp.CloseWrite()
	otherDone := make(chan error, 1)
	go func() { otherDone <- other.Run() }()
	data, err := io.ReadAll(otherApp)
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("x"), 4096)) {
		t.Fatalf("independent small-frame stream: bytes=%d err=%v", len(data), err)
	}

	waitResult(t, otherDone)

	close(leftSlow.resume)
	close(rightSlow.resume)
	reads := make(chan error, 2)
	for _, transfer := range []struct {
		conn *net.TCPConn
		want []byte
	}{{leftApp, download}, {rightApp, upload}} {
		go func() {
			data, err := io.ReadAll(transfer.conn)
			if err == nil && !bytes.Equal(data, transfer.want) {
				err = io.ErrUnexpectedEOF
			}

			reads <- err
		}()
	}

	for range 2 {
		waitResult(t, reads)
	}

	for range 4 {
		waitResult(t, results)
	}
}

func TestCloseWakesWindowWaiter(t *testing.T) {
	app, conn := tcpPair(t)
	full := make(chan struct{})
	sent := 0
	f := New(conn, func(op byte, data []byte) error {
		if op == proto.TCPTypeData {
			sent += len(data)
			if sent == proto.TCPWindowSize {
				close(full)
			}
		}

		return nil
	})
	defer f.Close()
	done := make(chan error, 1)
	go func() { done <- f.Run() }()
	go app.Write(bytes.Repeat([]byte("x"), proto.TCPWindowSize+1))

	select {
	case <-full:
	case <-time.After(3 * time.Second):
		t.Fatal("window not full")
	}

	f.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("abort reported as orderly EOF")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake window/receive waiters")
	}
}

func TestRejectInvalidFlowControl(t *testing.T) {
	for _, test := range []struct {
		name string
		op   byte
		data []byte
	}{
		{"short ACK", proto.TCPTypeAck, []byte{1}},
		{"zero ACK", proto.TCPTypeAck, make([]byte, 4)},
		{"unearned ACK", proto.TCPTypeAck, []byte{0, 0, 0, 1}},
		{"oversized ACK", proto.TCPTypeAck, []byte{255, 255, 255, 255}},
		{"empty data", proto.TCPTypeData, nil},
		{"oversized data", proto.TCPTypeData, make([]byte, proto.TCPMaxDataSize+1)},
		{"half close payload", proto.TCPTypeCloseWrite, []byte{1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()

			f := New(a, func(byte, []byte) error { return nil })
			if err := f.Handle(test.op, test.data); err == nil {
				t.Fatal("invalid message accepted")
			}
		})
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	f := New(a, func(byte, []byte) error { return nil })
	for range proto.TCPWindowSize / proto.TCPMaxDataSize {
		if err := f.Handle(proto.TCPTypeData, make([]byte, proto.TCPMaxDataSize)); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.Handle(proto.TCPTypeData, []byte{1}); err == nil {
		t.Fatal("receive window exceeded")
	}

	if err := f.Handle(proto.TCPTypeCloseWrite, nil); err != nil {
		t.Fatal(err)
	}

	if err := f.Handle(proto.TCPTypeData, []byte{1}); err == nil {
		t.Fatal("data accepted after half close")
	}
}

func TestAckBeforeSendReturns(t *testing.T) {
	app, conn := tcpPair(t)
	var f *Forwarder
	f = New(conn, func(op byte, data []byte) error {
		if op == proto.TCPTypeData {
			var ack [4]byte
			binary.BigEndian.PutUint32(ack[:], uint32(len(data)))
			return f.Handle(proto.TCPTypeAck, ack[:])
		}

		return nil
	})
	defer f.Close()
	done := make(chan error, 1)
	go func() { done <- f.Run() }()
	if err := f.Handle(proto.TCPTypeCloseWrite, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Write(bytes.Repeat([]byte("x"), proto.TCPWindowSize*4)); err != nil {
		t.Fatal(err)
	}

	app.CloseWrite()
	waitResult(t, done)
}
