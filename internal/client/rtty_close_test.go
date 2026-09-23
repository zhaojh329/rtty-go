package client

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestClientCloseClosesConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cli := New(Config{})
	cli.conn = clientConn

	if err := serverConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	cli.Close()

	if _, err := serverConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("client connection remained open: %v", err)
	}
}
