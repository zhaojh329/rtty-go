//go:build windows
// +build windows

package main

import (
	"os/exec"
	"os/user"
)

func setSysProcAttr(cmd *exec.Cmd, u *user.User) {
}
