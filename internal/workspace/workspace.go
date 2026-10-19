// Package workspace 实现工作区的生命周期、采集与有效树（§4）。
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/safefs"
	"github.com/chenyiwen/p37/internal/tree"
)

// 硬上限（§4.2、D20）。
//
// 这三个值是**不可逾越的天花板**：配置只能把它们调小，不能调大。
// D20 明确 max_active 是硬上限 64（64 并发 × 10 万文件的物化成本已经按
// 这个数算过），调大它会让 §4.8 的磁盘预算与 §4.7 的扫描调度失效。
const (
	HardMaxActive = 64
	HardMaxDepth  = 32
	HardMaxFanout = 16
)

// Errors
var (
	// ErrNotFound 表示工作区不存在。
	ErrNotFound = errors.New("workspace: 工作区不存在")
	// ErrLimitExceeded 表示触发了硬上限。
	ErrLimitExceeded = errors.New("workspace: 超出上限")
	// ErrHasChildren 表示有子工作区而拒绝 drop。
	ErrHasChildren = errors.New("workspace: 存在子工作区")
	// ErrWrongState 表示当前状态不允许该操作。
	ErrWrongState = errors.New("workspace: 状态不允许该操作")
	// ErrDirMissing 表示工作区目录缺失。
	ErrDirMissing = errors.New("workspace: 工作区目录缺失")
	// ErrNotMounted 表示工作区应当处于挂载态，但实际没有挂载。
	//
	// 这是**数据安全**防护：挂载丢失时工作区目录会是一个空目录，
	// 若继续采集会把「挂载丢了」误判成「agent 删光了所有文件」，
	// 落地后就会清空整棵仓库树。
	ErrNotMounted = errors.New("workspace: 工作区未挂载")
)

// Options 配置 Manager。
type Options struct {
	Ignore             *tree.Ignore
	Capture            capture.Options
	MaterializeDefault mountmgr.Mode
	InodeReliable      bool
	// MaxActive / MaxDepth / MaxFanout 覆盖硬上限（附录 B 的 workspace.*）。
	// 0 或超过硬上限时取硬上限；只能调小，不能调大（D20）。
	MaxActive int
	MaxDepth  int
	MaxFanout int
	// MaxMatches 是 place 的匹配上限（附录 B 的 merge.max_matches）。
	// 0 表示 op.DefaultMaxMatches。
	MaxMatches int
}

// maxMatches 返回生效的 place 匹配上限。
func (m *Manager) maxMatches() int {
	if m.opt.MaxMatches <= 0 {
		return op.DefaultMaxMatches
	}
	return m.opt.MaxMatches
}

// limits 返回生效的上限（已按硬上限钳制）。
func (m *Manager) limits() (active, depth, fanout int) {
	return clampLimit(m.opt.MaxActive, HardMaxActive),
		clampLimit(m.opt.MaxDepth, HardMaxDepth),
		clampLimit(m.opt.MaxFanout, HardMaxFanout)
}

// clampLimit 把配置值钳到 [1, hard]；非法值取 hard。
func clampLimit(v, hard int) int {
	if v <= 0 || v > hard {
		return hard
	}
	return v
}

// DefaultOptions 返回默认配置。
func DefaultOptions() Options {
	return Options{
		Capture:            capture.DefaultOptions(),
		MaterializeDefault: mountmgr.ModeAuto,
	}
}

// Manager 是工作区的编排层。
type Manager struct {
	repoRoot string
	db       *meta.DB
	store    *cas.Store
	mm       *mountmgr.Manager
	opt      Options
	now      func() int64

	// lastUnstable 记录每个工作区**最近一次采集**里被稳定点判定跳过的路径
	// （§5.2）。只用于可观测性，不参与任何判定。
	unstableMu   sync.Mutex
	lastUnstable map[string][]string
}

// New 创建 Manager。ig 为 nil 时使用内置忽略集。
func New(repoRoot string, db *meta.DB, store *cas.Store, mm *mountmgr.Manager, opt Options) *Manager {
	if opt.Ignore == nil {
		opt.Ignore, _ = tree.NewIgnore(true, nil)
	}
	if opt.Capture.ProbeBytes == 0 {
		opt.Capture = capture.DefaultOptions()
	}
	return &Manager{repoRoot: repoRoot, db: db, store: store, mm: mm, opt: opt,
		now: func() int64 { return time.Now().UnixMilli() }}
}

// DB 暴露元数据库。
func (m *Manager) DB() *meta.DB { return m.db }

