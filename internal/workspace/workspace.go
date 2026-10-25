// Package workspace 实现工作区的生命周期、采集与有效树（§4）。
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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
	// ErrCleanupFailed 表示工作区元数据已释放、配额已回收，但目录清理失败。
	//
	// 调用方应把它当作**警告**而不是失败：工作区已经不可见、也不再占用
	// max_active；残留目录由孤儿目录清理兜底。
	ErrCleanupFailed = errors.New("workspace: 工作区已释放，但目录清理失败")
	// ErrOpenInProgress 表示同一幂等键/同名工作区正在被另一个调用创建。
	ErrOpenInProgress = errors.New("workspace: 工作区正在创建")
	// ErrIdempotencyConflict 表示同一个幂等键被用于不同的创建请求。
	ErrIdempotencyConflict = errors.New("workspace: 幂等键被用于不同请求")
)

// OpenInProgressError 让调用方拿到可重试的 ws_id。
type OpenInProgressError struct {
	WSID         string
	RetryAfterMS int64
}

func (e *OpenInProgressError) Error() string {
	return fmt.Sprintf("%s: %s", ErrOpenInProgress, e.WSID)
}

func (e *OpenInProgressError) Unwrap() error { return ErrOpenInProgress }

// IdemConflictError 带出已有工作区，便于调用方修正请求。
type IdemConflictError struct {
	Key       string
	WSID      string
	Field     string
	Existing  string
	Requested string
}

func (e *IdemConflictError) Error() string {
	return fmt.Sprintf("%s: key=%s ws=%s %s existing=%q requested=%q",
		ErrIdempotencyConflict, e.Key, e.WSID, e.Field, e.Existing, e.Requested)
}

func (e *IdemConflictError) Unwrap() error { return ErrIdempotencyConflict }

// 上限作用域（quota_exceeded 的 detail.scope）。
const (
	ScopeActive = "workspace_active"
	ScopeHidden = "workspace_hidden"
	ScopeDepth  = "workspace_depth"
	ScopeFanout = "workspace_fanout"
)

// LimitError 是带用量信息的配额错误。
//
// 调用方（P34 等）需要事前做准入限流，所以 quota_exceeded 必须在 detail 里
// 带上 used/limit；仅靠错误文本无法稳定解析。
type LimitError struct {
	Scope string
	Used  int
	Limit int
}

func (e *LimitError) Error() string {
	switch e.Scope {
	case ScopeHidden:
		return fmt.Sprintf("%s: 隐藏工作区已达 %d（used=%d）", ErrLimitExceeded, e.Limit, e.Used)
	case ScopeDepth:
		return fmt.Sprintf("%s: 栈深将达 %d（used=%d）", ErrLimitExceeded, e.Limit, e.Used)
	case ScopeFanout:
		return fmt.Sprintf("%s: 子工作区已达 %d（used=%d）", ErrLimitExceeded, e.Limit, e.Used)
	default:
		return fmt.Sprintf("%s: 活跃工作区已达 %d（used=%d）", ErrLimitExceeded, e.Limit, e.Used)
	}
}

// Unwrap 让 errors.Is(err, ErrLimitExceeded) 继续成立。
func (e *LimitError) Unwrap() error { return ErrLimitExceeded }

// WorkspaceUsage 是 ws_quota 里“谁占着配额”的可观测项。
type WorkspaceUsage struct {
	WSID          string `json:"ws_id"`
	Name          string `json:"name"`
	State         string `json:"state"`
	Dirty         bool   `json:"dirty"`
	Ops           int64  `json:"ops"`
	AgeMS         int64  `json:"age_ms"`
	CreatedTS     int64  `json:"created_ts"`
	UpdatedTS     int64  `json:"updated_ts"`
	Landed        bool   `json:"landed"`
	LandedTS      int64  `json:"landed_ts,omitempty"`
	Hidden        bool   `json:"hidden"`
	Leases        int    `json:"leases"`
	ParentKind    string `json:"parent_kind"`
	ParentID      string `json:"parent_id"`
	BaseLine      string `json:"base_line,omitempty"`
	CountsToQuota bool   `json:"counts_to_quota"`
	NeedsRepair   bool   `json:"needs_repair,omitempty"`
	BrokenReason  string `json:"broken_reason,omitempty"`
}

// Quota 是 ws_quota 的返回（字段名遵循附录 K 的 snake_case 约定）。
type Quota struct {
	MaxActive       int `json:"max_active"`
	ActiveUsed      int `json:"active_used"`
	ActiveAvailable int `json:"active_available"`
	MaxHidden       int `json:"hidden_limit"`
	HiddenUsed      int `json:"hidden_used"`
	MaxDepth        int `json:"max_depth"`
	MaxFanout       int `json:"max_fanout"`
	Landed          int `json:"landed"`
	Broken          int `json:"broken"`
	// Workspaces 让上层能直接告诉用户“是谁占着配额”。
	Workspaces []WorkspaceUsage `json:"workspaces"`
}

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
	// MaxHidden 是隐藏 merge/preview 工作区上限（不占用户 max_active）。
	MaxHidden int
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

