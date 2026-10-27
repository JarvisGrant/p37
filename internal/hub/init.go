package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/repolock"
)

// InitOptions 控制 repo_init。
type InitOptions struct {
	Mode           repo.InitMode
	DefaultLine    string
	IdempotencyKey string
	Force          bool
	Rebind         bool
	// AllowNonPersistent 只应由 CLI 的 --allow-nonpersistent 或测试使用；
	// MCP 不暴露这个字段，避免生产调用方绕过持久卷检查。
	AllowNonPersistent bool
}

// IntegrityError 是初始化/注册相关的结构化错误。
type IntegrityError struct {
	Code       string
	Path       string
	Reason     string
	NextAction string
	BackupPath string
	Retryable  bool
	Step       string
}

func (e *IntegrityError) Error() string {
	msg := fmt.Sprintf("%s: %s (%s)", e.Code, e.Path, e.Reason)
	if e.BackupPath != "" {
		msg += "; backup=" + e.BackupPath
	}
	return msg
}

func (e *IntegrityError) Is(target error) bool {
	switch target {
	case ErrRepoNotInitialized:
		return e.Code == "repo_not_initialized"
	case ErrRepoNotFound:
		return e.Code == "repo_not_found"
	}
	return false
}

// InitRepo 在 path 上原子初始化/修复一个仓库，并把它注册进 registry。
func (h *Hub) InitRepo(name, path string, opt InitOptions) (*repo.EnsureResult, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: %q（只允许小写字母数字 . _ -，1-64 字符）", ErrInvalidName, name)
	}
	abs, err := h.validateRepoPath(path, false)
	if err != nil {
		return nil, &repo.PathError{Path: path, Reason: pathReason(err)}
	}
	mode := opt.Mode
	if mode == "" {
		mode = repo.InitEmpty
	}
	def := opt.DefaultLine
	if def == "" {
		def = "main"
	}
	req := repo.InitRequest{Path: abs, Mode: string(mode), Name: name,
		DefaultLine: def, Force: opt.Force}
	claim, err := h.claimInitKey(opt.IdempotencyKey, req)
	if err != nil {
		return nil, err
	}
	if claim.Done {
		if claim.Result == nil {
			return nil, &repo.InitInProgressError{Key: opt.IdempotencyKey}
		}
		return claim.Result, nil
	}
	if !claim.Owner {
		return nil, &repo.InitInProgressError{Key: opt.IdempotencyKey}
	}
	ok := false
	defer func() {
		if !ok && opt.IdempotencyKey != "" {
			h.failInitKey(opt.IdempotencyKey)
		}
	}()
	h.mu.Lock()
	res, err := h.initRepoLocked(name, abs, opt)
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	h.completeInitKey(opt.IdempotencyKey, req.Hash(), res)
	ok = true
	return res, nil
}