// Mounts 暴露物化管理器。
func (m *Manager) Mounts() *mountmgr.Manager { return m.mm }

// OpenRequest 是 ws_open 的参数（附录 K）。
type OpenRequest struct {
	Parent   string // version id | ws id | ""(=HEAD)
	Name     string
	TaskDesc string
	Leases   []string
	Mode     mountmgr.Mode
}

// Open 打开一个工作区并物化（§4.1、§4.6、§11.4）。
func (m *Manager) Open(req OpenRequest) (*meta.Workspace, error) {
	// 上限检查。
	n, err := m.db.CountWorkspaces()
	if err != nil {
		return nil, err
	}
	if limit, _, _ := m.limits(); n >= limit {
		return nil, fmt.Errorf("%w: 活跃工作区已达 %d", ErrLimitExceeded, limit)
	}

	parentKind, parentID, baseTree, baseVersion, err := m.resolveParent(req.Parent)
	if err != nil {
		return nil, err
	}

	wsID, err := newID("ws")
	if err != nil {
		return nil, err
	}
	now := m.now()

	w := meta.Workspace{
		ID:          wsID,
		Name:        req.Name,
		ParentKind:  parentKind,
		ParentID:    parentID,
		BaseTree:    baseTree,
		BaseVersion: baseVersion,
		TaskDesc:    req.TaskDesc,
		State:       meta.StateActive,
		Materialize: meta.MaterializeCopy,
		CreatedTS:   now,
		UpdatedTS:   now,
	}

	mode := req.Mode
	if mode == "" {
		mode = m.opt.MaterializeDefault
	}
	actual, err := m.mm.Open(wsID, baseTree, m.store, mode)
	if err != nil {
		return nil, err
	}
	w.Materialize = string(actual)
	w.Mounted = actual == mountmgr.ModeOverlay

	// 必须先插入工作区行，再回填 stat_cache —— stat_cache.ws_id 有外键约束，
	// 顺序颠倒会导致写入被拒（早前实现里该错误被静默吞掉，缓存其实没落库）。
	if err := m.db.InsertWorkspace(w); err != nil {
		_ = m.mm.Close(wsID, false, w.Mounted)
		return nil, err
	}
	// 记录物化完成时刻。采集靠它做惰性判定（见 capture.Options），
	// 因此 ws_open 完全不必走文件系统 —— 这正是「与文件数无关」的实现方式。
	w.MaterializedTS = time.Now().UnixNano()
	if err := m.db.UpdateWorkspaceMount(wsID, w.Materialize, w.Mounted, now, w.MaterializedTS); err != nil {
		_ = m.mm.Close(wsID, false, w.Mounted)
		_ = m.db.DeleteWorkspace(wsID)
		return nil, err
	}
	for _, p := range req.Leases {
		if err := m.db.AddLease(wsID, p, "shared", now); err != nil {
			return nil, err
		}
	}
	return &w, nil
}

// resolveParent 把 parent 参数解析为 (kind, id, baseTree, baseVersion)。
//
// parent 为工作区时，先强制采集父（B9），再用其有效树作为基准（D23）。
func (m *Manager) resolveParent(parent string) (string, string, hash.ID, hash.ID, error) {
	if parent == "" {
		head, ok, err := m.db.Head()
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, err
		}
		if !ok {
			return "", "", hash.ID{}, hash.ID{}, errors.New("workspace: 主干尚未初始化")
		}
		v, err := m.db.GetVersion(head)
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, err
		}
		return meta.ParentVersion, head.String(), v.Tree, head, nil
	}

	// 先当作版本。
	if h, err := hash.Parse(parent); err == nil {
		if v, err := m.db.GetVersion(h); err == nil {
			return meta.ParentVersion, parent, v.Tree, h, nil
		}
	}

	// 再当作工作区。
	pw, err := m.db.GetWorkspace(parent)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			return "", "", hash.ID{}, hash.ID{}, fmt.Errorf("%w: parent %s", ErrNotFound, parent)
		}
		return "", "", hash.ID{}, hash.ID{}, err
	}
	if pw.State == meta.StateBroken {
		return "", "", hash.ID{}, hash.ID{}, fmt.Errorf("%w: 父工作区处于 broken", ErrWrongState)
	}
	if _, landed, err := m.db.GetLandedWS(pw.ID); err != nil {
		return "", "", hash.ID{}, hash.ID{}, err
	} else if landed {
		return "", "", hash.ID{}, hash.ID{}, fmt.Errorf("%w: 父工作区已落地", ErrWrongState)
	}

	// B9：父必须先采集，否则子的基准不含父的最新改动。
	if err := m.Sync(pw.ID); err != nil {
		return "", "", hash.ID{}, hash.ID{}, err
	}
	et, err := m.EffectiveTree(pw.ID)
	if err != nil {
		return "", "", hash.ID{}, hash.ID{}, err
	}
	// 栈深检查。
	depth, err := m.depthOf(pw.ID)
	if err != nil {
		return "", "", hash.ID{}, hash.ID{}, err
	}
	if _, maxDepth, _ := m.limits(); depth+1 >= maxDepth {
		return "", "", hash.ID{}, hash.ID{}, fmt.Errorf("%w: 栈深将达 %d", ErrLimitExceeded, depth+1)
	}
	kids, err := m.db.Children(pw.ID)
	if err != nil {
		return "", "", hash.ID{}, hash.ID{}, err
	}
	if _, _, maxFanout := m.limits(); len(kids) >= maxFanout {
		return "", "", hash.ID{}, hash.ID{}, fmt.Errorf("%w: 子工作区已达 %d", ErrLimitExceeded, maxFanout)
	}
	return meta.ParentWorkspace, pw.ID, et, pw.BaseVersion, nil
}

