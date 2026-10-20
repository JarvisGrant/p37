//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const byLabelDir = "/dev/disk/by-label"

func mountPayload(label, target string, timeout time.Duration) error {
	dev, err := waitForLabel(label, timeout)
	if err != nil {
		return err
	}

	if src, mounted := mountSource(target); mounted {
		if sameDevice(src, dev) {
			return nil
		}
		return fmt.Errorf("目标 %s 已挂载自 %s，而当前投递盘是 %s；请重启实例后再换盘",
			target, src, dev)
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("创建挂载点 %s: %w", target, err)
	}
	// noload：只读挂载时不回放 ext4 journal，避免宿主上未清理的日志在
	// 实例内被写回（即使 MS_RDONLY 已经阻止大部分写入）。
	if err := unix.Mount(dev, target, "ext4",
		unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "noload"); err != nil {
		return fmt.Errorf("只读挂载 %s -> %s: %w", dev, target, err)
	}
	return nil
}

func waitForLabel(label string, timeout time.Duration) (string, error) {
	link := filepath.Join(byLabelDir, label)
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if p, err := filepath.EvalSymlinks(link); err == nil {
			if _, serr := os.Stat(p); serr == nil {
				return p, nil
			} else {
				last = serr
			}
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = errors.New("设备未出现")
			}
			return "", fmt.Errorf("等待 %s 超时（%s）: %w", link, timeout, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mountSource 从 /proc/self/mounts 判断 target 是否已挂载。
func mountSource(target string) (string, bool) {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == target {
			return f[0], true
		}
	}
	return "", false
}

func sameDevice(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}
