//go:build unix && !linux

package mountmgr

import (
	"os"
	"path/filepath"
	"syscall"
)

// IsMountPoint 报告 path 是否为挂载点。
//
// 非 Linux 平台没有 /proc/self/mountinfo，使用 st_dev 与父目录比较。
// 跨文件系统挂载会被识别；同一设备上的 bind mount 在这里无法识别
// （开发机场景足够，生产挂载点判断发生在 Linux）。
func IsMountPoint(path string) bool {
	want, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	fi, err := os.Stat(want)
	if err != nil || !fi.IsDir() {
		return false
	}
	parent := filepath.Dir(want)
	if parent == want {
		return false
	}
	pfi, err := os.Stat(parent)
	if err != nil {
		return false
	}
	return devOf(fi) != devOf(pfi)
}

func devOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}
