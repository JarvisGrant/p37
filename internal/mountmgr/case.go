package mountmgr

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// baseRefs / wsBaseRef 记录工作区引用的基底，供 Close 时释放引用。
//
// 放在无构建标签的文件里，使两个平台实现都能使用。
var baseRefs sync.Map // wsID -> hash.ID

// fsCaseSensitive 判定工作区目录所在文件系统是否区分大小写。
func (m *Manager) fsCaseSensitive(dir string) bool {
	return CaseSensitiveProbe(dir)
}

// CaseSensitiveProbe 是平台无关的大小写敏感性探测（Linux 与 macOS 同实现）。
func CaseSensitiveProbe(dir string) bool {
	if runtime.GOOS == "linux" {
		return probeCase(dir)
	}
	// macOS 默认不区分大小写（HFS+/APFS 常见配置），必须探测而非假设。
	return probeCase(dir)
}

// probeCase 通过创建一个临时文件并检查其大小写变体是否存在来探测。
func probeCase(dir string) bool {
	f, err := os.CreateTemp(dir, "P37case-*")
	if err != nil {
		return false // 保守：假设不区分，从而多执行一次冲突检查
	}
	name := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(name) }()

	upper := filepath.Join(filepath.Dir(name), "P37CASE-"+filepath.Base(name)[len("P37case-"):])
	if upper == name {
		return true
	}
	if _, err := os.Stat(upper); err == nil {
		return false
	}
	return true
}
