//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	conpty "github.com/qsocket/conpty-go"
	"github.com/rs/zerolog/log"
)

type Terminal struct {
	pty       *conpty.ConPty
	wait_ack  atomic.Int32
	cond      *sync.Cond
	ack_block int32
	closeOnce sync.Once
}

func NewTerminal(username string) (*Terminal, error) {
	// 使用 COMSPEC 环境变量定位系统 shell，提供更好的兼容性
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "C:\\Windows\\System32\\cmd.exe"
	}

	// /D - 禁止执行 AutoRun 命令（跳过注册表中的启动脚本）
	// /Q - 安静模式，关闭回显
	// 这些参数可以加快启动速度并避免意外配置导致的问题
	pty, err := conpty.Start(shell + " /D /Q")
	if err != nil {
		return nil, err
	}

	t := &Terminal{
		pty:       pty,
		ack_block: 4096,
		cond:      sync.NewCond(&sync.Mutex{}),
	}

	go func() {
		code, err := pty.Wait(context.Background())
		log.Info().Msgf("ConPTY child exited: code=%d err=%v", code, err)
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
