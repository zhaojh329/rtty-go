//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"context"
	"sync"
	"sync/atomic"

	conpty "github.com/qsocket/conpty-go"
)

type Terminal struct {
	pty       *conpty.ConPty
	wait_ack  atomic.Int32
	cond      *sync.Cond
	ack_block int32
	closeOnce sync.Once
	closed    bool
}

func NewTerminal(username string) (*Terminal, error) {
	pty, err := conpty.Start("cmd.exe")
	if err != nil {
		return nil, err
	}

	t := &Terminal{
		pty:       pty,
		ack_block: 4096,
		cond:      sync.NewCond(&sync.Mutex{}),
		closed:    false,
	}

	// 设置初始窗口大小，防止某些 Windows 版本中的零尺寸问题
	if err := pty.Resize(80, 24); err != nil {
		// 记录错误但继续，因为这不是致命错误
		// TODO: 添加日志记录
	}

	go func() {
		pty.Wait(context.Background())
		t.Close()
	}()

	return t, nil

}

func (t *Terminal) Read(buf []byte) (int, error) {
	return t.pty.Read(buf)
}

func (t *Terminal) Write(data []byte) (int, error) {
	return t.pty.Write(data)
}

func (t *Terminal) SetWinSize(cols, rows uint16) error {
	return t.pty.Resize(int(cols), int(rows))
}

func (t *Terminal) Close() error {
	t.closeOnce.Do(func() {
		t.closed = true
		t.wait_ack.Store(0)
		t.cond.Broadcast()
		t.pty.Close()
	})
	return nil
}

func (t *Terminal) Ack(n uint16) {
	t.wait_ack.Add(-int32(n))
	t.cond.Signal()
}

func (t *Terminal) WaitAck(len int) {
	if t.closed {
		return
	}

	newWaitAck := t.wait_ack.Add(int32(len))

	if newWaitAck > t.ack_block {
		t.cond.L.Lock()
		for !t.closed && t.wait_ack.Load() > t.ack_block {
			t.cond.Wait()
		}
		t.cond.L.Unlock()
	}
}
