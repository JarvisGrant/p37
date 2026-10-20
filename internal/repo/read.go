package repo

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/tree"
)

// TreeEntry 是 version_tree 的一项。
type TreeEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Type   string `json:"type"` // file | dir | symlink
	Size   int64  `json:"size,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Target string `json:"target,omitempty"`
}

// ResolveVersionRef 解析 ref 为版本 id。
func (r *Repo) ResolveVersionRef(ref string) (hash.ID, error) { return r.resolveRef(ref, false) }

// resolveTreeAt 返回版本在 path 处的树与可选的条目。
func (r *Repo) resolveTreeAt(ref, p string) (*tree.Tree, *tree.Entry, error) {
	id, err := r.resolveRef(ref, false)
	if err != nil {
		return nil, nil, err
	}
	v, err := r.DB.GetVersion(id)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrVersionNotFound, id)
	}
	t, err := r.Store.GetTree(v.Tree)
	if err != nil {
		return nil, nil, err
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return t, nil, nil
	}
	clean, err := codec.ValidatePath(p)
	if err != nil {
		return nil, nil, err
	}
	segs := strings.Split(clean, "/")
	cur := t
	for i, seg := range segs {
		var ent *tree.Entry
		for j := range cur.Entries {
			if cur.Entries[j].Name == seg {
				ent = &cur.Entries[j]
				break
			}
		}
		if ent == nil {
			return nil, nil, fmt.Errorf("%w: %s", ErrPathNotFound, p)
		}
		if i == len(segs)-1 {
			return cur, ent, nil
		}
		if ent.Kind != codec.EntryTree {
			return nil, nil, fmt.Errorf("%w: %s", ErrPathNotFound, p)
		}
		cur, err = r.Store.GetTree(ent.ID)
		if err != nil {
			return nil, nil, err
		}
	}
	return cur, nil, nil
}

func modeString(m uint64) string {
	if m == codec.ModeExecutable {
		return "100755"
	}
	return "100644"
}

// VersionTree 列出历史版本/line 的目录。
func (r *Repo) VersionTree(ref, p string, recursive bool, limit int) ([]TreeEntry, bool, error) {
	cur, ent, err := r.resolveTreeAt(ref, p)
	if err != nil {
		return nil, false, err
	}
	if ent != nil && ent.Kind != codec.EntryTree {
		e, err := r.entryView(p, ent)
		return []TreeEntry{e}, false, err
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	var out []TreeEntry
	truncated := false
	var walk func(prefix string, t *tree.Tree) error
	walk = func(prefix string, t *tree.Tree) error {
		entries := append([]tree.Entry{}, t.Entries...)
		sort.SliceStable(entries, func(i, j int) bool {
			di := entries[i].Kind == codec.EntryTree
			dj := entries[j].Kind == codec.EntryTree
			if di != dj {
				return di
			}
			return entries[i].Name < entries[j].Name
		})
		for i := range entries {
			e := &entries[i]
			full := e.Name
			if prefix != "" {
				full = prefix + "/" + e.Name
			}
			view, err := r.entryView(full, e)
			if err != nil {
				return err
			}
			if len(out) >= limit {
				truncated = true
				return nil
			}
			out = append(out, view)
			if recursive && e.Kind == codec.EntryTree {
				sub, err := r.Store.GetTree(e.ID)
				if err != nil {
					return err
				}
				if err := walk(full, sub); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(strings.Trim(p, "/"), cur); err != nil {
		return nil, false, err
	}
	return out, truncated, nil
}

func (r *Repo) entryView(p string, e *tree.Entry) (TreeEntry, error) {
	v := TreeEntry{Name: e.Name, Path: p, Mode: modeString(e.Mode)}
	switch e.Kind {
	case codec.EntryTree:
		v.Type = "dir"
		v.Hash = e.ID.String()
	case codec.EntrySymlink:
		v.Type = "symlink"
		b, err := r.Store.ReadBlob(e.ID)
		if err != nil {
			return v, err
		}
		v.Target = string(b)
		v.Size = int64(len(b))
		v.Hash = e.ID.String()
	default:
		v.Type = "file"
		fc, err := r.Store.GetFileContent(e.ID)
		if err != nil {
			return v, err
		}
		v.Size = fc.TotalSize
		v.Hash = e.ID.String()
	}
	return v, nil
}

// FileView 是 version_file 的结果。
type FileView struct {
	Path          string `json:"path"`
	Type          string `json:"type"`
	ContentBase64 string `json:"content_base64,omitempty"`
	Size          int64  `json:"size"`
	Mode          string `json:"mode"`
	Binary        bool   `json:"binary"`
	Truncated     bool   `json:"truncated"`
	Target        string `json:"target,omitempty"`
	Hash          string `json:"hash"`
}

// VersionFile 读取历史版本中的单个文件。
func (r *Repo) VersionFile(ref, p string, maxBytes int64) (*FileView, error) {
	_, ent, err := r.resolveTreeAt(ref, p)
	if err != nil {
		return nil, err
	}
	if ent == nil {
		return nil, fmt.Errorf("%w: %s", ErrPathNotFound, p)
	}
	if ent.Kind == codec.EntryTree {
		return nil, fmt.Errorf("%w: %s 是目录", ErrPathNotFound, p)
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	if maxBytes > 8<<20 {
		maxBytes = 8 << 20
	}
	v := &FileView{Path: strings.Trim(p, "/"), Mode: modeString(ent.Mode), Hash: ent.ID.String()}
	if ent.Kind == codec.EntrySymlink {
		b, err := r.Store.ReadBlob(ent.ID)
		if err != nil {
			return nil, err
		}
		v.Type = "symlink"
		v.Target = string(b)
		v.Size = int64(len(b))
		v.ContentBase64 = encodeBase64(b)
		return v, nil
	}
	v.Type = "file"
	fc, err := r.Store.GetFileContent(ent.ID)
	if err != nil {
		return nil, err
	}
	v.Size = fc.TotalSize
	v.Binary = detectBinaryObject(r, fc)
	if fc.TotalSize > maxBytes {
		v.Truncated = true
	}
	data, err := r.Store.ReadFileData(ent.ID)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		data = data[:maxBytes]
		v.Truncated = true
	}
	v.ContentBase64 = encodeBase64(data)
	return v, nil
}

func detectBinaryObject(r *Repo, fc *tree.FileContent) bool {
	if len(fc.Chunks) == 0 {
		return false
	}
	b, err := r.Store.GetBlob(fc.Chunks[0])
	if err != nil {
		return false
	}
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func encodeBase64(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	if len(b) == 0 {
		return ""
	}
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		out = append(out, table[(n>>18)&63], table[(n>>12)&63])
		if rem > 1 {
			out = append(out, table[(n>>6)&63])
		} else {
			out = append(out, '=')
		}
		if rem > 2 {
			out = append(out, table[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

// ArchiveResult 是 version_archive 的结果。
type ArchiveResult struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ExpiresAt int64  `json:"expires_at"`
	Format    string `json:"format"`
}

// VersionArchive 把历史版本的一个子树打成 tar.gz / zip，落在 VM 内临时目录。
func (r *Repo) VersionArchive(ref, p, format string) (*ArchiveResult, error) {
	if format == "" {
		format = "tar.gz"
	}
	if format != "tar.gz" && format != "zip" {
		return nil, fmt.Errorf("repo: 不支持的归档格式 %q", format)
	}
	cur, ent, err := r.resolveTreeAt(ref, p)
	if err != nil {
		return nil, err
	}
	if ent != nil && ent.Kind != codec.EntryTree {
		return nil, fmt.Errorf("repo: 只能归档目录")
	}
	dir := filepath.Join(r.Root, ".p37", "tmp", "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cleanupArchives(dir, time.Now().Add(-time.Hour))
	name := fmt.Sprintf("p37-%d.%s", time.Now().UnixNano(), format)
	dst := filepath.Join(dir, name)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(p, "/")
	var werr error
	switch format {
	case "tar.gz":
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		werr = r.writeTar(tw, cur, prefix)
		if werr == nil {
			werr = tw.Close()
		}
		if err := gz.Close(); werr == nil {
			werr = err
		}
	case "zip":
		zw := zip.NewWriter(f)
		werr = r.writeZip(zw, cur, prefix)
		if werr == nil {
			werr = zw.Close()
		}
	}
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		_ = os.Remove(dst)
		return nil, werr
	}
	h, size, err := sha256File(dst)
	if err != nil {
		return nil, err
	}
	return &ArchiveResult{Path: dst, Size: size, SHA256: h, Format: format,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
}

func (r *Repo) writeTar(tw *tar.Writer, t *tree.Tree, prefix string) error {
	for i := range t.Entries {
		e := &t.Entries[i]
		name := e.Name
		if prefix != "" {
			name = prefix + "/" + e.Name
		}
		switch e.Kind {
		case codec.EntryTree:
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755,
				Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)}); err != nil {
				return err
			}
			sub, err := r.Store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := r.writeTar(tw, sub, name); err != nil {
				return err
			}
		case codec.EntrySymlink:
			target, err := r.Store.ReadBlob(e.ID)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o777,
				Typeflag: tar.TypeSymlink, Linkname: string(target), ModTime: time.Unix(0, 0)}); err != nil {
				return err
			}
		default:
			data, err := r.Store.ReadFileData(e.ID)
			if err != nil {
				return err
			}
			mode := int64(0o644)
			if e.Mode == codec.ModeExecutable {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)),
				Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Repo) writeZip(zw *zip.Writer, t *tree.Tree, prefix string) error {
	for i := range t.Entries {
		e := &t.Entries[i]
		name := e.Name
		if prefix != "" {
			name = prefix + "/" + e.Name
		}
		switch e.Kind {
		case codec.EntryTree:
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Method: zip.Store}); err != nil {
				return err
			}
			sub, err := r.Store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := r.writeZip(zw, sub, name); err != nil {
				return err
			}
		case codec.EntrySymlink:
			target, err := r.Store.ReadBlob(e.ID)
			if err != nil {
				return err
			}
			h := &zip.FileHeader{Name: name, Method: zip.Store}
			h.SetMode(os.ModeSymlink | 0o777)
			w, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			if _, err := w.Write(target); err != nil {
				return err
			}
		default:
			data, err := r.Store.ReadFileData(e.ID)
			if err != nil {
				return err
			}
			h := &zip.FileHeader{Name: name, Method: zip.Deflate}
			if e.Mode == codec.ModeExecutable {
				h.SetMode(0o755)
			} else {
				h.SetMode(0o644)
			}
			w, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			if _, err := w.Write(data); err != nil {
				return err
			}
		}
	}
	return nil
}

func sha256File(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// WSDiffEntry 是 ws_diff 的文件级条目。
type WSDiffEntry struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A/M/D/R/C
	Kind    string `json:"kind,omitempty"`
	Dest    string `json:"dest_path,omitempty"`
	Added   int    `json:"add,omitempty"`
	Deleted int    `json:"del,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
}