// initRepoLocked 要求调用方持有 h.mu。
func (h *Hub) initRepoLocked(name, abs string, opt InitOptions) (*repo.EnsureResult, error) {
	def := opt.DefaultLine
	if def == "" {
		def = "main"
	}
	if opt.Mode == "" {
		opt.Mode = repo.InitEmpty
	}

	// 同名/同 path 的已有行：确定身份与是否需要 rebind。
	var existing *Entry
	if e, _ := h.entryByFieldLocked("name", name, false); e != nil {
		existing = e
		if filepath.Clean(e.Path) != filepath.Clean(abs) && !opt.Rebind {
			return nil, &PathMismatchError{Name: name, ExistingID: e.ID,
				ExistingPath: e.Path, RequestedPath: abs,
				Reason: "use_rebind_or_unregister_then_register"}
		}
	}
	if existing == nil {
		if e, _ := h.entryByFieldLocked("path", abs, false); e != nil {
			existing = e
		}
	}

	effID := ""
	effName := name
	if m, err := repo.ReadMeta(abs); err == nil && m != nil {
		effID = m.ID
		effName = m.Name
		if effName != name && !opt.Force {
			return nil, fmt.Errorf("%w: repo.json name=%q, 调用方 name=%q",
				ErrRepoNameMismatch, effName, name)
		}
		if opt.Force {
			effName = name
		}
		if m.DefaultLine != "" && opt.DefaultLine == "" {
			def = m.DefaultLine
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: 读取 .p37/repo.json 失败: %v", ErrInvalidPath, err)
	}
	if existing != nil {
		if existing.Name != name && !opt.Force {
			return nil, fmt.Errorf("%w: path=%s 已注册为 %q，调用方 name=%q",
				ErrRepoNameMismatch, abs, existing.Name, name)
		}
		if effID != "" && existing.ID != "" && effID != existing.ID {
			return nil, &PathMismatchError{Name: effName, ExistingID: existing.ID,
				ExistingPath: existing.Path, RequestedPath: abs,
				Reason: "target_repo_id_mismatch"}
		}
		if effID == "" {
			effID = existing.ID
		}
		if existing.DefaultLine != "" && opt.DefaultLine == "" {
			def = existing.DefaultLine
		}
	}
	if effID == "" {
		// 复用 path/name 的 tombstone，避免重启后出现两条身份。
		if e, _ := h.entryByFieldLocked("path", abs, true); e != nil {
			effID = e.ID
		} else if e, _ := h.entryByFieldLocked("name", effName, true); e != nil {
			effID = e.ID
		} else {
			id, err := NewID()
			if err != nil {
				return nil, err
			}
			effID = id
		}
	}

	allowNonPersistent := opt.AllowNonPersistent || h.allowNonPersistent
	if !allowNonPersistent && !h.isMountPoint(abs) {
		return nil, &repo.PathError{Path: abs, Reason: "not_persistent"}
	}

	// 已存在且完整：先做 activity/rebind 检查，再幂等返回。
	// mode=import 例外：它要对当前 worktree 与 v0 做一致/替换判断。
	if in, _ := repo.CheckIntegrity(abs); in != nil && in.Initialized {
		if opt.Mode == repo.InitImport {
			if existing != nil && filepath.Clean(existing.Path) != filepath.Clean(abs) {
				if err := h.rebindTargetLocked(existing, abs); err != nil {
					return nil, err
				}
			}
		} else if in.RefsMatch {
			if existing != nil && filepath.Clean(existing.Path) != filepath.Clean(abs) {
				if err := h.rebindTargetLocked(existing, abs); err != nil {
					return nil, err
				}
			}
			// 完整仓库不重新打开/修复（会触发 EnsureSchema 或抢 flock），
			// 只补 repo.json/config 这类不改变数据的身份锚。
			if err := h.ensureMetaAndConfig(abs, effID, effName, def); err != nil {
				return nil, err
			}
			res, err := repo.ResultFromDisk(abs)
			if err != nil {
				return nil, err
			}
			if err := h.upsertEntryLocked(effID, effName, abs, res.Repo.DefaultLine); err != nil {
				return nil, err
			}
			h.invalidateHealth(abs)
			return res, nil
		} else {
			// 数据完整、refs 落后：不要 early-return；下面统一走
			// repo.Ensure(mode=empty/repair) 做 refs-only repair。
			if existing != nil && filepath.Clean(existing.Path) != filepath.Clean(abs) {
				if err := h.rebindTargetLocked(existing, abs); err != nil {
					return nil, err
				}
			}
			if existing != nil && h.repos[existing.ID] != nil {
				if err := h.dropForRepairLocked(existing, abs); err != nil {
					return nil, err
				}
			} else {
				ent := existing
				if ent == nil {
					ent = &Entry{ID: "", Path: abs}
				}
				ws, leases, _, aerr := h.repoActivityAt(ent, abs)
				if aerr != nil {
					return nil, aerr
				}
				if len(ws) > 0 || len(leases) > 0 {
					return nil, &BusyError{Workspaces: ws, Leases: leases}
				}
			}
		}
	}

	// 半初始化仓库如果已被本进程打开，Ensure 会拿不到 .p37/lock。
	// 没有活跃工作区/租约时先卸载并关闭；有活跃项则返回 repo_busy。
	if existing != nil && h.repos[existing.ID] != nil {
		if err := h.dropForRepairLocked(existing, abs); err != nil {
			return nil, err
		}
	}
	res, err := repo.Ensure(abs, repo.EnsureOptions{
		Mode: opt.Mode, DefaultLine: def, Name: effName, RepoID: effID,
		IdempotencyKey: opt.IdempotencyKey, Force: opt.Force,
		RepoOptions: h.cfg.RepoOptions(), WriteConfig: h.writeConfig,
		MountProbe: h.isMountPoint, EnforcePersistent: !allowNonPersistent,
	})
	if err != nil {
		return nil, err
	}
	if err := h.upsertEntryLocked(effID, effName, abs, res.Repo.DefaultLine); err != nil {
		return nil, err
	}
	h.invalidateHealth(abs)
	return res, nil
}

func (h *Hub) getEnrichedLocked(ref string) (*Entry, error) {
	e, err := h.lookupLocked(ref)
	if err != nil {
		return nil, err
	}
	h.enrichEntry(e)
	return e, nil
}

func (h *Hub) writeConfig(controlDir string) error {
	return config.SaveControl(controlDir, h.cfg)
}

func (h *Hub) ensureMetaAndConfig(root, id, name, defaultLine string) error {
	if id == "" {
		if m, err := repo.ReadMeta(root); err == nil && m != nil {
			id = m.ID
		}
	}
	if id == "" {
		var err error
		id, err = NewID()
		if err != nil {
			return err
		}
	}
	if m, err := repo.ReadMeta(root); errors.Is(err, os.ErrNotExist) || m == nil {
		if err := repo.WriteMeta(filepath.Join(root, ".p37"), repo.Meta{
			ID: id, Name: name, DefaultLine: defaultLine,
		}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, ".p37", "config.toml")); errors.Is(err, os.ErrNotExist) {
		if err := h.writeConfig(filepath.Join(root, ".p37")); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) upsertEntryLocked(id, name, path, defaultLine string) error {
	if defaultLine == "" {
		defaultLine = "main"
	}
	now := time.Now().UnixMilli()
	return h.registryTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`DELETE FROM repo WHERE deleted_ts<>0 AND (name=? OR path=?) AND id<>?`,
			name, path, id); err != nil {
			return wrapRegistryErr(err)
		}
		res, err := tx.Exec(
			`UPDATE repo SET name=?, path=?, default_line=?, deleted_ts=0,
			        degraded=0, degraded_msg='' WHERE id=?`,
			name, path, defaultLine, id)
		if err != nil {
			return wrapRegistryErr(err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.Exec(
			`INSERT INTO repo(id,name,path,default_line,created_ts) VALUES(?,?,?,?,?)`,
			id, name, path, defaultLine, now)
		return wrapRegistryErr(err)
	})
}

// Health 返回 path/ref 对应的只读完整性快照。
//
// path 优先于 ref：P36 /ready 在 attach 失败后可能还没有 registry 行，
// 必须能只凭 mountPath 探测。
func (h *Hub) Health(ref, path string, deep bool) (*repo.Integrity, error) {
	var e *Entry
	if ref != "" {
		var err error
		e, err = h.Get(ref)
		if err != nil && path == "" {
			return nil, err
		}
	}
	if path == "" && e != nil {
		path = e.Path
	}
	if path == "" {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, ref)
	}
	abs, err := h.validateRepoPath(path, false)
	if err != nil {
		return nil, &repo.PathError{Path: path, Reason: pathReason(err)}
	}
	// 已打开时直接复用内存连接，避免再开一个只读连接。
	h.mu.Lock()
	var opened *repo.Repo
	if e != nil && opened == nil {
		opened = h.repos[e.ID]
	}
	if opened == nil {
		if r := h.repos[ref]; r != nil {
			opened = r
		}
	}
	h.mu.Unlock()
	if opened != nil && filepath.Clean(opened.Root) == filepath.Clean(abs) {
		in := repo.IntegrityOpen(opened)
		h.cacheHealth(abs, in, deep)
		return in, nil
	}

	if !deep {
		// 只缓存“未初始化”的探测结果；initialized 仓库的 refs_match 必须
		// 实时读，否则 land/merge 后 /ready 会短暂误报。
		if in := h.cachedHealth(abs, false); in != nil && !in.Initialized {
			return in, nil
		}
	}
	var in *repo.Integrity
	if deep {
		in, err = repo.CheckIntegrityDeep(abs)
	} else {
		in, err = repo.CheckIntegrity(abs)
	}
	if err != nil {
		return nil, err
	}
	h.cacheHealth(abs, in, deep)
	return in, nil
}

