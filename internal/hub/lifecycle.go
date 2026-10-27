package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// RegisterOptions 控制 repo_register 的行为。
type RegisterOptions struct {
	DefaultLine   string
	InitIfMissing bool
	// ForceRename 允许调用方用新 name 覆盖已有 .p37/repo.json 里的 name。
	// 默认 false：repo.json 是仓库身份锚，不能被静默改名。
	ForceRename bool
	// Rebind 允许在“同名已注册到旧路径”时把 registry 指针改到新路径。
	// 只改控制面，不搬数据；目标路径必须已有同一 repo 身份（repo.json 或 DB）。
	Rebind bool
	// IdempotencyKey 只在 init_if_missing=true 且确实触发初始化时生效；
	// strict register 忽略它。
	IdempotencyKey string
}

// UnregisterResult 是 repo_unregister 的结果。
type UnregisterResult struct {
	Entry   *Entry
	Already bool
}

// ScanOptions 控制 repo_scan。
type ScanOptions struct {
	Reconcile    bool
	PruneMissing bool
	DryRun       bool
	// Repair 为真时对检测到的半初始化仓库执行 InitRepoAt(mode=repair)。
	// 默认 false：daemon 启动 scan 必须保持非破坏性。
	Repair bool
}

// ScanIssue 描述一个检测到但未修复的初始化问题。
type ScanIssue struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Path       string `json:"path"`
	Reason     string `json:"reason"`
	NextAction string `json:"next_action"`
}

// ScanSkip 描述 scan 跳过的候选目录。
type ScanSkip struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanResult 是 repo_scan 的返回。
type ScanResult struct {
	Added         []Entry     `json:"added"`
	Removed       []Entry     `json:"removed"`
	Degraded      []Entry     `json:"degraded"`
	Skipped       []ScanSkip  `json:"skipped"`
	Uninitialized []ScanIssue `json:"uninitialized"`
	Repaired      []string    `json:"repaired"`
	Reconciled    bool        `json:"reconciled"`
	DryRun        bool        `json:"dry_run"`
}

const registryCols = `id,name,path,remote_key,remote_prefix,default_line,created_ts,deleted_ts,degraded,degraded_msg`

