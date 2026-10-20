// Package mountmgr 负责把工作区目录物化出来（§4.6、§4.9.2）。
//
// 两种模式：
//
//	overlay  Linux + OverlayFS：每棵「基底树」只物化一份只读基底，各工作区
//	         用它作为 lowerdir、自己只持有 upper 可写层。ws_open 与文件数无关。
//	copy     整树复制。跨平台可用（macOS 开发机走这条），但 64 并发下磁盘
//	         需求 = 并发数 × 仓库体积，因此只适用于小规模或调试。
//
// 纪律：lower 永远是**单层目录**（D23），即「某个冻结树的实体化目录」，其 id
// 就是 ws.base_tree。不使用 parent_upper:parent_lower 这类实时多层 lower。
package mountmgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/safefs"
	"github.com/chenyiwen/p37/internal/tree"
)

// Mode 是物化模式。
type Mode string

// 物化模式取值。
const (
	ModeOverlay Mode = "overlay"
	ModeCopy    Mode = "copy"
	ModeAuto    Mode = "auto"
)

// ErrUnavailable 表示 overlay 模式在当前环境不可用。
var ErrUnavailable = errors.New("mountmgr: OverlayFS 不可用")

// ErrCaseConflict 表示树内存在仅大小写不同的路径，而目标文件系统不区分大小写。
var ErrCaseConflict = errors.New("mountmgr: 路径存在大小写冲突")

// ErrDirExists 表示目标目录已存在且非空。
var ErrDirExists = errors.New("mountmgr: 目标目录已存在且非空")

// ContentSource 提供物化所需的对象读取。
type ContentSource interface {
	GetTree(id hash.ID) (*tree.Tree, error)
	ReadFileData(id hash.ID) ([]byte, error)
	// ReadBlob 读取符号链接的目标字节（符号链接条目存的是 Blob，不是 FileContent）。
	ReadBlob(id hash.ID) ([]byte, error)
}

// Options 控制 Manager。
type Options struct {
	// BaseCacheBytes 是 overlay 基底缓存的字节上限（LRU）。
	BaseCacheBytes int64
	// InodeReliable 由调用方在挂载成功后依据 spike 结论设置（§4.7）。
	InodeReliable bool
}

// Manager 管理与物化工作区目录。
type Manager struct {
	repoRoot string
	opt      Options

	// baseMu 串行化基底的首次物化。两个 ws_open 同时命中同一基底时，
	// 如果都去写 <base>.tmp 再 rename，会一个成功、另一个 ENOENT。
	baseMu  sync.Mutex
	mu      sync.Mutex
	bases   map[hash.ID]string // 已物化的基底树 -> 目录
	inUse   map[hash.ID]int    // 引用计数
	lastUse map[hash.ID]int64
	seq     int64
}

