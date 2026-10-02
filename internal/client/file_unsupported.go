//go:build !linux

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package client

import "fmt"

func handleFileMsg(cli *RttyClient, data []byte) error {
	return fmt.Errorf("file transfer is only supported on Linux")
}

type RttyFileContext struct {
	ses *TermSession
}

func (ctx *RttyFileContext) init() error {
	return nil
}

func (ctx *RttyFileContext) close() {
}
