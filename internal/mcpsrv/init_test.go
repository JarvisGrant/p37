package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

func halfRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(path); err != nil {
		t.Fatal(err)
	}
	bs, err := cas.NewLocalFS(filepath.Join(path, ".p37"))
	if err != nil {
		t.Fatal(err)
	}
	store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())
	tid, err := store.PutTree(&tree.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	v := version.New(hash.Zero, tid, 0, "p37-init", "init", time.Now().UnixMilli(), "init")
	if err := db.InsertVersion(v); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return v.ID().String()
}

func TestRepoInitMCPEmptyAndIdempotent(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	if err := os.MkdirAll(filepath.Join(path, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := hh.mustOK("repo_init", map[string]any{
		"repo": "app", "path": path, "mode": "empty", "default_line": "main",
	})
	repo, _ := res["repo"].(map[string]any)
	if repo["version_count"] != float64(1) || repo["mainline_head"] == "" {
		t.Fatalf("repo_init=%v", res)
	}
	line, _ := res["line"].(map[string]any)
	if line["name"] != "main" || line["head"] != repo["mainline_head"] {
		t.Fatalf("line=%v", res)
	}
	again := hh.mustOK("repo_init", map[string]any{
		"repo": "app", "path": path, "mode": "empty", "default_line": "main",
	})
	repo2, _ := again["repo"].(map[string]any)
	if repo2["mainline_head"] != repo["mainline_head"] {
		t.Fatalf("idempotent changed head: %v -> %v", repo["mainline_head"], repo2["mainline_head"])
	}
	info := hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["initialized"] != true || info["default_line_ready"] != true {
		t.Fatalf("repo_info=%v", info)
	}
	lines := hh.mustOK("line_list", map[string]any{"repo": "app"})
	arr, _ := lines["_array"].([]any)
	if len(arr) != 1 || arr[0].(map[string]any)["name"] != "main" {
		t.Fatalf("line_list=%v", lines)
	}
	health := hh.mustOK("repo_health", map[string]any{"repo": "app", "with_quota": false})
	if health["initialized"] != true || health["default_line_ready"] != true {
		t.Fatalf("health=%v", health)
	}
	if health["version_count"] != float64(1) || health["mainline_head"] == "" {
		t.Fatalf("health=%v", health)
	}
}

func TestRepoInitRepairBadDB(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "p34-59")
	want := halfRepo(t, path)
	res := hh.mustOK("repo_init", map[string]any{
		"repo": "p34-59", "path": path, "mode": "repair",
	})
	repo, _ := res["repo"].(map[string]any)
	if repo["mainline_head"] != want || repo["version_count"] != float64(1) {
		t.Fatalf("repair=%v want head=%s", res, want)
	}
	db, err := meta.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	st, err := db.RootState()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Roots) != 1 || st.Roots[0].String() != want {
		t.Fatalf("roots=%v want %s", st.Roots, want)
	}
	if len(st.Lines) != 1 || st.Lines[0].Head.String() != want {
		t.Fatalf("lines=%+v", st.Lines)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "refs", "mainline")); err != nil {
		t.Fatalf("refs missing: %v", err)
	}
}

func TestRepoHealthUninitializedByPath(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "p34-59")
	want := halfRepo(t, path)
	h := hh.mustOK("repo_health", map[string]any{
		"path": path, "with_quota": false,
	})
	if h["initialized"] != false {
		t.Fatalf("health=%v", h)
	}
	if h["next_action"] != "repo_init_repair" {
		t.Fatalf("health=%v", h)
	}
	zero := "0000000000000000000000000000000000000000000000000000000000000000"
	if h["mainline_head"] != "" && h["mainline_head"] != zero && h["mainline_head"] != want {
		t.Fatalf("health=%v", h)
	}
}

func TestRepoRegisterEmptyLeavesNoDB(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	e := hh.mustFail("repo_register", "repo_not_initialized", map[string]any{
		"repo": "app", "path": path,
	})
	detail, _ := e["detail"].(map[string]any)
	if detail["next_action"] != "repo_init" {
		t.Fatalf("detail=%v", detail)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); !os.IsNotExist(err) {
		t.Fatalf("repo_register 不应留下 DB: %v", err)
	}
}

func TestRepoRegisterInitIfMissingComplete(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	e := hh.mustOK("repo_register", map[string]any{
		"repo": "app", "path": path, "init_if_missing": true,
	})
	if e["available"] != true {
		t.Fatalf("register=%v", e)
	}
	info := hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["version_count"] != float64(1) || info["initialized"] != true {
		t.Fatalf("info=%v", info)
	}
}

func TestRepoImportUninitializedRegistered(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// 先注册成完整仓库，再把 .p37 移走，模拟“已注册但未初始化”。
	hh.mustOK("repo_register", map[string]any{
		"repo": "app", "path": path, "init_if_missing": true,
	})
	if err := os.RemoveAll(filepath.Join(path, ".p37")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := hh.mustOK("repo_import", map[string]any{"repo": "app", "dir": path})
	if res["initialized"] != true || res["version_id"] == "" {
		t.Fatalf("import=%v", res)
	}
	info := hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["version_count"] != float64(1) {
		t.Fatalf("info=%v", info)
	}
}

func TestRepoScanRepair(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	hh.mustOK("repo_init", map[string]any{"repo": "app", "path": path, "mode": "empty"})
	// 破坏 line + refs，保留 head/version：模拟可修复的半初始化。
	db, err := meta.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteLine("main", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, ".p37", "refs", "mainline")); err != nil {
		t.Fatal(err)
	}
	res := hh.mustOK("repo_scan", map[string]any{"reconcile": true, "repair": true})
	if len(res["repaired"].([]any)) == 0 {
		t.Fatalf("scan=%v", res)
	}
	info := hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["initialized"] != true || info["version_count"] != float64(1) {
		t.Fatalf("info=%v", info)
	}
}

func TestRepoRegisterHalfRepoStructured(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "p34-59")
	want := halfRepo(t, path)
	e := hh.mustFail("repo_register", "repo_not_initialized", map[string]any{
		"repo": "p34-59", "path": path,
	})
	detail, _ := e["detail"].(map[string]any)
	if detail["next_action"] != "repo_init_repair" {
		t.Fatalf("detail=%v", detail)
	}
	// 不能因为注册失败就留下 registry 行。
	if env, err := hh.callSafe("repo_info", map[string]any{"repo": "p34-59"}); err == nil {
		if ok, _ := env["ok"].(bool); ok {
			t.Fatal("半成品不应被注册")
		}
	}
	// init_if_missing 兼容路径必须完整修复。
	e2 := hh.mustOK("repo_register", map[string]any{
		"repo": "p34-59", "path": path, "init_if_missing": true,
	})
	if e2["available"] != true {
		t.Fatalf("register=%v", e2)
	}
	info := hh.mustOK("repo_info", map[string]any{"repo": "p34-59"})
	if info["version_count"] != float64(1) || info["mainline_head"] != want {
		t.Fatalf("info=%v want=%s", info, want)
	}
}
