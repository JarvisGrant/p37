// Package config 读取仓库配置（附录 B）。
//
// 配置是可选的：文件不存在时全部取默认值。所有「格式资产」类参数（CDC、编码）
// **不在配置里**——它们是冻结的格式，不随仓库而变。
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/tree"
)

// Config 是 config.toml 的结构（附录 B）。
type Config struct {
	Repo      RepoConfig      `toml:"repo"`
	Files     FilesConfig     `toml:"files"`
	Workspace WorkspaceConfig `toml:"workspace"`
	Merge     MergeConfig     `toml:"merge"`
	Retention RetentionConfig `toml:"retention"`
	Daemon    DaemonConfig    `toml:"daemon"`
	Journal   JournalConfig   `toml:"journal"`
	GC        GCConfig        `toml:"gc"`
	Remote    RemoteConfig    `toml:"remote"`
}

// RepoConfig 对应 [repo]。
type RepoConfig struct {
	FormatVer   int      `toml:"format_ver"`
	IgnoreExtra []string `toml:"ignore_extra"`
}

// FilesConfig 对应 [files]。
type FilesConfig struct {
	MaxFileSize      int64 `toml:"max_file_size"`
	MaxTextSize      int64 `toml:"max_text_size"`
	BinaryProbeBytes int   `toml:"binary_probe_bytes"`
	MaxPathBytes     int   `toml:"max_path_bytes"`
	MaxSegmentBytes  int   `toml:"max_segment_bytes"`
}

// WorkspaceConfig 对应 [workspace]。
type WorkspaceConfig struct {
	Materialize     string `toml:"materialize"`
	BaseCacheBytes  int64  `toml:"base_cache_bytes"`
	ScanInterval    string `toml:"scan_interval"`
	Debounce        string `toml:"debounce"`
	StableRetries   int    `toml:"stable_retries"`
	StableDelay     string `toml:"stable_delay"`
	MaxActive       int    `toml:"max_active"`
	MaxDepth        int    `toml:"max_depth"`
	MaxFanout       int    `toml:"max_fanout"`
	InotifyFallback bool   `toml:"inotify_fallback"`
	WarnDiskBytes   int64  `toml:"warn_disk_bytes"`
}

// MergeConfig 对应 [merge]。
type MergeConfig struct {
	ContextLines   int     `toml:"context_lines"`
	MoveSimilarity float64 `toml:"move_similarity"`
	MaxMatches     int     `toml:"max_matches"`
	MaxChainLength int     `toml:"max_chain_length"`
}

// RetentionConfig 对应 [retention]。
type RetentionConfig struct {
	KeepVersions int `toml:"keep_versions"`
	KeepEpochs   int `toml:"keep_epochs"`
	EpochSize    int `toml:"epoch_size"`
}

// DaemonConfig 对应 [daemon]。
type DaemonConfig struct {
	Socket       string `toml:"socket"`
	ListenHTTP   bool   `toml:"listen_http"`
	HTTPAddr     string `toml:"http_addr"`
	HTTPPort     int    `toml:"http_port"`
	AuthToken    string `toml:"auth_token"`
	MetricsHTTP  bool   `toml:"metrics_http"`
	LogLevel     string `toml:"log_level"`
	LandQueueMax int    `toml:"land_queue_max"`
}

// JournalConfig 对应 [journal]。
type JournalConfig struct {
	RedactTaskDesc bool `toml:"redact_task_desc"`
}

// GCConfig 对应 [gc]。
type GCConfig struct {
	ScrubSample  float64 `toml:"scrub_sample"`
	VerifyOnRead string  `toml:"verify_on_read"`
}

