//go:build !windows
// +build !windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

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

	"github.com/zhaojh329/rtty-go/utils"

	"github.com/rs/zerolog/log"
)

const (
	MsgTypeFileCtlRequestAccept = byte(iota)
	MsgTypeFileCtlProgress
	MsgTypeFileCtlInfo
	MsgTypeFileCtlBusy
	MsgTypeFileCtlAbort
	MsgTypeFileCtlNoSpace
	MsgTypeFileCtlErrExist
	MsgTypeFileCtlErr
)

const (
	fileSizeLimit int64 = 2 * 1024 * 1024 * 1024 // 2 GB

	fileCtlMsgSize = 129
)

var RttyFileMagic = [12]byte{0xb6, 0xbc, 0xbd}

func handleFileMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])
	typ := data[32]

	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("terminal session %s not found", sid)
		return nil
	}

	s := val.(*TermSession)

	data = data[33:]

	switch typ {
	case MsgTypeFileInfo:
		s.fc.startDownload(data)

	case MsgTypeFileData:
		if len(data) > 0 {
			if s.fc.file != nil {
				s.fc.file.Write(data)
				s.fc.remainSize -= uint32(len(data))
				if s.fc.notifyProgress() != nil {
					s.fc.reset()
				} else {
					if s.fc.remainSize == 0 {
						s.fc.reset()
					} else {
						cli.SendFileMsg(s.sid, MsgTypeFileAck, nil)
					}
				}
			}
		} else {
			s.fc.reset()
		}

	case MsgTypeFileAck:
		s.fc.sendData()

	case MsgTypeFileAbort:
		s.fc.sendControlMsg(MsgTypeFileCtlAbort, nil)
		s.fc.reset()
	}

	return nil
}

type RttyFileContext struct {
	ses        *TermSession
	file       *os.File
	fifo       *os.File
	busy       bool
	uid        uint32
	gid        uint32
	totalSize  uint32
	remainSize uint32
	savepath   string
	buf        [1024 * 63]byte
}

func (ctx *RttyFileContext) detect(data []byte) bool {
	if len(data) != len(RttyFileMagic) {
		return false
	}

	if data[0] != RttyFileMagic[0] || data[1] != RttyFileMagic[1] || data[2] != RttyFileMagic[2] {
		return false
	}

	pid := binary.NativeEndian.Uint32(data[4:])

	uid, err := utils.GetUidByPid(pid)
	if err != nil {
		syscall.Kill(int(pid), syscall.SIGTERM)
		log.Error().Err(err).Msgf("failed to get uid for pid %d", pid)
		return true
	}

	gid, err := utils.GetGidByPid(pid)
	if err != nil {
		syscall.Kill(int(pid), syscall.SIGTERM)
		log.Error().Err(err).Msgf("failed to get gid for pid %d", pid)
		return true
	}

	fifoName := fmt.Sprintf("/tmp/rtty-fifo-%d.fifo", pid)

	fifo, err := os.OpenFile(fifoName, os.O_WRONLY, 0)
	if err != nil {
		syscall.Kill(int(pid), syscall.SIGTERM)
		log.Error().Err(err).Msgf("Could not open fifo %s", fifoName)
		return true
	}

	ctx.fifo = fifo

	if ctx.busy {
		ctx.sendControlMsg(MsgTypeFileCtlBusy, nil)
		fifo.Close()
		return true
	}

	log.Debug().Msgf("detected file operation: sid=%s pid=%d, uid=%d, gid=%d", ctx.ses.sid, pid, uid, gid)

	if data[3] == 'R' {
		savepath, err := utils.GetCwdByPid(pid)
		if err != nil {
			ctx.sendControlMsg(MsgTypeFileCtlErr, nil)
			fifo.Close()
			log.Error().Err(err).Msgf("failed to get cwd for pid %d", pid)
			return true
		}

		ctx.savepath = savepath
		ctx.uid = uid
		ctx.gid = gid

		ctx.ses.cli.SendFileMsg(ctx.ses.sid, MsgTypeFileRecv, nil)

		ctx.sendControlMsg(MsgTypeFileCtlRequestAccept, nil)
	} else {
		fd := binary.NativeEndian.Uint32(data[8:])
		link := fmt.Sprintf("/proc/%d/fd/%d", pid, fd)

		path, err := os.Readlink(link)
		if err != nil {
			log.Error().Err(err).Msgf("failed to read link %s", link)
			ctx.sendControlMsg(MsgTypeFileCtlErr, nil)
			fifo.Close()
			return true
		}

		ctx.sendControlMsg(MsgTypeFileCtlRequestAccept, nil)

		err = ctx.startUpload(path)
		if err != nil {
			log.Error().Err(err).Msgf("failed to start upload file for path %s", path)
			ctx.sendControlMsg(MsgTypeFileCtlErr, nil)
			fifo.Close()
			return true
		}

	}

	ctx.busy = true

	return true
}