// depthOf 返回工作区在栈中的深度（直接挂在 version 上为 0）。
func (m *Manager) depthOf(wsID string) (int, error) {
	depth := 0
	cur := wsID
	for {
		w, err := m.db.GetWorkspace(cur)
		if err != nil {
			return depth, err
		}
		if w.ParentKind == meta.ParentVersion {
			return depth, nil
		}
		depth++
		cur = w.ParentID
		if _, maxDepth, _ := m.limits(); depth > maxDepth+2 {
			return depth, errors.New("workspace: 父链存在环")
		}
	}
}

// Get 读取工作区。
func (m *Manager) Get(wsID string) (*meta.Workspace, error) {
	w, err := m.db.GetWorkspace(wsID)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, wsID)
		}
		return nil, err
	}
	return w, nil
}

// List 返回全部工作区。
func (m *Manager) List() ([]meta.Workspace, error) { return m.db.ListWorkspaces() }

// Dir 返回工作区目录。
func (m *Manager) Dir(wsID string) string { return m.mm.WorkspaceDir(wsID) }

// flattenTreeIDs 返回基准树的扁平视图（仓库相对路径 -> 对象 id，不含目录）。
//
// 只读 Tree 对象，不读文件内容，因此成本与目录数相关而与文件大小无关。
func (m *Manager) flattenTreeIDs(root hash.ID) (map[string]hash.ID, error) {
	out := make(map[string]hash.ID, 1024)
	var walk func(dir string, t *tree.Tree) error
	walk = func(dir string, t *tree.Tree) error {
		for _, e := range t.Entries {
			rel := e.Name
			if dir != "" {
				rel = dir + "/" + e.Name
			}
			if e.Kind == codec.EntryTree {
				sub, err := m.store.GetTree(e.ID)
				if err != nil {
					return err
				}
				if err := walk(rel, sub); err != nil {
					return err
				}
				continue
			}
			out[rel] = e.ID
		}
		return nil
	}
	t, err := m.store.GetTree(root)
	if err != nil {
		return nil, err
	}
	if err := walk("", t); err != nil {
		return nil, err
	}
	return out, nil
}

