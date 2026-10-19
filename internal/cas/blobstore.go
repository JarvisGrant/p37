package cas

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ErrNotExist 表示 key 不存在。
var ErrNotExist = errors.New("cas: key 不存在")

// ErrExists 表示 key 已存在（PutIfAbsent 的语义）。
var ErrExists = errors.New("cas: key 已存在")

// ErrPreconditionFailed 表示 PutIfMatch 的 etag 不匹配或 key 不存在。
var ErrPreconditionFailed = errors.New("cas: 前置条件不满足")

// Stat 描述一个 key 的元信息。
type Stat struct {
	Key   string
	Size  int64
	ETag  string // 内容哈希（本地后端：对象 hash 的十六进制；文件：内容 BLAKE3）
	MTime int64  // Unix 毫秒
}

// BlobStore 是 S3 的能力子集（§8.1）。
//
// **禁止**：rename、原地改写、目录原子操作、文件锁。本地后端只是把 key 映射
// 到目录树。守这条纪律，换真 S3 时逻辑一行不改。
type BlobStore interface {
	// Get 读取 key 的内容。
	Get(key string) (io.ReadCloser, Stat, error)
	// PutIfAbsent 写入 key；若已存在则返回 ErrExists（内容不覆盖）。
	PutIfAbsent(key string, r io.Reader, size int64) error
	// ListPrefix 按前缀列举。
	ListPrefix(prefix string) ([]Stat, error)
	// Delete 删除 key；不存在时返回 ErrNotExist。
	Delete(key string) error
	// PutIfMatch 仅当当前 etag 匹配时覆盖写入，返回新 etag。
	// 若 key 不存在且 etag 为空字符串，则创建。
	PutIfMatch(key, etag string, data []byte) (string, error)
	// Stat 返回元信息。
	Stat(key string) (Stat, error)
	// Flush 确保此前写入的全部 key 已持久化。
	//
	// 调用方**必须**在推进主干 head 之前调用它（§7.3）。对真正的远端后端
	// （S3 等）它可以是 no-op。
	Flush() error
}

// LocalFS 是把 BlobStore 实现为本地目录树的后端（§8.2）。
//
// 持久化策略：`PutIfAbsent` **不做 fsync**，只把文件与目录记入待同步集合；
// 调用方在「提交点之前」（推进 head 之前）调用 `Flush` 统一落盘。
//
// 这样做的正确性依据是 §7.3：对象在 head 推进之前不必持久 —— head 未推进时
// 崩溃只会留下孤儿对象（无害，GC 回收）。而 1000 个文件会产生约 2000 个对象，
// 每个对象 2 次 fsync，串行执行就是 import 慢的根因。
type LocalFS struct {
	root string
	tmp  string

	syncMu   sync.Mutex
	pendingF map[string]struct{}
	pendingD map[string]struct{}
	noSync   bool
}

// NewLocalFS 创建/打开本地后端。root 通常为 <仓库根>/.p37。
func NewLocalFS(root string) (*LocalFS, error) {
	if root == "" {
		return nil, errors.New("cas: 本地后端 root 不能为空")
	}
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	return &LocalFS{
		root:     root,
		tmp:      tmp,
		pendingF: make(map[string]struct{}),
		pendingD: make(map[string]struct{}),
	}, nil
}

// DisableSyncForTest 关闭 fsync，仅用于基准测试对照。
func (l *LocalFS) DisableSyncForTest() {
	l.syncMu.Lock()
	l.noSync = true
	l.syncMu.Unlock()
}

// Root 返回后端根目录。
func (l *LocalFS) Root() string { return l.root }

// Flush 把此前写入的全部对象与目录落盘。
//
// 调用方必须在推进 head 之前调用它（§7.3）。
func (l *LocalFS) Flush() error {
	l.syncMu.Lock()
	if l.noSync {
		l.pendingF = make(map[string]struct{})
		l.pendingD = make(map[string]struct{})
		l.syncMu.Unlock()
		return nil
	}
	files := make([]string, 0, len(l.pendingF))
	for p := range l.pendingF {
		files = append(files, p)
	}
	dirs := make([]string, 0, len(l.pendingD))
	for p := range l.pendingD {
		dirs = append(dirs, p)
	}
	l.pendingF = make(map[string]struct{})
	l.pendingD = make(map[string]struct{})
	l.syncMu.Unlock()

	// 首选 syncfs：一次系统调用刷完整个文件系统。
	// 逐个 fsync 在 2 vCPU 虚拟机上要数秒（实测 2063 个对象），而创建这些
	// 文件本身只要 63ms —— 所以瓶颈一直在 fsync 上。
	if len(files)+len(dirs) > 16 {
		if err := syncFS(l.root); err == nil {
			return nil
		}
	}

	// 回退：并行逐个 fsync。
	// fsync 是阻塞系统调用，并行做能显著摊薄等待。
	if err := parallelSync(files); err != nil {
		return err
	}
	return parallelSyncDirs(dirs)
}

