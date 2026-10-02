//go:build linux

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package client

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/zhaojh329/rtty-go/internal/filetransfer"
	"github.com/zhaojh329/rtty-go/proto"
	"golang.org/x/sys/unix"
)

const (
	fileIdle = iota
	fileWaitRequest
	fileWaitAck
	fileWaitInfo
	fileReceiving
)

const fileHandshakeTimeout = 5 * time.Second

func handleFileMsg(cli *RttyClient, data []byte) error {
	if len(data) < 32 {
		return fmt.Errorf("invalid file session ID")
	}

	sid := string(data[:32])
	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("terminal session %s not found", sid)
		return nil
	}

	s, ok := val.(*TermSession)
	if !ok {
		return nil
	}

	s.fc.handleServerMessage(data[32:])
	return nil
}

type RttyFileContext struct {
	ses                 *TermSession
	mu                  sync.Mutex
	listener            *net.UnixListener
	controlConn         *net.UnixConn
	handshakeTimer      *time.Timer
	closed              bool
	state               int
	file                *os.File
	receiveDir          *os.File
	ownerUID            int
	ownerGID            int
	receiveMode         uint32
	totalSize           uint32
	remainSize          uint32
	awaitingProgressAck bool
	basename            string
	temporaryName       string
}

func (ctx *RttyFileContext) init() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	if ctx.closed {
		return nil
	}

	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}

	var mask uint64
	found := false
	for line := range strings.SplitSeq(string(status), "\n") {
		if after, ok := strings.CutPrefix(line, "Umask:"); ok {
			mask, err = strconv.ParseUint(strings.TrimSpace(after), 8, 9)
			if err != nil {
				return err
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("process umask unavailable")
	}
	ctx.receiveMode = 0644 &^ uint32(mask)

	address, err := filetransfer.SocketAddress(ctx.ses.term.pty)
	if err != nil {
		return err
	}

	listener, err := net.ListenUnix("unixpacket", address)
	if err != nil {
		return err
	}

	ctx.listener = listener
	go ctx.acceptConnections(listener)
	return nil
}

func (ctx *RttyFileContext) acceptConnections(listener *net.UnixListener) {
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}

		ctx.handleConnection(conn)
	}
}

func (ctx *RttyFileContext) handleConnection(conn *net.UnixConn) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	if ctx.closed {
		conn.Close()
		return
	}

	cred, err := filetransfer.PeerCredentials(conn)
	if err != nil || cred.Pid <= 0 {
		conn.Close()
		return
	}

	raw, err := ctx.ses.term.pty.SyscallConn()
	if err != nil {
		conn.Close()
		return
	}

	var ttySID int
	var ioctlErr error
	err = raw.Control(func(fd uintptr) {
		ttySID, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGSID)
	})
	peerSID, sessionErr := unix.Getsid(int(cred.Pid))
	if err != nil || ioctlErr != nil || sessionErr != nil || ttySID <= 0 || peerSID != ttySID {
		conn.Close()
		return
	}

	if ctx.controlConn != nil {
		code := make([]byte, 4)
		binary.BigEndian.PutUint32(code, uint32(unix.EBUSY))
		filetransfer.SendPacket(conn, filetransfer.Error, code, -1)
		conn.Close()
		return
	}

	ctx.controlConn = conn
	ctx.ownerUID, ctx.ownerGID = int(cred.Uid), int(cred.Gid)
	ctx.state = fileWaitRequest
	ctx.handshakeTimer = time.AfterFunc(fileHandshakeTimeout, func() {
		ctx.mu.Lock()
		defer ctx.mu.Unlock()

		if ctx.controlConn == conn && ctx.state == fileWaitRequest {
			ctx.finishTransfer(unix.ETIMEDOUT, false)
		}
	})

	if err := filetransfer.SendPacket(conn, filetransfer.Accept, nil, -1); err != nil {
		ctx.resetTransfer()
		return
	}

	go ctx.readControlPackets(conn)
}

func (ctx *RttyFileContext) readControlPackets(conn *net.UnixConn) {
	for {
		packet, err := filetransfer.ReceivePacket(conn)

		ctx.mu.Lock()
		if ctx.controlConn != conn {
			ctx.mu.Unlock()
			if packet.FD >= 0 {
				unix.Close(packet.FD)
			}
			return
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				err = unix.ECANCELED
			}
			ctx.finishTransfer(err, true)
			ctx.mu.Unlock()
			return
		}

		ctx.ses.active()
		if ctx.state == fileWaitRequest {
			err = ctx.startTransfer(&packet)
		} else if packet.Type == filetransfer.Progress && len(packet.Data) == 0 && packet.FD < 0 && ctx.awaitingProgressAck {
			ctx.awaitingProgressAck = false
		} else {
			err = unix.EPROTO
		}

		if packet.FD >= 0 {
			unix.Close(packet.FD)
		}
		if err != nil {
			ctx.finishTransfer(err, true)
		}
		active := ctx.controlConn == conn
		ctx.mu.Unlock()

		if !active {
			return
		}
	}
}

