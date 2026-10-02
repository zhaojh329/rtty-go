//go:build linux

package client

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"golang.org/x/sys/unix"
)

// Run in a separate controlling-terminal session to exercise peer credentials.
func TestFileLocalPeerProcess(t *testing.T) {
	scenario := os.Getenv("RTTY_TEST_FILE_PEER")
	if scenario == "" {
		return
	}

	tty, err := os.Open("/dev/tty")
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	address, err := filetransfer.SocketAddress(tty)
	if err != nil {
		t.Fatal(err)
	}

	var conn *net.UnixConn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.DialUnix("unixpacket", nil, address)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	receiveFilePacket(t, conn, filetransfer.Accept)

	switch scenario {
	case "busy":
		other, err := net.DialUnix("unixpacket", nil, address)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		other.SetDeadline(time.Now().Add(time.Second))
		packet := receiveFilePacket(t, other, filetransfer.Error)
		if binary.BigEndian.Uint32(packet.Data) != uint32(unix.EBUSY) {
			t.Fatalf("busy error: %x", packet.Data)
		}
		// The rejected request must not replace or close the original peer.
		filetransfer.SendPacket(conn, filetransfer.Send, nil, -1)
		packet = receiveFilePacket(t, conn, filetransfer.Error)
		if binary.BigEndian.Uint32(packet.Data) != uint32(unix.EPROTO) {
			t.Fatalf("original peer lost: %x", packet.Data)
		}
	case "timeout":
		packet := receiveFilePacket(t, conn, filetransfer.Error)
		if binary.BigEndian.Uint32(packet.Data) != uint32(unix.ETIMEDOUT) {
			t.Fatalf("timeout error: %x", packet.Data)
		}
	}

	// A failed or timed-out transfer leaves the terminal ready for another helper.
	next, err := net.DialUnix("unixpacket", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.SetDeadline(time.Now().Add(time.Second))
	receiveFilePacket(t, next, filetransfer.Accept)
}

func TestFileLocalListener(t *testing.T) {
	for _, scenario := range []string{"busy", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFileLocalPeerProcess$")
			cmd.Env = append(os.Environ(), "RTTY_TEST_FILE_PEER="+scenario)
			master, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer cmd.Process.Kill()

			session := &TermSession{term: &Terminal{pty: master}}
			ctx := &RttyFileContext{ses: session}
			if err := ctx.init(); err != nil {
				t.Fatal(err)
			}
			defer ctx.close()
			address, err := filetransfer.SocketAddress(master)
			if err != nil {
				t.Fatal(err)
			}

			// The parent has the same uid, but belongs to a different session.
			foreign, err := net.DialUnix("unixpacket", nil, address)
			if err != nil {
				t.Fatal(err)
			}
			foreign.SetDeadline(time.Now().Add(time.Second))
			_, err = filetransfer.ReceivePacket(foreign)
			foreign.Close()
			if !errors.Is(err, io.EOF) {
				t.Fatalf("foreign session was not rejected: %v", err)
			}

			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			select {
			case err := <-wait:
				if err != nil {
					buf := make([]byte, 4096)
					n, _ := master.Read(buf)
					t.Fatalf("peer: %v: %s", err, buf[:n])
				}
			case <-time.After(10 * time.Second):
				t.Fatal("peer did not exit")
			}

			ctx.close()
			if conn, err := net.DialUnix("unixpacket", nil, address); err == nil {
				conn.Close()
				t.Fatal("listener survived context close")
			}
		})
	}
}
