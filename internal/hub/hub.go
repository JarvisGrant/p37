// Package hub 实现单 p37d 进程下的多仓库控制面（registry + 懒加载）。
package hub

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/repolock"
)

// Errors.
var (
	ErrRepoNotFound       = errors.New("hub: repo 不存在")
	ErrRepoExists         = errors.New("hub: repo 已存在")
	ErrRepoBusy           = errors.New("hub: repo 仍有活跃工作区或租约")
	ErrInvalidName        = errors.New("hub: 非法 repo 名")
	ErrInvalidPath        = errors.New("hub: 非法 repo 路径")
	ErrRepoNotInitialized = errors.New("hub: repo 未初始化")
	ErrRepoNameMismatch   = errors.New("hub: repo 名与 repo.json 不一致")
	ErrRepoPathExists     = errors.New("hub: repo 路径或 id 已被注册")
	ErrRepoPathMismatch   = errors.New("hub: repo 名已注册在旧路径")
	ErrRepoIsMountpoint   = errors.New("hub: repo 路径是挂载点")
	ErrRegistryBusy       = errors.New("hub: registry 忙")
	ErrRepoLocked         = errors.New("hub: repo 锁被其它进程持有")
)

// BusyError 描述 unregister 被活跃工作区/租约挡住的具体原因。
type BusyError struct {
	Workspaces []string `json:"workspaces"`
	Leases     []string `json:"leases"`
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("%v: %d 个工作区, %d 个租约", ErrRepoBusy, len(e.Workspaces), len(e.Leases))
}

// Is 让 errors.Is(err, ErrRepoBusy) 成立。
func (e *BusyError) Is(target error) bool { return target == ErrRepoBusy }

// PathMismatchError 说明同名 repo 已经注册在另一个路径。
type PathMismatchError struct {
	Name          string `json:"name"`
	ExistingID    string `json:"existing_id"`
	ExistingPath  string `json:"existing_path"`
	RequestedPath string `json:"requested_path"`
	Reason        string `json:"reason"`
}

func (e *PathMismatchError) Error() string {
	return fmt.Sprintf("%v: name=%s existing=%s requested=%s", ErrRepoPathMismatch,
		e.Name, e.ExistingPath, e.RequestedPath)
}

func (e *PathMismatchError) Is(target error) bool { return target == ErrRepoPathMismatch }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Entry 是 registry 的一行。
type Entry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	RemoteKey    string `json:"remote_key,omitempty"`
	RemotePrefix string `json:"remote_prefix,omitempty"`
	DefaultLine  string `json:"default_line"`
	CreatedTS    int64  `json:"created_ts"`
	DeletedTS    int64  `json:"deleted_ts"`
	Degraded     bool   `json:"degraded"`
	DegradedMsg  string `json:"degraded_msg,omitempty"`
	// Mounted/Available 是每次 List/Get 时按文件系统实时计算的观测字段，
	// 不写入 registry，也不要求打开 repo。
	Mounted   bool `json:"mounted"`
	Available bool `json:"available"`
}

// Hub 管理多个仓库。
type Hub struct {
	root string
	cfg  config.Config
	db   *sql.DB

	mu    sync.Mutex
	repos map[string]*repo.Repo
	locks map[string]*repolock.Lock

	// mountProbe 可注入，单测不需要真实挂载点。默认使用 mountmgr.IsMountPoint。
	mountProbe func(string) bool

	// recoverWarnings 记录 registry 重建时的多候选选择，供启动日志展示。
	recoverWarnings []string
}

