//go:build !windows
// +build !windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package client

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"github.com/zhaojh329/rtty-go/internal/utils"
	"github.com/zhaojh329/rtty-go/proto"

	"github.com/rs/zerolog/log"
)

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
	case proto.MsgTypeFileInfo:
		s.fc.startDownload(data)

	case proto.MsgTypeFileData:
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
						cli.SendFileMsg(s.sid, proto.MsgTypeFileAck, nil)
					}
				}
			}
		} else {
			s.fc.reset()
		}

	case proto.MsgTypeFileAck:
		s.fc.sendData()

	case proto.MsgTypeFileAbort:
		s.fc.sendControlMsg(filetransfer.ControlAbort, nil)
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
	if !filetransfer.IsMagic(data) {
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
		ctx.sendControlMsg(filetransfer.ControlBusy, nil)
		fifo.Close()
		return true
	}

	log.Debug().Msgf("detected file operation: sid=%s pid=%d, uid=%d, gid=%d", ctx.ses.sid, pid, uid, gid)

	if data[3] == 'R' {
		savepath, err := utils.GetCwdByPid(pid)
		if err != nil {
			ctx.sendControlMsg(filetransfer.ControlErr, nil)
			fifo.Close()
			log.Error().Err(err).Msgf("failed to get cwd for pid %d", pid)
			return true
		}

		ctx.savepath = savepath
		ctx.uid = uid
		ctx.gid = gid

		ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileRecv, nil)

		ctx.sendControlMsg(filetransfer.ControlRequestAccept, nil)
	} else {
		fd := binary.NativeEndian.Uint32(data[8:])
		link := fmt.Sprintf("/proc/%d/fd/%d", pid, fd)

		path, err := os.Readlink(link)
		if err != nil {
			log.Error().Err(err).Msgf("failed to read link %s", link)
			ctx.sendControlMsg(filetransfer.ControlErr, nil)
			fifo.Close()
			return true
		}

		ctx.sendControlMsg(filetransfer.ControlRequestAccept, nil)

		err = ctx.startUpload(path)
		if err != nil {
			log.Error().Err(err).Msgf("failed to start upload file for path %s", path)
			ctx.sendControlMsg(filetransfer.ControlErr, nil)
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

	name := string(data[4:])
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		ctx.sendControlMsg(filetransfer.ControlErr, nil)
		ctx.reset()
		return
	}

	err := utils.CheckSpaceAvailable(ctx.savepath, uint64(ctx.totalSize))
	if err != nil {
		log.Error().Err(err).Msgf("download file fail for %s", ctx.savepath)
		ctx.sendControlMsg(filetransfer.ControlNoSpace, nil)
		ctx.reset()
		return
	}

	ctx.savepath = filepath.Join(ctx.savepath, name)

	if utils.FileExists(ctx.savepath) {
		log.Error().Msgf("file %s already exists", ctx.savepath)
		ctx.sendControlMsg(filetransfer.ControlErrExist, nil)
		ctx.reset()
		return
	}

	fd, err := os.OpenFile(ctx.savepath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		log.Error().Err(err).Msgf("failed to open file %s for writing", ctx.savepath)
		ctx.sendControlMsg(filetransfer.ControlErr, nil)
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

	ctx.sendControlMsg(filetransfer.ControlInfo, data)
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

	ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileSend, []byte(filepath.Base(path)))

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
	return ctx.sendControlMsg(filetransfer.ControlProgress, buf)
}

func (ctx *RttyFileContext) sendData() {
	if ctx.file == nil {
		return
	}

	n, err := ctx.file.Read(ctx.buf[:])
	if err != nil {
		if err != io.EOF {
			log.Error().Err(err).Msgf("failed to read file %s", ctx.ses.sid)
			ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileAbort, nil)
			ctx.sendControlMsg(filetransfer.ControlErr, nil)
			ctx.reset()
			return
		}
	}

	ctx.remainSize -= uint32(n)

	ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileData, ctx.buf[:n])

	if n == 0 {
		ctx.reset()
		return
	}

	if ctx.notifyProgress() != nil {
		ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileAbort, nil)
		ctx.reset()
		return
	}
}

func (ctx *RttyFileContext) sendControlMsg(typ byte, data []byte) error {
	buf := [filetransfer.ControlMessageSize]byte{typ}

	copy(buf[1:], data)

	if _, err := ctx.fifo.Write(buf[:]); err != nil {
		return err
	}

	return nil
}
