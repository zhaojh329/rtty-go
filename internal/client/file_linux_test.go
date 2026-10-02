//go:build linux

package client

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"github.com/zhaojh329/rtty-go/proto"
	"golang.org/x/sys/unix"
)

func fileSocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	var conns [2]*net.UnixConn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "test socket")
		conn, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn.(*net.UnixConn)
		conns[i].SetDeadline(time.Now().Add(5 * time.Second))
		t.Cleanup(func() { conn.Close() })
	}

	return conns[0], conns[1]
}

type fileTestRemote struct {
	typ  byte
	data []byte
}

func newFileTransfer(t *testing.T) (*RttyFileContext, *net.UnixConn, <-chan fileTestRemote) {
	t.Helper()
	cli, session := newTestTermSession()
	daemon, helper := fileSocketPair(t)
	ctx := session.fc
	ctx.controlConn = daemon
	ctx.state = fileWaitRequest
	ctx.receiveMode = 0644
	ctx.ownerUID, ctx.ownerGID = os.Getuid(), os.Getgid()
	ctx.handshakeTimer = time.AfterFunc(time.Hour, func() {})

	clientConn, serverConn := net.Pipe()
	cli.conn = clientConn
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)
	messages := make(chan fileTestRemote, 100)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn)
		for {
			typ, data, err := reader.Read()
			if err != nil {
				return
			}
			if typ == proto.MsgTypeFile {
				messages <- fileTestRemote{typ: data[32], data: bytes.Clone(data[33:])}
			}
		}
	}()

	localDone := make(chan struct{})
	go func() {
		defer close(localDone)
		ctx.readControlPackets(daemon)
	}()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
		ctx.close()
		<-readerDone
		<-localDone
	})
	return ctx, helper, messages
}

func receiveFilePacket(t *testing.T, conn *net.UnixConn, typ byte) filetransfer.Packet {
	t.Helper()
	packet, err := filetransfer.ReceivePacket(conn)
	if err != nil || packet.Type != typ {
		t.Fatalf("local packet: type %d, expected %d, data %x, error %v", packet.Type, typ, packet.Data, err)
	}
	return packet
}

func receiveRemoteFile(t *testing.T, messages <-chan fileTestRemote, typ byte) []byte {
	t.Helper()
	select {
	case msg := <-messages:
		if msg.typ != typ {
			t.Fatalf("remote type = %d, want %d", msg.typ, typ)
		}
		return msg.data
	case <-time.After(5 * time.Second):
		t.Fatal("remote message timeout")
		return nil
	}
}

func startReceive(t *testing.T, ctx *RttyFileContext, helper *net.UnixConn, messages <-chan fileTestRemote, directory string, name string, size uint32) {
	t.Helper()
	fd, err := unix.Open(directory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := filetransfer.SendPacket(helper, filetransfer.Recv, nil, fd); err != nil {
		t.Fatal(err)
	}
	receiveRemoteFile(t, messages, proto.MsgTypeFileRecv)
	info := make([]byte, 5)
	info[0] = proto.MsgTypeFileInfo
	binary.BigEndian.PutUint32(info[1:], size)
	ctx.handleServerMessage(append(info, name...))
}

func TestReceiveFile(t *testing.T) {
	for _, size := range []int{0, 1, 130000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx, helper, messages := newFileTransfer(t)
			dir := t.TempDir()
			name := strings.Repeat("n", 255)
			startReceive(t, ctx, helper, messages, dir, name, uint32(size))
			receiveFilePacket(t, helper, filetransfer.Info)
			if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination visible before completion: %v", err)
			}

			content := bytes.Repeat([]byte("a"), size)
			for offset := 0; ; {
				end := min(offset+30000, size)
				ctx.handleServerMessage(append([]byte{proto.MsgTypeFileData}, content[offset:end]...))
				if end == size {
					break
				}
				receiveRemoteFile(t, messages, proto.MsgTypeFileAck)
				offset = end
			}

			// The paused helper gets at most one progress update, then DONE.
			packet, err := filetransfer.ReceivePacket(helper)
			if err == nil && packet.Type == filetransfer.Progress {
				packet, err = filetransfer.ReceivePacket(helper)
			}
			if err != nil || packet.Type != filetransfer.Done {
				t.Fatalf("completion: %+v, %v", packet, err)
			}
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("received %d bytes, want %d: %v", len(got), size, err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("temporary files remain: %v", entries)
			}
		})
	}
}

