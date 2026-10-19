package remotesync

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// VerifyDir 校验一个包目录：清单、分片哈希、逐对象解压与内容哈希、版本对象齐全。
func VerifyDir(dir string) (*Manifest, error) {
	return verifyDir(dir, nil)
}

// ImportDir 校验包、把对象写入本地 CAS、并把版本/血缘元数据写回数据库。
func ImportDir(r *repo.Repo, dir string) (*Manifest, int, error) {
	n := 0
	m, err := verifyDir(dir, func(id hash.ID, stored []byte) error {
		if err := r.Store.PutStored(id, stored); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return nil, n, err
	}
	pm, err := readMeta(dir, m.MetaHash)
	if err != nil {
		return nil, n, err
	}
	if err := pm.apply(r); err != nil {
		return nil, n, err
	}
	return m, n, nil
}

func verifyDir(dir string, importFn func(id hash.ID, stored []byte) error) (*Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	var (
		objects int
		rawSum  int64
	)
	pm, err := readMeta(dir, m.MetaHash)
	if err != nil {
		return nil, err
	}
	metaVersions := make(map[string]bool, len(pm.Versions))
	for _, mv := range pm.Versions {
		if _, err := mv.toVersion(); err != nil {
			return nil, err
		}
		metaVersions[mv.ID] = true
	}
	if len(metaVersions) != len(m.Versions) {
		return nil, fmt.Errorf("%w: meta 版本数 %d != 清单 %d",
			ErrCorruptPackage, len(metaVersions), len(m.Versions))
	}
	for _, vid := range m.Versions {
		if !metaVersions[vid] {
			return nil, fmt.Errorf("%w: 清单版本 %s 不在 meta 中", ErrCorruptPackage, vid)
		}
	}
	for _, p := range m.Parts {
		b, err := os.ReadFile(filepath.Join(dir, p.Name))
		if err != nil {
			return nil, err
		}
		if got := hash.Sum(b).String(); got != p.Hash {
			return nil, fmt.Errorf("%w: 分片 %s 哈希不符", ErrCorruptPackage, p.Name)
		}
		objs, err := decodePayload(b)
		if err != nil {
			return nil, fmt.Errorf("%w: 分片 %s: %v", ErrCorruptPackage, p.Name, err)
		}
		if len(objs) != p.Objects {
			return nil, fmt.Errorf("%w: 分片 %s 对象数 %d != %d", ErrCorruptPackage, p.Name, len(objs), p.Objects)
		}
		for _, o := range objs {
			objects++
			rawSum += int64(len(o.raw))
			if importFn != nil {
				if err := importFn(o.id, encodeStoredFor(o)); err != nil {
					return nil, err
				}
			}
		}
	}
	if objects != m.ObjectCount {
		return nil, fmt.Errorf("%w: 对象总数 %d != 清单 %d", ErrCorruptPackage, objects, m.ObjectCount)
	}
	if rawSum != m.RawBytes {
		return nil, fmt.Errorf("%w: 原始字节 %d != 清单 %d", ErrCorruptPackage, rawSum, m.RawBytes)
	}
	return m, nil
}

// StagedRows 返回本地暂存的包记录。
func StagedRows(r *repo.Repo) ([]meta.PackageRow, error) { return r.DB.ListPackages() }

// ReadStaged 读取一条包记录的清单。
func ReadStaged(row meta.PackageRow) (*Manifest, error) {
	if row.LocalPath == "" {
		return nil, fmt.Errorf("remotesync: 包 %s 没有本地路径", row.ID)
	}
	return ReadManifest(row.LocalPath)
}

// PruneLocal 删除**已上传**的本地包（文件 + 记录），返回数量与释放字节。
func PruneLocal(r *repo.Repo) (int, int64, error) {
	rows, err := r.DB.ListPackages()
	if err != nil {
		return 0, 0, err
	}
	var (
		n     int
		bytes int64
	)
	for _, row := range rows {
		if !row.Uploaded {
			continue
		}
		if row.LocalPath != "" {
			_ = os.RemoveAll(row.LocalPath)
		}
		if err := r.DB.DeletePackage(row.ID); err != nil {
			return n, bytes, err
		}
		n++
		bytes += row.ByteSize
	}
	return n, bytes, nil
}