// RemoteConfig 对应 [remote]（§9 P3 远端同步）。
//
// 凭证**不在这里**：S3 的 access key / secret 只从环境变量读
// （P37_S3_ACCESS_KEY / P37_S3_SECRET_KEY / P37_S3_SESSION_TOKEN）。
type RemoteConfig struct {
	Enabled   bool   `toml:"enabled"`
	Backend   string `toml:"backend"` // s3 | dir
	Endpoint  string `toml:"endpoint"`
	Region    string `toml:"region"`
	Bucket    string `toml:"bucket"`
	Prefix    string `toml:"prefix"`
	RemoteKey string `toml:"remote_key"`
	PathStyle bool   `toml:"path_style"`
	AutoPush  bool   `toml:"auto_push"`
	// Dir 仅用于 backend=dir（本地目录演练/测试）。
	Dir string `toml:"dir"`

	// 打包策略：低频 + 压缩。
	Outbox          string `toml:"outbox"`
	MinVersions     int    `toml:"min_versions"`
	MinBytes        int64  `toml:"min_bytes"`
	CheckpointEvery int    `toml:"checkpoint_every"`
	CompressLevel   int    `toml:"compress_level"`
	MaxPackageBytes int64  `toml:"max_package_bytes"`
	// AutoPrepareEvery 预留给 daemon 定时 prepare（只落本地，不上传）。
	AutoPrepareEvery string `toml:"auto_prepare_every"`
}

// Default 返回附录 B 的默认配置。
func Default() Config {
	return Config{
		Repo: RepoConfig{FormatVer: 2},
		Files: FilesConfig{
			MaxFileSize:      2 << 30,
			MaxTextSize:      4 << 20,
			BinaryProbeBytes: tree.DefaultBinaryProbeBytes,
			MaxPathBytes:     4096,
			MaxSegmentBytes:  255,
		},
		Workspace: WorkspaceConfig{
			Materialize:    string(mountmgr.ModeAuto),
			BaseCacheBytes: 4 << 30,
			ScanInterval:   "30s",
			Debounce:       "500ms",
			StableRetries:  5,
			StableDelay:    "200ms",
			MaxActive:      8,
			MaxDepth:       32,
			MaxFanout:      16,
			WarnDiskBytes:  12 << 30,
		},
		Merge: MergeConfig{
			ContextLines:   tree.DefaultContextLines,
			MoveSimilarity: 0.6,
			MaxMatches:     64,
			MaxChainLength: 64,
		},
		Retention: RetentionConfig{KeepVersions: 200, KeepEpochs: 20, EpochSize: 50},
		Daemon: DaemonConfig{
			HTTPAddr: "127.0.0.1", HTTPPort: 7757,
			LogLevel: "info", LandQueueMax: 256,
		},
		GC: GCConfig{ScrubSample: 0.01, VerifyOnRead: "sample"},
		Remote: RemoteConfig{
			Backend: "s3", PathStyle: true,
			Outbox:          ".p37/sync/outbox",
			MinVersions:     10,
			MinBytes:        4 << 20,
			CompressLevel:   9,
			MaxPackageBytes: 512 << 20,
		},
	}
}

// Path 返回仓库配置文件路径。
func Path(root string) string { return filepath.Join(root, ".p37", "config.toml") }

