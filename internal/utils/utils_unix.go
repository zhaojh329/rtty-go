//go:build !windows
// +build !windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package utils

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func GetUidByPid(pid uint32) (uint32, error) {
	statusFile := fmt.Sprintf("/proc/%d/status", pid)

	file, err := os.Open(statusFile)
	if err != nil {
		return 0, fmt.Errorf("failed to open %s: %w", statusFile, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineCount := 0

	for scanner.Scan() && lineCount < 20 {
		line := scanner.Text()
		lineCount++

		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				uid, err := strconv.ParseUint(fields[1], 10, 32)
				if err != nil {
					return 0, fmt.Errorf("failed to parse uid from line '%s': %w", line, err)
				}
				return uint32(uid), nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("error reading %s: %w", statusFile, err)
	}

	return 0, fmt.Errorf("uid not found in %s", statusFile)
}

func GetGidByPid(pid uint32) (uint32, error) {
	statusFile := fmt.Sprintf("/proc/%d/status", pid)

	file, err := os.Open(statusFile)
	if err != nil {
		return 0, fmt.Errorf("failed to open %s: %w", statusFile, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "Gid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				gid, err := strconv.ParseUint(fields[1], 10, 32)
				if err != nil {
					return 0, fmt.Errorf("failed to parse gid from line '%s': %w", line, err)
				}
				return uint32(gid), nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("error reading %s: %w", statusFile, err)
	}

	return 0, fmt.Errorf("gid not found in %s", statusFile)
}

func GetCwdByPid(pid uint32) (string, error) {
	link := fmt.Sprintf("/proc/%d/cwd", pid)

	cwd, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("failed to read cwd for pid %d: %w", pid, err)
	}

	return cwd, nil
}
