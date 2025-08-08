//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package utils

import (
	"fmt"
)

type MountInfo struct {
	Device     string
	MountPoint string
	FileSystem string
	Options    string
}

func CheckSpaceAvailable(savePath string, totalSize uint64) error {
	return fmt.Errorf("not supported on Windows")
}

func GetUidByPid(pid uint32) (uint32, error) {
	return 0, fmt.Errorf("not supported on Windows")
}

func GetGidByPid(pid uint32) (uint32, error) {
	return 0, fmt.Errorf("not supported on Windows")
}

func GetCwdByPid(pid uint32) (string, error) {
	return "", fmt.Errorf("not supported on Windows")
}
