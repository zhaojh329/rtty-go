//go:build !windows
// +build !windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
)

type Terminal struct {
	pty       *os.File
	cmd       *exec.Cmd
	wait_ack  atomic.Int32
	cond      *sync.Cond
	ack_block int32
}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func NewTerminal(username string) (*Terminal, error) {
	var cmd *exec.Cmd
	if username != "" {
		cmd = exec.Command("/bin/login", "-f", username)
	} else {
		cmd = exec.Command("/bin/login")
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	t := &Terminal{
		pty:       ptmx,
		cmd:       cmd,
		ack_block: 4096,
		cond:      sync.NewCond(&sync.Mutex{}),
	}

	return t, nil
}

func (t *Terminal) Read(buf []byte) (int, error) {
	return t.pty.Read(buf)
}

func (t *Terminal) Write(data []byte) (int, error) {
	return t.pty.Write(data)
}

func (t *Terminal) SetWinSize(cols, rows uint16) error {
	ws := &winsize{
		Row: rows,
		Col: cols,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		t.pty.Fd(),
		uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(ws)),
	)
	if errno != 0 {
		return errno
	}

	return nil
}

func (t *Terminal) Close() error {
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}

	if t.pty != nil {
		return t.pty.Close()
	}

	return nil
}

func (t *Terminal) Ack(n uint16) {
	t.wait_ack.Add(-int32(n))
	t.cond.Signal()
}

func (t *Terminal) WaitAck(len int) {
	newWaitAck := t.wait_ack.Add(int32(len))

	if newWaitAck > t.ack_block {
		t.cond.L.Lock()
		for t.wait_ack.Load() > t.ack_block {
			t.cond.Wait()
		}
		t.cond.L.Unlock()
	}
}