// Sync 重新采集：把 op-log 重建为 diff(base_tree, 当前磁盘)（§4.1、§5.4）。
//
// 因为 op-log 的定义就是「base 与磁盘之差」，每次采集都整体重建，而不是追加。
func (m *Manager) Sync(wsID string) error {
	w, err := m.Get(wsID)
	if err != nil {
		return err
	}
	if w.State == meta.StateRebasing || w.State == meta.StateIntegrating {
		return fmt.Errorf("%w: 状态 %s 期间拒绝采集", ErrWrongState, w.State)
	}
	if !dirExists(m.mm.WorkspaceDir(wsID)) {
		if w.State != meta.StateBroken {
			_ = m.db.UpdateWorkspaceState(wsID, meta.StateBroken, m.now())
		}
		return fmt.Errorf("%w: %s", ErrDirMissing, wsID)
	}
	// 应当挂载却没挂载 -> 目录是空的，绝不能当成「文件被删光」。
	// （机器重启、daemon 被强杀、人为 umount 都会造成这种情况。）
	if w.Materialize == meta.MaterializeOverlay && !m.mm.IsMounted(wsID) {
		if w.State != meta.StateBroken {
			_ = m.db.UpdateWorkspaceState(wsID, meta.StateBroken, m.now())
		}
		return fmt.Errorf("%w: %s（应为 overlay 挂载但实际未挂载；可执行 ws_rebase 或重建工作区）",
			ErrNotMounted, wsID)
	}

	// 1. 构建当前树。
	//
	// 传入基准树的扁平视图与物化时刻：文件若不比物化时刻更新，就是基准树里
	// 那一份，可直接复用其对象 id（不读、不哈希）。
	copt := m.opt.Capture
	copt.MaterializedNS = w.MaterializedTS
	// §5.2 稳定点判定：把被跳过的文件记下来，供 ws_sync 回报。
	var skipped []string
	if copt.StableRetries > 0 {
		copt.OnUnstable = func(p string) { skipped = append(skipped, p) }
	}
	if copt.MaterializedNS > 0 {
		ids, err := m.flattenTreeIDs(w.BaseTree)
		if err != nil {
			return err
		}
		copt.BaseEntries = ids
	}
	stat := m.db.StatCache()
	nowTree, err := capture.Build(wsID, m.mm.WorkspaceDir(wsID), m.opt.Ignore, m.store, stat,
		capture.InodeReliable(m.opt.InodeReliable), copt)
	m.setLastUnstable(wsID, skipped)
	if err != nil {
		return err
	}

	// 2. 与基准树求差。
	baseTree, err := m.store.GetTree(w.BaseTree)
	if err != nil {
		return err
	}
	ops, err := capture.Diff(wsID, baseTree, nowTree, m.store, m.opt.Capture)
	if err != nil {
		return err
	}

	// 3. 整体替换 op-log。
	if err := m.db.DeleteOpsUnderRoot(wsID); err != nil {
		return err
	}
	ts := m.now()
	for i := range ops {
		ops[i].CapturedTS = ts
		ops[i].CapturedBy = "daemon"
	}
	if err := m.db.InsertOps(ops); err != nil {
		return err
	}
	if w.State == meta.StateBroken {
		_ = m.db.UpdateWorkspaceState(wsID, meta.StateActive, ts)
	}
	return nil
}

// LastUnstable 返回某工作区最近一次采集里被稳定点判定跳过的路径（§5.2）。
func (m *Manager) LastUnstable(wsID string) []string {
	m.unstableMu.Lock()
	defer m.unstableMu.Unlock()
	return m.lastUnstable[wsID]
}

func (m *Manager) setLastUnstable(wsID string, skipped []string) {
	m.unstableMu.Lock()
	defer m.unstableMu.Unlock()
	if m.lastUnstable == nil {
		m.lastUnstable = map[string][]string{}
	}
	if len(skipped) == 0 {
		delete(m.lastUnstable, wsID)
		return
	}
	m.lastUnstable[wsID] = skipped
}

// RemountAll 在 daemon 启动时恢复全部活跃工作区的挂载（§7.7 步骤 3）。
//
// 只恢复挂载，不动 upper 层，因此工作区内容不丢。返回恢复的个数。
func (m *Manager) RemountAll() (int, error) {
	all, err := m.db.ListWorkspaces()
	if err != nil {
		return 0, err
	}
	n := 0
	var firstErr error
	for i := range all {
		w := &all[i]
		if w.Materialize != meta.MaterializeOverlay {
			continue
		}
		switch w.State {
		case meta.StateActive, meta.StateConflicted, meta.StateBroken:
		default:
			continue // integrating 等中间态交给 ReconcileLand 处理
		}
		if m.mm.IsMounted(w.ID) {
			_ = m.db.UpdateWorkspaceMount(w.ID, w.Materialize, true, m.now(), w.MaterializedTS)
			continue
		}
		if err := m.mm.Remount(w.ID, w.BaseTree, m.store); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("重挂 %s: %w", w.ID, err)
			}
			continue
		}
		// 恢复后刷新物化时刻：期间没被改过的文件仍可走惰性判定。
		_ = m.db.UpdateWorkspaceMount(w.ID, w.Materialize, true, m.now(), time.Now().UnixNano())
		if w.State == meta.StateBroken {
			_ = m.db.UpdateWorkspaceState(w.ID, meta.StateActive, m.now())
		}
		n++
	}
	return n, firstErr
}

