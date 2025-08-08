//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"os/exec"
	"os/user"
)

func setSysProcAttr(cmd *exec.Cmd, u *user.User) {
}
