package hub_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

func newHub(t *testing.T) (*hub.Hub, string) {
	t.Helper()
	root := t.TempDir()
	h, err := hub.New(root, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	h.SetAllowNonPersistent(true)
	t.Cleanup(func() { _ = h.Close() })
	return h, root
}

func externalRepo(t *testing.T, path string, materialize mountmgr.Mode) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Materialize = string(materialize)
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(path, cfg.RepoOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeMeta(t *testing.T, path, id, name, line string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"id": id, "name": name, "default_line": line})
	if err := os.WriteFile(filepath.Join(path, ".p37", "repo.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterPathInitIfMissing(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app-a")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RegisterPath("app-a", path, hub.RegisterOptions{}); !errors.Is(err, hub.ErrRepoNotInitialized) {
		t.Fatalf("未初始化应报 repo_not_initialized: %v", err)
	}
	e, err := h.RegisterPath("app-a", path, hub.RegisterOptions{InitIfMissing: true, DefaultLine: "trunk"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "app-a" || e.Path != path || e.DefaultLine != "trunk" {
		t.Fatalf("entry=%+v", e)
	}
	if !strings.HasPrefix(e.ID, "repo_") || len(e.ID) != len("repo_")+32 {
		t.Fatalf("id=%q", e.ID)
	}
	if !e.Available || e.Mounted || e.Degraded {
		t.Fatalf("observability=%+v", e)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "repo.json")); err != nil {
		t.Fatal(err)
	}
	r, err := h.Open("app-a")
	if err != nil {
		t.Fatal(err)
	}
	if def, _ := r.DB.DefaultLine(); def != "trunk" {
		t.Fatalf("default_line=%q", def)
	}
}

func TestRegisterPathExistingRepoJSON(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	id, _ := hub.NewID()
	writeMeta(t, path, id, "app", "main")

	e, err := h.RegisterPath("app", path, hub.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != id {
		t.Fatalf("id=%s want %s", e.ID, id)
	}
	if _, err := h.RegisterPath("other", path, hub.RegisterOptions{}); !errors.Is(err, hub.ErrRepoNameMismatch) {
		t.Fatalf("name mismatch=%v", err)
	}
	e2, err := h.RegisterPath("other", path, hub.RegisterOptions{ForceRename: true})
	if err != nil {
		t.Fatal(err)
	}
	if e2.Name != "other" {
		t.Fatalf("force rename=%+v", e2)
	}
	mi, err := readMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Name != "other" {
		t.Fatalf("repo.json name=%q", mi.Name)
	}
}

func TestRegisterPathMissingMetaGenerated(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	e, err := h.RegisterPath("app", path, hub.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mi, err := readMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if mi.ID != e.ID || mi.Name != "app" || mi.DefaultLine != "main" {
		t.Fatalf("meta=%+v entry=%+v", mi, e)
	}
}

func TestRegisterPathIdempotent(t *testing.T) {
	h, root := newHub(t)
	p1 := filepath.Join(root, "a")
	p2 := filepath.Join(root, "b")
	externalRepo(t, p1, mountmgr.ModeCopy)
	externalRepo(t, p2, mountmgr.ModeCopy)
	e1, err := h.RegisterPath("a", p1, hub.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := h.RegisterPath("a", p1, hub.RegisterOptions{})
	if err != nil || e2.ID != e1.ID {
		t.Fatalf("same path/name 幂等: %v %+v", err, e2)
	}
	if _, err := h.RegisterPath("a", p2, hub.RegisterOptions{}); !errors.Is(err, hub.ErrRepoPathMismatch) {
		t.Fatalf("same name 不同路径必须显式 repo_path_mismatch: %v", err)
	}
	if _, err := h.RegisterPath("b", p1, hub.RegisterOptions{}); !errors.Is(err, hub.ErrRepoNameMismatch) {
		t.Fatalf("same path 但 name 与 repo.json 冲突应 mismatch: %v", err)
	}
}

func TestRegisterPathSafety(t *testing.T) {
	h, root := newHub(t)
	outside := t.TempDir()
	externalRepo(t, filepath.Join(outside, "x"), mountmgr.ModeCopy)
	if _, err := h.RegisterPath("x", filepath.Join(outside, "x"), hub.RegisterOptions{}); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("root 外: %v", err)
	}
	if _, err := h.RegisterPath("root", root, hub.RegisterOptions{}); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("root 自身: %v", err)
	}
	if _, err := h.RegisterPath("ctl", filepath.Join(root, ".p37"), hub.RegisterOptions{}); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf(".p37: %v", err)
	}
	if _, err := h.RegisterPath("dotdot", filepath.Join(root, "..", "escape"), hub.RegisterOptions{}); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("..: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(outside, "x"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RegisterPath("link", link, hub.RegisterOptions{}); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := h.RegisterPath("missing", filepath.Join(root, "missing"), hub.RegisterOptions{InitIfMissing: true}); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("不存在目录不应自动创建: %v", err)
	}
}

func TestUnregisterKeepsDataAndRestores(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	e, err := h.RegisterPath("app", path, hub.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateLine("keep", "mainline", false); err != nil {
		t.Fatal(err)
	}

	res, err := h.Unregister("app", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Already || res.Entry == nil || res.Entry.ID != e.ID {
		t.Fatalf("unregister=%+v", res)
	}
	if es, _ := h.List(); len(es) != 0 {
		t.Fatalf("list=%+v", es)
	}
	if _, err := h.Open("app"); !errors.Is(err, hub.ErrRepoNotFound) {
		t.Fatalf("open after unregister=%v", err)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); err != nil {
		t.Fatalf("数据应保留: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "workspaces")); err != nil {
		t.Fatalf("workspaces 应保留: %v", err)
	}
	res, err = h.Unregister("app", false)
	if err != nil || !res.Already {
		t.Fatalf("重复 unregister 应幂等: %v %+v", err, res)
	}

	e2, err := h.RegisterPath("app", path, hub.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if e2.ID != e.ID {
		t.Fatalf("重新注册应保持 id: %s != %s", e2.ID, e.ID)
	}
	if _, err := h.Open("app"); err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if es, _ := h.List(); len(es) != 1 {
		t.Fatalf("list=%+v", es)
	}
}

func TestUnregisterBusyThenForce(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	if _, err := h.RegisterPath("app", path, hub.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WS.Open(workspace.OpenRequest{Parent: "mainline", TaskDesc: "busy"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.Unregister("app", false)
	var be *hub.BusyError
	if !errors.Is(err, hub.ErrRepoBusy) || !errors.As(err, &be) || len(be.Workspaces) == 0 {
		t.Fatalf("busy=%v", err)
	}
	if es, _ := h.List(); len(es) != 1 {
		t.Fatal("busy 时不应注销")
	}
	if _, err := h.Unregister("app", true); err != nil {
		t.Fatal(err)
	}
	if es, _ := h.List(); len(es) != 0 {
		t.Fatal("force 后应注销")
	}
	if _, err := os.Stat(filepath.Join(path, "workspaces")); err != nil {
		t.Fatalf("force 不应删除数据: %v", err)
	}
}

func TestScanNonEmptyRegistryAndPrune(t *testing.T) {
	h, root := newHub(t)
	a := filepath.Join(root, "a")
	externalRepo(t, a, mountmgr.ModeCopy)
	writeMeta(t, a, mustID(t), "a", "main")
	if res, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil || len(res.Added) != 1 {
		t.Fatalf("scan=%+v err=%v", res, err)
	}
	if res, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil || len(res.Added) != 0 {
		t.Fatalf("幂等 scan=%+v err=%v", res, err)
	}
	b := filepath.Join(root, "b")
	externalRepo(t, b, mountmgr.ModeCopy)
	writeMeta(t, b, mustID(t), "b", "main")
	if res, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil || len(res.Added) != 1 {
		t.Fatalf("非空 registry 发现新 repo: %+v err=%v", res, err)
	}
	if es, _ := h.List(); len(es) != 2 {
		t.Fatalf("list=%+v", es)
	}

	// 缺 repo.json：degraded，不删除。
	if err := os.Remove(filepath.Join(b, ".p37", "repo.json")); err != nil {
		t.Fatal(err)
	}
	res, err := h.Scan(hub.ScanOptions{Reconcile: true})
	if err != nil || len(res.Degraded) != 1 {
		t.Fatalf("missing repo.json: %+v %v", res, err)
	}
	if _, err := h.Get("b"); err != nil {
		t.Fatal("缺 repo.json 不应删除 registry 行")
	}

	// path 不存在：prune=false -> degraded；prune=true -> removed。
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if res, _ := h.Scan(hub.ScanOptions{Reconcile: true}); len(res.Degraded) != 2 {
		t.Fatalf("degraded=%+v", res.Degraded)
	}
	if res, err := h.Scan(hub.ScanOptions{Reconcile: true, PruneMissing: true}); err != nil {
		t.Fatalf("prune scan: %v", err)
	} else if len(res.Removed) != 1 {
		t.Fatalf("removed=%+v", res.Removed)
	}
	if _, err := h.Get("a"); !errors.Is(err, hub.ErrRepoNotFound) {
		t.Fatalf("prune 后应删除行: %v", err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal("prune 不应删除磁盘数据")
	}
}

func TestScanDryRunAndTombstone(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	writeMeta(t, path, mustID(t), "app", "main")
	res, err := h.Scan(hub.ScanOptions{Reconcile: true, DryRun: true})
	if err != nil || len(res.Added) != 1 || res.Reconciled {
		t.Fatalf("dry run=%+v %v", res, err)
	}
	if es, _ := h.List(); len(es) != 0 {
		t.Fatal("dry_run 不应写 registry")
	}
	if _, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Unregister("app", false); err != nil {
		t.Fatal(err)
	}
	res, err = h.Scan(hub.ScanOptions{Reconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != "unregistered" {
		t.Fatalf("tombstone 不应被 scan 复活: %+v", res)
	}
	if es, _ := h.List(); len(es) != 0 {
		t.Fatal("已注销 repo 不应出现在 list")
	}
}

func TestScanSkipsHiddenAndSymlink(t *testing.T) {
	h, root := newHub(t)
	hidden := filepath.Join(root, ".hidden")
	externalRepo(t, hidden, mountmgr.ModeCopy)
	writeMeta(t, hidden, mustID(t), "hidden", "main")
	target := filepath.Join(root, "target")
	externalRepo(t, target, mountmgr.ModeCopy)
	writeMeta(t, target, mustID(t), "target", "main")
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	res, err := h.Scan(hub.ScanOptions{Reconcile: true})
	if err != nil || len(res.Added) != 1 || res.Added[0].Name != "target" {
		t.Fatalf("scan=%+v err=%v", res, err)
	}
}

func TestDeleteMountpointGuard(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	writeMeta(t, path, mustID(t), "app", "main")
	if _, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	h.SetMountPointProbe(func(p string) bool { return filepath.Clean(p) == filepath.Clean(path) })
	if _, err := h.Delete("app", true); !errors.Is(err, hub.ErrRepoIsMountpoint) {
		t.Fatalf("mountpoint delete=%v", err)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); err != nil {
		t.Fatalf("mountpoint 拒绝后数据应完整: %v", err)
	}
	if es, _ := h.List(); len(es) != 1 {
		t.Fatal("mountpoint 拒绝后 registry 行应保留")
	}
}

func TestDeleteRejectsOutsideAndRoot(t *testing.T) {
	h, root := newHub(t)
	outside := t.TempDir()
	externalRepo(t, filepath.Join(outside, "x"), mountmgr.ModeCopy)
	if _, err := h.Register("legacy", filepath.Join(outside, "x"), "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Delete("legacy", true); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("root 外 delete=%v", err)
	}
	if _, err := h.Register("rootrepo", root, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Delete("rootrepo", true); !errors.Is(err, hub.ErrInvalidPath) {
		t.Fatalf("root delete=%v", err)
	}
}

func TestListAvailableDegradedWithoutOpen(t *testing.T) {
	h, root := newHub(t)
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	writeMeta(t, path, mustID(t), "app", "main")
	if _, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	es, err := h.List()
	if err != nil || len(es) != 1 || !es[0].Available || es[0].Degraded {
		t.Fatalf("list=%+v err=%v", es, err)
	}
	// 模拟卷未挂载：目录消失，不应为了 list 去 Open/repo 报错。
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	e, err := h.Get("app")
	if err != nil {
		t.Fatal(err)
	}
	if e.Available || !e.Degraded || e.DegradedMsg == "" {
		t.Fatalf("entry=%+v", e)
	}
}

func TestUnregisterSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	h, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	writeMeta(t, path, mustID(t), "app", "main")
	if _, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Unregister("app", false); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Close() }()
	if es, _ := h2.List(); len(es) != 0 {
		t.Fatalf("重启后已注销 repo 不应复活: %+v", es)
	}
	res, err := h2.Scan(hub.ScanOptions{Reconcile: true})
	if err != nil || len(res.Added) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("重启 scan=%+v err=%v", res, err)
	}
	if _, err := h2.RegisterPath("app", path, hub.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	if es, _ := h2.List(); len(es) != 1 {
		t.Fatalf("显式 register 后应恢复: %+v", es)
	}
}

func TestRestartRegistryAndScan(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	h, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "app")
	externalRepo(t, path, mountmgr.ModeCopy)
	writeMeta(t, path, mustID(t), "app", "main")
	if _, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Close() }()
	if es, _ := h2.List(); len(es) != 1 || es[0].Name != "app" {
		t.Fatalf("restart list=%+v", es)
	}
	if res, err := h2.Scan(hub.ScanOptions{Reconcile: true}); err != nil || len(res.Added) != 0 {
		t.Fatalf("restart scan 不应重复: %+v %v", res, err)
	}
}

func readMeta(path string) (struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DefaultLine string `json:"default_line"`
}, error) {
	var m struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DefaultLine string `json:"default_line"`
	}
	b, err := os.ReadFile(filepath.Join(path, ".p37", "repo.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := hub.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRegisterPathMismatchAndRebind(t *testing.T) {
	h, root := newHub(t)
	oldPath := filepath.Join(root, "repo_old")
	externalRepo(t, oldPath, mountmgr.ModeCopy)
	e, err := h.RegisterPath("app", oldPath, hub.RegisterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, "app")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}

	// 同名不同路径：默认必须显式失败，不能静默继续用旧路径。
	if _, err := h.RegisterPath("app", newPath, hub.RegisterOptions{}); !errors.Is(err, hub.ErrRepoPathMismatch) {
		t.Fatalf("期望 repo_path_mismatch，实际: %v", err)
	}
	// 未 rebind 时 registry 仍指向旧路径。
	if got, _ := h.Get("app"); got == nil || filepath.Clean(got.Path) != filepath.Clean(oldPath) {
		t.Fatalf("registry 不应被静默改动: %+v", got)
	}

	got, err := h.RegisterPath("app", newPath, hub.RegisterOptions{Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got.Path) != filepath.Clean(newPath) || got.ID != e.ID {
		t.Fatalf("rebind entry=%+v want id=%s path=%s", got, e.ID, newPath)
	}
	r, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	st, err := r.Status()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(st.Root) != filepath.Clean(newPath) {
		t.Fatalf("repo.root 未自愈: %s want %s", st.Root, newPath)
	}
}

func TestRecoverRegistryPrefersMatchingBasename(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "repo_old")
	newPath := filepath.Join(root, "app")
	writeMeta(t, oldPath, "repo_0123456789abcdef0123456789abcdef", "app", "main")
	writeMeta(t, newPath, "repo_0123456789abcdef0123456789abcdef", "app", "main")
	h, err := hub.New(root, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	e, err := h.Get("app")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(e.Path) != filepath.Clean(newPath) {
		t.Fatalf("恢复选了 %s，期望 %s", e.Path, newPath)
	}
	if len(h.RecoverWarnings()) == 0 {
		t.Fatal("多候选应产生 RecoverWarnings")
	}
}

func TestRebindBusyWithActiveWorkspace(t *testing.T) {
	h, root := newHub(t)
	oldPath := filepath.Join(root, "repo_old")
	externalRepo(t, oldPath, mountmgr.ModeCopy)
	if _, err := h.RegisterPath("app", oldPath, hub.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WS.Open(workspace.OpenRequest{Parent: "mainline", TaskDesc: "busy"}); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, "app")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	_, err = h.RegisterPath("app", newPath, hub.RegisterOptions{Rebind: true})
	if !errors.Is(err, hub.ErrRepoBusy) {
		t.Fatalf("期望 repo_busy，实际: %v", err)
	}
	if got, _ := h.Get("app"); got == nil || filepath.Clean(got.Path) != filepath.Clean(oldPath) {
		t.Fatalf("busy 时不应改 registry: %+v", got)
	}
}

func TestInitRepoPersistentGate(t *testing.T) {
	h, root := newHub(t)
	h.SetAllowNonPersistent(false)
	h.SetMountPointProbe(func(string) bool { return false })
	if _, err := h.InitRepo("app", filepath.Join(root, "app"), hub.InitOptions{}); !errors.Is(err, hub.ErrInvalidPath) {
		var pe *repo.PathError
		if !errors.As(err, &pe) || pe.Reason != "not_persistent" {
			t.Fatalf("期望 not_persistent，实际: %v", err)
		}
	}
	h.SetMountPointProbe(func(string) bool { return true })
	path := filepath.Join(root, "persist")
	if err := os.MkdirAll(filepath.Join(path, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := h.InitRepo("persist", path, hub.InitOptions{Mode: repo.InitEmpty})
	if err != nil {
		t.Fatal(err)
	}
	if res.Repo.VersionCount != 1 {
		t.Fatalf("res=%+v", res)
	}
}