// EffectiveTree 计算并缓存父工作区的有效树（§4.9.1）。
func (m *Manager) EffectiveTree(wsID string) (hash.ID, error) {
	w, err := m.Get(wsID)
	if err != nil {
		return hash.ID{}, err
	}
	ops, err := m.db.ListOps(wsID)
	if err != nil {
		return hash.ID{}, err
	}
	epoch := int64(len(ops))

	if cached, ok, err := m.db.GetEffectiveTree(wsID, epoch); err != nil {
		return hash.ID{}, err
	} else if ok {
		return cached, nil
	}

	base, err := m.store.GetTree(w.BaseTree)
	if err != nil {
		return hash.ID{}, err
	}
	res, err := op.Replay(base, ops, m.store, op.Options{
		WsID: wsID, MaxMatches: m.maxMatches(), Resolver: m.db.Resolver(),
	})
	if err != nil {
		return hash.ID{}, err
	}
	if len(res.Conflicts) > 0 {
		return hash.ID{}, fmt.Errorf("workspace: 有效树重放出现 %d 个冲突", len(res.Conflicts))
	}
	id := res.Tree.ID()
	if err := m.db.PutEffectiveTree(wsID, epoch, id); err != nil {
		return hash.ID{}, err
	}
	return id, nil
}

// Status 是 ws_info 的返回（字段名遵循附录 K 的 snake_case 约定）。
type Status struct {
	ID            string  `json:"ws_id"`
	ParentKind    string  `json:"parent_kind"`
	ParentID      string  `json:"parent_id"`
	BaseVersion   hash.ID `json:"base_version"`
	Dir           string  `json:"dir"`
	Dirty         bool    `json:"dirty"`
	CapturedOps   int64   `json:"captured_ops"`
	ConflictCount int64   `json:"conflict_count"`
	State         string  `json:"state"`
	Materialize   string  `json:"materialize"`
}

// Info 返回工作区状态。
func (m *Manager) Info(wsID string) (*Status, error) {
	w, err := m.Get(wsID)
	if err != nil {
		return nil, err
	}
	ops, err := m.db.CountOps(wsID)
	if err != nil {
		return nil, err
	}
	conf, err := m.db.CountOpenConflicts(wsID)
	if err != nil {
		return nil, err
	}
	return &Status{
		ID:            w.ID,
		ParentKind:    w.ParentKind,
		ParentID:      w.ParentID,
		BaseVersion:   w.BaseVersion,
		Dir:           m.mm.WorkspaceDir(wsID),
		Dirty:         ops > 0,
		CapturedOps:   ops,
		ConflictCount: conf,
		State:         w.State,
		Materialize:   w.Materialize,
	}, nil
}

// Drop 放弃工作区（§11.4）。
func (m *Manager) Drop(wsID string, cascade, keepDir bool) (dropped, reparented []string, err error) {
	w, err := m.Get(wsID)
	if err != nil {
		return nil, nil, err
	}
	kids, err := m.db.Children(wsID)
	if err != nil {
		return nil, nil, err
	}
	if len(kids) > 0 && !cascade {
		return nil, nil, fmt.Errorf("%w: %s 有 %d 个子工作区", ErrHasChildren, wsID, len(kids))
	}

	// 子重挂到祖父（§4.4）。
	for _, k := range kids {
		kind, id := w.ParentKind, w.ParentID
		if err := m.db.UpdateWorkspaceParent(k.ID, kind, id, m.now()); err != nil {
			return nil, nil, err
		}
		reparented = append(reparented, k.ID)
	}

	overlay := w.Materialize == meta.MaterializeOverlay
	if err := m.mm.Close(wsID, keepDir, overlay); err != nil {
		return nil, nil, err
	}
	if err := m.db.DeleteWorkspace(wsID); err != nil {
		return nil, nil, err
	}
	return []string{wsID}, reparented, nil
}

// FoldSet 返回 land 应折叠的工作区集合（§7.1）：从 ws 向上直到最近的 version 祖先。
func (m *Manager) FoldSet(wsID string) ([]*meta.Workspace, error) {
	var out []*meta.Workspace
	cur := wsID
	seen := map[string]bool{}
	for {
		if seen[cur] {
			return nil, errors.New("workspace: 父链存在环")
		}
		seen[cur] = true

		w, err := m.db.GetWorkspace(cur)
		if err != nil {
			if errors.Is(err, meta.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, cur)
			}
			return nil, err
		}
		out = append(out, w)
		if w.ParentKind == meta.ParentVersion {
			break
		}
		cur = w.ParentID
	}
	// 反转成 [根…ws]。
	//
	// 注意：这里**只**做反转。此前还有一句
	//   sort.SliceStable(out, func(i, j int) bool { return i > j })
	// 它比较的是**下标**而不是元素，虽然效果也是反转，但紧接着又被下面的
	// 显式反转抵消，最终返回的顺序是 [叶子…根] —— 折叠集合顺序反了。
	// 后果：CollectOps 先拼叶子的 op 再拼父的，而叶子的 op 是**建立在父的
	// 改动之上**算出来的，重放顺序颠倒；且 `leaf := fold[len(fold)-1]`
	// 取到的是根，版本归属也会记到错误的工作区上。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CollectOps 按 §7.1 的顺序拼接折叠集合的全部 op。