func (ctx *RttyFileContext) startDownload(data []byte) {
	ctx.totalSize = binary.BigEndian.Uint32(data)
	ctx.remainSize = ctx.totalSize

	err := utils.CheckSpaceAvailable(ctx.savepath, uint64(ctx.totalSize))
	if err != nil {
		log.Error().Err(err).Msgf("download file fail for %s", ctx.savepath)
		ctx.sendControlMsg(MsgTypeFileCtlNoSpace, nil)
		ctx.reset()
		return
	}

	name := string(data[4:])

	ctx.savepath = filepath.Join(ctx.savepath, name)

	if utils.FileExists(ctx.savepath) {
		log.Error().Msgf("file %s already exists", ctx.savepath)
		ctx.sendControlMsg(MsgTypeFileCtlErrExist, nil)
		ctx.reset()
		return
	}

	fd, err := os.OpenFile(ctx.savepath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		log.Error().Err(err).Msgf("failed to open file %s for writing", ctx.savepath)
		ctx.sendControlMsg(MsgTypeFileCtlErr, nil)
		ctx.reset()
		return
	}

	log.Debug().Msgf("download file: %s, size: %d bytes", ctx.savepath, ctx.totalSize)

	err = fd.Chown(int(ctx.uid), int(ctx.gid))
	if err != nil {
		log.Warn().Err(err).Msgf("failed to change owner of file %s to uid=%d gid=%d", ctx.savepath, ctx.uid, ctx.gid)
	}

	if ctx.totalSize == 0 {
		fd.Close()
	} else {
		ctx.file = fd
	}

	data = []byte{0, 0, 0, 0}

	binary.NativeEndian.PutUint32(data, ctx.totalSize)

	data = append(data, []byte(name)...)

	ctx.sendControlMsg(MsgTypeFileCtlInfo, data)
}

func (ctx *RttyFileContext) startUpload(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", path, err)
	}

	info, _ := file.Stat()

	ctx.file = file
	ctx.totalSize = uint32(info.Size())
	ctx.remainSize = ctx.totalSize

	ctx.ses.cli.SendFileMsg(ctx.ses.sid, MsgTypeFileSend, []byte(filepath.Base(path)))

	log.Debug().Msgf("upload file: %s, size: %d bytes", path, ctx.totalSize)

	return nil
}

func (ctx *RttyFileContext) reset() {
	if ctx.file != nil {
		ctx.file.Close()
		ctx.file = nil
	}

	if ctx.fifo != nil {
		ctx.fifo.Close()
		ctx.fifo = nil
	}

	ctx.busy = false
}

func (ctx *RttyFileContext) notifyProgress() error {
	buf := make([]byte, 4)
	binary.NativeEndian.PutUint32(buf, ctx.remainSize)
	return ctx.sendControlMsg(MsgTypeFileCtlProgress, buf)
}

func (ctx *RttyFileContext) sendData() {
	if ctx.file == nil {
		return
	}

	n, err := ctx.file.Read(ctx.buf[:])
	if err != nil {
		if err != io.EOF {
			log.Error().Err(err).Msgf("failed to read file %s", ctx.ses.sid)
			ctx.ses.cli.SendFileMsg(ctx.ses.sid, MsgTypeFileAbort, nil)
			ctx.sendControlMsg(MsgTypeFileCtlErr, nil)
			ctx.reset()
			return
		}
	}

	ctx.remainSize -= uint32(n)

	ctx.ses.cli.SendFileMsg(ctx.ses.sid, MsgTypeFileData, ctx.buf[:n])

	if n == 0 {
		ctx.reset()
		return
	}

	if ctx.notifyProgress() != nil {
		ctx.ses.cli.SendFileMsg(ctx.ses.sid, MsgTypeFileAbort, nil)
		ctx.reset()
		return
	}
}

func (ctx *RttyFileContext) sendControlMsg(typ byte, data []byte) error {
	buf := [fileCtlMsgSize]byte{typ}

	copy(buf[1:], data)

	if _, err := ctx.fifo.Write(buf[:]); err != nil {
		return err
	}

	return nil
}

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

	RttyFileMagic[3] = typ

	binary.NativeEndian.PutUint32(RttyFileMagic[4:], uint32(pid))

	if typ == 'S' {
		fd := uint32(sfd.Fd())
		binary.NativeEndian.PutUint32(RttyFileMagic[8:], fd)
	}

	os.Stdout.Write(RttyFileMagic[:])
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
		buf := make([]byte, fileCtlMsgSize)

		_, err := io.ReadFull(ctlfd, buf)
		if err != nil {
			return
		}

		typ := buf[0]
		buf = buf[1:]

		switch typ {
		case MsgTypeFileCtlRequestAccept:
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

		case MsgTypeFileCtlInfo:
			totalSize = binary.NativeEndian.Uint32(buf)
			fmt.Printf("Transferring '%s'...\n", string(buf[4:]))
			if totalSize == 0 {
				fmt.Println("  100%%    0 B     0s")
				return
			}
			startTime = time.Now()

		case MsgTypeFileCtlProgress:
			remainSize := binary.NativeEndian.Uint32(buf)
			updateProgress(startTime, totalSize, remainSize)
			if remainSize == 0 {
				fmt.Println()
				return
			}

		case MsgTypeFileCtlAbort:
			fmt.Println("\nTransfer aborted")
			return

		case MsgTypeFileCtlBusy:
			fmt.Println("\033[31mRtty is busy to transfer file\033[0m")
			return

		case MsgTypeFileCtlNoSpace:
			fmt.Println("\033[31mNo enough space\033[0m")
			return

		case MsgTypeFileCtlErrExist:
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