// WSDiff 返回工作区相对 base 的文件级差异。
func (r *Repo) WSDiff(wsID string) ([]WSDiffEntry, error) {
	w, err := r.WS.Get(wsID)
	if err != nil {
		return nil, err
	}
	if err := r.WS.Sync(wsID); err != nil {
		return nil, err
	}
	base, err := r.Store.GetTree(w.BaseTree)
	if err != nil {
		return nil, err
	}
	eff, err := r.WS.EffectiveTree(wsID)
	if err != nil {
		return nil, err
	}
	next, err := r.Store.GetTree(eff)
	if err != nil {
		return nil, err
	}
	ops, err := r.diffTrees(base, next)
	if err != nil {
		return nil, err
	}
	out := make([]WSDiffEntry, 0, len(ops))
	for i := range ops {
		o := &ops[i]
		e := WSDiffEntry{Path: o.Path, Kind: string(o.Kind)}
		switch o.Kind {
		case "create":
			e.Status = "A"
		case "delete":
			e.Status = "D"
		case "rename":
			e.Status = "R"
			e.Dest = o.DestPath
		default:
			e.Status = "M"
		}
		for _, h := range o.Hunks {
			e.Added += len(h.Insert)
			e.Deleted += len(h.Remove)
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *Repo) diffTrees(a, b *tree.Tree) ([]op.Op, error) {
	return captureDiff(r.Store, "__diff__", a, b, r.CaptureOpt)
}

// Export 把 ref 的树物化到 dest 目录。dest 必须为空或不存在。
func (r *Repo) Export(ref, dest string) error {
	if dest == "" {
		return fmt.Errorf("repo: 缺少目标目录")
	}
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		return fmt.Errorf("repo: 目标目录非空: %s", dest)
	}
	t, _, err := r.resolveTreeAt(ref, "")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return writeTreeTo(dest, "", t, r.Store)
}

func writeTreeTo(dest, prefix string, t *tree.Tree, store interface {
	GetTree(hash.ID) (*tree.Tree, error)
	ReadFileData(hash.ID) ([]byte, error)
	ReadBlob(hash.ID) ([]byte, error)
}) error {
	for i := range t.Entries {
		e := &t.Entries[i]
		rel := e.Name
		if prefix != "" {
			rel = prefix + "/" + e.Name
		}
		p := filepath.Join(dest, filepath.FromSlash(rel))
		switch e.Kind {
		case codec.EntryTree:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			sub, err := store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := writeTreeTo(dest, rel, sub, store); err != nil {
				return err
			}
		case codec.EntrySymlink:
			b, err := store.ReadBlob(e.ID)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			_ = os.Remove(p)
			if err := os.Symlink(string(b), p); err != nil {
				return err
			}
		default:
			b, err := store.ReadFileData(e.ID)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if e.Mode == codec.ModeExecutable {
				mode = 0o755
			}
			if err := os.WriteFile(p, b, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func cleanupArchives(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
