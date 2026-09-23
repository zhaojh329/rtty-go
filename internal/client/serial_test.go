package client

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
	"go.bug.st/serial"
)

type testSerialPort struct {
	serial.Port
	data       []byte
	wrote      chan struct{}
	closed     bool
	closeCount int
}

func (p *testSerialPort) Write(data []byte) (int, error) {
	n := min(len(data), 2)
	p.data = append(p.data, data[:n]...)
	if p.wrote != nil {
		p.wrote <- struct{}{}
	}
	return n, nil
}

func (p *testSerialPort) Close() error {
	p.closed = true
	p.closeCount++
	return nil
}

type blockingSerialPort struct {
	serial.Port
	reading chan struct{}
	closed  chan struct{}
}

type ackSerialPort struct {
	serial.Port
	next   chan struct{}
	closed chan struct{}
	read   bool
}

func (p *ackSerialPort) Read(buf []byte) (int, error) {
	if !p.read {
		p.read = true
		return copy(buf, "abc"), nil
	}

	close(p.next)
	<-p.closed
	return 0, io.EOF
}

func (p *ackSerialPort) Close() error {
	close(p.closed)
	return nil
}

type ackWriteConn struct {
	net.Conn
	onWrite func()
}

func (c *ackWriteConn) Write(data []byte) (int, error) {
	c.onWrite()
	return len(data), nil
}

type slowSerialPort struct {
	serial.Port
	writing chan struct{}
	release chan struct{}
	written chan []byte
}

func (p *slowSerialPort) Write(data []byte) (int, error) {
	close(p.writing)
	<-p.release
	p.written <- append([]byte(nil), data...)
	return len(data), nil
}

func (p *slowSerialPort) Close() error { return nil }

func (p *blockingSerialPort) Read([]byte) (int, error) {
	close(p.reading)
	<-p.closed
	return 0, io.EOF
}

func (p *blockingSerialPort) Close() error {
	close(p.closed)
	return nil
}

func TestSerialSessionWriteAndCleanup(t *testing.T) {
	cli := New(Config{})
	port := &testSerialPort{wrote: make(chan struct{}, 3)}
	id := "0123456789abcdef0123456789abcdef"
	cli.nserial = 1
	s := &SerialSession{cli: cli, sid: id, port: port, input: make(chan []byte, 1), done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.sessions.Store(id, s)
	go s.writeLoop()

	if err := handleTermDataMsg(cli, append([]byte(id), []byte("hello")...)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-port.wrote:
		case <-time.After(time.Second):
			t.Fatal("serial input was not written")
		}
	}
	if got := string(port.data); got != "hello" {
		t.Fatalf("partial serial writes lost data: %q", got)
	}
	s.unacked = 5
	if err := handleAckMsg(cli, append([]byte(id), 0, 3)); err != nil || s.unacked != 2 {
		t.Fatalf("serial ack: unacked=%d error=%v", s.unacked, err)
	}
	if err := handleTermWinsizeMsg(cli, append([]byte(id), 0, 80, 0, 24)); err != nil {
		t.Fatalf("serial winsize: %v", err)
	}
	if err := handleLogoutMsg(cli, []byte(id)); err != nil {
		t.Fatal(err)
	}
	if !port.closed {
		t.Fatal("serial port was not closed")
	}
	if cli.nserial != 0 {
		t.Fatalf("session count leaked: %d", cli.nserial)
	}
	if _, ok := cli.sessions.Load(id); ok {
		t.Fatal("serial session was not removed")
	}
}

func TestCloseStopsSerialSession(t *testing.T) {
	cli := New(Config{})
	port := &testSerialPort{}
	id := "0123456789abcdef0123456789abcdef"
	cli.nserial = 1
	s := &SerialSession{cli: cli, sid: id, port: port, done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.sessions.Store(id, s)

	cli.Close()

	if !port.closed || cli.nserial != 0 {
		t.Fatalf("serial session not closed: port=%v count=%d", port.closed, cli.nserial)
	}
	if _, ok := cli.sessions.Load(id); ok {
		t.Fatal("serial session was not removed")
	}
}

func TestSerialSessionConcurrentStop(t *testing.T) {
	cli := New(Config{})
	port := &testSerialPort{}
	id := "0123456789abcdef0123456789abcdef"
	cli.nserial = 1
	s := &SerialSession{cli: cli, sid: id, port: port, done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.sessions.Store(id, s)

	var stopped atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.stop() {
				stopped.Add(1)
			}
		}()
	}
	wg.Wait()

	if stopped.Load() != 1 || port.closeCount != 1 || cli.nserial != 0 {
		t.Fatalf("stop count=%d closes=%d sessions=%d", stopped.Load(), port.closeCount, cli.nserial)
	}
	if _, ok := cli.sessions.Load(id); ok {
		t.Fatal("serial session was not removed")
	}
}

