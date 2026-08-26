/* SPDX-License-Identifier: MIT */

package main

import (
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/zhaojh329/rtty-go/proto"
)

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

func TestCloseDoesNotPanicOnPartialSessions(t *testing.T) {
	cli := &RttyClient{}
	cli.httpCons.Store([18]byte{1}, (*RttyHttpConn)(nil))
	cli.httpCons.Store([18]byte{2}, &RttyHttpConn{})
	cli.httpCons.Store([18]byte{3}, newRttyHttpConn())
	cli.sessions.Store("nil-session", (*TermSession)(nil))
	cli.sessions.Store("empty-session", &TermSession{})

	cli.Close()

	found := false
	cli.httpCons.Range(func(k, v any) bool {
		found = true
		return false
	})
	cli.sessions.Range(func(k, v any) bool {
		found = true
		return false
	})
	if found {
		t.Fatal("Close left leftover sessions or http connections")
	}
}

func TestCloseUnblocksReadMsg(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()

	cli := &RttyClient{
		conn: c1,
		msg:  proto.NewMsgReaderWriter(proto.RoleRtty, c1),
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := cli.ReadMsg()
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cli.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected read error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadMsg was not unblocked by Close")
	}
}

func TestReconnectAfterServerClose(t *testing.T) {
	old := reconnectWait
	reconnectWait = func() time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { reconnectWait = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	var accepted atomic.Int32

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go acceptThenClose(conn)
		}
	}()

	cli := &RttyClient{cfg: Config{
		host:      "127.0.0.1",
		port:      uint16(port),
		id:        "testdev",
		heartbeat: 30,
		reconnect: true,
	}}

	go cli.Run()
	t.Cleanup(cli.Stop)

	waitAccepts(t, &accepted, 2, 10*time.Second)
}

func TestReconnectAfterHeartbeatTimeout(t *testing.T) {
	old := reconnectWait
	reconnectWait = func() time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { reconnectWait = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	var accepted atomic.Int32

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go registerAndSilence(conn)
		}
	}()

	cli := &RttyClient{cfg: Config{
		host:      "127.0.0.1",
		port:      uint16(port),
		id:        "testdev",
		heartbeat: 5,
		reconnect: true,
	}}

	go cli.Run()
	t.Cleanup(cli.Stop)

	waitAccepts(t, &accepted, 2, 15*time.Second)
}

func waitAccepts(t *testing.T, accepted *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for accepted.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("accepted %d connections, want at least %d", accepted.Load(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func acceptThenClose(conn net.Conn) {
	defer conn.Close()

	msg := proto.NewMsgReaderWriter(proto.RoleRttys, conn)
	typ, _, err := msg.Read()
	if err != nil {
		return
	}
	if typ != proto.MsgTypeRegister {
		return
	}
	if err := msg.Write(proto.MsgTypeRegister, byte(0)); err != nil {
		return
	}
	time.Sleep(30 * time.Millisecond)
}

func registerAndSilence(conn net.Conn) {
	defer conn.Close()

	msg := proto.NewMsgReaderWriter(proto.RoleRttys, conn)
	typ, _, err := msg.Read()
	if err != nil {
		return
	}
	if typ != proto.MsgTypeRegister {
		return
	}
	if err := msg.Write(proto.MsgTypeRegister, byte(0)); err != nil {
		return
	}

	time.Sleep(15 * time.Second)
}
