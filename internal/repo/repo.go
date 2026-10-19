// Package repo 实现仓库级操作：init / import / land / revert / 状态（§2、§7）。
package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
	"github.com/chenyiwen/p37/internal/workspace"
)

// DefaultEpochSize 是 epoch 的版本数默认值（§9.4，附录 B 的 retention.epoch_size）。
const DefaultEpochSize = 50

// AgentImporter 是 import 产生的版本的 agent 名。
const AgentImporter = "p37-import"

// Repo 是一个已打开的仓库。
type Repo struct {
	Root  string
	DB    *meta.DB
	Store *cas.Store
	MM    *mountmgr.Manager
	WS    *workspace.Manager
	// Ignore 是生效的忽略规则。
	Ignore *tree.Ignore
	// CaptureOpt 是采集参数。
	CaptureOpt capture.Options

	// epochSize 是 epoch 的版本数（§9.4）。
	epochSize int
	// gc 是保留与回收参数（§10）。
	gc GCOptions

	// landSem 限制并发进入 land 流水线的请求数（超出即拒绝）。
	landSem chan struct{}
	// landMu 串行化整条 land 流水线（§7.4：一条全局 land 队列）。
	landMu sync.Mutex
}

// Errors
var (
	// ErrAlreadyInitialized 表示仓库已存在。
	ErrAlreadyInitialized = errors.New("repo: 仓库已初始化")
	// ErrNotInitialized 表示仓库不存在或未初始化。
	ErrNotInitialized = errors.New("repo: 仓库未初始化")
	// ErrLandConflict 表示 land 时存在未决冲突。
	ErrLandConflict = errors.New("repo: land 存在未决冲突")
	// ErrNotDirty 表示没有改动可落地。
	ErrNotDirty = errors.New("repo: 无改动")
	// ErrLandQueueFull 表示 land 队列已满。
	ErrLandQueueFull = errors.New("repo: land 队列已满")
)

// Options 配置打开仓库。
type Options struct {
	Ignore         *tree.Ignore
	ExtraIgnore    []string
	Capture        capture.Options
	Materialize    mountmgr.Mode
	InodeReliable  bool
	BaseCacheBytes int64
	// LandQueueMax 是 land 队列容量；超出即拒绝（LAND_QUEUE_FULL，§11.5）。
	// 0 表示使用 DefaultLandQueueMax。
	LandQueueMax int
	// RemountWorkspaces 为真时在 Open 阶段恢复全部活跃工作区的挂载
	// （§7.7 步骤 3）。**只有常驻 daemon 应该打开它**：CLI 每次调用都重挂
	// 既慢又会与 daemon 打架。
	RemountWorkspaces bool

	// EpochSize 是 epoch 的版本数（§9.4）。0 表示 DefaultEpochSize。
	EpochSize int
	// MaxActive / MaxDepth / MaxFanout 覆盖工作区上限（附录 B）。
	// 0 表示硬上限；只能调小，不能调大（D20）。
	MaxActive int
	MaxDepth  int
	MaxFanout int
	// MaxMatches 是 place 的匹配上限（附录 B 的 merge.max_matches）。
	// 0 表示 op.DefaultMaxMatches。
	MaxMatches int
	// GC 是保留与回收参数（§10）。
	GC GCOptions
}

// GCOptions 是保留与回收的参数（§10.3、§10.4）。
type GCOptions struct {
	// KeepVersions 是始终保留的最近版本数。
	KeepVersions int
	// KeepEpochs 是保留 checkpoint 的 epoch 数。
	KeepEpochs int
	// ScrubSample 是 scrub 的抽样比例（0–1）。
	ScrubSample float64
	// VerifyOnRead 是读时校验策略："off" / "sample" / "always"。
	VerifyOnRead string
}

// DefaultGCOptions 返回附录 B 的默认值。
func DefaultGCOptions() GCOptions {
	return GCOptions{KeepVersions: 200, KeepEpochs: 20, ScrubSample: 0.01, VerifyOnRead: "sample"}
}

// DefaultLandQueueMax 是 land 队列的默认容量（附录 B 的 daemon.land_queue_max）。
const DefaultLandQueueMax = 256