func TestReceiveRejectsInvalidName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../escaped", "nested/name", "x\x00y", strings.Repeat("x", 256)} {
		t.Run(name, func(t *testing.T) {
			ctx, helper, messages := newFileTransfer(t)
			dir := t.TempDir()
			startReceive(t, ctx, helper, messages, dir, name, 1)
			receiveRemoteFile(t, messages, proto.MsgTypeFileAbort)
			packet := receiveFilePacket(t, helper, filetransfer.Error)
			if binary.BigEndian.Uint32(packet.Data) != uint32(unix.EPROTO) {
				t.Fatalf("error = %x", packet.Data)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("created files for invalid name: %v", entries)
			}
		})
	}
}

func TestReceiveDoesNotOverwrite(t *testing.T) {
	for _, scenario := range []string{"existing", "symlink", "commit race"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, helper, messages := newFileTransfer(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "target")
			if scenario == "existing" {
				os.WriteFile(path, []byte("keep"), 0600)
			} else if scenario == "symlink" {
				os.Symlink("missing", path)
			}

			startReceive(t, ctx, helper, messages, dir, "target", 1)
			if scenario == "commit race" {
				receiveFilePacket(t, helper, filetransfer.Info)
				os.WriteFile(path, []byte("keep"), 0600)
				ctx.handleServerMessage([]byte{proto.MsgTypeFileData, 'x'})
			}
			receiveRemoteFile(t, messages, proto.MsgTypeFileAbort)
			packet := receiveFilePacket(t, helper, filetransfer.Error)
			if binary.BigEndian.Uint32(packet.Data) != uint32(unix.EEXIST) {
				t.Fatalf("error = %x", packet.Data)
			}
			if scenario != "symlink" {
				data, _ := os.ReadFile(path)
				if string(data) != "keep" {
					t.Fatalf("overwrote destination: %q", data)
				}
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("temporary file left behind: %v", entries)
			}
		})
	}
}

func TestReceiveCancellationAndProtocolErrors(t *testing.T) {
	for _, scenario := range []string{"cancel", "abort", "empty data", "oversize", "unexpected ack", "short info", "duplicate info", "write error", "close"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, helper, messages := newFileTransfer(t)
			dir := t.TempDir()
			startReceive(t, ctx, helper, messages, dir, "target", 2)
			receiveFilePacket(t, helper, filetransfer.Info)

			switch scenario {
			case "cancel":
				helper.Close()
				receiveRemoteFile(t, messages, proto.MsgTypeFileAbort)
			case "abort":
				ctx.handleServerMessage([]byte{proto.MsgTypeFileAbort})
			case "empty data":
				ctx.handleServerMessage([]byte{proto.MsgTypeFileData})
			case "oversize":
				ctx.handleServerMessage([]byte{proto.MsgTypeFileData, 1, 2, 3})
			case "unexpected ack":
				ctx.handleServerMessage([]byte{proto.MsgTypeFileAck})
			case "short info":
				ctx.handleServerMessage([]byte{proto.MsgTypeFileInfo, 0})
			case "duplicate info":
				ctx.handleServerMessage([]byte{proto.MsgTypeFileInfo, 0, 0, 0, 1, 'x'})
			case "write error":
				ctx.mu.Lock()
				ctx.file.Close()
				ctx.mu.Unlock()
				ctx.handleServerMessage([]byte{proto.MsgTypeFileData, 1})
			case "close":
				ctx.close()
			}

			if scenario != "cancel" && scenario != "close" {
				receiveFilePacket(t, helper, filetransfer.Error)
			}
			ctx.mu.Lock()
			defer ctx.mu.Unlock()
			if ctx.file != nil || ctx.receiveDir != nil || ctx.controlConn != nil || ctx.state != fileIdle {
				t.Fatal("transfer resources not reset")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("partial files remain: %v", entries)
			}
		})
	}
}

func TestReceiveUsesDirectoryDescriptor(t *testing.T) {
	ctx, helper, messages := newFileTransfer(t)
	root := t.TempDir()
	dir := filepath.Join(root, "before")
	os.Mkdir(dir, 0700)
	startReceive(t, ctx, helper, messages, dir, "target", 1)
	receiveFilePacket(t, helper, filetransfer.Info)
	moved := filepath.Join(root, "after")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	ctx.handleServerMessage([]byte{proto.MsgTypeFileData, 'x'})
	receiveFilePacket(t, helper, filetransfer.Done)
	data, err := os.ReadFile(filepath.Join(moved, "target"))
	if err != nil || string(data) != "x" {
		t.Fatalf("received file: %q, %v", data, err)
	}
}

