//go:build windows
// +build windows

/*
 * MIT License
 *
 * Copyright (c) 2025 Jianhui Zhao <zhaojh329@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
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