// Open 打开一个已初始化的仓库，并做启动自检与 land 修复（§7.7）。
func Open(root string, opt Options) (*Repo, error) {
	p37 := filepath.Join(root, ".p37")
	if _, err := os.Stat(p37); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotInitialized, root)
	}

	db, err := meta.Open(root)
	if err != nil {
		return nil, err
	}
	if err := db.CheckFormat(); err != nil {
		_ = db.Close()
		return nil, err
	}

	bs, err := cas.NewLocalFS(p37)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store := cas.NewStore(bs, db.ObjectIndex())

	mm, err := mountmgr.New(root, mountmgr.Options{
		BaseCacheBytes: opt.BaseCacheBytes,
		InodeReliable:  opt.InodeReliable,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	ig := opt.Ignore
	if ig == nil {
		ig, err = tree.NewIgnore(true, opt.ExtraIgnore)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	copt := opt.Capture
	if copt.ProbeBytes == 0 {
		copt = capture.DefaultOptions()
	}

	ws := workspace.New(root, db, store, mm, workspace.Options{
		Ignore:             ig,
		Capture:            copt,
		MaterializeDefault: opt.Materialize,
		InodeReliable:      opt.InodeReliable,
		MaxActive:          opt.MaxActive,
		MaxDepth:           opt.MaxDepth,
		MaxFanout:          opt.MaxFanout,
		MaxMatches:         opt.MaxMatches,
	})

	// 启动修复：补做/回滚未完成的 land（§7.7 步骤 2）。
	if _, err := db.ReconcileLand(); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 步骤 3：恢复活跃工作区的挂载。**必须在 land 修复之后**，因为修复可能
	// 焚毁/恢复工作区。只重建挂载，upper 层原样保留。
	if opt.RemountWorkspaces {
		if _, err := ws.RemountAll(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	qmax := opt.LandQueueMax
	if qmax <= 0 {
		qmax = DefaultLandQueueMax
	}
	epoch := opt.EpochSize
	if epoch <= 0 {
		epoch = DefaultEpochSize
	}
	gc := opt.GC
	if gc.KeepVersions <= 0 {
		gc.KeepVersions = DefaultGCOptions().KeepVersions
	}
	if gc.KeepEpochs <= 0 {
		gc.KeepEpochs = DefaultGCOptions().KeepEpochs
	}
	if gc.VerifyOnRead == "" {
		gc.VerifyOnRead = DefaultGCOptions().VerifyOnRead
	}
	// 读时校验策略（§8.3、附录 B 的 gc.verify_on_read）。
	store.SetVerifyOnRead(cas.ParseVerifyMode(gc.VerifyOnRead))

	return &Repo{Root: root, DB: db, Store: store, MM: mm, WS: ws,
		Ignore: ig, CaptureOpt: copt, epochSize: epoch, gc: gc,
		landSem: make(chan struct{}, qmax)}, nil
}

// EpochSize 返回生效的 epoch 版本数（§9.4）。
func (r *Repo) EpochSize() int { return r.epochSize }

// GCOptions 返回生效的保留与回收参数（§10）。
func (r *Repo) GCOptions() GCOptions { return r.gc }

// enterLand 取得一个 land 名额；队列满时立即失败（不阻塞）。
func (r *Repo) enterLand() (release func(), err error) {
	select {
	case r.landSem <- struct{}{}:
		return func() { <-r.landSem }, nil
	default:
		return nil, fmt.Errorf("%w（上限 %d）", ErrLandQueueFull, cap(r.landSem))
	}
}

// Init 创建一个空仓库，并写入 v0（空树）。
func Init(root string, opt Options) (*Repo, error) {
	p37 := filepath.Join(root, ".p37")
	if _, err := os.Stat(filepath.Join(p37, "p37.db")); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyInitialized, root)
	}
	for _, d := range []string{p37, filepath.Join(p37, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	db, err := meta.Open(root)
	if err != nil {
		return nil, err
	}
	if err := db.Init(root); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = db.Close()

	r, err := Open(root, opt)
	if err != nil {
		return nil, err
	}

	// 覆盖初始 v0（空树）。
	empty := &tree.Tree{}
	tid, err := r.Store.PutTree(empty)
	if err != nil {
		return nil, err
	}
	v := version.New(hash.Zero, tid, 0, "p37-init", "init", time.Now().UnixMilli(), "init")
	if err := r.DB.InsertVersion(v); err != nil {
		return nil, err
	}
	if err := r.writeHead(v.ID()); err != nil {
		return nil, err
	}
	if err := r.DB.SetMainlineHead(v.ID(), 1, 0); err != nil {
		return nil, err
	}
	return r, nil
}

// Import 把 srcdir 的目录树作为 v0 导入（§2.1）。
//
// 覆盖已有的 v0（空树），因此只能在尚无后续版本时调用。
func (r *Repo) Import(srcDir string) (hash.ID, error) {
	head, ok, err := r.DB.Head()
	if err != nil {
		return hash.ID{}, err
	}
	if !ok {
		return hash.ID{}, ErrNotInitialized
	}
	n, err := r.DB.CountVersions()
	if err != nil {
		return hash.ID{}, err
	}
	if n > 1 {
		return hash.ID{}, errors.New("repo: 只能在尚无后续版本时 import")
	}
	// 删除初始 v0，避免留下两条互不相连的根。
	if err := r.DB.DeleteVersion(head); err != nil {
		return hash.ID{}, err
	}

	// 导入没有对应的工作区行，因此**不写** stat 缓存（stat_cache.ws_id 有外键）。
	// 传 nil 即可：Build 会跳过全部缓存读写。
	// 稳定点判定只针对「agent 正在并发改工作区」的场景；import 是离线操作，
	// 没有并发写者，显式关掉以免白等（§5.2 的适用面就是工作区采集）。
	iopt := r.CaptureOpt
	iopt.StableRetries = 0
	t, err := capture.Build("", srcDir, r.Ignore, r.Store, nil,
		capture.InodeReliable(false), iopt)
	if err != nil {
		return hash.ID{}, err
	}

	v := version.New(hash.Zero, t.ID(),
		0, AgentImporter, "import "+srcDir, time.Now().UnixMilli(), "import")
	if err := r.DB.InsertVersion(v); err != nil {
		return hash.ID{}, err
	}
	// 写一条血缘记录：否则 blame 会把「导入」这一版报成「journal 已清理」，
	// 而它恰恰是最需要解释来源的一版。
	if _, err := r.DB.InsertJournal(meta.JournalEntry{
		VersionID: v.ID().String(),
		Agent:     AgentImporter,
		TaskDesc:  v.TaskDesc,
		TS:        v.TS,
		OpsJSON:   "[]",
	}); err != nil {
		return hash.ID{}, err
	}
	if err := r.writeHead(v.ID()); err != nil {
		return hash.ID{}, err
	}
	if err := r.DB.SetMainlineHead(v.ID(), 1, 0); err != nil {
		return hash.ID{}, err
	}
	_ = head
	return v.ID(), nil
}

// writeHead 原子写入 refs/mainline（§7.3）。
//
// 这是 land 的唯一提交点，因此**先**把此前写入的全部对象落盘，再推进 head：
// 保证「head 指向的对象一定已持久」这一不变量。
func (r *Repo) writeHead(id hash.ID) error {
	if err := r.Store.Flush(); err != nil {
		return fmt.Errorf("repo: 对象落盘失败: %w", err)
	}
	key := "refs/mainline"
	cur, err := r.Store.BlobStore().Stat(key)
	etag := ""
	if err == nil {
		etag = cur.ETag
	} else if !errors.Is(err, cas.ErrNotExist) {
		return err
	}
	_, err = r.Store.BlobStore().PutIfMatch(key, etag, []byte(id.String()+"\n"))
	return err
}

// Sync 采集一个工作区（workspace.Manager.Sync 的便捷入口）。
func (r *Repo) Sync(wsID string) error { return r.WS.Sync(wsID) }

// LiveWorkspaces 返回当前应当被 fs watcher 监听的工作区（id → 目录）。
//
// 只包含 active / conflicted 且**确实可读**的工作区：broken（目录缺失或未挂载）
// 与中间态（rebasing / integrating）都不监听 —— 前者监听的是空目录，后者
// 的采集会被 Sync 自己拒绝。
func (r *Repo) LiveWorkspaces() (map[string]string, error) {
	all, err := r.DB.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for i := range all {
		w := &all[i]
		switch w.State {
		case meta.StateActive, meta.StateConflicted:
		default:
			continue
		}
		dir := r.MM.WorkspaceDir(w.ID)
		if !r.MM.IsMounted(w.ID) && w.Materialize == meta.MaterializeOverlay {
			continue // 未挂载 -> 目录是空的，监听没有意义
		}
		out[w.ID] = dir
	}
	return out, nil
}

// Head 返回主干 head。
func (r *Repo) Head() (hash.ID, bool, error) { return r.DB.Head() }

// Close 关闭仓库。
func (r *Repo) Close() error { return r.DB.Close() }

// VersionSummary 是 version_log 的一项。
type VersionSummary struct {
	ID        hash.ID
	Parent    hash.ID
	TS        int64
	Agent     string
	Task      string
	Epoch     uint64
	Files     int
	Additions int
	Deletions int
}

// Log 返回版本列表。
func (r *Repo) Log(limit int, beforeTS int64) ([]VersionSummary, error) {
	rows, err := r.DB.ListVersions(limit, beforeTS)
	if err != nil {
		return nil, err
	}
	out := make([]VersionSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, VersionSummary{
			ID: row.ID, Parent: row.Parent, TS: row.TS,
			Agent: row.Agent, Task: row.Task, Epoch: row.Epoch,
		})
	}
	return out, nil
}

// Status 是 repo_status 的仓库部分。
type Status struct {
	Root         string
	MainlineHead hash.ID
	HasHead      bool
	VersionCount int64
	Epoch        uint64
	LastLandTS   int64
	FormatVer    int
	Workspaces   int
}

// Status 返回仓库状态。
func (r *Repo) Status() (*Status, error) {
	rp, err := r.DB.GetRepo()
	if err != nil {
		return nil, err
	}
	nWS, err := r.DB.CountWorkspaces()
	if err != nil {
		return nil, err
	}
	var lastTS int64
	if !rp.MainlineHead.IsZero() {
		if v, err := r.DB.GetVersion(rp.MainlineHead); err == nil {
			lastTS = v.TS
		}
	}
	return &Status{
		Root:         rp.Root,
		MainlineHead: rp.MainlineHead,
		HasHead:      !rp.MainlineHead.IsZero(),
		VersionCount: rp.VersionCount,
		Epoch:        rp.Epoch,
		LastLandTS:   lastTS,
		FormatVer:    rp.FormatVer,
		Workspaces:   nWS,
	}, nil
}

// journalOps 构造 journal 的 ops_json（§12.1）。
func journalOps(ops []op.Op) string {
	type item struct {
		OpID    string `json:"op_id"`
		Path    string `json:"path"`
		Kind    string `json:"kind"`
		Added   int    `json:"added"`
		Removed int    `json:"removed"`
	}
	items := make([]item, 0, len(ops))
	for i := range ops {
		o := &ops[i]
		add, del := 0, 0
		for _, h := range o.Hunks {
			add += len(h.Insert)
			del += len(h.Remove)
		}
		items = append(items, item{OpID: o.ID, Path: o.Path, Kind: string(o.Kind),
			Added: add, Removed: del})
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// toJSONLines / toJSONStrings 把行序列编码为 JSON 字符串数组（用于冲突展示）。
func toJSONLines(ls [][]byte) string {
	ss := make([]string, len(ls))
	for i, l := range ls {
		ss[i] = string(l)
	}
	return toJSONStrings(ss)
}

func toJSONStrings(ss []string) string {
	b, err := json.Marshal(ss)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// captureDiff 是 capture.Diff 的薄包装，避免 repo 直接依赖 capture 的类型别名。
func captureDiff(src capture.ObjectWriter, wsID string, base, next *tree.Tree, opt capture.Options) ([]op.Op, error) {
	return capture.Diff(wsID, base, next, src, opt)
}
