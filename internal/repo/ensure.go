package repo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repolock"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

// InitMode 是 repo_init / p37 init 的初始化模式。
type InitMode string

const (
	// InitEmpty 只允许空目录（排除 p37/lost+found/workspaces），创建空 v0。
	InitEmpty InitMode = "empty"
	// InitImport 把目录中的用户文件作为 v0 导入。
	InitImport InitMode = "import"
	// InitRepair 修复已有半初始化仓库；不删除用户文件。
	InitRepair InitMode = "repair"
)

// EnsureOptions 是一次仓库初始化/修复的参数。
type EnsureOptions struct {
	Mode        InitMode
	DefaultLine string
	// Name/RepoID 是 repo.json 的身份。Name 为空时用目录 basename；
	// RepoID 为空时生成 repo_<32hex>。
	Name           string
	RepoID         string
	IdempotencyKey string
	Force          bool

	// RepoOptions 提供 capture/ignore/epoch 等参数。
	RepoOptions Options
	// WriteConfig 在 rename 之前把 config.toml 写进控制目录（通常是
	// .p37.tmp）。为空时写默认配置（由调用方决定是否注入）。
	WriteConfig func(controlDir string) error

	// MountProbe 用于生产环境判断 path 是否为持久挂载卷。
	MountProbe func(string) bool
	// EnforcePersistent 为真且 MountProbe 判定 false 时拒绝初始化。
	EnforcePersistent bool
}

