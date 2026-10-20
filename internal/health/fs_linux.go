//go:build linux

package health

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var errNotApplicable = errors.New("health: not applicable")

func overlaySupported() bool { return true }

func fsType(p string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return "", err
	}
	// 0xEF53 同时覆盖 ext2/3/4；P36 的 L2 是 ext4，这里按 ext4 报告。
	switch uint64(st.Type) {
	case 0xEF53:
		return "ext4", nil
	case 0x58465342:
		return "xfs", nil
	case 0x9123683E:
		return "btrfs", nil
	case 0x01021994:
		return "tmpfs", nil
	case 0x794C7630:
		return "overlayfs", nil
	case 0x2FC12FC1:
		return "zfs", nil
	default:
		return fmt.Sprintf("type-0x%x", uint64(st.Type)), nil
	}
}

func inotifyLimits() (watches, instances int, err error) {
	w, err := readIntFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, 0, err
	}
	i, err := readIntFile("/proc/sys/fs/inotify/max_user_instances")
	if err != nil {
		return 0, 0, err
	}
	return w, i, nil
}

func readIntFile(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return n, nil
}
