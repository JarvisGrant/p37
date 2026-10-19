//go:build linux

package cas

import "golang.org/x/sys/unix"

// syncFS 把某个路径所在文件系统的全部未落盘数据刷下去。
//
// 用一次 syncfs 替代「对每个对象逐个 fsync」：实测在 2 vCPU 的虚拟机上，
// 2063 个对象的逐个 fsync 要数秒，而创建这些文件本身只要 63ms。
// 代价是它会连带刷新同一文件系统上的其它数据 —— 对「仓库独占一个文件系统」
// 的部署形态是合适的取舍。
func syncFS(root string) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Syncfs(fd)
}