type healthCacheEntry struct {
	in   *repo.Integrity
	ts   time.Time
	deep bool
}

func (h *Hub) cachedHealth(path string, deep bool) *repo.Integrity {
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	e, ok := h.healthCache[filepath.Clean(path)]
	if !ok || time.Since(e.ts) > time.Second {
		return nil
	}
	if deep && !e.deep {
		return nil
	}
	c := *e.in
	return &c
}

func (h *Hub) cacheHealth(path string, in *repo.Integrity, deep bool) {
	if in == nil {
		return
	}
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	if h.healthCache == nil {
		h.healthCache = map[string]healthCacheEntry{}
	}
	c := *in
	h.healthCache[filepath.Clean(path)] = healthCacheEntry{in: &c, ts: time.Now(), deep: deep}
}

func (h *Hub) invalidateHealth(path string) {
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	delete(h.healthCache, filepath.Clean(path))
}

func pathReason(err error) string {
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "symlink"):
		return "symlink_escape"
	case strings.Contains(low, "不是目录"):
		return "not_a_directory"
	case strings.Contains(low, "不存在"), strings.Contains(low, "no such file"):
		return "missing"
	case strings.Contains(low, "绝对路径"):
		return "outside_root"
	case strings.Contains(low, "不在 root"):
		return "outside_root"
	case strings.Contains(low, "控制目录"):
		return "outside_root"
	default:
		return "outside_root"
	}
}

