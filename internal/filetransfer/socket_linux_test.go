//go:build linux

package filetransfer

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "test socket")
		conn, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn.(*net.UnixConn)
		conns[i].SetDeadline(time.Now().Add(3 * time.Second))
		t.Cleanup(func() { conn.Close() })
	}

	return conns[0], conns[1]
}

func TestPacketWireFormat(t *testing.T) {
	sender, receiver := socketPair(t)
	for i, typ := range []byte{Send, Recv, Accept, Info, Progress, Done, Error} {
		if typ != byte(i+1) {
			t.Fatalf("wire type %d = %d", i+1, typ)
		}
		if err := SendPacket(sender, typ, []byte{0x12, 0x34}, -1); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 10)
		n, err := receiver.Read(buf)
		if err != nil || !bytes.Equal(buf[:n], []byte{2, byte(i + 1), 0, 2, 0x12, 0x34}) {
			t.Fatalf("wire: %x, %v", buf[:n], err)
		}
	}
}

func TestDescriptorTransfer(t *testing.T) {
	sender, receiver := socketPair(t)
	file, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("original data")
	file.Seek(0, 0)
	if err := SendPacket(sender, Send, nil, int(file.Fd())); err != nil {
		t.Fatal(err)
	}
	file.Close()
	os.Remove(file.Name())

	packet, err := ReceivePacket(receiver)
	if err != nil || packet.Type != Send || packet.FD < 0 {
		t.Fatalf("packet: %+v, %v", packet, err)
	}
	received := os.NewFile(uintptr(packet.FD), "received")
	defer received.Close()
	flags, err := unix.FcntlInt(received.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor not close-on-exec: %d, %v", flags, err)
	}
	data, err := io.ReadAll(received)
	if err != nil || string(data) != "original data" {
		t.Fatalf("data = %q, %v", data, err)
	}
}

func TestInvalidPacketsCloseDescriptors(t *testing.T) {
	for name, data := range map[string][]byte{
		"short": {2, Send}, "version": {1, Send, 0, 0},
		"length": {2, Send, 0, 2, 1}, "oversize": make([]byte, 4+MaxPayloadLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			sender, receiver := socketPair(t)
			file, err := os.Open("/dev/null")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			before, _ := os.ReadDir("/proc/self/fd")
			if _, _, err := sender.WriteMsgUnix(data, unix.UnixRights(int(file.Fd())), nil); err != nil {
				t.Fatal(err)
			}
			packet, err := ReceivePacket(receiver)
			if !errors.Is(err, unix.EPROTO) || packet.FD != -1 {
				t.Fatalf("packet = %+v, %v", packet, err)
			}
			after, _ := os.ReadDir("/proc/self/fd")
			if len(after) != len(before) {
				t.Fatalf("descriptor leak: before %d, after %d", len(before), len(after))
			}
		})
	}

	for _, count := range []int{2, 8} {
		sender, receiver := socketPair(t)
		file, err := os.Open("/dev/null")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		fds := make([]int, count)
		for i := range fds {
			fds[i] = int(file.Fd())
		}

		before, _ := os.ReadDir("/proc/self/fd")
		sender.WriteMsgUnix([]byte{2, Send, 0, 0}, unix.UnixRights(fds...), nil)
		packet, err := ReceivePacket(receiver)
		if !errors.Is(err, unix.EPROTO) || packet.FD != -1 {
			t.Fatalf("%d descriptors: %+v, %v", count, packet, err)
		}
		after, _ := os.ReadDir("/proc/self/fd")
		if len(after) != len(before) {
			t.Fatalf("%d descriptors leaked: before %d, after %d", count, len(before), len(after))
		}
	}
}

func TestFinalPacketWithPendingAcknowledgment(t *testing.T) {
	daemon, helper := socketPair(t)
	if err := SendPacket(helper, Progress, nil, -1); err != nil {
		t.Fatal(err)
	}
	if err := SendFinalPacket(daemon, Done, nil); err != nil {
		t.Fatal(err)
	}
	daemon.Close()

	packet, err := ReceivePacket(helper)
	if err != nil || packet.Type != Done {
		t.Fatalf("final packet = %+v, %v", packet, err)
	}
}

func TestSendDoesNotBlockOnPausedPeer(t *testing.T) {
	daemon, _ := socketPair(t)
	for range 10000 {
		err := SendPacket(daemon, Progress, []byte{0, 0, 0, 1}, -1)
		if errors.Is(err, unix.EAGAIN) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("did not fill local socket queue")
}
