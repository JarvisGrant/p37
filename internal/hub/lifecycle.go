package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/chenyiwen/p37/internal/config"
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
	Added      []Entry    `json:"added"`
	Removed    []Entry    `json:"removed"`
	Degraded   []Entry    `json:"degraded"`
	Skipped    []ScanSkip `json:"skipped"`
	Reconciled bool       `json:"reconciled"`
	DryRun     bool       `json:"dry_run"`
}

const registryCols = `id,name,path,remote_key,remote_prefix,default_line,created_ts,deleted_ts,degraded,degraded_msg`

// RegisterPath 把一个已存在的目录注册为 repo（不移动、不复制数据）。
func (h *Hub) RegisterPath(name, path string, opt RegisterOptions) (*Entry, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: %q（只允许小写字母数字 . _ -，1-64 字符）", ErrInvalidName, name)
	}
	defLine := opt.DefaultLine
	if defLine == "" {
		defLine = "main"
	}
	abs, err := h.validateRepoPath(path, true)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	metaInfo, metaErr := readRepoMeta(abs)
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: 读取 .p37/repo.json 失败: %v", ErrInvalidPath, metaErr)
	}
	hasMeta := metaErr == nil

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

	// 已注册：同 name 或同 path 幂等返回已有 Entry。
	if e, _ := h.entryByFieldLocked("name", name, false); e != nil {
		if err := h.ensureRepoMeta(e.Path, e, hasMeta); err != nil {
			return nil, err
		}
		h.enrichEntry(e)
		return e, nil
	}
	if e, _ := h.entryByFieldLocked("path", abs, false); e != nil {
		if e.Name == name || !opt.ForceRename {
			if err := h.ensureRepoMeta(e.Path, e, hasMeta); err != nil {
				return nil, err
			}
			h.enrichEntry(e)
			return e, nil
		}
		// force_rename：继续走更新路径，tx 里改 name。
		effectiveID = e.ID
	}
	if effectiveID != "" {
		if e, _ := h.entryByIDLocked(effectiveID, false); e != nil {
			if filepath.Clean(e.Path) != filepath.Clean(abs) {
				return nil, fmt.Errorf("%w: id=%s 已注册到 %s", ErrRepoPathExists, effectiveID, e.Path)
			}
			if e.Name == name || !opt.ForceRename {
				h.enrichEntry(e)
				return e, nil
			}
		}
	}

	dbPath := filepath.Join(abs, ".p37", "p37.db")
	hasDB := fileExists(dbPath)
	if !hasDB {
		if !opt.InitIfMissing {
			return nil, fmt.Errorf("%w: %s（init_if_missing=true 才能初始化）", ErrRepoNotInitialized, abs)
		}
		if effectiveID == "" {
			// 有 tombstone 时复用原 id，避免重启后出现两条身份。
			if e, _ := h.entryByFieldLocked("path", abs, true); e != nil {
				effectiveID = e.ID
			} else {
				id, err := NewID()
				if err != nil {
					return nil, err
				}
				effectiveID = id
			}
		}
		createdP37 := !fileExists(filepath.Join(abs, ".p37"))
		if err := h.initRepoAt(abs, effectiveID, effectiveName, effectiveDefault); err != nil {
			if createdP37 {
				_ = os.RemoveAll(filepath.Join(abs, ".p37"))
			}
			return nil, err
		}
		hasMeta = true
	} else {
		if effectiveID == "" {
			if e, _ := h.entryByFieldLocked("path", abs, true); e != nil {
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
	}

	now := time.Now().UnixMilli()
	var active *Entry
	err = h.registryTx(func(tx *sql.Tx) error {
		if e, qerr := queryEntry(tx,
			`SELECT `+registryCols+` FROM repo WHERE deleted_ts=0 AND (id=? OR name=? OR path=?) ORDER BY id LIMIT 1`,
			effectiveID, effectiveName, abs); qerr == nil {
			// 同 id/name/path 都按幂等处理；同 path 的 force_rename 继续更新。
			sameName := e.Name == effectiveName
			sameID := e.ID == effectiveID
			samePath := e.Path == abs
			if sameName || (!opt.ForceRename && (sameID || samePath)) {
				active = e
				return nil
			}
			if !samePath {
				return fmt.Errorf("%w: %s", ErrRepoPathExists, e.Path)
			}
		} else if !errors.Is(qerr, sql.ErrNoRows) {
			return qerr
		}

		// 清掉会挡 UNIQUE(name)/UNIQUE(path) 的其它 tombstone。
		if _, err := tx.Exec(`DELETE FROM repo WHERE deleted_ts<>0 AND (name=? OR path=?) AND id<>?`,
			effectiveName, abs, effectiveID); err != nil {
			return wrapRegistryErr(err)
		}
		res, err := tx.Exec(
			`UPDATE repo SET name=?, path=?, default_line=?, deleted_ts=0, degraded=0, degraded_msg=''
			   WHERE id=?`, effectiveName, abs, effectiveDefault, effectiveID)
		if err != nil {
			return wrapRegistryErr(err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.Exec(
			`INSERT INTO repo(id,name,path,default_line,created_ts) VALUES(?,?,?,?,?)`,
			effectiveID, effectiveName, abs, effectiveDefault, now)
		return wrapRegistryErr(err)
	})
	if err != nil {
		return nil, err
	}
	if active != nil {
		h.enrichEntry(active)
		return active, nil
	}
	e2, err := h.lookupLocked(effectiveID)
	if err != nil {
		return nil, err
	}
	h.enrichEntry(e2)
	return e2, nil
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
		Skipped: []ScanSkip{}, Reconciled: opt.Reconcile && !opt.DryRun, DryRun: opt.DryRun}
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
func (h *Hub) initRepoAt(path, id, name, defaultLine string) error {
	if defaultLine == "" {
		defaultLine = "main"
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return err
	}
	opt := h.cfg.RepoOptions()
	opt.RemountWorkspaces = false
	r, err := repo.Init(path, opt)
	if err != nil {
		return err
	}
	if defaultLine != "main" {
		head, _, _ := r.Head()
		if _, err := r.CreateLine(defaultLine, head.String(), true); err == nil {
			_ = r.DB.SetDefaultLine(defaultLine)
			_ = r.DB.DeleteLine("main", time.Now().UnixMilli())
		}
	}
	if err := r.Close(); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o750)
	_ = os.Chmod(filepath.Join(path, ".p37"), 0o700)
	// 把根配置作为该 repo 的默认配置落盘，后续 repo.Open 才能保持一致。
	if err := config.Save(path, h.cfg); err != nil {
		return err
	}
	return writeRepoMeta(path, repoMeta{ID: id, Name: name, DefaultLine: defaultLine})
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

func readRepoMeta(path string) (*repoMeta, error) {
	b, err := os.ReadFile(filepath.Join(path, ".p37", "repo.json"))
	if err != nil {
		return nil, err
	}
	var m repoMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.ID == "" || m.Name == "" {
		return nil, errors.New("repo.json 缺少 id/name")
	}
	if !nameRE.MatchString(m.Name) {
		return nil, fmt.Errorf("repo.json name 非法: %q", m.Name)
	}
	if m.DefaultLine == "" {
		m.DefaultLine = "main"
	}
	return &m, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