// hiddenLimit 返回隐藏 merge/preview 工作区上限。
//
// 0 表示默认 32；允许配置覆盖，但同样受 max_active 的硬上限约束思路影响，
// 这里再钳到 [1, 4*HardMaxActive]，避免误配成天文数字。
func (m *Manager) hiddenLimit() int {
	h := m.opt.MaxHidden
	if h <= 0 {
		return 32
	}
	if h > 4*HardMaxActive {
		return 4 * HardMaxActive
	}
	return h
}

// Quota 返回当前配额用量（供 ws_quota 与准入限流）。
func (m *Manager) Quota() (*Quota, error) {
	active, depth, fanout := m.limits()
	m.openMu.Lock()
	used, err := m.db.CountVisibleWorkspaces()
	if err != nil {
		m.openMu.Unlock()
		return nil, err
	}
	// 把「已预留、尚未落库」的 open 也算进去，避免调用方看到虚假的可用额度。
	used += m.pendingActive
	m.openMu.Unlock()
	hidden, err := m.db.CountHiddenWorkspaces()
	if err != nil {
		return nil, err
	}
	landed, err := m.db.CountLandedWorkspaces()
	if err != nil {
		return nil, err
	}
	broken, err := m.db.CountBrokenWorkspaces()
	if err != nil {
		return nil, err
	}
	avail := active - used
	if avail < 0 {
		avail = 0
	}
	usage, err := m.usage()
	if err != nil {
		return nil, err
	}
	return &Quota{
		MaxActive: active, ActiveUsed: used, ActiveAvailable: avail,
		MaxHidden: m.hiddenLimit(), HiddenUsed: hidden,
		MaxDepth: depth, MaxFanout: fanout,
		Landed: landed, Broken: broken, Workspaces: usage,
	}, nil
}

// usage 返回每个工作区的配额/占用详情。
func (m *Manager) usage() ([]WorkspaceUsage, error) {
	all, err := m.db.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	leases, _ := m.db.ListLeases()
	leaseCount := map[string]int{}
	for _, l := range leases {
		leaseCount[l.WsID]++
	}
	now := m.now()
	out := make([]WorkspaceUsage, 0, len(all))
	for i := range all {
		w := &all[i]
		ops, _ := m.db.CountOps(w.ID)
		state := w.State
		if w.LandedTS != 0 {
			state = "landed"
		}
		needsRepair, brokenReason := m.repairState(w)
		out = append(out, WorkspaceUsage{
			WSID: w.ID, Name: w.Name, State: state, Dirty: ops > 0, Ops: ops,
			AgeMS: now - w.CreatedTS, CreatedTS: w.CreatedTS, UpdatedTS: w.UpdatedTS,
			Landed: w.LandedTS != 0, LandedTS: w.LandedTS, Hidden: w.Hidden,
			Leases: leaseCount[w.ID], ParentKind: w.ParentKind, ParentID: w.ParentID,
			BaseLine: w.BaseLine,
			CountsToQuota: !w.Hidden && w.LandedTS == 0 &&
				(w.State == meta.StateActive || w.State == meta.StateConflicted || w.State == meta.StateRebasing),
			NeedsRepair: needsRepair, BrokenReason: brokenReason,
		})
	}
	return out, nil
}

// repairState 判断工作区是否处于“行在、目录/挂载不对”的需修复状态。
func (m *Manager) repairState(w *meta.Workspace) (bool, string) {
	if w.Hidden || w.Kind == "merge" {
		return false, ""
	}
	if !dirExists(m.mm.WorkspaceDir(w.ID)) {
		return true, "dir_missing"
	}
	if w.Materialize == meta.MaterializeOverlay && !m.mm.IsMounted(w.ID) {
		return true, "overlay_not_mounted"
	}
	if w.State == meta.StateBroken {
		return true, "state_broken"
	}
	return false, ""
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

	// openMu 保护「配额检查 + 名额预留」这一段，避免并发 ws_open 同时看到
	// 最后一个名额（TOCTOU）。物化本身不持锁，以免把 copy 模式的整树复制
	// 串行化。
	openMu        sync.Mutex
	pendingActive int

	// wsLocks 是按 ws_id 的读写锁：Sync/EffectiveTree/Apply/Rebase/Drop 用它
	// 串行化「op-log 与有效树」的一致性（见 locks.go）。
	wsLocks *wsLockSet
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
		wsLocks: newWSLockSet(),
		now:     func() int64 { return time.Now().UnixMilli() }}
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
	// IdempotencyKey 只由 OpenWithKey 使用；直接调用 Open 时忽略。
	IdempotencyKey string
	// ReservedID 只由 Ensure/OpenWithKey 在 claim 后预分配，直接调用时留空。
	ReservedID string
}

// OpenOutcome 是幂等 open/ensure 的返回。
type OpenOutcome struct {
	Workspace *meta.Workspace
	WSID      string
	Created   bool
	Reused    bool
	Replayed  bool
}

