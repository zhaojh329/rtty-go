//go:build linux

/* SPDX-License-Identifier: MIT */

package filetransfer

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

type Packet struct {
	Type byte
	Data []byte
	FD   int
}

func SocketAddress(tty *os.File) (*net.UnixAddr, error) {
	raw, err := tty.SyscallConn()
	if err != nil {
		return nil, err
	}

	var device uint32
	var ioctlErr error
	err = raw.Control(func(fd uintptr) {
		device, ioctlErr = unix.IoctlGetUint32(int(fd), unix.TIOCGDEV)
	})
	if err != nil {
		return nil, err
	}
	if ioctlErr != nil {
		return nil, ioctlErr
	}

	return &net.UnixAddr{Name: fmt.Sprintf("@rtty.file.tty.%d", device), Net: "unixpacket"}, nil
}

func PeerCredentials(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}

	var cred *unix.Ucred
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return nil, err
	}

	return cred, socketErr
}

// SendPacket never waits for a stopped peer to drain its receive queue.
func SendPacket(conn *net.UnixConn, typ byte, data []byte, passedFD int) error {
	if len(data) > MaxPayloadLen {
		return unix.EMSGSIZE
	}

	buf := make([]byte, 4+len(data))
	buf[0], buf[1] = 2, typ
	binary.BigEndian.PutUint16(buf[2:], uint16(len(data)))
	copy(buf[4:], data)

	var control []byte
	if passedFD >= 0 {
		control = unix.UnixRights(passedFD)
	}

	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var sendErr error
	err = raw.Control(func(fd uintptr) {
		for {
			sendErr = unix.Sendmsg(int(fd), buf, control, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
			if sendErr != unix.EINTR {
				break
			}
		}
	})
	if err != nil {
		return err
	}

	return sendErr
}

func ReceivePacket(conn *net.UnixConn) (Packet, error) {
	packet := Packet{FD: -1}
	raw, err := conn.SyscallConn()
	if err != nil {
		return packet, err
	}

	buf := make([]byte, 4+MaxPayloadLen)
	control := make([]byte, unix.CmsgSpace(4))
	var n, oobn, flags int
	var recvErr error
	err = raw.Read(func(fd uintptr) bool {
		for {
			n, oobn, flags, _, recvErr = unix.Recvmsg(int(fd), buf, control, unix.MSG_CMSG_CLOEXEC|unix.MSG_DONTWAIT)
			if recvErr != unix.EINTR {
				return recvErr != unix.EAGAIN
			}
		}
	})
	if err != nil {
		return packet, err
	}
	if recvErr != nil {
		return packet, recvErr
	}

	messages, err := unix.ParseSocketControlMessage(control[:oobn])
	invalid := err != nil
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			invalid = true
			continue
		}

		for _, fd := range fds {
			if packet.FD < 0 {
				packet.FD = fd
			} else {
				unix.Close(fd)
				invalid = true
			}
		}
	}

	if n == 0 && packet.FD < 0 && !invalid && flags&unix.MSG_CTRUNC == 0 {
		return packet, io.EOF
	}

	if invalid || n < 4 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || buf[0] != 2 ||
		int(binary.BigEndian.Uint16(buf[2:])) != n-4 {
		if packet.FD >= 0 {
			unix.Close(packet.FD)
			packet.FD = -1
		}
		return packet, unix.EPROTO
	}

	packet.Type, packet.Data = buf[1], buf[4:n]
	return packet, nil
}

func CheckConnection(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var pollErr error
	err = raw.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLRDHUP}}
		_, pollErr = unix.Poll(fds, 0)
		if pollErr == nil && fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR) != 0 {
			pollErr = unix.ECANCELED
		}
	})
	if err != nil {
		return err
	}

	return pollErr
}

// SendFinalPacket shuts down reads and drains queued packets before sending
// DONE or ERROR. The caller closes the connection after this returns, without
// unread packets resetting the peer before it can read the result.
func SendFinalPacket(conn *net.UnixConn, typ byte, data []byte) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	err = raw.Control(func(fd uintptr) {
		unix.Shutdown(int(fd), unix.SHUT_RD)
		buf := make([]byte, 4+MaxPayloadLen)
		control := make([]byte, unix.CmsgSpace(4))
		for {
			n, oobn, _, _, recvErr := unix.Recvmsg(int(fd), buf, control, unix.MSG_DONTWAIT|unix.MSG_CMSG_CLOEXEC)
			if recvErr == unix.EINTR {
				continue
			}
			if recvErr != nil {
				break
			}

			messages, _ := unix.ParseSocketControlMessage(control[:oobn])
			for _, message := range messages {
				fds, _ := unix.ParseUnixRights(&message)
				for _, received := range fds {
					unix.Close(received)
				}
			}
			if n == 0 {
				break
			}
		}
	})
	if err != nil {
		return err
	}

	return SendPacket(conn, typ, data, -1)
}