// RegisterPath 把一个已存在的目录注册为 repo（不移动、不复制数据）。
//
// 0.4.3 起只注册“完整仓库”：已有 p37.db 但缺 head/line/refs 时返回
// repo_not_initialized，detail 带 next_action=repo_init_repair，不再静默注册
// 一个半成品。init_if_missing=true 仍保留，内部走统一 InitRepo。
func (h *Hub) RegisterPath(name, path string, opt RegisterOptions) (*Entry, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: %q（只允许小写字母数字 . _ -，1-64 字符）", ErrInvalidName, name)
	}
	abs, err := h.validateRepoPath(path, true)
	if err != nil {
		return nil, err
	}
	// init_if_missing 的幂等键只在确实需要初始化时生效；完整仓库走
	// strict register，key 被忽略（不会因为 key 冲突失败）。
	if opt.InitIfMissing && opt.IdempotencyKey != "" && !opt.ForceRename {
		if in, _ := repo.CheckIntegrity(abs); in == nil || !in.Initialized {
			if _, err := h.InitRepo(name, abs, InitOptions{
				Mode: repo.InitEmpty, DefaultLine: opt.DefaultLine,
				Rebind: opt.Rebind, IdempotencyKey: opt.IdempotencyKey,
			}); err != nil {
				return nil, err
			}
			return h.Get(name)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registerPathLocked(name, abs, opt)
}

func (h *Hub) registerPathLocked(name, abs string, opt RegisterOptions) (*Entry, error) {
	defLine := opt.DefaultLine
	if defLine == "" {
		defLine = "main"
	}
	metaInfo, metaErr := readRepoMeta(abs)
	hasMeta := metaErr == nil
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: 读取 .p37/repo.json 失败: %v", ErrInvalidPath, metaErr)
	}
	// v1 -> v2：老仓库可能因为缺列被只读探测判成未初始化。保留 0.4.2 的
	// 升级行为：注册时自动走 repair/migrate，而不是要求先手工 p37 migrate。
	if in, _ := repo.CheckIntegrity(abs); in != nil &&
		in.FormatVer > 0 && in.FormatVer < meta.FormatVer {
		opt.InitIfMissing = true
	}

	effectiveID := ""
	effectiveName := name
	effectiveDefault := defLine
	if hasMeta {
		if metaInfo.Name != name && !opt.ForceRename {
			return nil, fmt.Errorf("%w: repo.json name=%q, 调用方 name=%q（显式 force_rename 才能改名）",
				ErrRepoNameMismatch, metaInfo.Name, name)
		}
		effectiveID = metaInfo.ID
		effectiveName = metaInfo.Name
		if opt.ForceRename {
			effectiveName = name
		}
		if metaInfo.DefaultLine != "" {
			effectiveDefault = metaInfo.DefaultLine
		}
	}

	// 同名已存在。
	if e, _ := h.entryByFieldLocked("name", name, false); e != nil {
		if filepath.Clean(e.Path) == filepath.Clean(abs) {
			in, _ := repo.CheckIntegrity(abs)
			if in != nil && in.Initialized {
				if err := h.ensureRepoMeta(abs, e, hasMeta); err != nil {
					return nil, err
				}
				h.enrichEntry(e)
				return e, nil
			}
			if in != nil && in.DataOK && !in.RootOK {
				// 卷迁移/重挂后 DB 里的 root 还是旧路径：数据完整，自愈即可。
				_ = updateRepoRootFile(abs)
				if err := h.ensureRepoMeta(abs, e, hasMeta); err != nil {
					return nil, err
				}
				h.enrichEntry(e)
				return e, nil
			}
			if !opt.InitIfMissing {
				return nil, integrityError(abs, in)
			}
			res, err := h.initRepoLocked(name, abs, InitOptions{
				Mode: repo.InitEmpty, DefaultLine: effectiveDefault,
				Force: opt.ForceRename, AllowNonPersistent: h.allowNonPersistent,
			})
			if err != nil {
				return nil, err
			}
			_ = res
			return h.getEnrichedLocked(name)
		}
		// 同名不同路径：必须显式 rebind，绝不静默复用旧路径。
		if !opt.Rebind {
			return nil, &PathMismatchError{Name: name, ExistingID: e.ID,
				ExistingPath: e.Path, RequestedPath: abs,
				Reason: "use_rebind_or_unregister_then_register"}
		}
		if hasMeta && metaInfo.ID != "" && e.ID != "" && metaInfo.ID != e.ID {
			return nil, &PathMismatchError{Name: name, ExistingID: e.ID,
				ExistingPath: e.Path, RequestedPath: abs,
				Reason: "target_repo_id_mismatch"}
		}
		in, _ := repo.CheckIntegrity(abs)
		if in == nil || (!in.Initialized && !in.DataOK) {
			return nil, integrityError(abs, in)
		}
		if err := h.rebindTargetLocked(e, abs); err != nil {
			return nil, err
		}
		e.Path = abs
		if hasMeta && metaInfo.DefaultLine != "" {
			e.DefaultLine = metaInfo.DefaultLine
		}
		if err := h.ensureRepoMeta(abs, e, hasMeta); err != nil {
			return nil, err
		}
		_ = updateRepoRootFile(abs)
		h.enrichEntry(e)
		return e, nil
	}

	// 同 path 已有其它 name。
	if e, _ := h.entryByFieldLocked("path", abs, false); e != nil {
		if e.Name != name && !opt.ForceRename {
			return nil, fmt.Errorf("%w: path=%s 已注册为 %q，调用方 name=%q（显式 force_rename 才能改名）",
				ErrRepoNameMismatch, abs, e.Name, name)
		}
		in, _ := repo.CheckIntegrity(abs)
		if in != nil && in.DataOK && !in.RootOK {
			_ = updateRepoRootFile(abs)
			in.Initialized = true
		}
		if in == nil || !in.Initialized {
			if !opt.InitIfMissing {
				return nil, integrityError(abs, in)
			}
			if _, err := h.initRepoLocked(e.Name, abs, InitOptions{
				Mode: repo.InitEmpty, DefaultLine: effectiveDefault,
				Force: opt.ForceRename, AllowNonPersistent: h.allowNonPersistent,
			}); err != nil {
				return nil, err
			}
			return h.getEnrichedLocked(e.Name)
		}
		effectiveID = e.ID
		effectiveName = e.Name
		if opt.ForceRename {
			effectiveName = name
		}
	}

	in, _ := repo.CheckIntegrity(abs)
	if in != nil && in.DataOK && !in.RootOK {
		_ = updateRepoRootFile(abs)
		in.Initialized = true
	}
	if in == nil || !in.Initialized {
		if !opt.InitIfMissing {
			return nil, integrityError(abs, in)
		}
		res, err := h.initRepoLocked(name, abs, InitOptions{
			Mode: repo.InitEmpty, DefaultLine: effectiveDefault,
			Force: opt.ForceRename, AllowNonPersistent: h.allowNonPersistent,
		})
		if err != nil {
			return nil, err
		}
		_ = res
		return h.getEnrichedLocked(effectiveName)
	}

	if effectiveID == "" {
		if e, _ := h.entryByFieldLocked("path", abs, true); e != nil {
			effectiveID = e.ID
		} else if e, _ := h.entryByFieldLocked("name", effectiveName, true); e != nil {
			effectiveID = e.ID
		} else {
			id, err := NewID()
			if err != nil {
				return nil, err
			}
			effectiveID = id
		}
	}
	if !hasMeta || opt.ForceRename {
		if err := writeRepoMeta(abs, repoMeta{ID: effectiveID, Name: effectiveName, DefaultLine: effectiveDefault}); err != nil {
			return nil, err
		}
	}
	if err := h.upsertEntryLocked(effectiveID, effectiveName, abs, effectiveDefault); err != nil {
		return nil, err
	}
	h.invalidateHealth(abs)
	return h.getEnrichedLocked(effectiveID)
}