// startTransfer clears packet.FD when it takes ownership. The caller holds ctx.mu.
func (ctx *RttyFileContext) startTransfer(packet *filetransfer.Packet) error {
	if len(packet.Data) != 0 || packet.FD < 0 {
		return unix.EPROTO
	}

	switch packet.Type {
	case filetransfer.Send:
		ctx.file = os.NewFile(uintptr(packet.FD), "rtty source")
		packet.FD = -1
		if err := ctx.prepareSendFile(); err != nil {
			return err
		}
		if err := ctx.notifyInfo(); err != nil {
			return err
		}

		ctx.state = fileWaitAck
		if err := ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileSend, []byte(ctx.basename)); err != nil {
			return err
		}

	case filetransfer.Recv:
		var st unix.Stat_t
		if err := unix.Fstat(packet.FD, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return unix.EPROTO
		}

		ctx.receiveDir = os.NewFile(uintptr(packet.FD), "rtty directory")
		packet.FD = -1
		ctx.state = fileWaitInfo
		if err := ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileRecv, nil); err != nil {
			return err
		}

	default:
		return unix.EPROTO
	}

	ctx.handshakeTimer.Stop()
	ctx.handshakeTimer = nil
	return nil
}

func (ctx *RttyFileContext) prepareSendFile() error {
	st, err := ctx.file.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return unix.EINVAL
	}
	if st.Size() < 0 || st.Size() > math.MaxUint32 {
		return unix.EFBIG
	}

	resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", ctx.file.Fd()))
	if err != nil {
		return err
	}

	ctx.basename = filepath.Base(resolved)
	if !filetransfer.ValidName([]byte(ctx.basename)) {
		return unix.EINVAL
	}

	ctx.totalSize = uint32(st.Size())
	ctx.remainSize = ctx.totalSize
	return nil
}

func (ctx *RttyFileContext) prepareReceiveFile(data []byte) error {
	if len(data) < 5 || !filetransfer.ValidName(data[4:]) {
		return unix.EPROTO
	}

	ctx.totalSize = binary.BigEndian.Uint32(data)
	ctx.remainSize = ctx.totalSize
	ctx.basename = string(data[4:])
	dirfd := int(ctx.receiveDir.Fd())
	var st unix.Stat_t
	err := unix.Fstatat(dirfd, ctx.basename, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return unix.EEXIST
	}
	if err != unix.ENOENT {
		return err
	}

	var fs unix.Statfs_t
	if err := unix.Fstatfs(dirfd, &fs); err != nil {
		return err
	}

	available := uint64(fs.Bavail) * uint64(fs.Bsize)
	if uint32(fs.Type) == unix.RAMFS_MAGIC {
		var info unix.Sysinfo_t
		if err := unix.Sysinfo(&info); err != nil {
			return err
		}
		available = uint64(info.Freeram) * uint64(info.Unit)
	}
	if uint64(ctx.totalSize) > available {
		return unix.ENOSPC
	}

	temporaryName := ".rtty-" + rand.Text() + ".part"
	fd, err := unix.Openat(dirfd, temporaryName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}

	ctx.file = os.NewFile(uintptr(fd), temporaryName)
	ctx.temporaryName = temporaryName
	return nil
}

func (ctx *RttyFileContext) commitReceivedFile() error {
	if err := filetransfer.CheckConnection(ctx.controlConn); err != nil {
		return err
	}

	if err := ctx.file.Chown(ctx.ownerUID, ctx.ownerGID); err != nil {
		log.Warn().Err(err).Msgf("failed to change owner of file %s", ctx.basename)
	}
	if err := ctx.file.Chmod(os.FileMode(ctx.receiveMode)); err != nil {
		return err
	}
	if err := ctx.file.Sync(); err != nil {
		return err
	}

	file := ctx.file
	ctx.file = nil
	if err := file.Close(); err != nil {
		return err
	}
	if err := filetransfer.CheckConnection(ctx.controlConn); err != nil {
		return err
	}

	dirfd := int(ctx.receiveDir.Fd())
	err := unix.Renameat2(dirfd, ctx.temporaryName, dirfd, ctx.basename, unix.RENAME_NOREPLACE)
	if err == nil {
		ctx.temporaryName = ""
		return nil
	}
	if err != unix.ENOSYS && err != unix.EINVAL && err != unix.EOPNOTSUPP {
		return err
	}

	return unix.Linkat(dirfd, ctx.temporaryName, dirfd, ctx.basename, 0)
}

