package client

import (
	"net"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
)

func TestHeartbeatStopsWithClient(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cli := New(Config{Heartbeat: 1})
	cli.conn = clientConn
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)
	cli.startHeartbeat()
	defer cli.Close()

	cli.mu.Lock()
	cli.heartbeatTimer.Reset(0)
	cli.mu.Unlock()

	if err := serverConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	typ, _, err := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn).Read()
	if err != nil || typ != proto.MsgTypeHeartbeat {
		t.Fatalf("heartbeat: type=%d error=%v", typ, err)
	}

	cli.Close()
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.heartbeatTimer != nil || cli.waitingHeartbeat {
		t.Fatal("heartbeat remained active after close")
	}
}