// integrityError 把只读探测结果映射成结构化错误。
func integrityError(path string, in *repo.Integrity) error {
	code := "repo_not_initialized"
	reason := "repo_db_missing_or_corrupt"
	next := "repo_init"
	backup := ""
	if in != nil {
		if in.NextAction != "" {
			next = in.NextAction
		}
		if in.Reason != "" {
			reason = in.Reason
		}
		dbExists := fileExists(filepath.Join(path, ".p37", "p37.db"))
		if (dbExists && !in.RepoDBOK) || in.Reason == "repo_db_unreadable" ||
			in.Reason == "multiple_root_versions" || in.Reason == "no_unique_head" ||
			in.Reason == "head_tree_missing" || in.Reason == "format_mismatch" ||
			in.Reason == "control_dir_without_db" {
			code = "repo_db_corrupt"
		}
		backup = in.BackupPath
	}
	return &IntegrityError{Code: code, Path: path, Reason: reason,
		NextAction: next, BackupPath: backup, Retryable: false}
}

// rebindPathLocked 只更新 registry 指针，不移动任何数据。
func (h *Hub) rebindPathLocked(e *Entry, abs string) error {
	return h.registryTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE repo SET path=?, deleted_ts=0, degraded=0, degraded_msg=''
			   WHERE id=? AND deleted_ts=0`, abs, e.ID)
		if err != nil {
			return wrapRegistryErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrRepoNotFound, e.Name)
		}
		return nil
	})
}

// updateRepoRootFile 把 repo.db 内的 root 修到新路径；DB 不存在则跳过。
func updateRepoRootFile(path string) error {
	dbPath := filepath.Join(path, ".p37", "p37.db")
	if !fileExists(dbPath) {
		return nil
	}
	db, err := meta.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.UpdateRepoRoot(path)
}

// ensureRepoMeta 在已注册 repo 缺 repo.json 时补写（不覆盖已有文件）。
func (h *Hub) ensureRepoMeta(path string, e *Entry, hasMeta bool) error {
	if hasMeta {
		return nil
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "repo.json")); err == nil {
		return nil
	}
	return writeRepoMeta(path, repoMeta{ID: e.ID, Name: e.Name, DefaultLine: e.DefaultLine})
}

// Unregister 关闭 repo 并把它从 registry 可见集合中移除（保留全部数据）。
func (h *Hub) Unregister(ref string, force bool) (*UnregisterResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e, err := h.lookupLocked(ref)
	if errors.Is(err, sql.ErrNoRows) {
		return &UnregisterResult{Already: true}, nil
	}
	if err != nil {
		return nil, err
	}

	r := h.repos[e.ID]
	if r == nil && fileExists(filepath.Join(e.Path, ".p37", "p37.db")) {
		var oerr error
		r, oerr = h.openLocked(e.ID, false)
		if oerr != nil {
			if errors.Is(oerr, ErrRepoLocked) {
				return nil, oerr
			}
			if !force {
				return nil, oerr
			}
			r = nil
		}
	}

	if r != nil {
		all, lerr := r.WS.List()
		if lerr != nil {
			return nil, lerr
		}
		busyWS := make([]string, 0)
		for i := range all {
			w := &all[i]
			if workspaceActive(w.State) {
				busyWS = append(busyWS, w.ID)
			}
		}
		leases, _ := r.DB.ListLeases()
		busyLeases := make([]string, 0, len(leases))
		for i := range leases {
			busyLeases = append(busyLeases, leases[i].WsID+":"+leases[i].Path)
		}
		if !force && (len(busyWS) > 0 || len(busyLeases) > 0) {
			return nil, &BusyError{Workspaces: busyWS, Leases: busyLeases}
		}
		// 注销前必须把所有 overlay 工作区卸掉，否则应用卷无法安全 detach。
		for i := range all {
			w := &all[i]
			mounted := w.Materialize == meta.MaterializeOverlay &&
				(w.Mounted || r.MM.IsMounted(w.ID))
			if !mounted {
				continue
			}
			if err := r.MM.Unmount(w.ID); err != nil {
				return nil, fmt.Errorf("hub: 卸载工作区 %s 失败: %w", w.ID, err)
			}
		}
	}
	if err := h.dropCachedLocked(e); err != nil {
		return nil, err
	}

	// 只有 close/unmount 全部成功后才写 tombstone；失败时 registry 行保持
	// 可见，调用方可以安全重试。
	err = h.registryTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE repo SET deleted_ts=?, degraded=0, degraded_msg='' WHERE id=? AND deleted_ts=0`,
			time.Now().UnixMilli(), e.ID)
		return wrapRegistryErr(err)
	})
	if err != nil {
		return nil, err
	}
	return &UnregisterResult{Entry: e}, nil
}

