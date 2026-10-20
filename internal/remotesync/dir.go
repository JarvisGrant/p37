package remotesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// DirRemote 把「远端」实现为一个本地目录。
//
// 只用于本地端到端测试与离线演练；生产用 S3。
type DirRemote struct {
	root string
}

// NewDirRemote 打开/创建目录远端。
func NewDirRemote(root string) (*DirRemote, error) {
	if err := os.MkdirAll(filepath.Join(root, "packages"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "refs"), 0o700); err != nil {
		return nil, err
	}
	return &DirRemote{root: root}, nil
}

func (d *DirRemote) pkgDir(id string) string {
	return filepath.Join(d.root, "packages", id)
}

func (d *DirRemote) refPath() string {
	return filepath.Join(d.root, "refs", "mainline.json")
}

// PutPackage 实现 Remote。
func (d *DirRemote) PutPackage(ctx context.Context, m *Manifest, srcDir string) error {
	dst := d.pkgDir(m.ID)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(srcDir, MetaName), filepath.Join(dst, MetaName)); err != nil {
		return err
	}
	for _, p := range m.Parts {
		src, err := partPath(srcDir, p.Name)
		if err != nil {
			return err
		}
		dstPath, err := partPath(dst, p.Name)
		if err != nil {
			return err
		}
		if err := copyFile(src, dstPath); err != nil {
			return err
		}
	}
	return copyFile(filepath.Join(srcDir, ManifestName), filepath.Join(dst, ManifestName))
}

// GetManifest 实现 Remote。
func (d *DirRemote) GetManifest(ctx context.Context, id string) (*Manifest, error) {
	return ReadManifest(d.pkgDir(id))
}

// GetPackage 实现 Remote。
func (d *DirRemote) GetPackage(ctx context.Context, id, dst string) (*Manifest, error) {
	m, err := d.GetManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return nil, err
	}
	if err := copyFile(filepath.Join(d.pkgDir(id), MetaName), filepath.Join(dst, MetaName)); err != nil {
		return nil, err
	}
	for _, p := range m.Parts {
		src, err := partPath(d.pkgDir(id), p.Name)
		if err != nil {
			return nil, err
		}
		dstPath, err := partPath(dst, p.Name)
		if err != nil {
			return nil, err
		}
		if err := copyFile(src, dstPath); err != nil {
			return nil, err
		}
	}
	if err := copyFile(filepath.Join(d.pkgDir(id), ManifestName), filepath.Join(dst, ManifestName)); err != nil {
		return nil, err
	}
	return m, nil
}

// ListPackages 实现 Remote。
func (d *DirRemote) ListPackages(ctx context.Context) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(d.root, "packages"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// GetRef 实现 Remote。
func (d *DirRemote) GetRef(ctx context.Context) (Ref, error) {
	b, err := os.ReadFile(d.refPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Ref{}, ErrNoRef
		}
		return Ref{}, err
	}
	return decodeRef(b)
}

// UpdateRef 实现 Remote：用 O_EXCL 锁文件把读-比较-写串行化。
func (d *DirRemote) UpdateRef(ctx context.Context, expected, next Ref) error {
	lock := filepath.Join(d.root, "refs", ".lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrRemoteMoved
		}
		return err
	}
	_ = f.Close()
	defer func() { _ = os.Remove(lock) }()

	cur, err := d.GetRef(ctx)
	if err != nil && !errors.Is(err, ErrNoRef) {
		return err
	}
	if expected.Head == "" {
		if !errors.Is(err, ErrNoRef) {
			return ErrRemoteMoved
		}
	} else if err != nil || cur.Head != expected.Head {
		return ErrRemoteMoved
	}
	tmp := d.refPath() + ".tmp"
	if err := os.WriteFile(tmp, next.Encode(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.refPath())
}

// DeletePackage 实现 Remote。
func (d *DirRemote) DeletePackage(ctx context.Context, id string) error {
	return os.RemoveAll(d.pkgDir(id))
}
