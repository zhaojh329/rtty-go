//go:build !windows

package client

import (
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
)

func newTestTermSession() (*RttyClient, *TermSession) {
	cli := New(Config{})
	term := &Terminal{
		cmd:      &exec.Cmd{},
		cond:     sync.NewCond(&sync.Mutex{}),
		waitDone: make(chan struct{}),
	}
	close(term.waitDone)

	s := &TermSession{cli: cli, sid: "0123456789abcdef0123456789abcdef", term: term}
	s.fc = &RttyFileContext{ses: s}
	cli.ntty = 1
	cli.sessions.Store(s.sid, s)
	return cli, s
}

func TestTermSessionStop(t *testing.T) {
	for name, stop := range map[string]func(*RttyClient, *TermSession) error{
		"server logout": func(cli *RttyClient, s *TermSession) error {
			return handleLogoutMsg(cli, []byte(s.sid))
		},
		"client close": func(cli *RttyClient, _ *TermSession) error {
			cli.Close()
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			cli, s := newTestTermSession()
			s.timer = time.AfterFunc(time.Hour, func() {})
			if err := stop(cli, s); err != nil {
				t.Fatal(err)
			}

			if cli.ntty != 0 || !s.term.isClosed() || s.timer != nil {
				t.Fatalf("term not stopped: count=%d closed=%v timer=%v", cli.ntty, s.term.isClosed(), s.timer)
			}
			if _, ok := cli.sessions.Load(s.sid); ok {
				t.Fatal("terminal session was not removed")
			}
			if s.stop() {
				t.Fatal("stopped terminal twice")
			}
			s.Run()
			if s.timer != nil {
				t.Fatal("stopped terminal started an inactivity timer")
			}
		})
	}
}

func TestTermSessionCloseReportsLogout(t *testing.T) {
	cli, s := newTestTermSession()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)

	done := make(chan struct{})
	go func() {
		s.close()
		close(done)
	}()
	reader := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn)
	typ, data, err := reader.Read()
	if err != nil || typ != proto.MsgTypeLogout || string(data) != s.sid {
		t.Fatalf("logout: type=%d data=%q error=%v", typ, data, err)
	}
	<-done

	s.close()
	if cli.ntty != 0 || !s.term.isClosed() {
		t.Fatalf("term not stopped: count=%d closed=%v", cli.ntty, s.term.isClosed())
	}
}

func TestTermSessionAck(t *testing.T) {
	cli, s := newTestTermSession()
	defer s.stop()
	s.term.wait_ack.Store(5)

	if err := handleAckMsg(cli, append([]byte(s.sid), 0, 3)); err != nil {
		t.Fatal(err)
	}
	if got := s.term.wait_ack.Load(); got != 2 {
		t.Fatalf("terminal ack: wait_ack=%d", got)
	}
}

func TestTermSessionWriteInput(t *testing.T) {
	cli, s := newTestTermSession()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	s.term.pty = writer
	defer s.stop()

	if err := handleTermDataMsg(cli, append([]byte(s.sid), []byte("hello")...)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("terminal input: %q, error %v", buf, err)
	}
}
