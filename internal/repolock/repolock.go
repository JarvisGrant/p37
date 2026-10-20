// Package repolock 提供仓库级排他锁。
//
// daemon 在生命周期内持有它；CLI 的维护命令（当前是 gc）用同一把锁判断
// daemon 是否在跑。必须共用同一实现，避免 daemon 用 flock、CLI 用别的
// 检测方式而互相看不见。
package repolock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// ErrBusy 表示锁已被其它进程持有。
var ErrBusy = errors.New("repolock: 锁已被占用")

// Lock 是一个已持有的排他锁。
type Lock struct {
	f *os.File
}

// TryExclusive 非阻塞地获取 path 的排他锁。
//
// 返回 ErrBusy 时锁仍由其它进程持有；其它错误是打开/系统调用失败。
func TryExclusive(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release 释放锁并关闭文件。
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
