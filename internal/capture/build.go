// Package capture 实现采集：从工作区目录构建树（§5.5），以及由两棵树推导
// op 序列（§5.3、§6.5）。
//
// 采集的 op 是**派生**的（由磁盘 diff 得出），不是一手采集（D8）。因此本包
// 的核心质量目标是让 op 的粒度尽量细、让 place 命中率尽量高。
package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/tree"
)

// Options 控制采集与 op 推导。
type Options struct {
	// ProbeBytes 是二进制探测长度（§3.5）。
	ProbeBytes int
	// MaxTextSize 是行级 diff 的字节上限；超过走 blob_put（§3.5）。
	MaxTextSize int64
	// MaxFileSize 是单文件上限；超过报 FILE_TOO_LARGE（§3.5、§13）。
	MaxFileSize int64
	// ContextLines 是 hunk 的上下文行数（§5.3）。
	ContextLines int
	// MaxChain 是 histogram diff 的链长上限（附录 I.3，merge.max_chain_length）。
	// 0 表示 tree.DefaultMaxChainLength。
	MaxChain int
	// MoveSimilarity 是「相似度移动」的判定阈值（§5.3 第 2 趟，
	// merge.move_similarity）。0 表示用 DefaultMoveSimilarity。
	MoveSimilarity float64
	// StableDelay / StableRetries 是采集的**稳定点判定**（§5.2）。
	// StableRetries <= 0 表示关闭（import 等离线场景用）。
	StableDelay   time.Duration
	StableRetries int
	// OnUnstable 在某个文件连续多轮仍不稳定、本轮被跳过时回调（可观测性用）。
	OnUnstable func(path string)

	// BaseEntries 是基准树的扁平视图（仓库相对路径 -> 对象 id，不含目录）。
	//
	// 与 MaterializedNS 一起用于**惰性判定**：工作区刚物化时 stat 缓存是空的，
	// 但「文件不比物化时刻更新」就说明它仍是基准树里那一份，可以直接复用其
	// 对象 id —— 不读、不哈希。这让 ws_open 完全不必走文件系统。
	BaseEntries map[string]hash.ID
	// MaterializedNS 是工作区的物化时刻（Unix 纳秒）。0 表示未启用惰性判定。
	MaterializedNS int64
}

// DefaultOptions 返回 §3.5、附录 B 的默认取值。
func DefaultOptions() Options {
	return Options{
		ProbeBytes:     tree.DefaultBinaryProbeBytes,
		MaxTextSize:    4 << 20,
		MaxFileSize:    2 << 30,
		ContextLines:   tree.DefaultContextLines,
		MaxChain:       tree.DefaultMaxChainLength,
		MoveSimilarity: DefaultMoveSimilarity,
	}
}

// DefaultMoveSimilarity 是「相似度移动」的默认阈值（附录 B 的 merge.move_similarity）。
const DefaultMoveSimilarity = 0.6

// ErrFileTooLarge 表示单文件超过上限。
var ErrFileTooLarge = errors.New("capture: 单文件超过上限")

// StatEntry 是 stat 缓存的一行（§4.7、§5.5）。
type StatEntry struct {
	Path        string
	Kind        uint8 // codec.EntryFile / EntryTree / EntrySymlink
	MtimeNS     int64
	Size        int64
	Inode       uint64
	Mode        uint64
	ContentHash hash.ID // 仅文件
	ObjectID    hash.ID // 文件=FileContent id；目录=Tree id
	EntriesSig  hash.ID // 仅目录
}

// StatCache 提供 stat 缓存的读写。
type StatCache interface {
	Get(wsID, path string) (StatEntry, bool)
	Put(wsID string, e StatEntry)
	// PutMany 批量写入。实现必须用**单个事务**完成，否则每次采集会产生
	// O(文件数) 个事务 —— 这是 ws_open / ws_sync 慢的主因。
	PutMany(wsID string, es []StatEntry) error
	// Load 一次性载入某工作区的全部缓存条目。
	//
	// 逐条 Get 会退化成 O(文件数) 次查询；批量载入把它压成 1 次。
	Load(wsID string) (map[string]StatEntry, error)
	// DeleteUnder 删除 prefix 及其下的全部条目。
	DeleteUnder(wsID, prefix string)
}

