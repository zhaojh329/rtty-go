//go:build freebsd || openbsd || netbsd || darwin
// +build freebsd openbsd netbsd darwin

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package utils

import (
	"fmt"

	"github.com/shirou/gopsutil/v3/disk"
)

func CheckSpaceAvailable(savePath string, totalSize uint64) error {
	usage, err := disk.Usage(savePath)
	if err != nil {
		return err
	}

	if usage.Free < totalSize {
		return fmt.Errorf("no enough space: need %d bytes, available %d bytes", totalSize, usage.Free)
	}

	return nil
}
