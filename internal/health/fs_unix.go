//go:build linux || darwin

package health

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func sameFilesystem(a, b string) (bool, string) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err.Error()
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err.Error()
	}
	da, db := uint64(sa.Dev), uint64(sb.Dev)
	if da == db {
		return true, fmt.Sprintf("dev=%d", da)
	}
	return false, fmt.Sprintf("不同设备：%d vs %d", da, db)
}

func diskAvailable(p string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
