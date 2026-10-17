//go:build unix

package capture

import (
	"io/fs"
	"syscall"
)

// inodeOf 返回文件的 inode 号。
//
// 注意：OverlayFS 下该值可能不稳定，调用方须通过 InodeReliable 决定是否使用
// （§4.7）。
func inodeOf(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
