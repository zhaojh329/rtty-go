//go:build !windows
// +build !windows

package main

import (
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd, u *user.User) {
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
}
