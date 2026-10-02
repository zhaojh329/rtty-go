//go:build !linux

/* SPDX-License-Identifier: MIT */

package main

import (
	"context"

	"github.com/urfave/cli/v3"
)

func requestFileTransfer(c context.Context, typ byte, path string) error {
	return cli.Exit("file transfer is only supported on Linux", 1)
}