// Scan 按 root 下实际存在的 repo.json 与 registry 做 reconcile。
//
// 只读 repo.json 与目录项，不打开 repo、不初始化、不做重操作。
func (h *Hub) Scan(opt ScanOptions) (*ScanResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	all, err := h.allEntriesLocked()
	if err != nil {
		return nil, err
	}
	byPath := map[string]*Entry{}
	byName := map[string]*Entry{}
	byID := map[string]*Entry{}
	for i := range all {
		e := &all[i]
		byPath[filepath.Clean(e.Path)] = e
		byName[e.Name] = e
		byID[e.ID] = e
	}

	res := &ScanResult{Added: []Entry{}, Removed: []Entry{}, Degraded: []Entry{},
		Skipped: []ScanSkip{}, Uninitialized: []ScanIssue{}, Repaired: []string{},
		Reconciled: opt.Reconcile && !opt.DryRun, DryRun: opt.DryRun}
	plannedID := map[string]bool{}
	plannedName := map[string]bool{}

	// 1) 扫 root 的直接子目录。
	des, err := os.ReadDir(h.root)
	if err != nil {
		return nil, err
	}
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(h.root, name)
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		mi, merr := readRepoMeta(path)
		if merr != nil {
			if e := byPath[filepath.Clean(path)]; e != nil && e.DeletedTS == 0 {
				continue // 已注册但 repo.json 缺失，交给第 2 步标 degraded
			}
			res.Skipped = append(res.Skipped, ScanSkip{Path: path, Reason: "no_repo_json"})
			continue
		}
		if existing := byPath[filepath.Clean(path)]; existing != nil {
			if existing.DeletedTS != 0 {
				res.Skipped = append(res.Skipped, ScanSkip{ID: existing.ID, Name: existing.Name,
					Path: path, Reason: "unregistered"})
			}
			continue // 已注册，交给第 2 步做健康检查
		}
		// tombstone 的 id/name/path 都不自动复活；显式 repo_register 才能恢复。
		if e := byID[mi.ID]; e != nil && e.DeletedTS != 0 {
			res.Skipped = append(res.Skipped, ScanSkip{ID: e.ID, Name: e.Name,
				Path: path, Reason: "unregistered"})
			continue
		}
		if e := byName[mi.Name]; e != nil {
			reason := "name_conflict"
			if e.DeletedTS != 0 {
				reason = "unregistered"
			}
			res.Skipped = append(res.Skipped, ScanSkip{ID: e.ID, Name: e.Name,
				Path: path, Reason: reason})
			continue
		}
		if in, _ := repo.CheckIntegrity(path); in == nil || (!in.Initialized && !in.DataOK) {
			issue := ScanIssue{ID: mi.ID, Name: mi.Name, Path: path,
				Reason: "repo_not_initialized", NextAction: "repo_init_repair"}
			if in != nil {
				if in.Reason != "" {
					issue.Reason = in.Reason
				}
				if in.NextAction != "" {
					issue.NextAction = in.NextAction
				}
			}
			res.Uninitialized = append(res.Uninitialized, issue)
			continue
		}
		if plannedID[mi.ID] || plannedName[mi.Name] {
			res.Skipped = append(res.Skipped, ScanSkip{ID: mi.ID, Name: mi.Name,
				Path: path, Reason: "duplicate_repo_json"})
			continue
		}
		plannedID[mi.ID] = true
		plannedName[mi.Name] = true
		res.Added = append(res.Added, Entry{ID: mi.ID, Name: mi.Name, Path: path,
			DefaultLine: mi.DefaultLine, Available: true})
	}

	// 2) 已注册行：path 是否还在、repo.json 是否还完整。
	for i := range all {
		e := &all[i]
		if e.DeletedTS != 0 {
			continue
		}
		if _, err := os.Stat(e.Path); err != nil {
			if opt.PruneMissing && h.repos[e.ID] == nil {
				res.Removed = append(res.Removed, snapshotEntry(e))
				continue
			}
			de := snapshotEntry(e)
			de.Degraded = true
			de.DegradedMsg = err.Error()
			de.Available = false
			res.Degraded = append(res.Degraded, de)
			continue
		}
		if _, err := readRepoMeta(e.Path); err != nil {
			de := snapshotEntry(e)
			de.Degraded = true
			de.DegradedMsg = "缺少 .p37/repo.json"
			de.Available = true
			res.Degraded = append(res.Degraded, de)
		}
		if in, _ := repo.CheckIntegrity(e.Path); in != nil && !in.Initialized {
			res.Uninitialized = append(res.Uninitialized, ScanIssue{
				ID: e.ID, Name: e.Name, Path: e.Path,
				Reason: in.Reason, NextAction: in.NextAction,
			})
		}
	}

	if opt.Repair && !opt.DryRun {
		for _, is := range res.Uninitialized {
			name := is.Name
			if name == "" {
				name = filepath.Base(is.Path)
			}
			if _, err := h.initRepoLocked(name, is.Path, InitOptions{
				Mode: repo.InitRepair, AllowNonPersistent: h.allowNonPersistent,
			}); err != nil {
				res.Skipped = append(res.Skipped, ScanSkip{
					ID: is.ID, Name: name, Path: is.Path,
					Reason: "repair_failed: " + err.Error(),
				})
				continue
			}
			res.Repaired = append(res.Repaired, is.Path)
		}
	}

	if !opt.Reconcile || opt.DryRun {
		return res, nil
	}

	err = h.registryTx(func(tx *sql.Tx) error {
		for i := range res.Added {
			a := &res.Added[i]
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO repo(id,name,path,default_line,created_ts) VALUES(?,?,?,?,?)`,
				a.ID, a.Name, a.Path, a.DefaultLine, time.Now().UnixMilli()); err != nil {
				return wrapRegistryErr(err)
			}
		}
		for i := range res.Removed {
			if _, err := tx.Exec(`DELETE FROM repo WHERE id=? AND deleted_ts=0`, res.Removed[i].ID); err != nil {
				return wrapRegistryErr(err)
			}
		}
		for i := range res.Degraded {
			d := &res.Degraded[i]
			if _, err := tx.Exec(`UPDATE repo SET degraded=1, degraded_msg=? WHERE id=?`,
				d.DegradedMsg, d.ID); err != nil {
				return wrapRegistryErr(err)
			}
		}
		// 健康行清除历史 degraded（scan 是轻量 reconcile，不探测 DB 损坏）。
		for i := range all {
			e := &all[i]
			if e.DeletedTS != 0 || !e.Degraded {
				continue
			}
			stillDegraded := false
			for j := range res.Degraded {
				if res.Degraded[j].ID == e.ID {
					stillDegraded = true
					break
				}
			}
			if stillDegraded {
				continue
			}
			for j := range res.Removed {
				if res.Removed[j].ID == e.ID {
					stillDegraded = true
					break
				}
			}
			if stillDegraded {
				continue
			}
			if _, err := tx.Exec(`UPDATE repo SET degraded=0, degraded_msg='' WHERE id=?`, e.ID); err != nil {
				return wrapRegistryErr(err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// guardDeletePath 拒绝 root 自身、root 外路径和挂载点。
func (h *Hub) guardDeletePath(path string) error {
	abs, err := h.validateRepoPath(path, false)
	if err != nil {
		return err
	}
	if h.isMountPoint(abs) {
		return fmt.Errorf("%w: %s；应用卷请先 repo_unregister，再 guest umount，最后在宿主删除卷", ErrRepoIsMountpoint, abs)
	}
	return nil
}

// validateRepoPath 确保 path 是 root 下的绝对路径，拒绝 ..、root 自身、
// root/.p37 以及 symlink 逃逸。
func (h *Hub) validateRepoPath(path string, mustExist bool) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: 需要绝对路径: %q", ErrInvalidPath, path)
	}
	abs := filepath.Clean(path)
	rootAbs, err := filepath.Abs(h.root)
	if err != nil {
		return "", err
	}
	rootAbs = filepath.Clean(rootAbs)
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == "." || rel == ".." ||
		filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s 不在 root %s 下", ErrInvalidPath, abs, rootAbs)
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if first == ".p37" {
		return "", fmt.Errorf("%w: 不能注册控制目录 %s", ErrInvalidPath, abs)
	}

	fi, err := os.Lstat(abs)
	if err != nil {
		if mustExist || !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s: %v", ErrInvalidPath, abs, err)
		}
		return abs, nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: path 是 symlink: %s", ErrInvalidPath, abs)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%w: path 不是目录: %s", ErrInvalidPath, abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalidPath, abs, err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		rootReal = rootAbs
	}
	rrel, err := filepath.Rel(rootReal, real)
	if err != nil || rrel == "." || rrel == ".." ||
		filepath.IsAbs(rrel) || strings.HasPrefix(rrel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: symlink 逃逸到 root 外: %s -> %s", ErrInvalidPath, abs, real)
	}
	return abs, nil
}

// initRepoAt 初始化目录并写入 repo.json。调用方必须持有 h.mu。
//
// 这是 repo_create 的 L2 scratch 路径：明确不要求挂载点（mounted=false），
// 但仍然走统一的原子初始化，避免留下半套 .p37。
func (h *Hub) initRepoAt(path, id, name, defaultLine string) error {
	if defaultLine == "" {
		defaultLine = "main"
	}
	_, err := repo.Ensure(path, repo.EnsureOptions{
		Mode: repo.InitEmpty, DefaultLine: defaultLine, Name: name, RepoID: id,
		RepoOptions: h.cfg.RepoOptions(), WriteConfig: h.writeConfig,
		MountProbe: h.isMountPoint, EnforcePersistent: false,
	})
	return err
}

func workspaceActive(state string) bool {
	switch state {
	case meta.StateActive, meta.StateConflicted, meta.StateRebasing, meta.StateIntegrating:
		return true
	}
	return false
}

func (h *Hub) entryByFieldLocked(field, val string, includeDeleted bool) (*Entry, error) {
	switch field {
	case "id", "name", "path":
	default:
		return nil, fmt.Errorf("hub: 非法 registry 字段 %q", field)
	}
	q := `SELECT ` + registryCols + ` FROM repo WHERE ` + field + `=?`
	if !includeDeleted {
		q += ` AND deleted_ts=0`
	}
	q += ` ORDER BY deleted_ts LIMIT 1`
	return queryEntry(h.db, q, val)
}

func (h *Hub) entryByIDLocked(id string, includeDeleted bool) (*Entry, error) {
	return h.entryByFieldLocked("id", id, includeDeleted)
}

func (h *Hub) allEntriesLocked() ([]Entry, error) {
	rows, err := h.db.Query(`SELECT ` + registryCols + ` FROM repo ORDER BY created_ts, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func queryEntry(q interface{ QueryRow(string, ...any) *sql.Row }, query string, args ...any) (*Entry, error) {
	return scanEntry(q.QueryRow(query, args...))
}

func snapshotEntry(e *Entry) Entry {
	c := *e
	return c
}

func (h *Hub) deleteTombstoneLocked(field, val string) error {
	switch field {
	case "id", "name", "path":
	default:
		return fmt.Errorf("hub: 非法 registry 字段 %q", field)
	}
	_, err := h.db.Exec(`DELETE FROM repo WHERE `+field+`=? AND deleted_ts<>0`, val)
	return wrapRegistryErr(err)
}

func (h *Hub) registryTx(fn func(*sql.Tx) error) error {
	tx, err := h.db.Begin()
	if err != nil {
		return wrapRegistryErr(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrapRegistryErr(err)
	}
	return nil
}

func wrapRegistryErr(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		// SQLITE_BUSY=5, SQLITE_LOCKED=6
		if se.Code() == 5 || se.Code() == 6 {
			return fmt.Errorf("%w: %v", ErrRegistryBusy, err)
		}
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "database is locked") || strings.Contains(low, "database table is locked") {
		return fmt.Errorf("%w: %v", ErrRegistryBusy, err)
	}
	return err
}

type repoMeta = repo.Meta

func readRepoMeta(path string) (*repoMeta, error) { return repo.ReadMeta(path) }

func writeRepoMeta(path string, m repoMeta) error {
	return repo.WriteMeta(filepath.Join(path, ".p37"), m)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