// New 打开/创建 registry。root 通常是 /srv/repos。
func New(root string, cfg config.Config) (*Hub, error) {
	if root == "" {
		return nil, errors.New("hub: 缺少 root")
	}
	ctl := filepath.Join(root, ".p37")
	for _, d := range []string{root, ctl, filepath.Join(ctl, "tmp"), filepath.Join(ctl, "trash")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	_ = os.Chmod(ctl, 0o700)
	regPath := filepath.Join(ctl, "registry.db")
	db, err := sql.Open("sqlite", "file:"+regPath+
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"+
		"&_pragma=busy_timeout(15000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	_ = os.Chmod(regPath, 0o600)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS repo (
	  id            TEXT PRIMARY KEY,
	  name          TEXT NOT NULL UNIQUE,
	  path          TEXT NOT NULL UNIQUE,
	  remote_key    TEXT NOT NULL DEFAULT '',
	  remote_prefix TEXT NOT NULL DEFAULT '',
	  default_line  TEXT NOT NULL DEFAULT 'main',
	  created_ts    INTEGER NOT NULL,
	  deleted_ts    INTEGER NOT NULL DEFAULT 0,
	  degraded      INTEGER NOT NULL DEFAULT 0,
	  degraded_msg  TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(regPath, 0o600)
	_ = os.Chmod(regPath+"-wal", 0o600)
	_ = os.Chmod(regPath+"-shm", 0o600)
	h := &Hub{root: root, cfg: cfg, db: db, repos: map[string]*repo.Repo{}, locks: map[string]*repolock.Lock{},
		mountProbe: mountmgr.IsMountPoint}
	h.recoverRegistry()
	return h, nil
}

// SetMountPointProbe 注入挂载点判断（测试用）。nil 恢复默认实现。
func (h *Hub) SetMountPointProbe(fn func(string) bool) {
	if fn == nil {
		fn = mountmgr.IsMountPoint
	}
	h.mountProbe = fn
}

func (h *Hub) isMountPoint(path string) bool {
	fn := h.mountProbe
	if fn == nil {
		return mountmgr.IsMountPoint(path)
	}
	return fn(path)
}

// Root 返回多仓库根目录。
func (h *Hub) Root() string { return h.root }

// SocketPath 返回 daemon socket。
func (h *Hub) SocketPath() string { return filepath.Join(h.root, ".p37", "p37d.sock") }

// NewID 生成一个不可猜的 repo id。
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "repo_" + hex.EncodeToString(b[:]), nil
}

// Create 创建一个新 repo（含空 v0 与默认 line）。
func (h *Hub) Create(name string, remoteKey, remotePrefix string) (*Entry, error) {
	return h.CreateEx(name, "main", remoteKey, remotePrefix)
}

// CreateEx 创建仓库并可指定默认 line。
func (h *Hub) CreateEx(name, defaultLine, remoteKey, remotePrefix string) (*Entry, error) {
	if defaultLine == "" {
		defaultLine = "main"
	}
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: %q（只允许小写字母数字 . _ -，1-64 字符）", ErrInvalidName, name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// 同 key/同名幂等：同名已存在直接返回。
	if e, err := h.lookupLocked(name); err == nil && e != nil {
		return e, nil
	}
	// 同名 tombstone 会让 UNIQUE(name) 插入失败；显式创建新 repo 表示
	// 用户要复用这个名字，清掉旧 tombstone（旧目录数据不会被删除）。
	if err := h.deleteTombstoneLocked("name", name); err != nil {
		return nil, err
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(h.root, id)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: 目录已存在 %s", ErrRepoExists, path)
	}
	now := time.Now().UnixMilli()
	_, err = h.db.Exec(
		`INSERT INTO repo(id,name,path,remote_key,remote_prefix,default_line,created_ts)
		 VALUES(?,?,?,?,?,?,?)`, id, name, path, remoteKey, remotePrefix, defaultLine, now)
	if err != nil {
		return nil, err
	}
	if err := h.initRepoAt(path, id, name, defaultLine); err != nil {
		_, _ = h.db.Exec(`DELETE FROM repo WHERE id=?`, id)
		_ = os.RemoveAll(path)
		return nil, err
	}
	return h.lookupLocked(id)
}

// Register 把已有仓库目录注册进 registry（不移动目录）。
func (h *Hub) Register(name, path, defaultLine string) (*Entry, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, _ := h.lookupLocked(name); e != nil {
		return e, nil
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	_, err = h.db.Exec(
		`INSERT INTO repo(id,name,path,default_line,created_ts) VALUES(?,?,?,?,?)`,
		id, name, path, defaultLine, now)
	if err != nil {
		return nil, err
	}
	_ = writeRepoMeta(path, repoMeta{ID: id, Name: name, DefaultLine: defaultLine})
	return h.lookupLocked(id)
}

func (h *Hub) lookupLocked(ref string) (*Entry, error) {
	row := h.db.QueryRow(
		`SELECT id,name,path,remote_key,remote_prefix,default_line,created_ts,deleted_ts,degraded,degraded_msg
		   FROM repo WHERE (id=? OR name=?) AND deleted_ts=0`, ref, ref)
	return scanEntry(row)
}

// Get 按 id/name 查 registry。
func (h *Hub) Get(ref string) (*Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, err := h.lookupLocked(ref)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, ref)
	}
	if err != nil {
		return nil, err
	}
	h.enrichEntry(e)
	return e, nil
}

