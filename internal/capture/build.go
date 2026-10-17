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
}

// DefaultOptions 返回 §3.5、附录 B 的默认取值。
func DefaultOptions() Options {
	return Options{
		ProbeBytes:   tree.DefaultBinaryProbeBytes,
		MaxTextSize:  4 << 20,
		MaxFileSize:  2 << 30,
		ContextLines: tree.DefaultContextLines,
	}
}

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
	return t, nil
}

type builder struct {
	wsID    string
	rootDir string
	ig      *tree.Ignore
	ow      ObjectWriter
	stat    StatCache
	inodeOK InodeReliable
	opt     Options
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
	sig := entriesSig(sub)

	if prev, ok := b.stat.Get(b.wsID, repoPath); ok &&
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

	// 快路径：stat 未变则复用 §4.7 的判定。
	if prev, ok := b.stat.Get(b.wsID, repoPath); ok && prev.Kind == codec.EntryFile &&
		b.sameStat(prev, info) && !prev.ObjectID.IsZero() {
		return prev.ObjectID, true, nil
	}

	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return hash.ID{}, false, nil
		}
		return hash.ID{}, false, err
	}
	cid := hash.Sum(data)

	if prev, ok := b.stat.Get(b.wsID, repoPath); ok && prev.Kind == codec.EntryFile &&
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
func (b *builder) sameStat(prev StatEntry, info os.FileInfo) bool {
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
	b.stat.Put(b.wsID, StatEntry{
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

// entriesSig 是目录条目的规范化签名，用于子树复用。
func entriesSig(entries []tree.Entry) hash.ID {
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