// Load 读取配置；文件不存在时返回默认值。
func Load(root string) (Config, error) {
	cfg, err := LoadTemplate(Path(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return cfg, err
	}
	if cfg.Daemon.Socket == "" {
		cfg.Daemon.Socket = filepath.Join(root, ".p37", "p37d.sock")
	}
	return cfg, nil
}

// LoadTemplate 读取一个配置模板，并以 Default() 为底做覆盖。
//
// 与 Load 的区别：文件不存在时返回错误（引导脚本需要区分“没给模板”和
// “模板给了但读不了”）；socket 的相对路径由调用方按仓库根解析。
func LoadTemplate(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if _, err := toml.Decode(string(b), &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// SocketPath 返回 daemon 的 unix socket 路径。
//
// 配置里没写时用 <root>/.p37/p37d.sock；相对路径相对仓库根，避免 CLI 与
// daemon 因 cwd 不同而连接到不同 socket。
func SocketPath(root string, cfg Config) string {
	p := cfg.Daemon.Socket
	if p == "" {
		return filepath.Join(root, ".p37", "p37d.sock")
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(root, p)
	}
	return p
}

// Save 写出配置（供 init 生成模板）。
func Save(root string, cfg Config) error {
	var sb strings.Builder
	enc := toml.NewEncoder(&sb)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	p := Path(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(sb.String()), 0o644)
}

// Duration 解析时长字符串，非法时返回默认值。
func Duration(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// CaptureOptions 由配置导出采集参数。
func (c Config) CaptureOptions() capture.Options {
	o := capture.Options{
		ProbeBytes:     c.Files.BinaryProbeBytes,
		MaxTextSize:    c.Files.MaxTextSize,
		MaxFileSize:    c.Files.MaxFileSize,
		ContextLines:   c.Merge.ContextLines,
		MaxChain:       c.Merge.MaxChainLength,
		MoveSimilarity: c.Merge.MoveSimilarity,
	}
	// 稳定点判定（§5.2）只在工作区采集里开启；import 是离线操作不需要。
	return o
}

// StableOptions 返回稳定点判定的参数（§5.2）。
//
// 单独一个方法是因为 import 要显式关掉它（离线导入不需要、也不应该等）。
func (c Config) StableOptions() (time.Duration, int) {
	return Duration(c.Workspace.StableDelay, 200*time.Millisecond), c.Workspace.StableRetries
}

// WarnDiskBytes 返回磁盘告警阈值（附录 B 的 workspace.warn_disk_bytes）。
func (c Config) WarnDiskBytes() int64 {
	if c.Workspace.WarnDiskBytes <= 0 {
		return 12 << 30
	}
	return c.Workspace.WarnDiskBytes
}

// InotifyFallback 报告是否应把 fs watcher 降级为只监听根目录（附录 G.3）。
func (c Config) InotifyFallback() bool { return c.Workspace.InotifyFallback }

// LogLevel 返回日志级别。
func (c Config) LogLevel() string {
	if c.Daemon.LogLevel == "" {
		return "info"
	}
	return c.Daemon.LogLevel
}

// RepoOptions 由配置导出 repo.Options。
//
// CLI 与 daemon 共用同一份映射，避免两处配置走样（这正是「配置写了不生效」
// 这类问题的常见来源）。
func (c Config) RepoOptions() repo.Options {
	delay, retries := c.StableOptions()
	copt := c.CaptureOptions()
	copt.StableDelay, copt.StableRetries = delay, retries
	return repo.Options{
		ExtraIgnore:    c.Repo.IgnoreExtra,
		Capture:        copt,
		Materialize:    c.MountMode(),
		BaseCacheBytes: c.Workspace.BaseCacheBytes,
		LandQueueMax:   c.Daemon.LandQueueMax,
		RedactTaskDesc: c.Journal.RedactTaskDesc,
		EpochSize:      c.Retention.EpochSize,
		MaxActive:      c.Workspace.MaxActive,
		MaxDepth:       c.Workspace.MaxDepth,
		MaxFanout:      c.Workspace.MaxFanout,
		MaxMatches:     c.Merge.MaxMatches,
		GC: repo.GCOptions{
			KeepVersions: c.Retention.KeepVersions,
			KeepEpochs:   c.Retention.KeepEpochs,
			ScrubSample:  c.GC.ScrubSample,
			VerifyOnRead: c.GC.VerifyOnRead,
		},
	}
}

// RemoteOptions 由 [remote] 导出同步打包参数。
func (c Config) RemoteOptions() remotesync.Options {
	r := c.Remote
	return remotesync.Options{
		Outbox:          r.Outbox,
		MinVersions:     r.MinVersions,
		MinBytes:        r.MinBytes,
		CheckpointEvery: r.CheckpointEvery,
		CompressLevel:   r.CompressLevel,
		MaxPackageBytes: r.MaxPackageBytes,
	}
}

// MountMode 由配置导出物化模式。
func (c Config) MountMode() mountmgr.Mode {
	switch mountmgr.Mode(c.Workspace.Materialize) {
	case mountmgr.ModeOverlay:
		return mountmgr.ModeOverlay
	case mountmgr.ModeCopy:
		return mountmgr.ModeCopy
	default:
		return mountmgr.ModeAuto
	}
}
