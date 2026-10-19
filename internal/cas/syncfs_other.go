//go:build !linux

package cas

import "errors"

// syncFS 非 Linux 平台不可用（回退到逐文件 fsync）。
func syncFS(root string) error { return errors.New("cas: syncfs 不可用") }