func TestSendFile(t *testing.T) {
	for _, size := range []int{0, 1, 150000} {
		ctx, helper, messages := newFileTransfer(t)
		path := filepath.Join(t.TempDir(), "source")
		content := bytes.Repeat([]byte("b"), size)
		os.WriteFile(path, content, 0600)
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := filetransfer.SendPacket(helper, filetransfer.Send, nil, int(file.Fd())); err != nil {
			t.Fatal(err)
		}
		file.Close()
		receiveFilePacket(t, helper, filetransfer.Info)
		receiveRemoteFile(t, messages, proto.MsgTypeFileSend)
		os.Remove(path)
		os.WriteFile(path, []byte("replacement"), 0600)

		var got []byte
		for {
			ctx.handleServerMessage([]byte{proto.MsgTypeFileAck})
			data := receiveRemoteFile(t, messages, proto.MsgTypeFileData)
			if len(data) == 0 {
				break
			}
			got = append(got, data...)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("sent %d bytes, expected %d", len(got), size)
		}
		packet, err := filetransfer.ReceivePacket(helper)
		if err == nil && packet.Type == filetransfer.Progress {
			packet, err = filetransfer.ReceivePacket(helper)
		}
		if err != nil || packet.Type != filetransfer.Done {
			t.Fatalf("completion: %+v, %v", packet, err)
		}
	}
}

func TestSendSourceValidation(t *testing.T) {
	for _, scenario := range []string{"directory", "too large", "max size", "shortened"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, helper, messages := newFileTransfer(t)
			path := t.TempDir()
			if scenario != "directory" {
				path = filepath.Join(path, "source")
				os.WriteFile(path, []byte("data"), 0600)
			}
			if scenario == "too large" {
				if err := os.Truncate(path, int64(math.MaxUint32)+1); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "max size" {
				if err := os.Truncate(path, math.MaxUint32); err != nil {
					t.Fatal(err)
				}
			}

			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			filetransfer.SendPacket(helper, filetransfer.Send, nil, int(file.Fd()))
			file.Close()
			if scenario == "max size" || scenario == "shortened" {
				receiveFilePacket(t, helper, filetransfer.Info)
				receiveRemoteFile(t, messages, proto.MsgTypeFileSend)
				if scenario == "max size" {
					ctx.close()
					return
				}
				os.Truncate(path, 0)
				ctx.handleServerMessage([]byte{proto.MsgTypeFileAck})
				receiveRemoteFile(t, messages, proto.MsgTypeFileAbort)
			}
			receiveFilePacket(t, helper, filetransfer.Error)
		})
	}
}

func TestIdleFileMessages(t *testing.T) {
	cli, s := newTestTermSession()
	defer s.stop()
	for _, data := range [][]byte{nil, {proto.MsgTypeFileInfo}, {proto.MsgTypeFileAck}} {
		if err := handleFileMsg(cli, append([]byte(s.sid), data...)); err != nil {
			t.Fatal(err)
		}
	}
	if err := handleFileMsg(cli, []byte("short")); err == nil {
		t.Fatal("accepted short session ID")
	}
}

func TestFileMsgIgnoresSerialSession(t *testing.T) {
	cli := New(Config{})
	id := "0123456789abcdef0123456789abcdef"
	cli.sessions.Store(id, &SerialSession{})

	if err := handleFileMsg(cli, append([]byte(id), proto.MsgTypeFileAck)); err != nil {
		t.Fatal(err)
	}
}

func TestFileMagicIsTerminalOutput(t *testing.T) {
	cli, session := newTestTermSession()
	session.term.ack_block = 4096
	defer session.stop()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	serverConn.SetDeadline(time.Now().Add(time.Second))
	cli.msg = proto.NewMsgReaderWriter(proto.RoleRtty, clientConn)

	magic := []byte{0xb6, 0xbc, 0xbd, 'S', 1, 0, 0, 0, 9, 0, 0, 0}
	result := make(chan error, 1)
	go func() {
		_, err := session.Write(magic)
		result <- err
	}()

	reader := proto.NewMsgReaderWriter(proto.RoleRttys, serverConn)
	typ, data, err := reader.Read()
	if err != nil || typ != proto.MsgTypeTermData || !bytes.Equal(data, append([]byte(session.sid), magic...)) {
		t.Fatalf("terminal output: %d, %x, %v", typ, data, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