// List 返回全部 repo。
func (h *Hub) List() ([]Entry, error) {
	rows, err := h.db.Query(
		`SELECT id,name,path,remote_key,remote_prefix,default_line,created_ts,deleted_ts,degraded,degraded_msg
		   FROM repo WHERE deleted_ts=0 ORDER BY created_ts, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]Entry, 0)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		h.enrichEntry(e)
		out = append(out, *e)
	}
	return out, rows.Err()
}

// enrichEntry 只做轻量 stat/mount 探测，绝不打开 repo。
func (h *Hub) enrichEntry(e *Entry) {
	if e == nil {
		return
	}
	fi, err := os.Stat(e.Path)
	e.Available = err == nil && fi.IsDir()
	e.Mounted = e.Available && h.isMountPoint(e.Path)
	if !e.Available {
		e.Degraded = true
		if e.DegradedMsg == "" {
			if err != nil {
				e.DegradedMsg = err.Error()
			} else {
				e.DegradedMsg = "path 不是目录"
			}
		}
		return
	}
	if _, err := os.Stat(filepath.Join(e.Path, ".p37", "repo.json")); err != nil {
		e.Degraded = true
		if e.DegradedMsg == "" {
			e.DegradedMsg = "缺少 .p37/repo.json"
		}
	}
}

func scanEntry(s interface{ Scan(...any) error }) (*Entry, error) {
	var e Entry
	var degraded int
	if err := s.Scan(&e.ID, &e.Name, &e.Path, &e.RemoteKey, &e.RemotePrefix,
		&e.DefaultLine, &e.CreatedTS, &e.DeletedTS, &degraded, &e.DegradedMsg); err != nil {
		return nil, err
	}
	e.Degraded = degraded != 0
	return &e, nil
}

// Open 打开（或复用）一个 repo。ref 可为 id 或 name。
func (h *Hub) Open(ref string) (*repo.Repo, error) {
	// 整个 open 过程持有 h.mu：同一 repo 的并发首次访问会等待第一个完成，
	// 而不是第二个去抢 repolock 然后报「正被另一个进程持有」。
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.openLocked(ref, true)
}

// openLocked 在持有 h.mu 时打开 repo。remount 为 false 时只打开元数据，
// 不恢复 workspace overlay 挂载（unregister 检查工作区时使用）。
func (h *Hub) openLocked(ref string, remount bool) (*repo.Repo, error) {
	if r := h.repos[ref]; r != nil {
		return r, nil
	}
	e, err := h.lookupLocked(ref)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, ref)
	}
	if err != nil {
		return nil, err
	}
	if r := h.repos[e.ID]; r != nil {
		return r, nil
	}

	_ = os.Chmod(e.Path, 0o750)
	_ = os.Chmod(filepath.Join(e.Path, ".p37"), 0o700)
	cfg, err := config.Load(e.Path)
	if err != nil {
		return nil, err
	}
	lock, err := repolock.TryExclusive(filepath.Join(e.Path, ".p37", "lock"))
	if err != nil {
		if errors.Is(err, repolock.ErrBusy) {
			return nil, fmt.Errorf("%w: repo %s", ErrRepoLocked, e.Name)
		}
		return nil, err
	}
	opt := cfg.RepoOptions()
	opt.RemountWorkspaces = remount
	r, err := repo.Open(e.Path, opt)
	if err != nil {
		_ = lock.Release()
		h.markDegraded(e.ID, err)
		return nil, err
	}
	if old := h.repos[e.ID]; old != nil {
		_ = r.Close()
		_ = lock.Release()
		return old, nil
	}
	h.repos[e.ID] = r
	h.repos[e.Name] = r
	h.locks[e.ID] = lock
	return r, nil
}

// OpenEntry 按 registry 行打开。
func (h *Hub) OpenEntry(e *Entry) (*repo.Repo, error) { return h.Open(e.ID) }

func (h *Hub) markDegraded(id string, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, _ = h.db.Exec(`UPDATE repo SET degraded=1, degraded_msg=? WHERE id=?`, msg, id)
}

// CloseRepo 关闭并移除缓存（不删数据）。
func (h *Hub) CloseRepo(ref string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, err := h.lookupLocked(ref)
	if err != nil {
		return err
	}
	return h.dropCachedLocked(e)
}

// dropCachedLocked 关闭 repo 的 DB/overlay 引用并释放 per-repo flock。
func (h *Hub) dropCachedLocked(e *Entry) error {
	var firstErr error
	if r := h.repos[e.ID]; r != nil {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(h.repos, e.ID)
		delete(h.repos, e.Name)
	}
	if l := h.locks[e.ID]; l != nil {
		if err := l.Release(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(h.locks, e.ID)
	}
	return firstErr
}

// Delete 删除一个 repo。默认硬删；有活跃工作区/租约时拒绝。
func (h *Hub) Delete(ref string, force bool) (*Entry, error) {
	e, err := h.Get(ref)
	if err != nil {
		return nil, err
	}
	// 挂载点绝不能 rename；应用卷请走 repo_unregister + guest umount。
	if err := h.guardDeletePath(e.Path); err != nil {
		return nil, err
	}
	r, err := h.Open(e.ID)
	if err != nil && !force {
		return nil, err
	}
	if r != nil {
		all, _ := r.WS.List()
		if len(all) > 0 && !force {
			return nil, fmt.Errorf("%w: %d 个工作区", ErrRepoBusy, len(all))
		}
		leases, _ := r.DB.ListLeases()
		if len(leases) > 0 && !force {
			return nil, fmt.Errorf("%w: %d 个租约", ErrRepoBusy, len(leases))
		}
		for i := range all {
			if all[i].Materialize == "overlay" && all[i].Mounted {
				_ = r.MM.Unmount(all[i].ID)
			}
		}
	}
	// 先写 tombstone，再移目录，最后删 registry。
	_, _ = h.db.Exec(`UPDATE repo SET deleted_ts=? WHERE id=?`, time.Now().UnixMilli(), e.ID)
	_ = h.CloseRepo(e.ID)
	trash := filepath.Join(h.root, ".p37", "trash", e.ID+"-"+fmt.Sprint(time.Now().UnixMilli()))
	if err := os.Rename(e.Path, trash); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, err = h.db.Exec(`DELETE FROM repo WHERE id=?`, e.ID)
	return e, err
}

// Close 关闭全部 repo 与 registry。
func (h *Hub) Close() error {
	h.mu.Lock()
	seen := map[*repo.Repo]bool{}
	for _, r := range h.repos {
		if r == nil || seen[r] {
			continue
		}
		seen[r] = true
		if all, err := r.WS.List(); err == nil {
			for i := range all {
				w := &all[i]
				if w.Materialize == "overlay" && w.Mounted {
					_ = r.MM.Unmount(w.ID)
				}
			}
		}
		_ = r.Close()
	}
	h.repos = map[string]*repo.Repo{}
	for id, l := range h.locks {
		_ = l.Release()
		delete(h.locks, id)
	}
	h.mu.Unlock()
	return h.db.Close()
}

// RegisterLegacy 把旧单仓库 /srv/repo 原地注册为 default。
func (h *Hub) RegisterLegacy(legacy string) (*Entry, error) {
	if _, err := os.Stat(filepath.Join(legacy, ".p37", "p37.db")); err != nil {
		return nil, nil
	}
	return h.Register("default", legacy, "main")
}

// UpdateRemote 更新 registry 中的远端标识。
func (h *Hub) UpdateRemote(ref, remoteKey, remotePrefix string) error {
	e, err := h.Get(ref)
	if err != nil {
		return err
	}
	_, err = h.db.Exec(`UPDATE repo SET remote_key=?, remote_prefix=? WHERE id=?`,
		remoteKey, remotePrefix, e.ID)
	return err
}

// CleanName 供 CLI 诊断。
func CleanName(s string) string { return strings.TrimSpace(s) }

type repoMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DefaultLine string `json:"default_line"`
}

func writeRepoMeta(path string, m repoMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, ".p37", "repo.json"), b, 0o600)
}

// recoverRegistry 在 registry 丢失时从各 repo 的 .p37/repo.json 重建。
//
// 同名候选可能同时存在（历史 repo_<hash> 与新应用卷 /<name>）。这里优先选
// 目录名 == repo name 的候选；没有则按路径排序取第一个，并把冲突写进
// RecoverWarnings，由 p37d 启动日志暴露，避免静默选错 L2 旧路径。
func (h *Hub) recoverRegistry() {
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM repo`).Scan(&n); err != nil || n > 0 {
		return
	}
	entries, err := os.ReadDir(h.root)
	if err != nil {
		return
	}
	type cand struct {
		path string
		base string
		meta repoMeta
	}
	byName := map[string][]cand{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(h.root, e.Name())
		b, err := os.ReadFile(filepath.Join(path, ".p37", "repo.json"))
		if err != nil {
			continue
		}
		var m repoMeta
		if json.Unmarshal(b, &m) != nil || m.ID == "" || m.Name == "" {
			continue
		}
		byName[m.Name] = append(byName[m.Name], cand{path: path, base: e.Name(), meta: m})
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	seenID := map[string]string{}
	for _, name := range names {
		cs := byName[name]
		sort.Slice(cs, func(i, j int) bool { return cs[i].path < cs[j].path })
		chosen := cs[0]
		for _, c := range cs {
			if c.base == name {
				chosen = c
				break
			}
		}
		if len(cs) > 1 {
			paths := make([]string, 0, len(cs))
			for _, c := range cs {
				paths = append(paths, c.path)
			}
			h.recoverWarnings = append(h.recoverWarnings,
				fmt.Sprintf("repo name %q 有多个候选，选择 %s；其它: %s",
					name, chosen.path, strings.Join(paths, ", ")))
		}
		if prevName, ok := seenID[chosen.meta.ID]; ok && prevName != name {
			h.recoverWarnings = append(h.recoverWarnings,
				fmt.Sprintf("repo id %s 同时被 %q 与 %q 引用，保留 %q",
					chosen.meta.ID, prevName, name, prevName))
			continue
		}
		seenID[chosen.meta.ID] = name
		_, _ = h.db.Exec(
			`INSERT OR IGNORE INTO repo(id,name,path,default_line,created_ts)
			 VALUES(?,?,?,?,?)`, chosen.meta.ID, name, chosen.path, chosen.meta.DefaultLine, time.Now().UnixMilli())
	}
}

// RecoverWarnings 返回 registry 重建时的冲突/选择说明。
func (h *Hub) RecoverWarnings() []string { return append([]string{}, h.recoverWarnings...) }
