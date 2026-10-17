package cas

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
}

// LocalFS 是把 BlobStore 实现为本地目录树的后端（§8.2）。
type LocalFS struct {
	root string
	tmp  string
}

// NewLocalFS 创建/打开本地后端。root 通常为 <仓库根>/.p37。
func NewLocalFS(root string) (*LocalFS, error) {
	if root == "" {
		return nil, errors.New("cas: 本地后端 root 不能为空")
	}
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	return &LocalFS{root: root, tmp: tmp}, nil
}

// Root 返回后端根目录。
func (l *LocalFS) Root() string { return l.root }

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

// PutIfAbsent 实现 BlobStore：写临时文件 → fsync → link 到最终路径（EEXIST 表示已存在）。
//
// 用 link 而非 rename，因为 rename 会静默覆盖，违背「不可变 key」的纪律。
func (l *LocalFS) PutIfAbsent(key string, r io.Reader, size int64) error {
	final, err := l.pathForKey(key)
	if err != nil {
		return err
	}
	if _, err := os.Stat(final); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(l.tmp, "put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Link(tmpName, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		return err
	}
	return syncDir(filepath.Dir(final))
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
	return newETag, syncDir(filepath.Dir(p))
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