// New 创建 Manager。repoRoot 是仓库容器目录。
func New(repoRoot string, opt Options) (*Manager, error) {
	if opt.BaseCacheBytes <= 0 {
		opt.BaseCacheBytes = 4 << 30
	}
	m := &Manager{
		repoRoot: repoRoot,
		opt:      opt,
		bases:    map[hash.ID]string{},
		inUse:    map[hash.ID]int{},
		lastUse:  map[hash.ID]int64{},
	}
	// 权限收紧（A3）：.p37 与 workspaces 都只允许属主访问，否则同机其它
	// 用户可以读到整份源码内容与全部元数据。
	for _, d := range []string{
		filepath.Join(repoRoot, ".p37"),
		filepath.Join(repoRoot, ".p37", "overlay"),
		filepath.Join(repoRoot, ".p37", "overlay", "bases"),
		filepath.Join(repoRoot, ".p37", "overlay", "ws"),
		filepath.Join(repoRoot, ".p37", "ws"),
		filepath.Join(repoRoot, ".p37", "tmp"),
		filepath.Join(repoRoot, "workspaces"),
	} {
		if err := safefs.HardenDir(d); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// WorkspaceDir 返回工作区的挂载点/目录路径。
//
// 安全兜底：wsID 一律做格式校验。正常路径上 wsID 都来自数据库（由 crypto/rand
// 生成），但这里是唯一把 wsID 拼进路径的地方，因此在此处把关，避免任何新增
// 调用点漏检导致路径穿越。
func (m *Manager) WorkspaceDir(wsID string) string {
	if !ValidWSID(wsID) {
		// 返回一个必定不存在的哨兵路径，而不是可逃逸的拼接结果。
		return filepath.Join(m.repoRoot, ".p37", "invalid-ws-id")
	}
	return filepath.Join(m.repoRoot, "workspaces", wsID)
}

// ValidWSID 报告工作区 id 是否符合生成格式 `ws_<16 位小写十六进制>`。
func ValidWSID(s string) bool {
	if len(s) != 3+16 || s[:3] != "ws_" {
		return false
	}
	for i := 3; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// StatePath 返回工作区标记文件路径（**位于被跟踪树之外**，§2、§4.6）。
func (m *Manager) StatePath(wsID string) string {
	return filepath.Join(m.repoRoot, ".p37", "ws", wsID, "state")
}

func (m *Manager) upperDir(wsID string) string {
	return filepath.Join(m.repoRoot, ".p37", "overlay", "ws", wsID, "upper")
}

func (m *Manager) workDir(wsID string) string {
	return filepath.Join(m.repoRoot, ".p37", "overlay", "ws", wsID, "work")
}

func (m *Manager) wsMetaDir(wsID string) string {
	return filepath.Join(m.repoRoot, ".p37", "overlay", "ws", wsID)
}

func (m *Manager) baseDir(id hash.ID) string {
	return filepath.Join(m.repoRoot, ".p37", "overlay", "bases", id.String())
}

// Open 物化一个工作区。
//
// 返回实际生效的模式。mode 为 ModeAuto 时优先尝试 overlay，失败回退 copy。
func (m *Manager) Open(wsID string, baseTree hash.ID, src ContentSource,
	mode Mode) (Mode, error) {
	dir := m.WorkspaceDir(wsID)
	if isEmptyOrAbsent(dir) == false {
		return "", ErrDirExists
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	// 大小写冲突检查只在目标文件系统不区分大小写时才必要（§3.3）。
	if !m.fsCaseSensitive(dir) {
		if err := m.checkCaseConflict(src, baseTree); err != nil {
			_ = os.Remove(dir)
			return "", err
		}
	}

	switch mode {
	case ModeCopy:
		if err := m.materializeInto(dir, baseTree, src); err != nil {
			return "", err
		}
	case ModeOverlay:
		if err := m.mountOverlay(wsID, baseTree, src); err != nil {
			return "", err
		}
	case ModeAuto, "":
		if err := m.mountOverlay(wsID, baseTree, src); err != nil {
			// 回退 copy：目录已建，先清空。
			_ = os.RemoveAll(dir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			if err2 := m.materializeInto(dir, baseTree, src); err2 != nil {
				return "", errors.Join(err, err2)
			}
			if err := m.writeState(wsID); err != nil {
				return "", err
			}
			return ModeCopy, nil
		}
	default:
		return "", fmt.Errorf("mountmgr: 未知物化模式 %q", mode)
	}

	if err := m.writeState(wsID); err != nil {
		return "", err
	}
	if mode == ModeCopy {
		return ModeCopy, nil
	}
	return ModeOverlay, nil
}

// Close 卸载并清理一个工作区。
func (m *Manager) Close(wsID string, keepDir bool, overlay bool) error {
	dir := m.WorkspaceDir(wsID)
	var errs []error
	if overlay {
		if err := m.unmountOverlay(wsID); err != nil {
			errs = append(errs, err)
		}
	}
	if !keepDir {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(m.wsMetaDir(wsID)); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(m.StatePath(wsID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	m.releaseBase(wsID)
	return errors.Join(errs...)
}

func (m *Manager) writeState(wsID string) error {
	p := m.StatePath(wsID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return os.WriteFile(p, []byte(wsID+"\n"), 0o600)
}

// StateExists 报告工作区标记是否存在。
func (m *Manager) StateExists(wsID string) bool {
	_, err := os.Stat(m.StatePath(wsID))
	return err == nil
}

// materializeInto 把树整树复制到 dst（copy 模式）。
func (m *Manager) materializeInto(dst string, treeID hash.ID, src ContentSource) error {
	t, err := src.GetTree(treeID)
	if err != nil {
		return err
	}
	return m.writeTree(dst, t, src)
}

// writeTree 把树物化到 dir。
//
// 安全性：逐条目使用 safefs 原语，绝不穿过已存在的符号链接（A2）。
func (m *Manager) writeTree(dir string, t *tree.Tree, src ContentSource) error {
	if err := safefs.EnsureDirUnder(dir, "."); err != nil {
		// dir 自身可能还不存在；"./ " 会被 safefs 拒绝，直接退化为 MkdirAll 语义。
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return m.writeTreeRel(dir, "", t, src)
}

func (m *Manager) writeTreeRel(root, prefix string, t *tree.Tree, src ContentSource) error {
	for _, e := range t.Entries {
		rel := e.Name
		if prefix != "" {
			rel = prefix + "/" + e.Name
		}
		switch e.Kind {
		case codec.EntryTree:
			sub, err := src.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := safefs.EnsureDirUnder(root, rel); err != nil {
				return err
			}
			if err := m.writeTreeRel(root, rel, sub, src); err != nil {
				return err
			}
		case codec.EntrySymlink:
			data, err := src.ReadBlob(e.ID)
			if err != nil {
				return err
			}
			if err := safefs.SymlinkUnder(root, rel, string(data)); err != nil {
				return err
			}
		default:
			data, err := src.ReadFileData(e.ID)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if e.Mode == codec.ModeExecutable {
				mode = 0o755
			}
			if err := safefs.WriteFileUnder(root, rel, data, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCaseConflict 遍历树，检测仅大小写不同的同级路径（§3.3）。
func (m *Manager) checkCaseConflict(src ContentSource, root hash.ID) error {
	t, err := src.GetTree(root)
	if err != nil {
		return err
	}
	return m.caseWalk(src, t, "")
}

func (m *Manager) caseWalk(src ContentSource, t *tree.Tree, prefix string) error {
	seen := make(map[string]string, len(t.Entries))
	for _, e := range t.Entries {
		lower := strings.ToLower(e.Name)
		if prev, ok := seen[lower]; ok && prev != e.Name {
			return fmt.Errorf("%w: %q 与 %q（位于 %q）", ErrCaseConflict, prev, e.Name, prefix)
		}
		seen[lower] = e.Name

		if e.Kind == codec.EntryTree {
			sub, err := src.GetTree(e.ID)
			if err != nil {
				return err
			}
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if err := m.caseWalk(src, sub, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureBase 物化（或复用）一棵基底树，返回其目录并增加引用计数。
func (m *Manager) ensureBase(treeID hash.ID, src ContentSource) (string, error) {
	m.baseMu.Lock()
	defer m.baseMu.Unlock()

	m.mu.Lock()
	if dir, ok := m.bases[treeID]; ok {
		m.inUse[treeID]++
		m.seq++
		m.lastUse[treeID] = m.seq
		m.mu.Unlock()
		return dir, nil
	}
	m.mu.Unlock()

	dir := m.baseDir(treeID)
	if _, err := os.Stat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		tmp := dir + ".tmp"
		_ = os.RemoveAll(tmp)
		if err := m.materializeInto(tmp, treeID, src); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
		if err := os.Rename(tmp, dir); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}

	m.mu.Lock()
	m.bases[treeID] = dir
	m.inUse[treeID]++
	m.seq++
	m.lastUse[treeID] = m.seq
	m.mu.Unlock()
	return dir, nil
}

// releaseBase 释放一个工作区对基底的引用（Close / Relower 时调用）。
//
// 注意用的是 case.go 里的 `baseRefs` —— 写入它的地方是 mountWithLower /
// Relower。早先这里读的是另一个**从未被写入**的 map，于是 inUse 只增不减，
// `Evict` 会认为所有基底都在被引用，基底缓存永远腾不出空间。
func (m *Manager) releaseBase(wsID string) {
	v, ok := baseRefs.LoadAndDelete(wsID)
	if !ok {
		return
	}
	id, ok := v.(hash.ID)
	if !ok {
		// 防御：类型不符时宁可漏记一次引用计数，也绝不 panic ——
		// 这里的 panic 会顺着 land 一路把整个 daemon 带走。
		return
	}
	m.mu.Lock()
	if m.inUse[id] > 0 {
		m.inUse[id]--
	}
	m.mu.Unlock()
}

// Evict 在超过缓存上限时淘汰最久未用的基底。被引用的基底不淘汰。
func (m *Manager) Evict() (int64, error) {
	m.mu.Lock()
	type cand struct {
		id  hash.ID
		use int64
		dir string
	}
	var cands []cand
	for id, dir := range m.bases {
		if m.inUse[id] > 0 {
			continue
		}
		cands = append(cands, cand{id: id, use: m.lastUse[id], dir: dir})
	}
	m.mu.Unlock()

	if len(cands) == 0 {
		return 0, nil
	}
	// 简单策略：先不管上限，由调用方按总占用决定是否继续调用。
	var freed int64
	for _, c := range cands {
		size := dirSize(c.dir)
		if err := os.RemoveAll(c.dir); err != nil {
			return freed, err
		}
		m.mu.Lock()
		delete(m.bases, c.id)
		delete(m.lastUse, c.id)
		m.mu.Unlock()
		freed += size
	}
	return freed, nil
}

// BaseCacheSize 返回已物化基底的总字节数。
func (m *Manager) BaseCacheSize() int64 {
	m.mu.Lock()
	dirs := make([]string, 0, len(m.bases))
	for _, d := range m.bases {
		dirs = append(dirs, d)
	}
	m.mu.Unlock()
	var total int64
	for _, d := range dirs {
		total += dirSize(d)
	}
	return total
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

func isEmptyOrAbsent(dir string) bool {
	des, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	return len(des) == 0
}

// Unmount 卸载一个 overlay 工作区（不动 upper 层，§4.6）。
func (m *Manager) Unmount(wsID string) error { return m.unmountOverlay(wsID) }
