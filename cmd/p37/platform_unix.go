//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// sameFilesystem 报告两个路径是否位于同一文件系统（OverlayFS 的
// lower/upper/work 必须同 FS，§4.6）。
func sameFilesystem(a, b string) (bool, string) {
	sa, err := statDev(a)
	if err != nil {
		return false, err.Error()
	}
	sb, err := statDev(b)
	if err != nil {
		return false, err.Error()
	}
	if sa == sb {
		return true, fmt.Sprintf("dev=%d", sa)
	}
	return false, fmt.Sprintf("不同设备：%d vs %d", sa, sb)
}

func statDev(p string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// diskAvailable 返回路径所在文件系统的可用字节数。
func diskAvailable(p string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// maxUserWatches 返回 inotify 的 watch 上限；非 Linux 返回错误表示不适用。
func maxUserWatches() (int, error) {
	if !isLinux {
		return 0, errors.New("not linux")
	}
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(string(b), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}