// Open 打开一个工作区并物化（§4.1、§4.6、§11.4）。
func (m *Manager) Open(req OpenRequest) (*meta.Workspace, error) {
	// 上限检查 + 名额预留：并发 open 不能同时看到最后一个名额。
	activeLimit, _, _ := m.limits()
	hlimit := m.hiddenLimit()
	if err := m.reserveActive(activeLimit, hlimit); err != nil {
		return nil, err
	}
	reserved := true
	release := func() {
		if reserved {
			m.openMu.Lock()
			m.pendingActive--
			m.openMu.Unlock()
			reserved = false
		}
	}
	defer release()

	parentKind, parentID, baseTree, baseVersion, baseLine, err := m.resolveParent(req.Parent)
	if err != nil {
		return nil, err
	}

	wsID := req.ReservedID
	if wsID == "" {
		wsID, err = newID("ws")
		if err != nil {
			return nil, err
		}
	}
	now := m.now()

	w := meta.Workspace{
		ID:          wsID,
		Name:        req.Name,
		ParentKind:  parentKind,
		ParentID:    parentID,
		BaseTree:    baseTree,
		BaseVersion: baseVersion,
		BaseLine:    baseLine,
		TaskDesc:    req.TaskDesc,
		State:       meta.StateActive,
		Materialize: meta.MaterializeCopy,
		Kind:        "user",
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
	if err := m.insertReserved(w); err != nil {
		_ = m.mm.Close(wsID, false, w.Mounted)
		return nil, err
	}
	// 行已经落库，之后由 CountVisibleWorkspaces 统计；预留名额已在
	// insertReserved 内释放。
	reserved = false
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
			_ = m.mm.Close(wsID, false, w.Mounted)
			_ = m.db.DeleteWorkspace(wsID)
			return nil, err
		}
	}
	return &w, nil
}

// reserveActive 在配额检查通过后预留一个 active 名额。
//
// pendingActive 覆盖「已经通过检查、但行还没落库」的窗口。计数时把 pending
// 加上，宁可短暂保守（少放行一个），也不会让并发 open 超限。
func (m *Manager) reserveActive(activeLimit, hiddenLimit int) error {
	m.openMu.Lock()
	defer m.openMu.Unlock()
	n, err := m.db.CountVisibleWorkspaces()
	if err != nil {
		return err
	}
	if n+m.pendingActive >= activeLimit {
		return &LimitError{Scope: ScopeActive, Used: n + m.pendingActive, Limit: activeLimit}
	}
	hn, err := m.db.CountHiddenWorkspaces()
	if err != nil {
		return err
	}
	if hn >= hiddenLimit {
		return &LimitError{Scope: ScopeHidden, Used: hn, Limit: hiddenLimit}
	}
	m.pendingActive++
	return nil
}

// insertReserved 在同一临界区内插入工作区行并释放预留名额。
//
// 必须原子：如果先插入再单独释放，openMu 之外的 reserveActive 会同时看到
// 「行已存在」和「pending 还未减」，把最后一个名额重复扣掉（保守到并发度
// 退化为 1）。
func (m *Manager) insertReserved(w meta.Workspace) error {
	m.openMu.Lock()
	defer m.openMu.Unlock()
	if err := m.db.InsertWorkspace(w); err != nil {
		return err
	}
	m.pendingActive--
	return nil
}

// resolveParent 把 parent 参数解析为 (kind, id, baseTree, baseVersion)。
//
// parent 为工作区时，先强制采集父（B9），再用其有效树作为基准（D23）。
func (m *Manager) resolveParent(parent string) (string, string, hash.ID, hash.ID, string, error) {
	// 显式 ref 语法。
	if len(parent) > 5 && parent[:5] == "line:" {
		name := parent[5:]
		h, err := m.db.LineHead(name)
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, "", fmt.Errorf("%w: line %s", ErrNotFound, name)
		}
		v, err := m.db.GetVersion(h)
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, "", err
		}
		return meta.ParentVersion, h.String(), v.Tree, h, name, nil
	}
	if len(parent) > 10 && parent[:10] == "workspace:" {
		parent = parent[10:]
	} else if len(parent) > 8 && parent[:8] == "version:" {
		parent = parent[8:]
	}
	if parent == "mainline" {
		parent = ""
	}
	if parent == "" {
		def, err := m.db.DefaultLine()
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, "", err
		}
		head, err := m.db.LineHead(def)
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, "", errors.New("workspace: 默认 line 尚未初始化")
		}
		v, err := m.db.GetVersion(head)
		if err != nil {
			return "", "", hash.ID{}, hash.ID{}, "", err
		}
		return meta.ParentVersion, head.String(), v.Tree, head, def, nil
	}

	// 先当作版本。
	if h, err := hash.Parse(parent); err == nil {
		if v, err := m.db.GetVersion(h); err == nil {
			return meta.ParentVersion, parent, v.Tree, h, "", nil
		}
	} else if isHexPrefix(parent) {
		// 也接受版本 id 的唯一前缀（与 p37 log 的短 id 对齐）。
		if h, err := m.db.FindVersionByPrefix(parent); err == nil {
			if v, err := m.db.GetVersion(h); err == nil {
				return meta.ParentVersion, h.String(), v.Tree, h, "", nil
			}
		}
	}
	// 再当作裸 line 名。
	if h, err := m.db.LineHead(parent); err == nil {
		v, err := m.db.GetVersion(h)
		if err == nil {
			return meta.ParentVersion, h.String(), v.Tree, h, parent, nil
		}
	}

	// 再当作工作区。
	pw, err := m.db.GetWorkspace(parent)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			return "", "", hash.ID{}, hash.ID{}, "", fmt.Errorf("%w: parent %s", ErrNotFound, parent)
		}
		return "", "", hash.ID{}, hash.ID{}, "", err
	}
	if pw.State != meta.StateActive {
		return "", "", hash.ID{}, hash.ID{}, "", fmt.Errorf("%w: 父工作区状态为 %s", ErrWrongState, pw.State)
	}
	if pw.LandedTS != 0 {
		return "", "", hash.ID{}, hash.ID{}, "", fmt.Errorf("%w: 父工作区已落地", ErrWrongState)
	}
	if pw.Hidden || pw.Kind == "merge" {
		return "", "", hash.ID{}, hash.ID{}, "", fmt.Errorf("%w: 不能以隐藏工作区为父", ErrWrongState)
	}

	// B9：父必须先采集，否则子的基准不含父的最新改动。
	// 采集与取有效树在同一把工作区写锁内完成，避免「刚取到旧树，父又被改写」。
	unlockParent := m.wsLocks.lock(pw.ID)
	if err := m.SyncLocked(pw.ID); err != nil {
		unlockParent()
		return "", "", hash.ID{}, hash.ID{}, "", err
	}
	et, err := m.EffectiveTreeLocked(pw.ID)
	unlockParent()
	if err != nil {
		return "", "", hash.ID{}, hash.ID{}, "", err
	}
	// 栈深检查。
	depth, err := m.depthOf(pw.ID)
	if err != nil {
		return "", "", hash.ID{}, hash.ID{}, "", err
	}
	if _, maxDepth, _ := m.limits(); depth+1 >= maxDepth {
		return "", "", hash.ID{}, hash.ID{}, "", &LimitError{Scope: ScopeDepth, Used: depth + 1, Limit: maxDepth}
	}
	kids, err := m.db.Children(pw.ID)
	if err != nil {
		return "", "", hash.ID{}, hash.ID{}, "", err
	}
	// 只数**真正活跃**的子工作区：已落地（landed_ts!=0）与隐藏 merge/preview
	// 不参与 fanout，否则 land_into 的每次冲突 preview 都会永久吃掉一个名额。
	activeKids := 0
	for i := range kids {
		if kids[i].Hidden || kids[i].LandedTS != 0 {
			continue
		}
		activeKids++
	}
	if _, _, maxFanout := m.limits(); activeKids >= maxFanout {
		return "", "", hash.ID{}, hash.ID{}, "", &LimitError{Scope: ScopeFanout, Used: activeKids, Limit: maxFanout}
	}
	return meta.ParentWorkspace, pw.ID, et, pw.BaseVersion, "", nil
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
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	return m.SyncLocked(wsID)
}

