package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
)

func startClientForTest(t *testing.T, cli *RttyClient) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); cli.Run() }()
	t.Cleanup(func() {
		cli.Stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("client Run did not stop")
		}
	})
	return done
}

func waitAttempt(t *testing.T, cli *RttyClient, previous *RttyClient) *RttyClient {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cli.mu.Lock()
		attempt := cli.active
		cli.mu.Unlock()
		if attempt != nil && attempt != previous {
			attempt.mu.Lock()
			ready := attempt.heartbeatTimer != nil
			attempt.mu.Unlock()
			if ready {
				return attempt
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("registered connection attempt did not appear")
	return nil
}

func TestReconnectIsolatesOldConnectionWork(t *testing.T) {
	oldWait := reconnectWait
	reconnectWait = func() time.Duration { return time.Millisecond }
	t.Cleanup(func() { reconnectWait = oldWait })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for i := 0; i < 2; i++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			msg := proto.NewMsgReaderWriter(proto.RoleRttys, c)
			if _, _, err := msg.Read(); err != nil {
				c.Close()
				return
			}
			if err := msg.Write(proto.MsgTypeRegister, byte(0)); err != nil {
				c.Close()
				return
			}
			accepted <- c
		}
	}()
	cli := &RttyClient{cfg: Config{host: "127.0.0.1", port: uint16(ln.Addr().(*net.TCPAddr).Port), id: "generation", heartbeat: 30, reconnect: true}}
	startClientForTest(t, cli)
	first := <-accepted
	old := waitAttempt(t, cli, nil)
	first.Close()
	second := <-accepted
	defer second.Close()
	current := waitAttempt(t, cli, old)
	if err := old.WriteMsg(proto.MsgTypeHeartbeat); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("old connection write = %v", err)
	}
	old.Stop()
	select {
	case <-old.ctx.Done():
	default:
		t.Fatal("old connection context was not canceled")
	}
	if current.closed.Load() {
		t.Fatal("old Stop closed new connection")
	}
	msg := proto.NewMsgReaderWriter(proto.RoleRttys, second)
	done := make(chan error, 1)
	go func() { done <- current.WriteMsg(proto.MsgTypeHeartbeat) }()
	second.SetReadDeadline(time.Now().Add(time.Second))
	typ, _, err := msg.Read()
	if err != nil || typ != proto.MsgTypeHeartbeat {
		t.Fatalf("new link unusable: type=%d error=%v", typ, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStopInterruptsReconnectDelay(t *testing.T) {
	oldWait := reconnectWait
	waiting := make(chan struct{}, 1)
	reconnectWait = func() time.Duration { waiting <- struct{}{}; return time.Minute }
	t.Cleanup(func() { reconnectWait = oldWait })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	cli := &RttyClient{cfg: Config{host: "127.0.0.1", port: uint16(ln.Addr().(*net.TCPAddr).Port), id: "stop", reconnect: true}}
	done := startClientForTest(t, cli)
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("did not reach reconnect delay")
	}
	cli.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for reconnect delay")
	}
}

func TestHeartbeatRetriesBeforeDisconnect(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	cli := &RttyClient{conn: client, msg: proto.NewMsgReaderWriter(proto.RoleRtty, client)}
	defer cli.Close()
	var sent atomic.Int32
	go func() {
		msg := proto.NewMsgReaderWriter(proto.RoleRttys, server)
		for {
			if _, _, err := msg.Read(); err != nil {
				return
			}
			if sent.Add(1) >= 2 {
				cli.recvSeq.Add(1)
			}
		}
	}()
	timer := time.AfterFunc(time.Hour, func() {})
	cli.heartbeatTimer = timer
	const interval = 30 * time.Second
	cli.lastHeartbeat = time.Now().Add(-interval)
	cli.onHeartbeatTimer(timer, interval)
	cli.mu.Lock()
	cli.lastHeartbeat = time.Now().Add(-interval - time.Second)
	cli.pendingSince = cli.lastHeartbeat
	cli.mu.Unlock()
	cli.onHeartbeatTimer(timer, interval)
	deadline := time.Now().Add(time.Second)
	for cli.recvSeq.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cli.recvSeq.Load() == 0 || sent.Load() < 2 {
		t.Fatal("disconnected before retry could receive a response")
	}
	cli.mu.Lock()
	cli.pendingSince = time.Now().Add(-3 * interval)
	cli.mu.Unlock()
	cli.onHeartbeatTimer(timer, interval)
	if err := cli.WriteMsg(proto.MsgTypeHeartbeat); err != nil {
		t.Fatalf("retry response was ignored: %v", err)
	}
}

type clientShortDeadlineConn struct{ net.Conn }

func (c clientShortDeadlineConn) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() {
		d = time.Now().Add(30 * time.Millisecond)
	}
	return c.Conn.SetWriteDeadline(d)
}

func TestBlockedClientWriteUnblocksRead(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	conn := clientShortDeadlineConn{client}
	cli := &RttyClient{conn: conn, msg: proto.NewMsgReaderWriter(proto.RoleRtty, conn)}
	defer cli.Close()
	done := make(chan error, 1)
	go func() { _, _, err := cli.ReadMsg(); done <- err }()
	if err := cli.WriteMsg(proto.MsgTypeHeartbeat); err == nil {
		t.Fatal("blocked write did not time out")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("read still blocked")
	}
}

func TestTerminalWriteFailureDoesNotWaitForACK(t *testing.T) {
	cli := &RttyClient{}
	cli.Close()
	s := &TermSession{cli: cli, fc: &RttyFileContext{}}
	if _, err := s.Write([]byte("terminal data")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after connection close = %v", err)
	}
}

func TestTerminalCloseReleasesACKWaiters(t *testing.T) {
	term := &Terminal{cond: sync.NewCond(&sync.Mutex{}), ack_block: 4096}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); term.WaitAck(8192) }()
	}
	term.stopFlow()
	done := make(chan struct{})
	go func() { wg.Wait(); term.WaitAck(8192); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed terminal still waits for ACK")
	}
}

func TestClosedConnectionCannotRestartTimers(t *testing.T) {
	cli := &RttyClient{}
	cli.Close()
	cli.startHeartbeat()
	if cli.heartbeatTimer != nil {
		t.Fatal("closed connection restarted heartbeat")
	}
	s := &TermSession{}
	s.Run(cli)
	if s.timer != nil {
		t.Fatal("late session goroutine restarted timeout")
	}
}

func TestRegisterWireFormatUnchanged(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	cli := &RttyClient{conn: client, msg: proto.NewMsgReaderWriter(proto.RoleRtty, client), cfg: Config{id: "wire", heartbeat: 30}}
	defer cli.Close()
	done := make(chan error, 1)
	go func() { done <- cli.Register() }()
	want := []byte{0, 0, 12, 5, 0, 0, 1, 30, 1, 0, 4, 'w', 'i', 'r', 'e'}
	got := make([]byte, len(want))
	server.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("register frame=%x, want %x", got, want)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
