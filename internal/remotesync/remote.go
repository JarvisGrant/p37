package remotesync

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Ref 是远端主干指针（存在远端对象存储里）。
type Ref struct {
	Head    string `json:"head"`
	Package string `json:"package"`
	TS      int64  `json:"ts"`
	// ETag 仅用于本地实现的条件写，不参与 JSON。
	ETag string `json:"-"`
}

// Encode 返回 ref 的确定性 JSON。
func (r Ref) Encode() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return b
}

// decodeRef 解析 ref JSON。
func decodeRef(b []byte) (Ref, error) {
	var r Ref
	if err := json.Unmarshal(b, &r); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// Errors
var (
	// ErrNoRef 表示远端还没有 ref（空仓库）。
	ErrNoRef = errors.New("remotesync: 远端尚无主干指针")
	// ErrRemoteMoved 表示远端已被别人推进（条件写失败）。
	ErrRemoteMoved = errors.New("remotesync: 远端已被推进，请重新 prepare")
)

// Remote 是同步对端的抽象。
//
// S3 与本地目录实现共用它；包是内容寻址的，因此重复 Put 是幂等的。
type Remote interface {
	// PutPackage 上传一个包（manifest + 全部分片）。
	PutPackage(ctx context.Context, m *Manifest, srcDir string) error
	// GetManifest 只取清单。
	GetManifest(ctx context.Context, id string) (*Manifest, error)
	// GetPackage 下载清单与全部分片到 dst。
	GetPackage(ctx context.Context, id, dst string) (*Manifest, error)
	// ListPackages 列出远端包 id。
	ListPackages(ctx context.Context) ([]string, error)
	// GetRef 读取主干指针；空仓库返回 ErrNoRef。
	GetRef(ctx context.Context) (Ref, error)
	// UpdateRef 条件更新主干指针（expected 为空时表示「不存在才创建」）。
	UpdateRef(ctx context.Context, expected, next Ref) error
	// DeletePackage 删除一个包。
	DeletePackage(ctx context.Context, id string) error
}

// copyFile 复制文件并保留内容。
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
