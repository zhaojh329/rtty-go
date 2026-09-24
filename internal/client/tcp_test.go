package client

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/proto"
)

func TestTCPForwardEchoAndHalfClose(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	cli := &RttyClient{conn: clientConn, msg: proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)}
	defer cli.Close()
	reader := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn)
	id := "0123456789abcdef0123456789abcdef"
	var targetAddr [6]byte
	copy(targetAddr[:4], net.IPv4(127, 0, 0, 1).To4())
	binary.BigEndian.PutUint16(targetAddr[4:], uint16(target.Addr().(*net.TCPAddr).Port))
	if err := handleTCPMsg(cli, append(append([]byte(id), proto.TCPTypeOpen), targetAddr[:]...)); err != nil {
		t.Fatal(err)
	}
	serverConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, data, err := reader.Read()
	if err != nil || typ != proto.MsgTypeTCP || len(data) != 34 || data[33] != proto.TCPOpenOK {
		t.Fatalf("open result: %d %v %v", typ, data, err)
	}
	if err := handleTCPMsg(cli, append(append([]byte(id), proto.TCPTypeData), []byte("hello")...)); err != nil {
		t.Fatal(err)
	}
	var echoed, acknowledged bool
	for !echoed || !acknowledged {
		typ, data, err = reader.Read()
		if err != nil || typ != proto.MsgTypeTCP {
			t.Fatalf("TCP response: %d %v %v", typ, data, err)
		}

		switch data[32] {
		case proto.TCPTypeAck:
			if len(data) != 37 || binary.BigEndian.Uint32(data[33:]) != 5 {
				t.Fatalf("invalid acknowledgement: %v", data)
			}
			acknowledged = true
		case proto.TCPTypeData:
			if string(data[33:]) != "hello" {
				t.Fatalf("echo: %q", data[33:])
			}
			echoed = true

			var ack [4]byte
			binary.BigEndian.PutUint32(ack[:], 5)
			if err := handleTCPMsg(cli, append(append([]byte(id), proto.TCPTypeAck), ack[:]...)); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected TCP operation: %d", data[32])
		}
	}
	if err := handleTCPMsg(cli, append([]byte(id), proto.TCPTypeCloseWrite)); err != nil {
		t.Fatal(err)
	}
	typ, data, err = reader.Read()
	if err != nil || typ != proto.MsgTypeTCP || data[32] != proto.TCPTypeCloseWrite {
		t.Fatalf("half close: %d %v %v", typ, data, err)
	}
	if err := handleTCPMsg(cli, append([]byte(id), proto.TCPTypeClose)); err != nil {
		t.Fatal(err)
	}
}