func (m *Manager) CollectOps(fold []*meta.Workspace) ([]op.Op, error) {
	var all []op.Op
	for _, w := range fold {
		ops, err := m.db.ListOps(w.ID)
		if err != nil {
			return nil, err
		}
		all = append(all, ops...)
	}
	return all, nil
}

// --- 辅助 ---------------------------------------------------------------

func newID(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// RebaseResult 是 ws_rebase 的结果（§4.9.2）。
type RebaseResult struct {
	Rebased   bool     `json:"rebased"`
	Conflicts []string `json:"conflicts"`
}

// Rebase 把工作区对齐到新的基准（§4.9.2）。
//
// 步骤：算新基准 → 取出自己的工作 → 重放到新基准 → 覆写工作区 → 更新 base_tree。
// 第 4 步失败时**完全不改状态**（原子性）。
func (m *Manager) Rebase(wsID string) (*RebaseResult, error) {
	w, err := m.Get(wsID)
	if err != nil {
		return nil, err
	}
	if w.State != meta.StateActive && w.State != meta.StateConflicted {
		return nil, fmt.Errorf("%w: 状态 %s", ErrWrongState, w.State)
	}
	if err := m.Sync(wsID); err != nil {
		return nil, err
	}

	// 算新基准。
	newKind, newID, newBase := w.ParentKind, w.ParentID, hash.ID{}
	switch w.ParentKind {
	case meta.ParentVersion:
		v, err := m.db.GetVersion(w.BaseVersion)
		if err != nil {
			return nil, err
		}
		newBase = v.Tree
		if head, ok, _ := m.db.Head(); ok && head != w.BaseVersion {
			hv, err := m.db.GetVersion(head)
			if err == nil {
				newKind, newID, newBase = meta.ParentVersion, head.String(), hv.Tree
			}
		}
	case meta.ParentWorkspace:
		et, err := m.EffectiveTree(w.ParentID)
		if err != nil {
			return nil, err
		}
		newBase = et
	default:
		return nil, fmt.Errorf("%w: 未知父类型 %q", ErrWrongState, w.ParentKind)
	}

	if newBase == w.BaseTree {
		return &RebaseResult{Rebased: false}, nil
	}

	// 自己的工作。
	ops, err := m.db.ListOps(wsID)
	if err != nil {
		return nil, err
	}
	newTree, err := m.store.GetTree(newBase)
	if err != nil {
		return nil, err
	}
	res, err := op.Replay(newTree, ops, m.store, op.Options{
		WsID: wsID, MaxMatches: m.maxMatches(), Resolver: m.db.Resolver(),
	})
	if err != nil {
		return nil, err
	}
	if len(res.Conflicts) > 0 {
		keys := make([]string, 0, len(res.Conflicts))
		for i := range res.Conflicts {
			keys = append(keys, res.Conflicts[i].Key.String())
		}
		// 不改 base、不改磁盘（§4.9.2）。
		_ = m.db.UpdateWorkspaceState(wsID, meta.StateConflicted, m.now())
		return &RebaseResult{Rebased: false, Conflicts: keys}, nil
	}

	// 让磁盘内容等于 res.Tree。
	//
	// overlay：**换 lower 重挂 + 只写差异**（§4.9.2）。这样 upper 里只有真正
	// 改过的文件，而不是整棵树 —— 64 并发 × 10 万文件时这是数量级的差别。
	// copy 模式没有 lower 可换，退回整树覆写。
	if err := m.rebaseWorkspace(wsID, newBase, res.Tree); err != nil {
		return nil, err
	}
	// 重写完成后必须让「惰性 stat 判定」失效。
	//
	// 惰性判定的不变量是「物化完成时磁盘内容 == base_tree」。rebase **打破**了
	// 它：磁盘被写成了 res.Tree，而 base_tree 是新基准，两者在改动过的路径上
	// 不同。若不处理，下一次采集会把这些路径直接判成「未变、等于基准」，于是
	// **agent 的全部改动从 op-log 里消失**（真机上表现为 rebase 之后
	// ws_info 的 captured_ops 变成 0、land 报 NOT_DIRTY）。
	//
	// 两件事一起做：
	//   - 物化时刻置 0 -> 下一次采集走全量读，结果一定正确；
	//   - 清掉本工作区的 stat 缓存 -> 防止「mtime/size 恰好没变」被快路径误判。
	// 那次全量采集之后缓存就重新填好了，后续采集照常走快路径。
	if _, err := m.db.StatCache().Clear(wsID); err != nil {
		return nil, err
	}
	matTS := int64(0)
	if err := m.db.UpdateWorkspaceBase(wsID, newBase, w.BaseVersion, m.now()); err != nil {
		return nil, err
	}
	if err := m.db.UpdateWorkspaceParent(wsID, newKind, newID, m.now()); err != nil {
		return nil, err
	}
	if err := m.db.UpdateWorkspaceMount(wsID, w.Materialize, w.Mounted, m.now(), matTS); err != nil {
		return nil, err
	}
	if err := m.Sync(wsID); err != nil {
		return nil, err
	}
	_ = m.db.UpdateWorkspaceState(wsID, meta.StateActive, m.now())
	return &RebaseResult{Rebased: true}, nil
}

// rebaseWorkspace 让工作区磁盘内容等于 target，同时把 lower 换成 newBase。
//
// overlay 时走「换 lower 重挂 + 只写差异」；其余情况退回整树覆写。
func (m *Manager) rebaseWorkspace(wsID string, newBase hash.ID, target *tree.Tree) error {
	if m.mm.IsMounted(wsID) {
		if err := m.mm.Relower(wsID, newBase, m.store); err != nil {
			// 换 lower 失败（例如内核不允许）：退化为整树覆写，宁可慢不能错。
			return m.overwriteWorkspace(wsID, target)
		}
		return m.applyDelta(wsID, newBase, target)
	}
	return m.overwriteWorkspace(wsID, target)
}

// flatEntry 是树的扁平视图里的一项。
type flatEntry struct {
	kind uint8
	mode uint64
	id   hash.ID
}

// flattenEntries 把一棵树压成 路径 -> {kind, mode, id}。
func (m *Manager) flattenEntries(root hash.ID) (map[string]flatEntry, error) {
	out := map[string]flatEntry{}
	var walk func(id hash.ID, prefix string) error
	walk = func(id hash.ID, prefix string) error {
		t, err := m.store.GetTree(id)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			out[p] = flatEntry{kind: e.Kind, mode: e.Mode, id: e.ID}
			if e.Kind == codec.EntryTree {
				if err := walk(e.ID, p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// applyDelta 只把「新基准 -> target」的差异写进工作区。
//
// 前置条件：工作区已经以 newBase 为 lower 挂载，且 upper 里保存着 agent 的改动。
// 于是「两边都没变的文件」根本不用碰 —— 它们从新 lower 直接读。
//
// 两个方向都要处理：
//   - target 有、基准没有或不同 -> 写入（走 safefs，绝不穿过已有符号链接）
//   - 基准有、target 没有       -> 删除（overlayfs 会自动在 upper 落白洞）
func (m *Manager) applyDelta(wsID string, baseID hash.ID, target *tree.Tree) error {
	dir := m.mm.WorkspaceDir(wsID)
	base, err := m.flattenEntries(baseID)
	if err != nil {
		return err
	}
	want := map[string]flatEntry{}
	if err := m.collectEntries(target, "", want); err != nil {
		return err
	}

	// 1. 删除基准里有、目标里没有的路径。
	for p := range base {
		if _, ok := want[p]; ok {
			continue
		}
		if _, err := codec.ValidatePath(p); err != nil {
			continue
		}
		if err := safefs.RemoveUnder(dir, p); err != nil {
			return err
		}
	}

	// 2. 写入有差异的路径（未变的子树整棵跳过）。
	return m.writeDeltaRel(dir, "", target, base)
}

// collectEntries 递归收集树里全部条目的扁平视图（含目录）。
func (m *Manager) collectEntries(t *tree.Tree, prefix string, out map[string]flatEntry) error {
	for _, e := range t.Entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		out[p] = flatEntry{kind: e.Kind, mode: e.Mode, id: e.ID}
		if e.Kind == codec.EntryTree {
			sub, err := m.store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := m.collectEntries(sub, p, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeDeltaRel 递归写入 target 中与 base 不同的条目。
func (m *Manager) writeDeltaRel(root, prefix string, t *tree.Tree, base map[string]flatEntry) error {
	for _, e := range t.Entries {
		rel := e.Name
		if prefix != "" {
			rel = prefix + "/" + e.Name
		}
		if b, ok := base[rel]; ok && b.kind == e.Kind && b.id == e.ID && b.mode == e.Mode {
			continue // 与基准完全一致 -> 从 lower 读，不落 upper
		}
		switch e.Kind {
		case codec.EntryTree:
			if err := safefs.EnsureDirUnder(root, rel); err != nil {
				return err
			}
			sub, err := m.store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := m.writeDeltaRel(root, rel, sub, base); err != nil {
				return err
			}
		case codec.EntrySymlink:
			data, err := m.store.ReadBlob(e.ID)
			if err != nil {
				return err
			}
			if err := safefs.SymlinkUnder(root, rel, string(data)); err != nil {
				return err
			}
		default:
			data, err := m.store.ReadFileData(e.ID)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if e.Mode == codec.ModeExecutable {
				mode = 0o755
			}
			if err := safefs.WriteFileUnder(root, rel, data, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// overwriteWorkspace 让工作区磁盘内容等于目标树（保留未跟踪文件不动）。
//
// 安全性：所有写入都经 safefs，绝不穿过工作区内已存在的符号链接（见 A2）。
func (m *Manager) overwriteWorkspace(wsID string, target *tree.Tree) error {
	dir := m.mm.WorkspaceDir(wsID)
	// 计算当前磁盘树的路径集合，删掉目标树中不存在的跟踪路径。
	cur, err := capture.Build(wsID, dir, m.opt.Ignore, m.store, m.db.StatCache(),
		capture.InodeReliable(m.opt.InodeReliable), m.opt.Capture)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	if err := m.collectPaths(target, "", want); err != nil {
		return err
	}
	have := map[string]bool{}
	if err := m.collectPaths(cur, "", have); err != nil {
		return err
	}
	for p := range have {
		if want[p] {
			continue
		}
		if _, err := codec.ValidatePath(p); err != nil {
			continue
		}
		if err := safefs.RemoveUnder(dir, p); err != nil {
			return err
		}
	}
	// 写目标树内容。
	return writeTreeInto(dir, target, m.store)
}

// collectPaths 递归收集树中全部**非目录**条目的相对路径。
//
// 注意：必须递归。早期版本只收集顶层，导致 ws_rebase 不会删除嵌套的旧文件
// （见 B1 与 TestRebaseDeletesNestedPaths）。
func (m *Manager) collectPaths(t *tree.Tree, prefix string, out map[string]bool) error {
	for _, e := range t.Entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Kind == codec.EntryTree {
			sub, err := m.store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := m.collectPaths(sub, p, out); err != nil {
				return err
			}
			continue
		}
		out[p] = true
	}
	return nil
}

// writeTreeInto 把树物化到 root，逐条目使用 safefs 原语。
//
// 与 mountmgr 中的同名逻辑保持同一安全约定：目标树说「这里是目录/文件/符号
// 链接」，工作区里若已有类型不符的条目（尤其符号链接），一律先删除再建。
func writeTreeInto(root string, t *tree.Tree, cs treeContentSource) error {
	return writeTreeRel(root, "", t, cs)
}

// treeContentSource 是物化所需的最小读取接口。
type treeContentSource interface {
	GetTree(id hash.ID) (*tree.Tree, error)
	ReadFileData(id hash.ID) ([]byte, error)
	// ReadBlob 读取符号链接的目标字节（符号链接条目存的是 Blob）。
	ReadBlob(id hash.ID) ([]byte, error)
}

func writeTreeRel(root, prefix string, t *tree.Tree, cs treeContentSource) error {
	for _, e := range t.Entries {
		rel := e.Name
		if prefix != "" {
			rel = prefix + "/" + e.Name
		}
		switch e.Kind {
		case codec.EntryTree:
			sub, err := cs.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := safefs.EnsureDirUnder(root, rel); err != nil {
				return err
			}
			if err := writeTreeRel(root, rel, sub, cs); err != nil {
				return err
			}
		case codec.EntrySymlink:
			data, err := cs.ReadBlob(e.ID)
			if err != nil {
				return err
			}
			if err := safefs.SymlinkUnder(root, rel, string(data)); err != nil {
				return err
			}
		default:
			data, err := cs.ReadFileData(e.ID)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if e.Mode == codec.ModeExecutable {
				mode = 0o755
			}
			if err := safefs.WriteFileUnder(root, rel, data, mode); err != nil {
				return err
			}
		}
	}
	return nil
}
