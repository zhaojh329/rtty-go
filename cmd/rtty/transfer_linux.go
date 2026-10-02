//go:build linux

/* SPDX-License-Identifier: MIT */

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"github.com/zhaojh329/rtty-go/internal/utils"
	"golang.org/x/sys/unix"
)

func requestFileTransfer(c context.Context, typ byte, path string) (err error) {
	defer func() {
		if err == nil {
			return
		}
		if _, ok := err.(cli.ExitCoder); ok {
			return
		}

		err = cli.Exit(fmt.Sprintf("File transfer failed: %v", err), 1)
	}()

	var fd int

	if typ == 'S' {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	} else {
		if err := unix.Access(".", unix.W_OK|unix.X_OK); err != nil {
			return err
		}
		fd, err = unix.Open(".", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return err
	}
	defer func() {
		if fd >= 0 {
			unix.Close(fd)
		}
	}()

	tty, err := os.Open("/dev/tty")
	if err != nil {
		return fmt.Errorf("file transfer is unavailable without a controlling terminal: %w", err)
	}
	defer tty.Close()

	address, err := filetransfer.SocketAddress(tty)
	if err != nil {
		return err
	}

	conn, err := net.DialUnix("unixpacket", nil, address)
	if err != nil {
		return fmt.Errorf("file transfer is unavailable in this terminal: %w", err)
	}
	defer conn.Close()

	cred, err := filetransfer.PeerCredentials(conn)
	if err != nil {
		return err
	}
	if cred.Uid != 0 {
		return unix.EACCES
	}

	foregroundPGID, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return err
	}

	transferContext, cancel := context.WithCancel(c)
	defer cancel()

	if foregroundPGID == unix.Getpgrp() {
		attrs, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil {
			return err
		}

		var stop context.CancelFunc
		transferContext, stop = signal.NotifyContext(transferContext, unix.SIGINT)
		defer stop()
		defer func() {
			if err := unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, attrs); err != nil {
				fmt.Fprintf(os.Stderr, "restore terminal: %v\n", err)
			}
		}()

		transferAttrs := *attrs
		transferAttrs.Lflag &^= unix.ECHOCTL
		if err := unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, &transferAttrs); err != nil {
			return err
		}
	}

	finished := make(chan struct{})
	watcherDone := make(chan struct{})

	go func() {
		defer close(watcherDone)
		select {
		case <-transferContext.Done():
			conn.Close()
		case <-finished:
		}
	}()
	defer func() {
		close(finished)
		<-watcherDone
	}()

	err = runTransferControl(conn, typ, &fd)
	if transferContext.Err() != nil {
		fmt.Println()
		return cli.Exit("", 130)
	}

	return err
}

func receiveControlPacket(conn *net.UnixConn) (filetransfer.Packet, error) {
	packet, err := filetransfer.ReceivePacket(conn)
	if errors.Is(err, io.EOF) {
		return packet, unix.ECONNRESET
	}
	if err != nil {
		return packet, err
	}
	if packet.FD >= 0 {
		unix.Close(packet.FD)
		return packet, unix.EPROTO
	}

	if packet.Type == filetransfer.Error {
		if len(packet.Data) != 4 {
			return packet, unix.EPROTO
		}

		code := binary.BigEndian.Uint32(packet.Data)
		if code == 0 || code > math.MaxInt32 {
			return packet, unix.EPROTO
		}
		return packet, unix.Errno(code)
	}

	return packet, nil
}

func runTransferControl(conn *net.UnixConn, typ byte, fd *int) error {
	packet, err := receiveControlPacket(conn)
	if err != nil {
		return err
	}
	if packet.Type != filetransfer.Accept || len(packet.Data) != 0 {
		return unix.EPROTO
	}

	requestType := filetransfer.Send
	if typ == 'R' {
		requestType = filetransfer.Recv
	}

	if err := filetransfer.SendPacket(conn, requestType, nil, *fd); err != nil {
		return err
	}

	unix.Close(*fd)
	*fd = -1

	if typ == 'R' {
		fmt.Println("Waiting to receive. Press Ctrl+C to cancel")
	}

	var start time.Time
	var total, remaining uint32
	haveInfo := false

	for {
		packet, err := receiveControlPacket(conn)
		if err != nil {
			return err
		}

		switch packet.Type {
		case filetransfer.Info:
			if haveInfo || len(packet.Data) < 5 || !filetransfer.ValidName(packet.Data[4:]) {
				return unix.EPROTO
			}

			total = binary.BigEndian.Uint32(packet.Data)
			remaining = total
			start = time.Now()
			haveInfo = true
			fmt.Printf("Transferring '%s'...Press Ctrl+C to cancel\n", packet.Data[4:])

		case filetransfer.Progress:
			if !haveInfo || len(packet.Data) != 4 {
				return unix.EPROTO
			}

			reportedRemaining := binary.BigEndian.Uint32(packet.Data)
			if reportedRemaining > remaining {
				return unix.EPROTO
			}
			remaining = reportedRemaining
			updateProgress(start, total, remaining, false)

			// DONE may already be queued and the daemon may have closed its socket.
			filetransfer.SendPacket(conn, filetransfer.Progress, nil, -1)

		case filetransfer.Done:
			if !haveInfo || len(packet.Data) != 0 {
				return unix.EPROTO
			}

			updateProgress(start, total, 0, true)
			fmt.Println()
			return nil

		default:
			return unix.EPROTO
		}
	}
}

func updateProgress(startTime time.Time, totalSize uint32, remainSize uint32, complete bool) {
	elapsed := time.Since(startTime).Seconds()

	transferred := totalSize - remainSize
	percentage := uint64(100)
	if totalSize != 0 {
		percentage = uint64(transferred) * 100 / uint64(totalSize)
	}

	var speed float64
	if elapsed > 0 {
		speed = float64(transferred) / elapsed / 1024 / 1024
	}

	fmt.Printf("%100c\r", ' ')
	fmt.Printf("  %d%%    %s    %.2f MB/s", percentage, utils.FormatSize(uint64(transferred)), speed)
	if complete {
		fmt.Printf("    %.3fs", elapsed)
	}
	fmt.Print("\r")

	os.Stdout.Sync()
}