// note 记下待同步的文件与目录。
func (l *LocalFS) note(file string) {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	if l.noSync {
		return
	}
	l.pendingF[file] = struct{}{}
	l.pendingD[filepath.Dir(file)] = struct{}{}
}

// parallelSync 并行 fsync 一批文件。
func parallelSync(paths []string) error {
	return runParallel(paths, func(p string) error {
		f, err := os.Open(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		defer func() { _ = f.Close() }()
		return f.Sync()
	})
}

// parallelSyncDirs 并行 fsync 一批目录。
func parallelSyncDirs(paths []string) error {
	return runParallel(paths, func(p string) error {
		d, err := os.Open(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		defer func() { _ = d.Close() }()
		_ = d.Sync() // 部分文件系统（如 macOS）对目录 fsync 返回 EINVAL
		return nil
	})
}

// runParallel 用有界并发执行 fn；返回首个错误。
func runParallel(paths []string, fn func(string) error) error {
	if len(paths) == 0 {
		return nil
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	if workers > len(paths) {
		workers = len(paths)
	}
	if workers < 1 {
		workers = 1
	}

	ch := make(chan string)
	var wg sync.WaitGroup
	var (
		mu  sync.Mutex
		err error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ch {
				if e := fn(p); e != nil {
					mu.Lock()
					if err == nil {
						err = e
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, p := range paths {
		ch <- p
	}
	close(ch)
	wg.Wait()
	return err
}

// pathForKey 把 BlobStore key 映射为本地路径。
//
// 若 key 的最后一段是 64 位十六进制（对象 hash），按前 2/2 字节分两级目录，
// 避免单目录条目过多（§8.2）。
func (l *LocalFS) pathForKey(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	parts := strings.Split(key, "/")
	last := parts[len(parts)-1]
	base := filepath.Join(append([]string{l.root}, parts[:len(parts)-1]...)...)
	if isHex64(last) {
		base = filepath.Join(l.root, filepath.Join(parts[:len(parts)-1]...), last[:2], last[2:4])
	}
	return filepath.Join(base, last), nil
}

// isHex64 报告 s 是否为 64 字符小写十六进制。
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validateKey(key string) error {
	if key == "" {
		return errors.New("cas: 空 key")
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("cas: key %q 不允许前导/尾随 /", key)
	}
	for seg := range strings.SplitSeq(key, "/") {
		switch seg {
		case "", ".", "..":
			return fmt.Errorf("cas: key %q 含非法路径段", key)
		}
	}
	return nil
}

// Stat 实现 BlobStore。
func (l *LocalFS) Stat(key string) (Stat, error) {
	p, err := l.pathForKey(key)
	if err != nil {
		return Stat{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Stat{}, ErrNotExist
		}
		return Stat{}, err
	}
	etag := ""
	if isHex64(filepath.Base(p)) {
		etag = filepath.Base(p)
	} else {
		etag, err = fileETag(p)
		if err != nil {
			return Stat{}, err
		}
	}
	return Stat{Key: key, Size: fi.Size(), ETag: etag, MTime: fi.ModTime().UnixMilli()}, nil
}

// Get 实现 BlobStore。
func (l *LocalFS) Get(key string) (io.ReadCloser, Stat, error) {
	p, err := l.pathForKey(key)
	if err != nil {
		return nil, Stat{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Stat{}, ErrNotExist
		}
		return nil, Stat{}, err
	}
	st, err := l.Stat(key)
	if err != nil {
		_ = f.Close()
		return nil, Stat{}, err
	}
	return f, st, nil
}

// PutIfAbsent 实现 BlobStore。
//
// 直接以 O_CREAT|O_EXCL 写最终路径（而不是「写临时文件 → link」）：
// key 是内容寻址的，**替换同内容的文件是无害的**，所以不需要原子替换；
// 少掉 CreateTemp + remove 两次元数据操作，这在 10 万文件规模上是主要开销。
//
// 崩溃安全：写一半崩掉会在最终路径留下不完整文件，但
//  1. 此时 head 未推进（Flush 在提交点之前），不会被任何版本引用；
//  2. 下次写入同一对象时 EEXIST 分支会**校验大小**，不一致就删除重写。
//
// 因此不会出现「被引用的对象是残片」。
func (l *LocalFS) PutIfAbsent(key string, r io.Reader, size int64) error {
	final, err := l.pathForKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}

	ok, err := l.putExclusive(final, r, size)
	if err != nil {
		return err
	}
	if !ok {
		return ErrExists
	}
	// 不在此处 fsync：统一到 Flush（调用方在推进 head 之前调用）。
	l.note(final)
	return nil
}

// putExclusive 尝试独占创建 final 并写入；返回 false 表示已存在（去重命中）。
func (l *LocalFS) putExclusive(final string, r io.Reader, size int64) (bool, error) {
	f, err := os.OpenFile(final, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return false, err
		}
		// 已存在：校验大小。不一致说明上次写崩留下了残片 —— 删掉重写。
		fi, statErr := os.Stat(final)
		if statErr == nil && size > 0 && fi.Size() == size {
			return false, nil // 完整，去重命中
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return false, statErr
		}
		if err := os.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if f, err = os.OpenFile(final, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return false, nil // 并发写入抢先完成，视为命中
			}
			return false, err
		}
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(final)
		return false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(final)
		return false, err
	}
	return true, nil
}

// ListPrefix 实现 BlobStore。仅返回文件（不返回目录）。
//
// key 与本地路径之间隔着「对象目录分片」这一层，因此列举时先还原 key 再按
// 前缀过滤。前缀以 "/" 结尾时可直接下钻对应目录，否则从根遍历。
func (l *LocalFS) ListPrefix(prefix string) ([]Stat, error) {
	if prefix != "" && strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("cas: 前缀 %q 不允许前导 /", prefix)
	}

	walkRoot := l.root
	if prefix != "" && strings.HasSuffix(prefix, "/") {
		walkRoot = filepath.Join(l.root, filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
	}

	var out []Stat
	err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if filepath.Clean(p) == filepath.Clean(l.tmp) {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		key := unshardKey(filepath.ToSlash(rel))
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		etag := ""
		if isHex64(filepath.Base(p)) {
			etag = filepath.Base(p)
		}
		out = append(out, Stat{Key: key, Size: fi.Size(), ETag: etag, MTime: fi.ModTime().UnixMilli()})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// unshardKey 把 objects/ab/cd/<hash> 还原为 objects/<hash>。
func unshardKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) < 4 {
		return key
	}
	last := parts[len(parts)-1]
	if !isHex64(last) || len(parts[len(parts)-2]) != 2 || len(parts[len(parts)-3]) != 2 {
		return key
	}
	if parts[len(parts)-3] != last[:2] || parts[len(parts)-2] != last[2:4] {
		return key
	}
	return strings.Join(append(parts[:len(parts)-3], last), "/")
}

// Delete 实现 BlobStore。
func (l *LocalFS) Delete(key string) error {
	p, err := l.pathForKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotExist
		}
		return err
	}
	return nil
}

// PutIfMatch 实现 BlobStore。etag 为空表示「不存在才创建」。
func (l *LocalFS) PutIfMatch(key, etag string, data []byte) (string, error) {
	p, err := l.pathForKey(key)
	if err != nil {
		return "", err
	}
	cur, statErr := l.Stat(key)
	switch {
	case etag == "":
		if statErr == nil {
			return "", ErrPreconditionFailed
		}
	case statErr != nil:
		return "", ErrPreconditionFailed
	case cur.ETag != etag:
		return "", ErrPreconditionFailed
	}

	newETag := contentETag(data)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(l.tmp, "ref-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", err
	}
	// refs 是提交点本身，这里必须同步落盘。
	if !l.isNoSync() {
		if f, err := os.Open(p); err == nil {
			if err := f.Sync(); err != nil {
				_ = f.Close()
				return "", err
			}
			_ = f.Close()
		}
		if err := syncDir(filepath.Dir(p)); err != nil {
			return "", err
		}
	}
	return newETag, nil
}

// isNoSync 报告是否处于测试用的免同步模式。
func (l *LocalFS) isNoSync() bool {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	return l.noSync
}

// fileETag 计算小文件的内容哈希。
func fileETag(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return contentETag(b), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	// 在 macOS 上目录 fsync 可能返回 EINVAL，忽略之。
	_ = d.Sync()
	return nil
}

// Compile-time 断言。
var _ BlobStore = (*LocalFS)(nil)