// SyncLocked 与 Sync 相同，但调用方必须已经持有 LockWorkspace(wsID)。
func (m *Manager) SyncLocked(wsID string) error { return m.syncLocked(wsID, false) }

// SyncStrict 做一次全量读盘采集：禁用 stat 快路径与惰性基准判定。
//
// land_into 在合并前后使用它，确保「父/子最新改动」不会因为 mtime 恰好
// 没变而被快路径漏判。成本是读一遍全部已跟踪文件，land 频率低，可以接受。
func (m *Manager) SyncStrict(wsID string) error {
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	return m.syncLocked(wsID, true)
}

// SyncStrictLocked 与 SyncStrict 相同，但调用方必须已持有写锁。
func (m *Manager) SyncStrictLocked(wsID string) error { return m.syncLocked(wsID, true) }

func (m *Manager) syncLocked(wsID string, strict bool) error {
	w, err := m.Get(wsID)
	if err != nil {
		return err
	}
	if w.State == meta.StateRebasing || w.State == meta.StateIntegrating {
		return fmt.Errorf("%w: 状态 %s 期间拒绝采集", ErrWrongState, w.State)
	}
	if w.LandedTS != 0 {
		return fmt.Errorf("%w: 工作区已落地（只读）", ErrWrongState)
	}
	if w.Hidden || w.Kind == "merge" {
		return fmt.Errorf("%w: 隐藏 merge 工作区没有可采集目录", ErrWrongState)
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
	if strict {
		// 全量读：绕过 (mtime,size) 与「不比物化时刻新」两条快路径。
		copt.ForceHash = true
		copt.MaterializedNS = 0
	} else {
		copt.MaterializedNS = w.MaterializedTS
	}
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

	// 3. 整体替换 op-log（同一事务内推进 ops_epoch 并清掉有效树缓存）。
	ts := m.now()
	for i := range ops {
		ops[i].CapturedTS = ts
		ops[i].CapturedBy = "daemon"
	}
	if err := m.db.ReplaceOps(wsID, ops); err != nil {
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

// EffectiveTree 计算并缓存工作区的有效树（§4.9.1）。
func (m *Manager) EffectiveTree(wsID string) (hash.ID, error) {
	unlock := m.wsLocks.rlock(wsID)
	defer unlock()
	return m.EffectiveTreeLocked(wsID)
}

// EffectiveTreeLocked 与 EffectiveTree 相同，但调用方必须持有
// LockWorkspace(wsID) 或 RLockWorkspace(wsID)。
func (m *Manager) EffectiveTreeLocked(wsID string) (hash.ID, error) {
	w, err := m.Get(wsID)
	if err != nil {
		return hash.ID{}, err
	}
	// 缓存键必须是单调的 ops_epoch，不能用 len(ops)：同一批路径再改一次时
	// op 条数不变、内容却变了（会话 #90 的回退根因）。
	epoch := w.OpsEpoch

	if cached, ok, err := m.db.GetEffectiveTree(wsID, epoch); err != nil {
		return hash.ID{}, err
	} else if ok {
		return cached, nil
	}

	ops, err := m.db.ListOps(wsID)
	if err != nil {
		return hash.ID{}, err
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
	ID          string  `json:"ws_id"`
	Name        string  `json:"name"`
	ParentKind  string  `json:"parent_kind"`
	ParentID    string  `json:"parent_id"`
	BaseVersion hash.ID `json:"base_version"`
	BaseLine    string  `json:"base_line,omitempty"`
	BaseTree    hash.ID `json:"base_tree"`
	// EffectiveTree 是当前 op-log 对应的有效树（只读计算，不触发采集）。
	// 磁盘上尚未采集的改动不会反映在这里；需要最新值请先 ws_sync。
	EffectiveTree hash.ID `json:"effective_tree,omitempty"`
	OpsEpoch      int64   `json:"ops_epoch"`
	// ParentTree 仅对 ParentKind=workspace 的子工作区有意义：父工作区的
	// 当前有效树。它是 P34 传给 ws_land_into(expected_parent_tree) 的令牌。
	ParentTree    hash.ID `json:"parent_tree,omitempty"`
	ParentTreeErr string  `json:"parent_tree_error,omitempty"`
	Dir           string  `json:"dir"`
	Dirty         bool    `json:"dirty"`
	CapturedOps   int64   `json:"captured_ops"`
	ConflictCount int64   `json:"conflict_count"`
	State         string  `json:"state"`
	Materialize   string  `json:"materialize"`
	Hidden        bool    `json:"hidden,omitempty"`
	Kind          string  `json:"kind,omitempty"`
	LandedTS      int64   `json:"landed_ts,omitempty"`
	BaseStale     bool    `json:"base_stale,omitempty"`
	DiskBytes     int64   `json:"disk_bytes"`
	AgeMS         int64   `json:"age_ms"`
	CreatedTS     int64   `json:"created_ts"`
	UpdatedTS     int64   `json:"updated_ts"`
	LeaseCount    int     `json:"lease_count"`
	NeedsRepair   bool    `json:"needs_repair,omitempty"`
	BrokenReason  string  `json:"broken_reason,omitempty"`
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
	state := w.State
	if w.LandedTS != 0 {
		state = "landed"
	}
	pk, pid := w.ParentKind, w.ParentID
	if w.BaseLine != "" {
		pk, pid = "line", w.BaseLine
	}
	leases, _ := m.db.ListLeases()
	leaseCount := 0
	for _, l := range leases {
		if l.WsID == w.ID {
			leaseCount++
		}
	}
	needsRepair, brokenReason := m.repairState(w)
	now := m.now()
	st := &Status{
		ID:            w.ID,
		Name:          w.Name,
		ParentKind:    pk,
		ParentID:      pid,
		BaseVersion:   w.BaseVersion,
		BaseLine:      w.BaseLine,
		BaseTree:      w.BaseTree,
		OpsEpoch:      w.OpsEpoch,
		Dir:           m.mm.WorkspaceDir(wsID),
		Dirty:         ops > 0,
		CapturedOps:   ops,
		ConflictCount: conf,
		State:         state,
		Materialize:   w.Materialize,
		Hidden:        w.Hidden,
		Kind:          w.Kind,
		LandedTS:      w.LandedTS,
		AgeMS:         now - w.CreatedTS,
		CreatedTS:     w.CreatedTS,
		UpdatedTS:     w.UpdatedTS,
		LeaseCount:    leaseCount,
		NeedsRepair:   needsRepair,
		BrokenReason:  brokenReason,
	}
	if dir := m.mm.WorkspaceDir(wsID); dirExists(dir) {
		_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && info != nil && !info.IsDir() {
				if sys, ok := info.Sys().(*syscall.Stat_t); ok && sys.Blocks > 0 {
					st.DiskBytes += sys.Blocks * 512
				} else {
					st.DiskBytes += info.Size()
				}
			}
			return nil
		})
	}
	if w.BaseLine != "" {
		if h, err := m.db.LineHead(w.BaseLine); err == nil && h != w.BaseVersion {
			st.BaseStale = true
		}
	} else if w.ParentKind == meta.ParentVersion {
		if h, ok, _ := m.db.Head(); ok && h != w.BaseVersion {
			st.BaseStale = true
		}
	}
	return st, nil
}

// FillRevisions 计算并填充有效树 / 父树（只读，不触发采集）。
//
// 单独提供而不是放进 Info：ws_disk_usage / repo_health 会遍历全部工作区，
// 每个都做两次重放是没必要的。只有 ws_info 需要精确 revision。
func (m *Manager) FillRevisions(st *Status) {
	if st == nil {
		return
	}
	if eff, err := m.EffectiveTree(st.ID); err == nil {
		st.EffectiveTree = eff
	}
	if st.ParentKind == meta.ParentWorkspace && st.BaseLine == "" && st.ParentID != "" {
		if eff, err := m.EffectiveTree(st.ParentID); err == nil {
			st.ParentTree = eff
		} else {
			st.ParentTreeErr = err.Error()
		}
	}
}

// DropOptions 让回收路径带上可解释的 reason / 落地目标。
type DropOptions struct {
	Cascade       bool
	KeepDir       bool
	Reason        string // abandoned | expired | unknown | landed_and_dropped
	Why           string
	Line          string
	LineHead      string
	TargetKind    string
	TargetID      string
	LandedVersion string
}

// Drop 放弃工作区（默认 reason=abandoned，兼容旧调用）。
func (m *Manager) Drop(wsID string, cascade, keepDir bool) (dropped, reparented []string, err error) {
	return m.DropWithOptions(wsID, DropOptions{Cascade: cascade, KeepDir: keepDir, Reason: "abandoned"})
}

// DropWithOptions 放弃工作区（§11.4）。
//
// **配额优先**：只要元数据删除成功，max_active 名额立即回收；目录/挂载清理
// 失败只返回 ErrCleanupFailed 警告，不再让工作区行残留。历史实现把 Close
// 放在 DeleteWorkspace 之前并直接 return，overlay umount 遇到 EBUSY（P34 的
// exec 还留着进程/工作目录）时行会永久残留，配额随时间单调衰减。
func (m *Manager) DropWithOptions(wsID string, opt DropOptions) (dropped, reparented []string, err error) {
	cascade, keepDir := opt.Cascade, opt.KeepDir
	w, err := m.Get(wsID)
	if err != nil {
		return nil, nil, err
	}
	// 与 land / rebase / 采集互斥：删除目录与元数据期间不能让合并继续写盘。
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	kids, err := m.db.Children(wsID)
	if err != nil {
		return nil, nil, err
	}
	if len(kids) > 0 && !cascade {
		return nil, nil, fmt.Errorf("%w: %s 有 %d 个子工作区", ErrHasChildren, wsID, len(kids))
	}

	// 子重挂到祖父（§4.4）。重挂失败是真正的错误：不能留下悬挂的 parent_id。
	for _, k := range kids {
		kind, id := w.ParentKind, w.ParentID
		if err := m.db.UpdateWorkspaceParent(k.ID, kind, id, m.now()); err != nil {
			return nil, nil, err
		}
		reparented = append(reparented, k.ID)
	}

	overlay := w.Materialize == meta.MaterializeOverlay
	dir := m.mm.WorkspaceDir(wsID)
	closeErr := m.mm.Close(wsID, keepDir, overlay)

	// 回收记录必须和删行在同一个事务里：崩溃后要么行还在，要么 tombstone 在。
	ops, _ := m.db.CountOps(wsID)
	t := meta.WSTombstone{
		WSID: w.ID, Name: w.Name, ParentKind: w.ParentKind, ParentID: w.ParentID,
		Reason: opt.Reason, Why: opt.Why, StateBefore: w.State,
		DroppedAt: m.now(), CreatedTS: w.CreatedTS,
		LandedVersion: opt.LandedVersion, TargetKind: opt.TargetKind,
		TargetID: opt.TargetID, Line: opt.Line, LineHead: opt.LineHead,
		DirtyOps: ops,
	}
	// 已落地工作区即使调用方传了 abandoned，也应以 landed_and_dropped 解释。
	if landed, ok, _ := m.db.GetLandedWSInfo(wsID); ok {
		t.Reason = "landed_and_dropped"
		if t.LandedVersion == "" {
			t.LandedVersion = landed.VersionID
		}
		if t.TargetKind == "" {
			t.TargetKind = landed.TargetKind
		}
		if t.TargetID == "" {
			t.TargetID = landed.TargetID
		}
		if t.Line == "" && landed.TargetKind == "line" {
			t.Line = landed.TargetID
		}
		if t.LineHead == "" {
			t.LineHead = landed.VersionID
		}
	}
	if t.Reason == "" {
		t.Reason = "abandoned"
	}
	if err := m.db.DeleteWorkspaceWithTombstone(wsID, t); err != nil {
		return nil, nil, err
	}
	// 同名 claim 只在工作区“活着”时有效；回收后释放，下一次 ws_ensure 可以
	// 用同一个 name 创建新一代工作区（旧 ws_id 仍由 tombstone 解释）。
	_ = m.releaseNameClaims(wsID)
	if closeErr != nil {
		return []string{wsID}, reparented,
			fmt.Errorf("%w: %s: %v", ErrCleanupFailed, dir, closeErr)
	}
	return []string{wsID}, reparented, nil
}

// ReapOrphanDirs 删除 workspaces/ 下没有对应数据库行的孤儿目录（启动兜底）。
//
// 行是唯一事实来源：目录存在但行不存在，说明进程在 Land/Open 的中间态崩溃，
// 或早前 Drop 的元数据删除成功而目录清理失败。调用方负责记录日志。
func (m *Manager) ReapOrphanDirs() ([]string, error) {
	all, err := m.db.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(all))
	for i := range all {
		known[all[i].ID] = true
	}
	root := filepath.Join(m.repoRoot, "workspaces")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var reaped []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		// mountmgr.Close 会先尽力卸载（overlay），再删目录与状态文件。
		// 非 ws_* 目录（历史遗留命名）走直接删除。
		var err error
		if mountmgr.ValidWSID(e.Name()) {
			err = m.mm.Close(e.Name(), false, true)
		} else {
			err = os.RemoveAll(filepath.Join(root, e.Name()))
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		reaped = append(reaped, e.Name())
	}
	return reaped, errors.Join(errs...)
}

// ReapStaleHidden 回收超过 ttl 的隐藏 merge/preview 工作区。
//
// land_into 冲突不再创建 hidden preview（D 修复），但老仓库里可能残留。
// 这些行不占 max_active，却会吃满 hidden 上限，必须有过期兜底。
func (m *Manager) ReapStaleHidden(ttl time.Duration) ([]string, error) {
	all, err := m.db.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	cutoff := m.now() - ttl.Milliseconds()
	var reaped []string
	var errs []error
	for i := range all {
		w := &all[i]
		if !w.Hidden || w.UpdatedTS > cutoff {
			continue
		}
		if _, _, err := m.DropWithOptions(w.ID, DropOptions{
			Cascade: true, Reason: "expired", Why: "hidden_ttl",
		}); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		reaped = append(reaped, w.ID)
	}
	return reaped, errors.Join(errs...)
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
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	w, err := m.Get(wsID)
	if err != nil {
		return nil, err
	}
	if w.State != meta.StateActive && w.State != meta.StateConflicted {
		return nil, fmt.Errorf("%w: 状态 %s", ErrWrongState, w.State)
	}
	if err := m.SyncLocked(wsID); err != nil {
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
	// 跟随对侧已落地的重命名（§6.3 rename_vs_edit）。
	var follow map[string]string
	if !w.BaseTree.IsZero() && w.BaseTree != newBase {
		baseTree, err := m.store.GetTree(w.BaseTree)
		if err != nil {
			return nil, err
		}
		if follow, err = op.RenameFollows(m.store, baseTree, newTree); err != nil {
			return nil, err
		}
	}
	res, err := op.Replay(newTree, ops, m.store, op.Options{
		WsID: wsID, MaxMatches: m.maxMatches(), Resolver: m.db.Resolver(), Follow: follow,
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
	if err := m.SyncLocked(wsID); err != nil {
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

// isHexPrefix 报告 s 是否为一段十六进制字符（用于版本 id 前缀）。
func isHexPrefix(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// RebaseTo 把工作区对齐到显式目标（version / line / workspace）。
// ref 为空时退化为 Rebase 的默认行为。
func (m *Manager) RebaseTo(wsID, ref string) (*RebaseResult, error) {
	if ref == "" {
		return m.Rebase(wsID)
	}
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	w, err := m.Get(wsID)
	if err != nil {
		return nil, err
	}
	if w.LandedTS != 0 || w.Hidden {
		return nil, fmt.Errorf("%w: 工作区已落地/隐藏", ErrWrongState)
	}
	if w.State != meta.StateActive && w.State != meta.StateConflicted {
		return nil, fmt.Errorf("%w: 状态 %s", ErrWrongState, w.State)
	}
	if err := m.SyncLocked(wsID); err != nil {
		return nil, err
	}
	kind, id, baseLine, newBase, err := m.resolveRebaseTarget(w, ref)
	if err != nil {
		return nil, err
	}
	if newBase == w.BaseTree {
		return &RebaseResult{Rebased: false}, nil
	}
	ops, err := m.db.ListOps(wsID)
	if err != nil {
		return nil, err
	}
	newTree, err := m.store.GetTree(newBase)
	if err != nil {
		return nil, err
	}
	var follow map[string]string
	if !w.BaseTree.IsZero() && w.BaseTree != newBase {
		baseTree, err := m.store.GetTree(w.BaseTree)
		if err != nil {
			return nil, err
		}
		if follow, err = op.RenameFollows(m.store, baseTree, newTree); err != nil {
			return nil, err
		}
	}
	res, err := op.Replay(newTree, ops, m.store, op.Options{
		WsID: wsID, MaxMatches: m.maxMatches(), Resolver: m.db.Resolver(), Follow: follow,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Conflicts) > 0 {
		keys := make([]string, 0, len(res.Conflicts))
		for i := range res.Conflicts {
			keys = append(keys, res.Conflicts[i].Key.String())
		}
		_ = m.db.UpdateWorkspaceState(wsID, meta.StateConflicted, m.now())
		return &RebaseResult{Rebased: false, Conflicts: keys}, nil
	}
	if err := m.rebaseWorkspace(wsID, newBase, res.Tree); err != nil {
		return nil, err
	}
	if _, err := m.db.StatCache().Clear(wsID); err != nil {
		return nil, err
	}
	bv, _ := hash.Parse(id)
	if err := m.db.UpdateWorkspaceBaseLine(wsID, newBase, bv, baseLine, m.now()); err != nil {
		return nil, err
	}
	if err := m.db.UpdateWorkspaceParent(wsID, kind, id, m.now()); err != nil {
		return nil, err
	}
	matTS := int64(0)
	if err := m.db.UpdateWorkspaceMount(wsID, w.Materialize, w.Mounted, m.now(), matTS); err != nil {
		return nil, err
	}
	if err := m.SyncLocked(wsID); err != nil {
		return nil, err
	}
	_ = m.db.UpdateWorkspaceState(wsID, meta.StateActive, m.now())
	return &RebaseResult{Rebased: true}, nil
}

func (m *Manager) resolveRebaseTarget(w *meta.Workspace, ref string) (string, string, string, hash.ID, error) {
	resolveVersion := func(h hash.ID) (hash.ID, error) {
		v, err := m.db.GetVersion(h)
		if err != nil {
			return hash.ID{}, err
		}
		return v.Tree, nil
	}
	if len(ref) > 5 && ref[:5] == "line:" {
		name := ref[5:]
		h, err := m.db.LineHead(name)
		if err != nil {
			return "", "", "", hash.ID{}, err
		}
		t, err := resolveVersion(h)
		return meta.ParentVersion, h.String(), name, t, err
	}
	if ref == "mainline" {
		def, _ := m.db.DefaultLine()
		h, err := m.db.LineHead(def)
		if err != nil {
			return "", "", "", hash.ID{}, err
		}
		t, err := resolveVersion(h)
		return meta.ParentVersion, h.String(), def, t, err
	}
	if len(ref) > 10 && ref[:10] == "workspace:" {
		ref = ref[10:]
		if _, err := m.Get(ref); err == nil {
			t, err := m.EffectiveTree(ref)
			return meta.ParentWorkspace, ref, "", t, err
		}
	}
	if len(ref) > 8 && ref[:8] == "version:" {
		id, err := hash.Parse(ref[8:])
		if err != nil {
			return "", "", "", hash.ID{}, err
		}
		t, err := resolveVersion(id)
		return meta.ParentVersion, id.String(), "", t, err
	}
	if id, err := hash.Parse(ref); err == nil {
		if t, err := resolveVersion(id); err == nil {
			return meta.ParentVersion, id.String(), "", t, nil
		}
	}
	if h, err := m.db.FindVersionByPrefix(ref); err == nil {
		t, err := resolveVersion(h)
		return meta.ParentVersion, h.String(), "", t, err
	}
	if h, err := m.db.LineHead(ref); err == nil {
		t, err := resolveVersion(h)
		return meta.ParentVersion, h.String(), ref, t, err
	}
	return "", "", "", hash.ID{}, fmt.Errorf("%w: 无法解析 rebase 目标 %q", ErrNotFound, ref)
}

// ApplyTree 把工作区目录与 op-log 更新为目标树（供内部整树重建使用）。
func (m *Manager) ApplyTree(wsID string, target *tree.Tree) error {
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	return m.ApplyTreeLocked(wsID, target)
}

// ApplyTreeLocked 与 ApplyTree 相同，但调用方必须已持有工作区写锁。
func (m *Manager) ApplyTreeLocked(wsID string, target *tree.Tree) error {
	w, err := m.Get(wsID)
	if err != nil {
		return err
	}
	if w.Hidden || w.LandedTS != 0 {
		return fmt.Errorf("%w: 工作区不可写", ErrWrongState)
	}
	if w.Materialize == meta.MaterializeOverlay && !m.mm.IsMounted(wsID) {
		return fmt.Errorf("%w: %s", ErrNotMounted, wsID)
	}
	if err := m.overwriteWorkspace(wsID, target); err != nil {
		return err
	}
	if _, err := m.db.StatCache().Clear(wsID); err != nil {
		return err
	}
	// 让下一次采集走全量读取：磁盘刚被改写，惰性 stat 判定不再成立。
	if err := m.db.UpdateWorkspaceMount(wsID, w.Materialize, w.Mounted, m.now(), 0); err != nil {
		return err
	}
	return m.SyncLocked(wsID)
}

// ApplyTreeDelta 只把 before → target 有差异的路径写进工作区，并重新采集。
//
// 这是 ws_land_into 的落盘入口：父工作区里「子会话没改」的路径完全不会被
// 触碰，因此即使采集漏判或 agent 在合并窗口里直写，也不会被整树重写放大成
// 回退（会话 #90 的第二个根因）。
func (m *Manager) ApplyTreeDelta(wsID string, before hash.ID, target *tree.Tree) error {
	unlock := m.wsLocks.lock(wsID)
	defer unlock()
	return m.ApplyTreeDeltaLocked(wsID, before, target)
}

// ApplyTreeDeltaLocked 与 ApplyTreeDelta 相同，但调用方必须已持有工作区写锁。
func (m *Manager) ApplyTreeDeltaLocked(wsID string, before hash.ID, target *tree.Tree) error {
	w, err := m.Get(wsID)
	if err != nil {
		return err
	}
	if w.Hidden || w.LandedTS != 0 {
		return fmt.Errorf("%w: 工作区不可写", ErrWrongState)
	}
	if w.Materialize == meta.MaterializeOverlay && !m.mm.IsMounted(wsID) {
		return fmt.Errorf("%w: %s", ErrNotMounted, wsID)
	}
	if before.IsZero() {
		return fmt.Errorf("%w: ApplyTreeDelta 需要 before tree", ErrWrongState)
	}
	if err := m.applyDelta(wsID, before, target); err != nil {
		return err
	}
	if _, err := m.db.StatCache().Clear(wsID); err != nil {
		return err
	}
	// 不把 materialized_ts 置 0：只写了差异路径，未变文件仍然等于基准，
	// 惰性判定依旧成立；新写入路径的 mtime 必然大于物化时刻，会被重新读取。
	return m.SyncLocked(wsID)
}

// MarkLanded 标记工作区已落地但保留目录与元数据（drop_workspace=false）。
func (m *Manager) MarkLanded(wsID string) error {
	return m.db.SetWorkspaceLanded(wsID, m.now())
}

// SetHidden 仅供内部 merge/preview 工作区使用。
func (m *Manager) SetHidden(wsID string, hidden bool, kind string) error {
	return m.db.SetWorkspaceHidden(wsID, hidden, kind)
}
