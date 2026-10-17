//go:build !windows

package main

import (
	"os"
	"syscall"
)

// flockExclusive 对文件取排他锁（非阻塞）。
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// flockRelease 释放锁。
func flockRelease(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// maxUserWatches 返回 inotify 的 watch 上限；非 Linux 返回错误表示不适用。
func maxUserWatches() (int, error) {
	const p = "/proc/sys/fs/inotify/max_user_watches"
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 0, os.ErrNotExist
	}
	return n, nil
}
