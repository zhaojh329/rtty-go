//go:build !windows

/* SPDX-License-Identifier: MIT */

package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"github.com/zhaojh329/rtty-go/internal/utils"
)

const fileSizeLimit int64 = 2 * 1024 * 1024 * 1024 // 2 GB

func requestTransferFile(typ byte, path string) {
	var totalSize uint32
	var sfd *os.File
	var err error

	pid := os.Getpid()

	if typ == 'R' {
		info, err := os.Stat(".")
		if err != nil {
			fmt.Println("Permission denied")
			os.Exit(1)
		}

		// Check the write and execute permissions of the current directory
		if info.Mode().Perm()&0200 == 0 {
			fmt.Println("Permission denied")
			os.Exit(1)
		}
	} else {
		sfd, err = os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("open '%s' failed: No such file\n", path)
			} else {
				fmt.Printf("open '%s' failed: %s\n", path, err.Error())
			}
			os.Exit(1)
		}
		defer sfd.Close()

		stat, err := sfd.Stat()
		if err != nil {
			fmt.Printf("stat '%s' failed: %s\n", path, err.Error())
			os.Exit(1)
		}

		if !stat.Mode().IsRegular() {
			fmt.Printf("'%s' is not a regular file\n", path)
			os.Exit(1)
		}

		if stat.Size() > fileSizeLimit {
			fmt.Printf("'%s' is too large(> %d Byte)\n", path, fileSizeLimit)
			os.Exit(1)
		}

		totalSize = uint32(stat.Size())
	}

	fifoName := fmt.Sprintf("/tmp/rtty-fifo-%d.fifo", pid)

	if err := syscall.Mkfifo(fifoName, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Could not create fifo %s\n", fifoName)
		os.Exit(1)
	}

	setupSignalHandler(fifoName)

	defer os.Remove(fifoName)

	time.Sleep(10 * time.Millisecond)

	var fd uint32
	if typ == 'S' {
		fd = uint32(sfd.Fd())
	}

	magic := filetransfer.NewMagic(typ, uint32(pid), fd)
	os.Stdout.Write(magic[:])
	os.Stdout.Sync()

	ctlfd, err := os.OpenFile(fifoName, os.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not open fifo %s\n", fifoName)
		os.Exit(1)
	}
	defer ctlfd.Close()

	handleFileControlMsg(ctlfd, sfd, totalSize, path)
}

func handleFileControlMsg(ctlfd *os.File, sfd *os.File, totalSize uint32, path string) {
	var startTime time.Time

	for {
		buf := make([]byte, filetransfer.ControlMessageSize)

		_, err := io.ReadFull(ctlfd, buf)
		if err != nil {
			return
		}

		typ := buf[0]
		buf = buf[1:]

		switch typ {
		case filetransfer.ControlRequestAccept:
			if sfd != nil {
				sfd.Close()
				startTime = time.Now()
				fmt.Printf("Transferring '%s'...Press Ctrl+C to cancel\n", filepath.Base(path))

				if totalSize == 0 {
					fmt.Println("  100%%    0 B     0s")
				}
			} else {
				fmt.Println("Waiting to receive. Press Ctrl+C to cancel")
			}

		case filetransfer.ControlInfo:
			totalSize = binary.NativeEndian.Uint32(buf)
			fmt.Printf("Transferring '%s'...\n", string(buf[4:]))
			if totalSize == 0 {
				fmt.Println("  100%%    0 B     0s")
				return
			}
			startTime = time.Now()

		case filetransfer.ControlProgress:
			remainSize := binary.NativeEndian.Uint32(buf)
			updateProgress(startTime, totalSize, remainSize)
			if remainSize == 0 {
				fmt.Println()
				return
			}

		case filetransfer.ControlAbort:
			fmt.Println("\nTransfer aborted")
			return

		case filetransfer.ControlBusy:
			fmt.Println("\033[31mRtty is busy to transfer file\033[0m")
			return

		case filetransfer.ControlNoSpace:
			fmt.Println("\033[31mNo enough space\033[0m")
			return

		case filetransfer.ControlErrExist:
			fmt.Println("\033[31mThe file already exists\033[0m")
			return
		}
	}
}

func setupSignalHandler(fifoName string) {
	c := make(chan os.Signal, 1)

	signal.Notify(c, syscall.SIGINT)

	go func() {
		<-c
		fmt.Println()
		os.Remove(fifoName)
		os.Exit(0)
	}()
}

func updateProgress(startTime time.Time, totalSize uint32, remainSize uint32) {
	elapsed := time.Since(startTime).Seconds()

	transferred := totalSize - remainSize
	percentage := uint64(transferred) * 100 / uint64(totalSize)

	fmt.Printf("%100c\r", ' ')
	fmt.Printf("  %d%%    %s     %.3fs\r", percentage, utils.FormatSize(uint64(transferred)), elapsed)

	os.Stdout.Sync()
}
