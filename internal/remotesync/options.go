package remotesync

import (
	"errors"
	"path/filepath"
)

// Options 控制打包与暂存（对应附录 B 的 [remote]）。
type Options struct {
	// Outbox 是本地暂存目录；相对路径按仓库根解析。
	Outbox string
	// MinVersions 是触发打包的最少版本数（--force 可覆盖）。
	MinVersions int
	// MinBytes 是触发打包的最少 payload 字节数（--force 可覆盖）。
	MinBytes int64
	// CheckpointEvery > 0 时，每累积这么多版本强制一次 checkpoint
	// （会与远端已有对象重复，默认 0 = 只在首次推送时 checkpoint）。
	CheckpointEvery int
	// CompressLevel > 0 时按该 zstd 等级重压对象；0 = 复用已存字节。
	CompressLevel int
	// MaxPackageBytes 是单个 payload 分片的大小上限（超过则切分）。
	MaxPackageBytes int64
}

// DefaultOptions 返回默认打包参数。
func DefaultOptions(root string) Options {
	return Options{
		Outbox:          filepath.Join(root, ".p37", "sync", "outbox"),
		MinVersions:     10,
		MinBytes:        4 << 20,
		CheckpointEvery: 0,
		CompressLevel:   9,
		MaxPackageBytes: 512 << 20,
	}
}

func (o Options) withDefaults(root string) Options {
	d := DefaultOptions(root)
	if o.Outbox == "" {
		o.Outbox = d.Outbox
	} else if !filepath.IsAbs(o.Outbox) {
		o.Outbox = filepath.Join(root, o.Outbox)
	}
	if o.MinVersions <= 0 {
		o.MinVersions = d.MinVersions
	}
	if o.MinBytes <= 0 {
		o.MinBytes = d.MinBytes
	}
	if o.MaxPackageBytes <= 0 {
		o.MaxPackageBytes = d.MaxPackageBytes
	}
	return o
}

// Errors
var (
	// ErrUpToDate 表示本地 head 与基准相同，没有可打包的内容。
	ErrUpToDate = errors.New("remotesync: 已是最新，无需打包")
	// ErrNothingToPackage 表示变更量未达到打包阈值。
	ErrNothingToPackage = errors.New("remotesync: 变更量未达到打包阈值（可用 --force）")
	// ErrNotAncestor 表示基准版本不是本地 head 的祖先（远端已分叉）。
	ErrNotAncestor = errors.New("remotesync: 基准不是本地 head 的祖先（远端已分叉）")
)