func (ctx *RttyFileContext) handleServerMessage(data []byte) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	if len(data) == 0 {
		ctx.finishTransfer(unix.EPROTO, true)
		return
	}
	if ctx.state == fileIdle || ctx.state == fileWaitRequest {
		return
	}

	ctx.ses.active()
	typ, data := data[0], data[1:]
	var err error
	switch typ {
	case proto.MsgTypeFileInfo:
		if ctx.state != fileWaitInfo {
			err = unix.EPROTO
			break
		}
		if err = ctx.prepareReceiveFile(data); err != nil {
			break
		}

		ctx.state = fileReceiving
		err = ctx.notifyInfo()

	case proto.MsgTypeFileData:
		if ctx.state != fileReceiving || uint64(len(data)) > uint64(ctx.remainSize) || len(data) == 0 && ctx.remainSize != 0 {
			err = unix.EPROTO
			break
		}
		err = ctx.receiveData(data)

	case proto.MsgTypeFileAck:
		if ctx.state != fileWaitAck || len(data) != 0 {
			err = unix.EPROTO
			break
		}
		err = ctx.sendData()

	case proto.MsgTypeFileAbort:
		if len(data) != 0 {
			err = unix.EPROTO
			break
		}
		ctx.finishTransfer(unix.ECANCELED, false)

	default:
		err = unix.EPROTO
	}

	if err != nil {
		ctx.finishTransfer(err, true)
	}
}

func (ctx *RttyFileContext) sendData() error {
	buf := make([]byte, min(ctx.remainSize, 63*1024))
	n, err := ctx.file.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n == 0 && len(buf) != 0 {
		return unix.EIO
	}
	if err := ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileData, buf[:n]); err != nil {
		return err
	}

	ctx.remainSize -= uint32(n)
	if n == 0 {
		ctx.finishTransfer(nil, false)
		return nil
	}

	return ctx.notifyProgress()
}

func (ctx *RttyFileContext) receiveData(data []byte) error {
	if _, err := ctx.file.Write(data); err != nil {
		return err
	}

	ctx.remainSize -= uint32(len(data))

	if ctx.remainSize == 0 {
		if err := ctx.commitReceivedFile(); err != nil {
			return err
		}
		ctx.finishTransfer(nil, false)
		return nil
	}
	if err := ctx.notifyProgress(); err != nil {
		return err
	}

	return ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileAck, nil)
}

func (ctx *RttyFileContext) notifyInfo() error {
	data := make([]byte, 4, 4+len(ctx.basename))
	binary.BigEndian.PutUint32(data, ctx.totalSize)
	data = append(data, ctx.basename...)
	return filetransfer.SendPacket(ctx.controlConn, filetransfer.Info, data, -1)
}

func (ctx *RttyFileContext) notifyProgress() error {
	if ctx.awaitingProgressAck {
		return nil
	}

	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, ctx.remainSize)
	if err := filetransfer.SendPacket(ctx.controlConn, filetransfer.Progress, data, -1); err != nil {
		return err
	}

	ctx.awaitingProgressAck = true
	return nil
}

func (ctx *RttyFileContext) finishTransfer(err error, abortRemote bool) {
	if ctx.controlConn == nil {
		return
	}

	if abortRemote && ctx.state != fileWaitRequest {
		ctx.ses.cli.SendFileMsg(ctx.ses.sid, proto.MsgTypeFileAbort, nil)
	}

	if err != nil {
		code := unix.EIO
		errors.As(err, &code)
		data := make([]byte, 4)
		binary.BigEndian.PutUint32(data, uint32(code))
		filetransfer.SendFinalPacket(ctx.controlConn, filetransfer.Error, data)
	} else {
		filetransfer.SendFinalPacket(ctx.controlConn, filetransfer.Done, nil)
	}

	ctx.resetTransfer()
}

func (ctx *RttyFileContext) resetTransfer() {
	if ctx.handshakeTimer != nil {
		ctx.handshakeTimer.Stop()
		ctx.handshakeTimer = nil
	}
	if ctx.file != nil {
		ctx.file.Close()
		ctx.file = nil
	}
	if ctx.temporaryName != "" {
		if err := unix.Unlinkat(int(ctx.receiveDir.Fd()), ctx.temporaryName, 0); err != nil {
			log.Error().Err(err).Msgf("failed to remove temporary file %s", ctx.temporaryName)
		}
		ctx.temporaryName = ""
	}
	if ctx.receiveDir != nil {
		ctx.receiveDir.Close()
		ctx.receiveDir = nil
	}
	if ctx.controlConn != nil {
		ctx.controlConn.Close()
		ctx.controlConn = nil
	}

	ctx.state = fileIdle
	ctx.awaitingProgressAck = false
}

func (ctx *RttyFileContext) close() {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	ctx.closed = true
	if ctx.listener != nil {
		ctx.listener.Close()
		ctx.listener = nil
	}
	ctx.resetTransfer()
}
