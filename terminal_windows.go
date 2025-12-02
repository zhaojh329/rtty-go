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
		t.wait_ack.Store(0)
		t.cond.Signal()
		t.pty.Close()
	})
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
