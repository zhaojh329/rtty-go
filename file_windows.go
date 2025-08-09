//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"fmt"
)

func handleFileMsg(cli *RttyClient, data []byte) error {
	return fmt.Errorf("not supported on Windows")
}

type RttyFileContext struct {
	ses *TermSession
}

func (ctx *RttyFileContext) detect(_ []byte) bool {
	return false
}

func (ctx *RttyFileContext) reset() {
}

func requestTransferFile(typ byte, path string) {
}