// ObjectWriter 是采集所需的写入能力，由 cas.Store 提供。
type ObjectWriter interface {
	op.ContentStore
	PutTree(t *tree.Tree) (hash.ID, error)
	PutBlob(data []byte) (hash.ID, error)
	GetTree(id hash.ID) (*tree.Tree, error)
	// ReadBlob 读取符号链接的目标字节（其条目 id 指向 Blob）。
	ReadBlob(id hash.ID) ([]byte, error)
}

// InodeReliable 报告当前物化模式下的 inode 是否可用于快路径判定。
//
// overlay 模式下 OverlayFS 的 inode 是合成的、跨重启可能变化，因此判定必须
// 降级为 (mtime_ns, size)（§4.7）。
type InodeReliable bool

// Build 从目录递归构建树，并利用 stat 缓存做子树复用（§5.5）。
func Build(wsID, root string, ig *tree.Ignore, ow ObjectWriter,
	stat StatCache, inodeOK InodeReliable, opt Options) (*tree.Tree, error) {
	if opt.ProbeBytes <= 0 {
		opt = DefaultOptions()
	}
	b := &builder{wsID: wsID, rootDir: root, ig: ig, ow: ow, stat: stat, inodeOK: inodeOK, opt: opt}
	// §5.2 稳定点判定：必须在正式遍历之前，且必须在任何对象写入之前。
	if err := b.stabilize(); err != nil {
		return nil, err
	}
	entries, err := b.dir(root)
	if err != nil {
		return nil, err
	}
	t := &tree.Tree{Entries: entries}
	t.Sort()
	// 根树也必须写入 CAS —— 子目录由 subtree 写入，根目录由这里写入。
	if _, err := b.ow.PutTree(t); err != nil {
		return nil, err
	}
	// stat 缓存批量落库（单事务）。缓存写失败不影响正确性（下一轮会重算）。
	if b.stat != nil {
		if err := b.flushStat(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// statBatch 是批量落库的分片大小。
const statBatch = 2000

// flushStat 把累积的 stat 条目分批写入（每批一个事务）。
func (b *builder) flushStat() error {
	if len(b.pendingStat) == 0 {
		return nil
	}
	for len(b.pendingStat) > 0 {
		n := min(statBatch, len(b.pendingStat))
		if err := b.stat.PutMany(b.wsID, b.pendingStat[:n]); err != nil {
			return err
		}
		b.pendingStat = b.pendingStat[n:]
	}
	return nil
}

type builder struct {
	wsID    string
	rootDir string
	ig      *tree.Ignore
	ow      ObjectWriter
	stat    StatCache
	inodeOK InodeReliable
	opt     Options

	// pendingStat 累积待写入的 stat 条目，最后批量落库。
	pendingStat []StatEntry

	// cache 是本次构建用的 stat 缓存快照（惰性载入一次）。
	cache     map[string]StatEntry
	cacheOnce bool

	// stable 是稳定点判定**确认过**的文件内容（§5.2）：主遍历直接取用，
	// 不再重新读取，避免又读到中途状态。
	stable map[string][]byte
	// unstable 是连续多轮仍不稳定的文件，本轮跳过。
	unstable map[string]bool
}

// BuildStats 汇总一次采集的判定统计（可观测性用）。
type BuildStats struct {
	// SkippedUnstable 是连续多轮不稳定、本轮被跳过的路径。
	SkippedUnstable []string
}

// candidate 是一个「可能正在被写」的待确认文件。
type candidate struct {
	path  string // 仓库相对路径
	abs   string
	mtime int64
	size  int64
	hash  hash.ID
}

// stabilize 是 §5.2 的**稳定点判定**。
//
// 背景：agent 直接改工作区目录，采集可能在文件**正被写入**时撞上去，
// 于是把一个被截断的内容当成新版本落进 op —— 这一步就是防它。
//
// 判定规则（§5.2）：
//
//	对疑似变更的文件，读到 (hash, mtime, size)；
//	隔 stable_delay 再读一次；三元组一致才算稳定。
//	连续 stable_retries 轮仍不一致 -> 跳过该文件本轮，下一轮再试。
//
// 两个工程上的关键点：
//
//  1. **只检查「刚被写过」的文件**（mtime 距今 < stable_delay）。mtime 已经
//     很久没动的文件不可能正在被写，无需付等待成本。
//  2. **等待是批量的**：先把所有候选读一遍，再睡一次，再统一复核。
//     否则「一次改了 1000 个文件」会变成 1000 × 200ms = 200 秒。
func (b *builder) stabilize() error {
	if b.opt.StableRetries <= 0 || b.opt.StableDelay <= 0 {
		return nil
	}
	pending, err := b.collectCandidates()
	if err != nil || len(pending) == 0 {
		return err
	}

	// 第 0 轮：把候选读一遍作为基线。
	live := pending[:0]
	for i := range pending {
		c := &pending[i]
		data, err := os.ReadFile(c.abs)
		if err != nil {
			continue // 采集期间消失/不可读：交给主遍历处理
		}
		c.hash = hash.Sum(data)
		live = append(live, *c)
	}
	pending = live

	for round := 0; round < b.opt.StableRetries && len(pending) > 0; round++ {
		time.Sleep(b.opt.StableDelay)
		still := pending[:0]
		for i := range pending {
			c := &pending[i]
			info, err := os.Lstat(c.abs)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(c.abs)
			if err != nil {
				continue
			}
			h := hash.Sum(data)
			if info.ModTime().UnixNano() == c.mtime && info.Size() == c.size && h == c.hash {
				if b.stable == nil {
					b.stable = make(map[string][]byte, len(pending))
				}
				b.stable[c.path] = data
				continue
			}
			c.mtime, c.size, c.hash = info.ModTime().UnixNano(), info.Size(), h
			still = append(still, *c)
		}
		pending = still
	}

	for i := range pending {
		if b.unstable == nil {
			b.unstable = make(map[string]bool, len(pending))
		}
		b.unstable[pending[i].path] = true
		if b.opt.OnUnstable != nil {
			b.opt.OnUnstable(pending[i].path)
		}
	}
	return nil
}

// collectCandidates 找出「需要真正读取、且刚被写过」的文件。
func (b *builder) collectCandidates() ([]candidate, error) {
	var out []candidate
	var walk func(dir string) error
	walk = func(dir string) error {
		des, err := os.ReadDir(dir)
		if err != nil {
			return nil // 单个子目录读不了不影响其它
		}
		for _, de := range des {
			child := filepath.Join(dir, de.Name())
			rp := b.rel(child)
			info, err := os.Lstat(child)
			if err != nil {
				continue
			}
			if b.ig != nil && b.ig.Match(rp, info.IsDir()) {
				continue
			}
			switch {
			case info.IsDir():
				if err := walk(child); err != nil {
					return err
				}
			case !info.Mode().IsRegular():
				// 符号链接等不读内容，无关稳定点。
			default:
				if info.Size() > b.opt.MaxFileSize {
					continue
				}
				if time.Since(info.ModTime()) >= b.opt.StableDelay {
					continue // 很久没动 -> 不可能正在被写
				}
				if b.isFastPath(rp, info) {
					continue // 快路径命中 -> 根本不会读
				}
				out = append(out, candidate{
					path: rp, abs: child,
					mtime: info.ModTime().UnixNano(), size: info.Size(),
				})
			}
		}
		return nil
	}
	err := walk(b.rootDir)
	return out, err
}

// isFastPath 报告该文件是否会命中「不读取」的快路径。
func (b *builder) isFastPath(repoPath string, info os.FileInfo) bool {
	if prev, ok := b.cached(repoPath); ok && prev.Kind == codec.EntryFile &&
		b.sameStat(prev, info) && !prev.ObjectID.IsZero() {
		return true
	}
	if b.opt.MaterializedNS > 0 {
		if id, found := b.opt.BaseEntries[repoPath]; found && !id.IsZero() &&
			info.ModTime().UnixNano() <= b.opt.MaterializedNS {
			return true
		}
	}
	return false
}

// loadCache 惰性载入 stat 缓存快照（一次查询）。
func (b *builder) loadCache() {
	if b.cacheOnce {
		return
	}
	b.cacheOnce = true
	if b.stat == nil {
		return
	}
	m, err := b.stat.Load(b.wsID)
	if err != nil {
		return // 缓存读失败不影响正确性：退化为全部重新哈希
	}
	b.cache = m
}

// cached 读取 stat 缓存；stat 为 nil 时视为未命中。
func (b *builder) cached(path string) (StatEntry, bool) {
	if b.stat == nil {
		return StatEntry{}, false
	}
	b.loadCache()
	e, ok := b.cache[path]
	return e, ok
}

func (b *builder) rel(p string) string {
	r, err := filepath.Rel(b.rootDir, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(r)
}

// dir 递归构建一个目录的条目。
func (b *builder) dir(dirPath string) ([]tree.Entry, error) {
	des, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	entries := make([]tree.Entry, 0, len(des))
	for _, de := range des {
		name := codec.NFC(de.Name())
		child := filepath.Join(dirPath, de.Name())
		repoPath := b.rel(child)

		info, err := os.Lstat(child)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // 采集期间被删除
			}
			return nil, err
		}
		isDir := info.IsDir()

		if b.ig != nil && b.ig.Match(repoPath, isDir) {
			continue
		}

		switch {
		case isDir:
			id, n, err := b.subtree(child, repoPath, info)
			if err != nil {
				return nil, err
			}
			if n == 0 {
				continue // 空目录不跟踪（§17 U2）
			}
			entries = append(entries, tree.Entry{
				Name: name, Mode: codec.ModeNormal, Kind: codec.EntryTree, ID: id,
			})

		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(child)
			if err != nil {
				return nil, err
			}
			// 符号链接的条目 id 是链接目标的 Blob id（附录 A.3）。
			id, err := b.ow.PutBlob([]byte(target))
			if err != nil {
				return nil, err
			}
			b.putStat(repoPath, codec.EntrySymlink, info, id, hash.ID{})
			entries = append(entries, tree.Entry{
				Name: name, Mode: codec.ModeNormal, Kind: codec.EntrySymlink, ID: id,
			})

		case info.Mode().IsRegular():
			mode := codec.ModeNormal
			if info.Mode()&0o111 != 0 {
				mode = codec.ModeExecutable
			}
			id, ok, err := b.file(child, repoPath, info)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // 采集期间消失
			}
			entries = append(entries, tree.Entry{
				Name: name, Mode: mode, Kind: codec.EntryFile, ID: id,
			})

		default:
			continue // 套接字、设备等一律忽略
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// subtree 处理一个子目录：条目签名未变则直接复用其 Tree id（§5.5）。
//
// 返回的计数 n 是该目录下的条目数；n == 0 表示空目录，调用方应跳过它
// （§17 U2：空目录不跟踪）。
func (b *builder) subtree(dirPath, repoPath string, info os.FileInfo) (hash.ID, int, error) {
	sub, err := b.dir(dirPath)
	if err != nil {
		return hash.ID{}, 0, err
	}
	if len(sub) == 0 {
		if b.stat != nil {
			b.stat.DeleteUnder(b.wsID, repoPath)
		}
		return hash.ID{}, 0, nil
	}
	sig := EntriesSig(sub)

	if prev, ok := b.cached(repoPath); ok &&
		prev.Kind == codec.EntryTree && prev.EntriesSig == sig && !prev.ObjectID.IsZero() {
		b.putStatWith(repoPath, codec.EntryTree, info, hash.ID{}, prev.ObjectID, sig)
		return prev.ObjectID, len(sub), nil
	}

	t := &tree.Tree{Entries: sub}
	id, err := b.ow.PutTree(t)
	if err != nil {
		return hash.ID{}, 0, err
	}
	b.putStatWith(repoPath, codec.EntryTree, info, hash.ID{}, id, sig)
	return id, len(sub), nil
}

// file 处理一个普通文件，返回 (FileContent id, 是否仍存在)。
func (b *builder) file(p, repoPath string, info os.FileInfo) (hash.ID, bool, error) {
	if info.Size() > b.opt.MaxFileSize {
		return hash.ID{}, false, fmt.Errorf("%w: %s (%d 字节)", ErrFileTooLarge, repoPath, info.Size())
	}

	// 快路径 1：已有缓存条目（含播种条目，见 sameStat）。
	if prev, ok := b.cached(repoPath); ok && prev.Kind == codec.EntryFile &&
		b.sameStat(prev, info) && !prev.ObjectID.IsZero() {
		return prev.ObjectID, true, nil
	}
	// 快路径 2：惰性判定 —— 基准树里有这个路径，且文件不比物化时刻更新，
	// 说明它仍是基准树里那一份，直接复用对象 id（不读、不哈希）。
	if b.opt.MaterializedNS > 0 {
		if id, found := b.opt.BaseEntries[repoPath]; found && !id.IsZero() &&
			info.ModTime().UnixNano() <= b.opt.MaterializedNS {
			b.putStatWith(repoPath, codec.EntryFile, info, hash.ID{}, id, hash.ID{})
			return id, true, nil
		}
	}

	// 稳定点判定已经确认过的内容：直接用，不重读（§5.2）。
	if data, ok := b.stable[repoPath]; ok {
		return b.commitFile(repoPath, info, data)
	}
	// 连续多轮都不稳定 -> 本轮跳过。
	//
	// 「跳过」的正确含义是**让这一轮看不到它**，而不是把它当成已删除：
	//   - 基准里有这个路径 -> 沿用基准条目（Diff 于是看不到变化）
	//   - 否则不产出条目（下轮再采）
	// 两种情况都**不更新 stat 缓存**，下一轮才会重新检查。
	if b.unstable[repoPath] {
		if id, found := b.opt.BaseEntries[repoPath]; found && !id.IsZero() {
			return id, true, nil
		}
		if prev, ok := b.cached(repoPath); ok && prev.Kind == codec.EntryFile &&
			!prev.ObjectID.IsZero() {
			return prev.ObjectID, true, nil
		}
		return hash.ID{}, false, nil
	}

	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return hash.ID{}, false, nil
		}
		return hash.ID{}, false, err
	}
	return b.commitFile(repoPath, info, data)
}

// commitFile 把一份已确认的内容落成 FileContent 并更新 stat 缓存。
func (b *builder) commitFile(repoPath string, info os.FileInfo, data []byte) (hash.ID, bool, error) {
	cid := hash.Sum(data)
	if prev, ok := b.cached(repoPath); ok && prev.Kind == codec.EntryFile &&
		prev.ContentHash == cid && !prev.ObjectID.IsZero() {
		// 内容未变（只是 mtime 变了），复用对象 id。
		b.putStatWith(repoPath, codec.EntryFile, info, cid, prev.ObjectID, hash.ID{})
		return prev.ObjectID, true, nil
	}
	fid, err := b.ow.PutFileData(data)
	if err != nil {
		return hash.ID{}, false, err
	}
	b.putStatWith(repoPath, codec.EntryFile, info, cid, fid, hash.ID{})
	return fid, true, nil
}

// sameStat 实现 §4.7 的快路径判定。
//
// 两种条目：
//
//	播种条目（Inode == 0）：ws_open 时由**版本树**直接播种，没有 stat 过。
//	  只要文件不比物化时刻更新，就认为未变。
//	  依据：物化会把 mtime 写成「物化时刻」；agent 之后任何修改都会让 mtime 变大。
//	  这样 ws_open 完全不必读文件系统、不必哈希，做到与文件数近似无关。
//
//	真实条目：精确匹配 mtime_ns + size（overlay 下不比 inode）。
func (b *builder) sameStat(prev StatEntry, info os.FileInfo) bool {
	if prev.Inode == 0 && prev.MtimeNS != 0 {
		return info.ModTime().UnixNano() <= prev.MtimeNS
	}
	if prev.MtimeNS != info.ModTime().UnixNano() || prev.Size != info.Size() {
		return false
	}
	if !b.inodeOK {
		return true // overlay：inode 不可靠，只用 (mtime_ns, size)
	}
	return prev.Inode == inodeOf(info)
}

func (b *builder) putStat(repoPath string, kind uint8, info os.FileInfo,
	objectID, sig hash.ID) {
	b.putStatWith(repoPath, kind, info, hash.ID{}, objectID, sig)
}

func (b *builder) putStatWith(repoPath string, kind uint8, info os.FileInfo,
	contentHash, objectID, sig hash.ID) {
	if b.stat == nil {
		return
	}
	mode := codec.ModeNormal
	if info.Mode()&0o111 != 0 {
		mode = codec.ModeExecutable
	}
	b.pendingStat = append(b.pendingStat, StatEntry{
		Path:        repoPath,
		Kind:        kind,
		MtimeNS:     info.ModTime().UnixNano(),
		Size:        info.Size(),
		Inode:       inodeOf(info),
		Mode:        mode,
		ContentHash: contentHash,
		ObjectID:    objectID,
		EntriesSig:  sig,
	})
}

// EntriesSig 是目录条目的规范化签名，用于子树复用与 stat 缓存播种。
func EntriesSig(entries []tree.Entry) hash.ID {
	e := codec.NewEncoder()
	e.Uvarint(uint64(len(entries)))
	for _, en := range entries {
		e.Str(en.Name)
		e.Uvarint(en.Mode)
		e.U8(en.Kind)
		e.Hash(en.ID)
	}
	return hash.Sum(e.Out())
}

// MemStat 是 StatCache 的内存实现（测试与轻量场景用）。
type MemStat struct{ M map[string]StatEntry }

// NewMemStat 返回空缓存。
func NewMemStat() *MemStat { return &MemStat{M: map[string]StatEntry{}} }

func memKey(wsID, path string) string { return wsID + "\x00" + path }

// Get 实现 StatCache。
func (m *MemStat) Get(wsID, path string) (StatEntry, bool) {
	e, ok := m.M[memKey(wsID, path)]
	return e, ok
}

// Put 实现 StatCache。
func (m *MemStat) Put(wsID string, e StatEntry) { m.M[memKey(wsID, e.Path)] = e }

// PutMany 实现 StatCache。
func (m *MemStat) PutMany(wsID string, es []StatEntry) error {
	for _, e := range es {
		m.M[memKey(wsID, e.Path)] = e
	}
	return nil
}

// Load 实现 StatCache。
func (m *MemStat) Load(wsID string) (map[string]StatEntry, error) {
	out := make(map[string]StatEntry)
	p := wsID + "\x00"
	for k, v := range m.M {
		if len(k) > len(p) && k[:len(p)] == p {
			out[k[len(p):]] = v
		}
	}
	return out, nil
}

// DeleteUnder 实现 StatCache。
func (m *MemStat) DeleteUnder(wsID, prefix string) {
	p := wsID + "\x00" + prefix
	for k := range m.M {
		if k == p || (len(k) > len(p) && k[:len(p)] == p && k[len(p)] == '/') {
			delete(m.M, k)
		}
	}
}

var _ StatCache = (*MemStat)(nil)
