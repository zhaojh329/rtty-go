//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"fmt"
	"os"
)

func handleFileMsg(cli *RttyClient, data []byte) error {
	return fmt.Errorf("not supported on Windows")
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

func (ctx *RttyFileContext) detect(_ []byte) bool {
	return false
}

func (ctx *RttyFileContext) reset() {
}

func requestTransferFile(typ byte, path string) {
}
