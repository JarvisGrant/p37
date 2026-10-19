package mountmgr

import (
	"os"
	"path/filepath"
	"sync"
)

// baseRefs 记录每个工作区当前引用的**基底树 id**，供 Close / Relower 释放
// `ensureBase` 里累加的引用计数。
//
// 注意存的是 `hash.ID` 而不是目录路径：releaseBase 要按树 id 递减 `inUse`，
// 存字符串会让类型断言炸掉（这个 bug 真机上表现为 **land 时 daemon panic**）。
// 放在无构建标签的文件里，使两个平台实现都能使用。
var baseRefs sync.Map // wsID -> hash.ID

// fsCaseSensitive 判定工作区目录所在文件系统是否区分大小写。
func (m *Manager) fsCaseSensitive(dir string) bool {
	return CaseSensitiveProbe(dir)
}

// CaseSensitiveProbe 探测目标目录所在文件系统是否区分大小写（§3.3）。
//
// 通过创建一个临时文件、再检查其大小写变体是否存在来判断。macOS 默认不区分
// 大小写，Linux 默认区分——但不要假设，一律实测。
func CaseSensitiveProbe(dir string) bool {
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
