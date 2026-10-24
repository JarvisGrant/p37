//go:build linux

package mountmgr

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// IsMountPoint 报告 path 是否为挂载点。
//
// 两道判断结合使用：
//   - /proc/self/mountinfo 能识别 bind mount、tmpfs、同设备子卷等 st_dev 判断
//     会漏掉的场景；
//   - st_dev 与父目录比较作为兜底，覆盖 mountinfo 不可用或路径含有特殊字符
//     而无法精确匹配的场景。
func IsMountPoint(path string) bool {
	want, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	fi, err := os.Stat(want)
	if err != nil || !fi.IsDir() {
		return false
	}
	if mountinfoHas(want) {
		return true
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

func mountinfoHas(want string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// 格式: id parent major:minor root mountpoint options...
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountField(fields[4])
		if mp == want {
			return true
		}
	}
	return false
}

// unescapeMountField 还原 mountinfo 里的八进制转义（空格等）。
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func devOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}
