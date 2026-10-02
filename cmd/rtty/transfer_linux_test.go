//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"golang.org/x/sys/unix"
)

func captureProgress(t *testing.T, start time.Time, total, remaining uint32, complete bool) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "progress")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	stdout := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = stdout }()
	updateProgress(start, total, remaining, complete)
	file.Seek(0, 0)
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTransferProgress(t *testing.T) {
	text := captureProgress(t, time.Now().Add(-time.Second), 2*1024*1024, 1024*1024, false)
	if !strings.Contains(text, "50%    1.0 MB    ") || !strings.HasSuffix(text, " MB/s\r") {
		t.Fatalf("progress = %q", text)
	}

	text = captureProgress(t, time.Now().Add(-time.Second), 1024*1024, 0, false)
	if !strings.HasSuffix(text, "MB/s\r") {
		t.Fatalf("elapsed time displayed before DONE: %q", text)
	}

	text = captureProgress(t, time.Now().Add(-time.Second), 1024*1024, 0, true)
	if !strings.Contains(text, "100%") || !strings.Contains(text, " MB/s    ") || !strings.HasSuffix(text, "s\r") {
		t.Fatalf("completion = %q", text)
	}

	text = captureProgress(t, time.Now().Add(time.Hour), 0, 0, false)
	if !strings.Contains(text, "100%    0 B    0.00 MB/s\r") {
		t.Fatalf("zero size / nonpositive elapsed = %q", text)
	}
}

func helperSocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conns [2]*net.UnixConn
	for i, fd := range fds {
		file := os.NewFile(uintptr(fd), "test socket")
		conn, err := net.FileConn(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn.(*net.UnixConn)
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		t.Cleanup(func() { conn.Close() })
	}
	return conns[0], conns[1]
}

func TestHelperRequiresDone(t *testing.T) {
	for _, scenario := range []string{"done", "eof", "increasing progress", "duplicate info", "early done", "error"} {
		t.Run(scenario, func(t *testing.T) {
			daemon, helper := helperSocketPair(t)
			file, err := os.CreateTemp(t.TempDir(), "source")
			if err != nil {
				t.Fatal(err)
			}
			fd, err := unix.Dup(int(file.Fd()))
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if fd >= 0 {
					unix.Close(fd)
				}
			}()

			result := make(chan error, 1)
			go func() { result <- runTransferControl(helper, 'S', &fd) }()
			filetransfer.SendPacket(daemon, filetransfer.Accept, nil, -1)
			request, err := filetransfer.ReceivePacket(daemon)
			if err != nil || request.Type != filetransfer.Send || request.FD < 0 {
				t.Fatalf("request = %+v, %v", request, err)
			}
			unix.Close(request.FD)

			info := []byte{0, 0, 0, 1, 'x'}
			if scenario != "early done" {
				filetransfer.SendPacket(daemon, filetransfer.Info, info, -1)
			}
			switch scenario {
			case "done", "eof":
				filetransfer.SendPacket(daemon, filetransfer.Progress, []byte{0, 0, 0, 0}, -1)
				ack, err := filetransfer.ReceivePacket(daemon)
				if err != nil || ack.Type != filetransfer.Progress || len(ack.Data) != 0 {
					t.Fatalf("progress ack = %+v, %v", ack, err)
				}
				select {
				case err := <-result:
					t.Fatalf("helper completed before DONE: %v", err)
				default:
				}
				if scenario == "done" {
					filetransfer.SendFinalPacket(daemon, filetransfer.Done, nil)
				}
				daemon.Close()
			case "increasing progress":
				filetransfer.SendPacket(daemon, filetransfer.Progress, []byte{0, 0, 0, 2}, -1)
			case "duplicate info":
				filetransfer.SendPacket(daemon, filetransfer.Info, info, -1)
			case "early done":
				filetransfer.SendPacket(daemon, filetransfer.Done, nil, -1)
			case "error":
				code := make([]byte, 4)
				binary.BigEndian.PutUint32(code, uint32(unix.ENOSPC))
				filetransfer.SendPacket(daemon, filetransfer.Error, code, -1)
			}

			err = <-result
			if scenario == "done" && err != nil {
				t.Fatal(err)
			}
			if scenario != "done" && err == nil {
				t.Fatal("invalid completion succeeded")
			}
			if scenario == "error" && !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("error not propagated: %v", err)
			}
		})
	}
}

func TestFileTransferCLIError(t *testing.T) {
	if path := os.Getenv("RTTY_TEST_MISSING_TRANSFER_FILE"); path != "" {
		os.Args = []string{"rtty", "-S", path}
		main()
		return
	}

	path := filepath.Join(t.TempDir(), "missing")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileTransferCLIError$")
	cmd.Env = append(os.Environ(), "RTTY_TEST_MISSING_TRANSFER_FILE="+path)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, output = %q", err, output)
	}
	if string(output) != "File transfer failed: no such file or directory\n" {
		t.Fatalf("unexpected CLI error output: %q", output)
	}
}