// EnsureResult 是初始化/修复结果。字段名直接对齐 MCP 返回。
type EnsureResult struct {
	Repo struct {
		Name         string `json:"name"`
		Path         string `json:"path"`
		DefaultLine  string `json:"default_line"`
		MainlineHead string `json:"mainline_head"`
		VersionCount int64  `json:"version_count"`
	} `json:"repo"`
	Line struct {
		Name string `json:"name"`
		Head string `json:"head"`
	} `json:"line"`
	Version struct {
		ID string `json:"id"`
	} `json:"version"`

	// 附加观测字段（CLI/日志用）。
	Created  bool     `json:"created,omitempty"`
	Repaired bool     `json:"repaired,omitempty"`
	Imported bool     `json:"imported,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// PathError 表示 path 不满足初始化前置条件。
type PathError struct {
	Path   string
	Reason string // outside_root | not_mounted | symlink_escape | not_persistent
}

func (e *PathError) Error() string {
	return fmt.Sprintf("repo: 初始化路径不可用: %s (%s)", e.Path, e.Reason)
}

// NotEmptyError 表示 mode=empty 时目录里存在用户文件。
type NotEmptyError struct {
	Path       string
	UserFiles  int
	NextAction string
}

func (e *NotEmptyError) Error() string {
	return fmt.Sprintf("repo: 目录非空（%d 个用户文件）: %s", e.UserFiles, e.Path)
}

func (e *NotEmptyError) Unwrap() error { return ErrRepoNotEmpty }

// HistoryNotEmptyError 表示仓库已有后续版本或非默认 line，不能覆盖 v0。
type HistoryNotEmptyError struct {
	Path         string
	VersionCount int64
	Lines        []string
}

func (e *HistoryNotEmptyError) Error() string {
	return fmt.Sprintf("repo: 仓库已有历史版本或命名 line，不能覆盖 v0: %s", e.Path)
}

// ErrRepoHistoryNotEmpty 是 HistoryNotEmptyError 的 sentinel。
var ErrRepoHistoryNotEmpty = errors.New("repo: 仓库已有历史版本或命名 line")

func (e *HistoryNotEmptyError) Unwrap() error { return ErrRepoHistoryNotEmpty }

// BusyError 表示仓库仍有活跃工作区/租约。
type BusyError struct {
	Workspaces []string `json:"workspaces"`
	Leases     []string `json:"leases"`
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("%v: %d 个工作区, %d 个租约", ErrRepoBusy, len(e.Workspaces), len(e.Leases))
}

func (e *BusyError) Unwrap() error { return ErrRepoBusy }

// InitInProgressError 表示同一个幂等 key 正在被另一个请求处理。
type InitInProgressError struct {
	Key string
}

func (e *InitInProgressError) Error() string {
	return fmt.Sprintf("repo: 幂等键正在处理中: %s", e.Key)
}

// DBCorruptError 表示 p37.db 损坏或结构无法恢复。
type DBCorruptError struct {
	Path       string
	BackupPath string
	Reason     string
}

func (e *DBCorruptError) Error() string {
	reason := e.Reason
	if reason == "control_dir_without_db" {
		reason = "仓库不完整：.p37 存在但 p37.db 缺失（拒绝覆盖现有配置）"
	}
	if e.BackupPath != "" {
		return fmt.Sprintf("repo: 元数据库损坏（%s）: %s；备份 %s", reason, e.Path, e.BackupPath)
	}
	return fmt.Sprintf("repo: 元数据库损坏（%s）: %s", reason, e.Path)
}

// InitBusyError 表示初始化锁/仓库锁被其它进程持有。
type InitBusyError struct {
	Path  string
	What  string
	Cause error
}

func (e *InitBusyError) Error() string {
	return fmt.Sprintf("repo: 初始化被占用（%s）: %s", e.What, e.Path)
}

func (e *InitBusyError) Unwrap() error { return e.Cause }

// Ensure 初始化或修复 root 下的仓库。
//
// 这是 MCP repo_init、CLI p37 init、repo_register(init_if_missing=true)
// 的唯一入口。它的关键不变量：
//   - 新仓库在 .p37.tmp 里完整建好后再 rename，失败不留下半套 .p37；
//   - 已有半成品只 repair（认领唯一 root v0），不会新建第二个根；
//   - mode=empty 只把“用户文件”当成非空，ext4 的 lost+found 不算。
func Ensure(root string, opt EnsureOptions) (*EnsureResult, error) {
	root = filepath.Clean(root)
	if opt.Mode == "" {
		opt.Mode = InitEmpty
	}
	switch opt.Mode {
	case InitEmpty, InitImport, InitRepair:
	default:
		return nil, fmt.Errorf("repo: 非法初始化模式 %q", opt.Mode)
	}
	if opt.DefaultLine == "" {
		opt.DefaultLine = "main"
	}
	if !validLineName(opt.DefaultLine) {
		return nil, fmt.Errorf("repo: 非法默认 line 名 %q", opt.DefaultLine)
	}
	if opt.Name == "" {
		opt.Name = SanitizeName(filepath.Base(root))
	}
	if opt.RepoID == "" {
		id, err := newRepoID()
		if err != nil {
			return nil, err
		}
		opt.RepoID = id
	}
	if !ValidName(opt.Name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRepoName, opt.Name)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	if opt.EnforcePersistent && opt.MountProbe != nil && !opt.MountProbe(root) {
		return nil, &PathError{Path: root, Reason: "not_persistent"}
	}
	if fi, err := os.Lstat(root); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, &PathError{Path: root, Reason: "not_a_directory"}
	}

	// 先拿 root 级 init 锁：它不随 .p37 的 rename 改变 inode。
	lock, err := repolock.Acquire(filepath.Join(root, ".p37.init.lock"), 30*time.Second)
	if err != nil {
		return nil, &InitBusyError{Path: root, What: "init lock", Cause: err}
	}
	defer func() { _ = lock.Release() }()

	// 如果 .p37 已存在，再拿 daemon 用的 .p37/lock，避免 p37d 同时打开。
	var repoLock *repolock.Lock
	p37 := filepath.Join(root, ".p37")
	if dirExists(p37) {
		repoLock, err = repolock.TryExclusive(filepath.Join(p37, "lock"))
		if err != nil {
			return nil, &InitBusyError{Path: root, What: "repo lock", Cause: err}
		}
		defer func() { _ = repoLock.Release() }()
	}

	prev, _ := readInitState(p37)
	req := InitRequest{Path: root, Mode: string(opt.Mode), Name: opt.Name,
		DefaultLine: opt.DefaultLine, Force: opt.Force}
	if opt.IdempotencyKey != "" {
		if rec, ok := prev.lookup(opt.IdempotencyKey); ok && rec.RequestHash != req.Hash() {
			return nil, initConflict(opt.IdempotencyKey, rec, req)
		}
	}

	// 已有完整仓库：幂等返回。先做只读判定，避免 repair 误伤。
	// mode=import 例外：它需要对当前 worktree 与 v0 做一致/替换判断。
	if in, _ := CheckIntegrity(root); in != nil && in.Initialized && opt.Mode != InitImport {
		res, err := ResultFromDisk(root)
		if err != nil {
			return nil, err
		}
		if err := ensureIdentityAndConfig(root, p37, opt); err != nil {
			return nil, err
		}
		// 只 land 到命名 line 时 refs/mainline 可能落后于 DB head；repair
		// 场景显式对齐，避免同步/运维读到旧 ref。
		if !in.RefsMatch && in.MainlineHead != "" {
			if head, perr := hash.Parse(in.MainlineHead); perr == nil {
				if werr := writeRef(p37, head); werr != nil {
					return nil, werr
				}
			}
		}
		istate := initStateFor(opt, root, res.Version.ID, string(opt.Mode), false, false, prev)
		_ = writeInitState(p37, istate)
		return res, nil
	}

	// 残留的 .p37.tmp 只可能是上次崩溃留下的；在锁内清理。
	tmp := filepath.Join(root, ".p37.tmp")
	if dirExists(tmp) {
		if err := os.RemoveAll(tmp); err != nil {
			return nil, err
		}
	}

	hasDB := fileExists(filepath.Join(p37, "p37.db"))
	if !hasDB {
		// .p37 存在但没有 DB：只接受空目录（P36 旧镜像会预建 .p37）。
		if dirExists(p37) && !controlDirTrivial(p37) {
			return nil, &DBCorruptError{Path: root, Reason: "control_dir_without_db"}
		}
		userFiles := CountUserFiles(root)
		if opt.Mode != InitImport && userFiles > 0 {
			return nil, &NotEmptyError{Path: root, UserFiles: userFiles,
				NextAction: "repo_init_import"}
		}
		if opt.Mode == InitRepair {
			// repair 的语义是“已有 DB 但缺 head/line/refs”；没有 DB 不是
			// repair 能解决的问题，交给 repo_init(empty/import)。
			return nil, ErrDBMissing
		}
		return buildAtomic(root, tmp, p37, opt, opt.Mode)
	}

	if opt.Mode == InitImport {
		return importInPlace(root, opt)
	}
	// empty/repair：数据本身完整、只缺 refs/root 时 repair 是安全的；
	// 但如果 v0 为空且有用户文件，repair 会留下“文件在、版本里没有”的状态。
	in, _ := CheckIntegrity(root)
	uf := CountUserFiles(root)
	if in != nil && in.DataOK && !(in.RootTreeEmpty && uf > 0) {
		return repairInPlace(root, opt)
	}
	if uf > 0 {
		return nil, &NotEmptyError{Path: root, UserFiles: uf,
			NextAction: "repo_init_import"}
	}
	return repairInPlace(root, opt)
}

func buildAtomic(root, tmp, p37 string, opt EnsureOptions, mode InitMode) (*EnsureResult, error) {
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()
	for _, d := range []string{"refs", "objects", "ws", "tmp", "overlay", "overlay/bases", "overlay/ws"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := meta.OpenControl(tmp)
	if err != nil {
		return nil, err
	}
	if err := db.Init(root); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initFault("after_db"); err != nil {
		_ = db.Close()
		return nil, err
	}
	bs, err := cas.NewLocalFS(tmp)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())

	var (
		tid     hash.ID
		agent   = "p37-init"
		task    = "init"
		msg     = "init"
		journal *meta.JournalEntry
	)
	if mode == InitImport {
		ig, err := importIgnore(root, opt.RepoOptions)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		copt := opt.RepoOptions.Capture
		if copt.ProbeBytes == 0 {
			copt = capture.DefaultOptions()
		}
		copt.StableRetries = 0
		t, err := capture.Build("", root, ig, store, nil,
			capture.InodeReliable(false), copt)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		tid = t.ID()
		agent = AgentImporter
		task = "import " + root
		msg = "import"
	} else {
		empty := &tree.Tree{}
		tid, err = store.PutTree(empty)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.Flush(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initFault("after_objects"); err != nil {
		_ = db.Close()
		return nil, err
	}
	v := version.New(hash.Zero, tid, 0, agent, task, time.Now().UnixMilli(), msg)
	if mode == InitImport {
		journal = &meta.JournalEntry{
			VersionID: v.ID().String(), Agent: AgentImporter,
			TaskDesc: task, TS: v.TS, OpsJSON: "[]",
		}
	}
	if err := db.Bootstrap(root, v, opt.DefaultLine, journal); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initFault("after_v0"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if err := writeRef(tmp, v.ID()); err != nil {
		return nil, err
	}
	if err := initFault("after_refs"); err != nil {
		return nil, err
	}
	if opt.WriteConfig != nil {
		if err := opt.WriteConfig(tmp); err != nil {
			return nil, err
		}
	}
	m := Meta{ID: opt.RepoID, Name: opt.Name, DefaultLine: opt.DefaultLine}
	if err := WriteMeta(tmp, m); err != nil {
		return nil, err
	}
	st := initStateFor(opt, root, v.ID().String(), string(mode), false, mode == InitImport, nil)
	if err := writeInitState(tmp, st); err != nil {
		return nil, err
	}
	if err := syncControlDir(tmp); err != nil {
		return nil, err
	}
	if err := initFault("before_rename"); err != nil {
		return nil, err
	}
	if dirExists(p37) {
		// POSIX 允许 rename 覆盖空目录；非空目录（半初始化）不应走到这里。
		if !controlDirTrivial(p37) {
			return nil, &DBCorruptError{Path: root, Reason: "control_dir_not_empty"}
		}
		// .p37 里可能有我们刚创建的 lock（fd 仍持有）；RemoveAll 在 Unix
		// 上会 unlink 它但锁继续有效，随后 rename 覆盖目录。
		if err := os.RemoveAll(p37); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, p37); err != nil {
		return nil, err
	}
	committed = true
	if err := initFault("before_parent_fsync"); err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(p37)); err != nil {
		return nil, err
	}
	res, err := ResultFromDisk(root)
	if err != nil {
		return nil, err
	}
	res.Created = true
	res.Imported = mode == InitImport
	return res, nil
}

// importInPlace 在已有 DB 上执行 mode=import 的状态机。
// 注意：这里不调用 repo.Open，避免 EnsureSchema/ReconcileLand 在
// force=false 且内容不同的时候提前改写 line/head（用户要求“不改 DB”）。
func importInPlace(root string, opt EnsureOptions) (*EnsureResult, error) {
	p37 := filepath.Join(root, ".p37")
	db, err := meta.Open(root)
	if err != nil {
		return nil, backupAndCorrupt(root, p37, "db_open_failed")
	}
	if ver, verr := db.UserVersion(); verr == nil {
		if ver > meta.FormatVer {
			_ = db.Close()
			return nil, backupAndCorrupt(root, p37, "format_mismatch")
		}
		if ver > 0 && ver < meta.FormatVer {
			if _, merr := db.MigrateIfNeeded(); merr != nil {
				_ = db.Close()
				return nil, backupAndCorrupt(root, p37, "format_migrate_failed")
			}
		}
	}
	if err := db.Init(root); err != nil {
		_ = db.Close()
		return nil, backupAndCorrupt(root, p37, "db_init_failed")
	}
	bs, err := cas.NewLocalFS(p37)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())
	ig, err := importIgnore(root, opt.RepoOptions)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	copt := opt.RepoOptions.Capture
	if copt.ProbeBytes == 0 {
		copt = capture.DefaultOptions()
	}
	copt.StableRetries = 0
	r := &Repo{Root: root, DB: db, Store: store, Ignore: ig,
		CaptureOpt: copt, redactTaskDesc: opt.RepoOptions.RedactTaskDesc}
	res, ierr := r.ImportV0(root, opt.Force)
	_ = r.Close()
	if ierr != nil {
		return nil, ierr
	}
	prev, _ := readInitState(p37)
	istate := initStateFor(opt, root, res.Version, string(InitImport), res.Repaired, res.Imported, prev)
	if err := writeInitState(p37, istate); err != nil {
		return nil, err
	}
	if err := ensureIdentityAndConfig(root, p37, opt); err != nil {
		return nil, err
	}
	out, err := ResultFromDisk(root)
	if err != nil {
		return nil, err
	}
	out.Repaired = res.Repaired
	out.Imported = res.Imported
	return out, nil
}

func repairInPlace(root string, opt EnsureOptions) (*EnsureResult, error) {
	p37 := filepath.Join(root, ".p37")
	db, err := meta.Open(root)
	if err != nil {
		return nil, backupAndCorrupt(root, p37, "db_open_failed")
	}
	// 未来格式直接拒绝；v1 走与 repo.Open 相同的自动迁移（先备份）。
	if ver, verr := db.UserVersion(); verr == nil {
		if ver > meta.FormatVer {
			_ = db.Close()
			return nil, backupAndCorrupt(root, p37, "format_mismatch")
		}
		if ver > 0 && ver < meta.FormatVer {
			if _, merr := db.MigrateIfNeeded(); merr != nil {
				_ = db.Close()
				return nil, backupAndCorrupt(root, p37, "format_migrate_failed")
			}
		}
	}
	// 半初始化 DB 可能没有 repo 行（只有表）；Init 是幂等的。
	if err := db.Init(root); err != nil {
		_ = db.Close()
		return nil, backupAndCorrupt(root, p37, "db_init_failed")
	}
	st, err := db.RootState()
	if err != nil {
		_ = db.Close()
		return nil, backupAndCorrupt(root, p37, "root_state_failed")
	}
	defaultLine := opt.DefaultLine
	if defaultLine == "" {
		defaultLine = st.Repo.DefaultLine
	}
	if defaultLine == "" {
		defaultLine = "main"
	}
	head, created, err := chooseRepairHead(db, st, root, defaultLine)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !created {
		// 认领已有 v0 时，tree 对象必须存在（空树可以补写）。
		bs, berr := cas.NewLocalFS(p37)
		if berr != nil {
			_ = db.Close()
			return nil, berr
		}
		store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())
		if v, verr := db.GetVersion(head); verr == nil {
			if _, terr := store.GetTree(v.Tree); terr != nil {
				emptyID := (&tree.Tree{}).ID()
				if v.Tree == emptyID {
					if _, perr := store.PutTree(&tree.Tree{}); perr != nil {
						_ = db.Close()
						return nil, perr
					}
				} else {
					_ = db.Close()
					return nil, &DBCorruptError{Path: root, Reason: "head_tree_missing"}
				}
			}
		}
		_ = store.Flush()
	}
	count, err := db.CountReachable(head)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if count <= 0 {
		count = 1
	}
	if err := db.SetRootState(root, defaultLine, head, count, 0); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if err := writeRef(p37, head); err != nil {
		return nil, err
	}
	if err := ensureIdentityAndConfig(root, p37, opt); err != nil {
		return nil, err
	}
	prev, _ := readInitState(p37)
	istate := initStateFor(opt, root, head.String(), string(InitRepair), true, false, prev)
	_ = writeInitState(p37, istate)
	res, err := ResultFromDisk(root)
	if err != nil {
		return nil, err
	}
	res.Repaired = true
	return res, nil
}

// chooseRepairHead 按确定性优先级认领一个 head：
// repo.mainline_head → 默认 line head → 唯一 root → 唯一 leaf。
func chooseRepairHead(db *meta.DB, st *meta.RootState, root, defaultLine string) (hash.ID, bool, error) {
	if !st.Repo.MainlineHead.IsZero() {
		if _, err := db.GetVersion(st.Repo.MainlineHead); err == nil {
			return st.Repo.MainlineHead, false, nil
		}
	}
	for i := range st.Lines {
		if st.Lines[i].Name != defaultLine {
			continue
		}
		if !st.Lines[i].Head.IsZero() {
			if _, err := db.GetVersion(st.Lines[i].Head); err == nil {
				return st.Lines[i].Head, false, nil
			}
		}
	}
	if len(st.Roots) == 1 {
		if _, err := db.GetVersion(st.Roots[0]); err == nil {
			return st.Roots[0], false, nil
		}
	}
	if len(st.Roots) > 1 {
		return hash.ID{}, false, &DBCorruptError{Path: root, Reason: "multiple_root_versions"}
	}
	if len(st.Leaves) == 1 {
		if _, err := db.GetVersion(st.Leaves[0]); err == nil {
			return st.Leaves[0], false, nil
		}
	}
	if st.VersionCount == 0 {
		// 没有任何版本：补一个空 v0。
		p37 := filepath.Join(root, ".p37")
		bs, err := cas.NewLocalFS(p37)
		if err != nil {
			return hash.ID{}, false, err
		}
		store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())
		tid, err := store.PutTree(&tree.Tree{})
		if err != nil {
			return hash.ID{}, false, err
		}
		if err := store.Flush(); err != nil {
			return hash.ID{}, false, err
		}
		v := version.New(hash.Zero, tid, 0, "p37-init", "init", time.Now().UnixMilli(), "init")
		if err := db.Bootstrap(root, v, defaultLine, nil); err != nil {
			return hash.ID{}, false, err
		}
		return v.ID(), true, nil
	}
	return hash.ID{}, false, &DBCorruptError{Path: root, Reason: "no_unique_head"}
}

func ensureIdentityAndConfig(root, controlDir string, opt EnsureOptions) error {
	m, err := ReadMeta(root)
	if err == nil && m != nil {
		if opt.Name != "" && m.Name != opt.Name && !opt.Force {
			return fmt.Errorf("%w: repo.json name=%q, 调用方 name=%q",
				ErrRepoNameMismatch, m.Name, opt.Name)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := WriteMeta(controlDir, Meta{
			ID: opt.RepoID, Name: opt.Name, DefaultLine: opt.DefaultLine,
		}); err != nil {
			return err
		}
	} else {
		return err
	}
	if opt.WriteConfig != nil {
		if _, err := os.Stat(filepath.Join(controlDir, "config.toml")); errors.Is(err, os.ErrNotExist) {
			if err := opt.WriteConfig(controlDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResultFromDisk 读取完整仓库的 repo/line/version 视图（只读）。
func ResultFromDisk(root string) (*EnsureResult, error) {
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	st, err := db.RootState()
	if err != nil {
		return nil, err
	}
	head := st.Repo.MainlineHead
	if head.IsZero() && len(st.Leaves) == 1 {
		head = st.Leaves[0]
	}
	if head.IsZero() {
		return nil, &DBCorruptError{Path: root, Reason: "no_head"}
	}
	def := st.Repo.DefaultLine
	if def == "" {
		def = "main"
	}
	m, _ := ReadMeta(root)
	name := optNameFromMeta(m, root)
	res := &EnsureResult{}
	res.Repo.Name = name
	res.Repo.Path = root
	res.Repo.DefaultLine = def
	res.Repo.MainlineHead = head.String()
	res.Repo.VersionCount = st.Repo.VersionCount
	if res.Repo.VersionCount <= 0 {
		if n, err := db.CountReachable(head); err == nil {
			res.Repo.VersionCount = n
		}
	}
	res.Line.Name = def
	res.Line.Head = head.String()
	res.Version.ID = head.String()
	return res, nil
}

func optNameFromMeta(m *Meta, root string) string {
	if m != nil && m.Name != "" {
		return m.Name
	}
	return filepath.Base(root)
}

func importIgnore(root string, opt Options) (*tree.Ignore, error) {
	extra := append([]string{}, opt.ExtraIgnore...)
	extra = append(extra, "workspaces/", "lost+found/", ".p37.init.lock", ".p37.tmp/", ".p37.lock")
	if lines, err := tree.LoadIgnoreFile(root); err == nil && len(lines) > 0 {
		extra = append(extra, lines...)
	} else if err != nil {
		return nil, err
	}
	return tree.NewIgnore(true, extra)
}

func writeRef(controlDir string, head hash.ID) error {
	bs, err := cas.NewLocalFS(controlDir)
	if err != nil {
		return err
	}
	etag := ""
	if st, err := bs.Stat("refs/mainline"); err == nil {
		etag = st.ETag
	} else if !errors.Is(err, cas.ErrNotExist) {
		return err
	}
	if _, err := bs.PutIfMatch("refs/mainline", etag, []byte(head.String()+"\n")); err != nil {
		return err
	}
	return bs.Flush()
}

// initFault 是测试/验收用的确定性失败注入。
func initFault(step string) error {
	if os.Getenv("P37_INIT_FAULT_AT") == step {
		return fmt.Errorf("repo: injected init fault at %s", step)
	}
	return nil
}

func syncControlDir(controlDir string) error {
	for _, name := range []string{
		"p37.db", "p37.db-wal", "p37.db-shm",
		"config.toml", "repo.json", "init.json", "refs/mainline",
	} {
		p := filepath.Join(controlDir, name)
		if f, err := os.Open(p); err == nil {
			_ = f.Sync()
			_ = f.Close()
		}
	}
	return syncDir(controlDir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	// macOS 上目录 fsync 可能返回 EINVAL；与 cas 的实现保持一致。
	_ = d.Sync()
	return nil
}

func backupAndCorrupt(root, p37, reason string) error {
	backup := ""
	if fileExists(filepath.Join(p37, "p37.db")) {
		name := fmt.Sprintf("p37.db.corrupt-%d", time.Now().UnixMilli())
		backup = filepath.Join(p37, name)
		if err := copyFile(filepath.Join(p37, "p37.db"), backup); err != nil {
			backup = ""
		} else {
			_ = copyFile(filepath.Join(p37, "p37.db-wal"), backup+"-wal")
			_ = copyFile(filepath.Join(p37, "p37.db-shm"), backup+"-shm")
		}
	}
	return &DBCorruptError{Path: root, BackupPath: backup, Reason: reason}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func controlDirTrivial(p37 string) bool {
	des, err := os.ReadDir(p37)
	if err != nil {
		return false
	}
	for _, de := range des {
		switch de.Name() {
		case "tmp":
			// p37 自己的临时目录，内容可丢弃。
			if !de.IsDir() {
				return false
			}
		case "lock":
			if de.IsDir() {
				return false
			}
			if fi, err := de.Info(); err != nil || fi.Size() > 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func newRepoID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "repo_" + hex.EncodeToString(b[:]), nil
}

// ErrInvalidRepoName 表示 repo 名不合法。
var ErrInvalidRepoName = errors.New("repo: 非法 repo 名")

// ErrRepoNameMismatch 表示 repo.json 与请求的 name 不一致。
var ErrRepoNameMismatch = errors.New("repo: repo 名与 repo.json 不一致")

// ErrRepoBusy 表示还有活跃工作区/租约，不能做破坏性替换。
var ErrRepoBusy = errors.New("repo: 仍有活跃工作区或租约")

// ErrRepoNotEmpty 表示仓库已有内容/版本，不能静默覆盖。
var ErrRepoNotEmpty = errors.New("repo: 仓库已有版本")