// dropForRepairLocked 在修复前关闭本进程缓存的 repo。
//
// 有活跃工作区/租约时拒绝；只有隐藏 preview 的挂载时先卸载再关闭。
func (h *Hub) dropForRepairLocked(e *Entry, abs string) error {
	ws, leases, mounted, err := h.repoActivityAt(e, abs)
	if err != nil {
		return err
	}
	if len(ws) > 0 || len(leases) > 0 {
		return &BusyError{Workspaces: ws, Leases: leases}
	}
	if r := h.repos[e.ID]; r != nil {
		for _, w := range mounted {
			if err := r.MM.Unmount(w); err != nil {
				return err
			}
		}
	}
	return h.dropCachedLocked(e)
}

// rebindTargetLocked 在目标路径已完整时把 registry 指针改过去。
func (h *Hub) rebindTargetLocked(e *Entry, abs string) error {
	ws, leases, mounted, err := h.repoActivityAt(e, abs)
	if err != nil {
		return err
	}
	if len(ws) > 0 || len(leases) > 0 {
		return &BusyError{Workspaces: ws, Leases: leases}
	}
	// 目标卷若被其它进程持有 .p37/lock，不能改指针。
	if !stringsEqualClean(e.Path, abs) {
		if r := h.repos[e.ID]; r != nil {
			// 只有打开的 repo 就在目标路径上时，mounted 列表才属于它的 MM。
			if filepath.Clean(r.Root) == filepath.Clean(abs) {
				for _, w := range mounted {
					_ = r.MM.Unmount(w)
				}
			} else if l, lerr := repolockTry(abs); lerr != nil {
				return lerr
			} else {
				_ = l.Release()
			}
			if err := h.dropCachedLocked(e); err != nil {
				return err
			}
		} else if l, lerr := repolockTry(abs); lerr != nil {
			return lerr
		} else {
			_ = l.Release()
		}
	}
	if err := h.rebindPathLocked(e, abs); err != nil {
		return err
	}
	_ = updateRepoRootFile(abs)
	e.Path = abs
	return nil
}

// repoActivityAt 统计目标 repo 的活跃工作区/租约/已挂载工作区。
func (h *Hub) repoActivityAt(e *Entry, abs string) (ws, leases, mounted []string, err error) {
	if r := h.repos[e.ID]; r != nil && filepath.Clean(r.Root) == filepath.Clean(abs) {
		all, gerr := r.WS.List()
		if gerr != nil {
			return nil, nil, nil, gerr
		}
		for i := range all {
			w := &all[i]
			if w.LandedTS == 0 && workspaceActive(w.State) {
				ws = append(ws, w.ID)
			}
			if w.Materialize == meta.MaterializeOverlay && (w.Mounted || r.MM.IsMounted(w.ID)) {
				mounted = append(mounted, w.ID)
			}
		}
		ls, _ := r.DB.ListLeases()
		for _, l := range ls {
			leases = append(leases, l.WsID+":"+l.Path)
		}
		return ws, leases, mounted, nil
	}
	db, derr := meta.OpenReadOnly(abs)
	if derr != nil {
		return nil, nil, nil, derr
	}
	defer func() { _ = db.Close() }()
	all, gerr := db.ListWorkspaces()
	if gerr != nil {
		return nil, nil, nil, gerr
	}
	for i := range all {
		w := &all[i]
		if w.LandedTS == 0 && workspaceActive(w.State) {
			ws = append(ws, w.ID)
		}
		if w.Materialize == meta.MaterializeOverlay && w.Mounted {
			mounted = append(mounted, w.ID)
		}
	}
	ls, _ := db.ListLeases()
	for _, l := range ls {
		leases = append(leases, l.WsID+":"+l.Path)
	}
	return ws, leases, mounted, nil
}

func repolockTry(abs string) (*repolock.Lock, error) {
	path := filepath.Join(abs, ".p37", "lock")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	l, err := repolock.TryExclusive(path)
	if err != nil {
		if errors.Is(err, repolock.ErrBusy) {
			return nil, fmt.Errorf("%w: repo %s 正被其它进程使用", ErrRepoBusy, abs)
		}
		return nil, err
	}
	return l, nil
}

func stringsEqualClean(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
