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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type MountInfo struct {
	Device     string
	MountPoint string
	FileSystem string
	Options    string
}

func CheckSpaceAvailable(savePath string, totalSize uint64) error {
	mountInfo, err := findMountPoint(savePath)
	if err != nil {
		return fmt.Errorf("not found mount point of '%s': %w", savePath, err)
	}

	var avail uint64

	if mountInfo.FileSystem == "ramfs" {
		avail, err = getAvailableRAM()
		if err != nil {
			return fmt.Errorf("failed to get available RAM: %w", err)
		}
	} else {
		avail, err = getAvailableSpace(mountInfo.MountPoint)
		if err != nil {
			return fmt.Errorf("failed to get available space: %w", err)
		}
	}

	if totalSize > avail {
		return fmt.Errorf("no enough space: need %d bytes, available %d bytes", totalSize, avail)
	}

	return nil
}

func findMountPoint(name string) (*MountInfo, error) {
	absPath, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}

	var stat syscall.Stat_t
	if err := syscall.Stat(absPath, &stat); err != nil {
		return nil, err
	}

	devnoOfName := stat.Dev

	if (stat.Mode&syscall.S_IFMT) == syscall.S_IFBLK ||
		(stat.Mode&syscall.S_IFMT) == syscall.S_IFCHR {
		return nil, fmt.Errorf("path is a device file")
	}

	mounts, err := readMountInfo()
	if err != nil {
		return nil, err
	}

	var bestMatch *MountInfo

	for _, mount := range mounts {
		if mount.FileSystem == "rootfs" {
			continue
		}

		if absPath == mount.MountPoint {
			return &mount, nil
		}

		var mountStat syscall.Stat_t
		if syscall.Stat(mount.MountPoint, &mountStat) == nil && mountStat.Dev == devnoOfName {
			bestMatch = &mount
		}
	}

	if bestMatch != nil {
		return bestMatch, nil
	}

	return nil, fmt.Errorf("mount point not found")
}

func readMountInfo() ([]MountInfo, error) {
	file, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var mounts []MountInfo
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			mounts = append(mounts, MountInfo{
				Device:     fields[0],
				MountPoint: fields[1],
				FileSystem: fields[2],
				Options:    fields[3],
			})
		}
	}

	return mounts, scanner.Err()
}

func getAvailableRAM() (uint64, error) {
	var sysinfo syscall.Sysinfo_t
	if err := syscall.Sysinfo(&sysinfo); err != nil {
		return 0, err
	}
	return uint64(sysinfo.Freeram) * uint64(sysinfo.Unit), nil
}

func getAvailableSpace(mountPoint string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mountPoint, &stat); err != nil {
		return 0, err
	}

	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

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
