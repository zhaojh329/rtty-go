package client

import (
	"net"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
)

type observedWriteConn struct {
	net.Conn
	entered chan struct{}
}

func (c *observedWriteConn) Write(data []byte) (int, error) {
	c.entered <- struct{}{}
	return c.Conn.Write(data)
}

func TestWriteMsgSerializesFrames(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	entered := make(chan struct{}, 2)
	cli := New(Config{})
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, &observedWriteConn{clientConn, entered})
	reader := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn)
	id := "0123456789abcdef0123456789abcdef"

	first := make(chan error, 1)
	go func() { first <- cli.WriteMsg(proto.MsgTypeLogin, id, byte(0)) }()
	<-entered

	second := make(chan error, 1)
	go func() { second <- cli.WriteMsg(proto.MsgTypeLogout, id) }()
	select {
	case <-entered:
		t.Fatal("second frame started before the first finished")
	case <-time.After(25 * time.Millisecond):
	}

	for _, want := range []struct {
		typ  byte
		data string
	}{{proto.MsgTypeLogin, id + "\x00"}, {proto.MsgTypeLogout, id}} {
		typ, data, err := reader.Read()
		if err != nil || typ != want.typ || string(data) != want.data {
			t.Fatalf("frame: type=%d data=%q error=%v", typ, data, err)
		}
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}