func TestSerialSessionStopUnblocksRead(t *testing.T) {
	cli := New(Config{})
	port := &blockingSerialPort{reading: make(chan struct{}), closed: make(chan struct{})}
	id := "0123456789abcdef0123456789abcdef"
	cli.nserial = 1
	s := &SerialSession{cli: cli, sid: id, port: port, done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.sessions.Store(id, s)

	done := make(chan struct{})
	go func() {
		s.run()
		close(done)
	}()
	<-port.reading
	if !s.stop() {
		t.Fatal("failed to stop serial session")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serial read did not finish after port close")
	}
	if cli.nserial != 0 {
		t.Fatalf("session count leaked: %d", cli.nserial)
	}
}

func TestSerialSessionAckBeforeSendReturns(t *testing.T) {
	cli := New(Config{})
	port := &ackSerialPort{next: make(chan struct{}), closed: make(chan struct{})}
	id := "0123456789abcdef0123456789abcdef"
	s := &SerialSession{cli: cli, sid: id, port: port, done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.nserial = 1
	cli.sessions.Store(id, s)
	defer s.stop()
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, &ackWriteConn{onWrite: func() { s.ack(3) }})

	finished := make(chan struct{})
	go func() {
		s.run()
		close(finished)
	}()
	select {
	case <-port.next:
	case <-time.After(time.Second):
		t.Fatal("serial reader did not advance after ACK")
	}

	s.ackMu.Lock()
	unacked := s.unacked
	s.ackMu.Unlock()
	if unacked != 0 {
		t.Fatalf("ACK arrived before send returned, but %d bytes remain outstanding", unacked)
	}
	s.stop()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("serial reader did not finish after close")
	}
}

func TestSerialInputDoesNotBlockMessageHandler(t *testing.T) {
	cli := New(Config{})
	port := &slowSerialPort{
		writing: make(chan struct{}), release: make(chan struct{}, 1), written: make(chan []byte, 1),
	}
	id := "0123456789abcdef0123456789abcdef"
	s := &SerialSession{cli: cli, sid: id, port: port, input: make(chan []byte, 1), done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.nserial = 1
	cli.sessions.Store(id, s)
	go s.writeLoop()
	defer func() {
		select {
		case port.release <- struct{}{}:
		default:
		}
		s.stop()
	}()

	frame := append([]byte(id), []byte("hello")...)
	finished := make(chan error, 1)
	go func() { finished <- handleTermDataMsg(cli, frame) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("serial write blocked the message handler")
	}

	select {
	case <-port.writing:
	case <-time.After(time.Second):
		t.Fatal("serial writer did not start")
	}
	frame[32] = 'X'
	port.release <- struct{}{}
	select {
	case data := <-port.written:
		if string(data) != "hello" {
			t.Fatalf("serial input changed after enqueue: %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("serial writer did not finish")
	}
}

func TestSerialInputQueueFullClosesSession(t *testing.T) {
	cli := New(Config{})
	port := &slowSerialPort{
		writing: make(chan struct{}), release: make(chan struct{}, 1), written: make(chan []byte, 1),
	}
	id := "0123456789abcdef0123456789abcdef"
	s := &SerialSession{cli: cli, sid: id, port: port, input: make(chan []byte, 1), done: make(chan struct{})}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.nserial = 1
	cli.sessions.Store(id, s)
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, &ackWriteConn{onWrite: func() {}})
	go s.writeLoop()
	defer func() {
		select {
		case port.release <- struct{}{}:
		default:
		}
		s.stop()
	}()

	s.writeInput([]byte("one"))
	select {
	case <-port.writing:
	case <-time.After(time.Second):
		t.Fatal("serial writer did not start")
	}
	s.writeInput([]byte("two"))
	s.writeInput([]byte("three"))

	if _, ok := cli.sessions.Load(id); ok || cli.nserial != 0 {
		t.Fatal("full serial input queue did not close the session")
	}
}

func TestSerialOpenRejectedSettings(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cli := New(Config{})
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)
	id := "0123456789abcdef0123456789abcdef"
	done := make(chan error, 1)
	go func() {
		done <- handleSerialOpenMsg(cli, append([]byte(id), []byte{0, 0, 0, 0, 8, 1, 0, 'C', 'O', 'M', '3'}...))
	}()

	reader := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn)
	_, data, err := reader.Read()
	if err != nil || data[32] != proto.SerialInvalidSettings {
		t.Fatalf("invalid settings: %q %v", data, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
