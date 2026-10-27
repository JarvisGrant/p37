package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
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

func importedHalfMCP(t *testing.T, path, worktree string) string {
	t.Helper()
	r, err := repo.Init(path, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := r.DB.Head()
	_ = r.Close()
	if worktree != "" {
		if err := os.WriteFile(filepath.Join(path, "a.txt"), []byte(worktree), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := meta.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteLine("main", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.Remove(filepath.Join(path, ".p37", "refs", "mainline")); err != nil {
		t.Fatal(err)
	}
	return head.String()
}

func TestRepoInitUserFileRules(t *testing.T) {
	hh, _ := newMultiHarness(t)
	// 无 DB + 用户文件：empty 拒绝。
	p1 := filepath.Join(hh.root, "p1")
	if err := os.MkdirAll(p1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p1, "user.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := hh.mustFail("repo_init", "repo_not_empty", map[string]any{
		"repo": "p1", "path": p1, "mode": "empty"})
	detail, _ := e["detail"].(map[string]any)
	if detail["next_action"] != "repo_init_import" {
		t.Fatalf("detail=%v", detail)
	}
	if _, err := os.Stat(filepath.Join(p1, ".p37", "p37.db")); !os.IsNotExist(err) {
		t.Fatalf("empty 拒绝后不应有 DB: %v", err)
	}
	// import 成功。
	res := hh.mustOK("repo_init", map[string]any{
		"repo": "p1", "path": p1, "mode": "import"})
	repoInfo, _ := res["repo"].(map[string]any)
	if repoInfo["version_count"] != float64(1) || res["imported"] != true {
		t.Fatalf("import=%v", res)
	}

	// 半 DB + 空 v0 + 用户文件：empty/repair 拒绝，import 成功。
	p2 := filepath.Join(hh.root, "p2")
	hh.mustOK("repo_init", map[string]any{"repo": "p2", "path": p2, "mode": "empty"})
	if err := os.WriteFile(filepath.Join(p2, "user.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(p2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteLine("main", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_ = os.Remove(filepath.Join(p2, ".p37", "refs", "mainline"))
	for _, mode := range []string{"empty", "repair"} {
		e := hh.mustFail("repo_init", "repo_not_empty", map[string]any{
			"repo": "p2", "path": p2, "mode": mode})
		detail, _ := e["detail"].(map[string]any)
		if detail["next_action"] != "repo_init_import" {
			t.Fatalf("mode=%s detail=%v", mode, detail)
		}
	}
	res2 := hh.mustOK("repo_init", map[string]any{
		"repo": "p2", "path": p2, "mode": "import"})
	if res2["imported"] != true {
		t.Fatalf("import=%v", res2)
	}
}

func TestRepoInitUserFileForce(t *testing.T) {
	hh, _ := newMultiHarness(t)
	p1 := filepath.Join(hh.root, "same")
	old := importedHalfMCP(t, p1, "A\n")
	res := hh.mustOK("repo_init", map[string]any{
		"repo": "same", "path": p1, "mode": "import"})
	if res["repaired"] != true || res["imported"] == true {
		t.Fatalf("same content should repair only: %v", res)
	}
	info := hh.mustOK("repo_info", map[string]any{"repo": "same"})
	if info["mainline_head"] != old || info["version_count"] != float64(1) {
		t.Fatalf("info=%v old=%s", info, old)
	}

	p2 := filepath.Join(hh.root, "diff")
	old2 := importedHalfMCP(t, p2, "B\n")
	e := hh.mustFail("repo_init", "repo_not_empty", map[string]any{
		"repo": "diff", "path": p2, "mode": "import"})
	detail, _ := e["detail"].(map[string]any)
	if detail["next_action"] != "repo_init_import_force" {
		t.Fatalf("detail=%v", detail)
	}
	res2 := hh.mustOK("repo_init", map[string]any{
		"repo": "diff", "path": p2, "mode": "import", "force": true})
	if res2["imported"] != true {
		t.Fatalf("force=%v", res2)
	}
	info2 := hh.mustOK("repo_info", map[string]any{"repo": "diff"})
	if info2["mainline_head"] == old2 || info2["version_count"] != float64(1) {
		t.Fatalf("info=%v old=%s", info2, old2)
	}
}

func TestRepoHealthUserFileNextAction(t *testing.T) {
	hh, _ := newMultiHarness(t)
	// 半 DB + 空 v0 + 用户文件。
	p1 := filepath.Join(hh.root, "p1")
	hh.mustOK("repo_init", map[string]any{"repo": "p1", "path": p1, "mode": "empty"})
	if err := os.WriteFile(filepath.Join(p1, "u.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, _ := meta.Open(p1)
	_ = db.DeleteLine("main", time.Now().UnixMilli())
	_ = db.Close()
	_ = os.Remove(filepath.Join(p1, ".p37", "refs", "mainline"))
	h := hh.mustOK("repo_health", map[string]any{"path": p1, "with_quota": false})
	if h["initialized"] != false || h["next_action"] != "repo_init_import" {
		t.Fatalf("health=%v", h)
	}
	if h["user_files"] == float64(0) {
		t.Fatalf("health=%v", h)
	}

	// 半 DB + 非空 v0 + worktree 空：repair 安全，不自动 import force。
	p2 := filepath.Join(hh.root, "p2")
	importedHalfMCP(t, p2, "")
	h2 := hh.mustOK("repo_health", map[string]any{"path": p2, "with_quota": false})
	if h2["initialized"] != false || h2["next_action"] != "repo_init_repair" {
		t.Fatalf("health=%v", h2)
	}
	if h2["root_tree_empty"] != false || h2["user_files"] != float64(0) {
		t.Fatalf("health=%v", h2)
	}
}

func TestRepoInitIdempotencyMCP(t *testing.T) {
	hh, _ := newMultiHarness(t)
	p1 := filepath.Join(hh.root, "p1")
	res := hh.mustOK("repo_init", map[string]any{
		"repo": "p1", "path": p1, "mode": "empty", "idempotency_key": "k1"})
	repo1, _ := res["repo"].(map[string]any)
	again := hh.mustOK("repo_init", map[string]any{
		"repo": "p1", "path": p1, "mode": "empty", "idempotency_key": "k1"})
	repo2, _ := again["repo"].(map[string]any)
	if repo1["mainline_head"] != repo2["mainline_head"] {
		t.Fatalf("idempotent changed head: %v -> %v", repo1["mainline_head"], repo2["mainline_head"])
	}
	e := hh.mustFail("repo_init", "idempotency_conflict", map[string]any{
		"repo": "p1", "path": p1, "mode": "import", "idempotency_key": "k1"})
	detail, _ := e["detail"].(map[string]any)
	if detail["field"] != "mode" {
		t.Fatalf("detail=%v", detail)
	}
	p2 := filepath.Join(hh.root, "p2")
	hh.mustFail("repo_init", "idempotency_conflict", map[string]any{
		"repo": "p2", "path": p2, "mode": "empty", "idempotency_key": "k1"})
	if _, err := os.Stat(filepath.Join(p2, ".p37", "p37.db")); !os.IsNotExist(err) {
		t.Fatalf("冲突不应创建 p2: %v", err)
	}
}

func TestRepoRegisterInitIfMissingUserFile(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "u.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := hh.mustFail("repo_register", "repo_not_empty", map[string]any{
		"repo": "app", "path": path, "init_if_missing": true})
	detail, _ := e["detail"].(map[string]any)
	if detail["next_action"] != "repo_init_import" {
		t.Fatalf("detail=%v", detail)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); !os.IsNotExist(err) {
		t.Fatalf("register 不应留下 DB: %v", err)
	}
}

func TestRepoRegisterKeyOnlyOnInit(t *testing.T) {
	hh, _ := newMultiHarness(t)
	p1 := filepath.Join(hh.root, "p1")
	if err := os.MkdirAll(p1, 0o755); err != nil {
		t.Fatal(err)
	}
	hh.mustOK("repo_register", map[string]any{
		"repo": "p1", "path": p1, "init_if_missing": true, "idempotency_key": "kreg"})
	hh.mustFail("repo_init", "idempotency_conflict", map[string]any{
		"repo": "p1", "path": p1, "mode": "import", "idempotency_key": "kreg"})
	// strict register 忽略 key。
	p2 := filepath.Join(hh.root, "p2")
	hh.mustOK("repo_init", map[string]any{"repo": "p2", "path": p2, "mode": "empty"})
	hh.mustOK("repo_register", map[string]any{
		"repo": "p2", "path": p2, "idempotency_key": "k-strict"})
}

func TestRepoHealthRefsMatchAfterDefaultLand(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_ensure", map[string]any{"name": "refs-health"})
	dir := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustOK("ws_land", map[string]any{"ws_id": w["ws_id"].(string)})
	hh := h.mustOK("repo_health", map[string]any{"with_quota": false})
	if hh["initialized"] != true || hh["refs_ok"] != true || hh["refs_match"] != true {
		t.Fatalf("health=%v", hh)
	}
}

func TestRepoImportForceSameStateMachine(t *testing.T) {
	hh, _ := newMultiHarness(t)
	p := filepath.Join(hh.root, "imp")
	hh.mustOK("repo_init", map[string]any{"repo": "imp", "path": p, "mode": "empty"})
	r, err := repo.Open(p, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err := os.WriteFile(filepath.Join(p, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, _ := meta.Open(p)
	_ = db.DeleteLine("main", time.Now().UnixMilli())
	_ = db.Close()
	_ = os.Remove(filepath.Join(p, ".p37", "refs", "mainline"))
	e := hh.mustFail("repo_import", "repo_not_empty", map[string]any{
		"repo": "imp", "dir": p})
	detail, _ := e["detail"].(map[string]any)
	if detail["next_action"] != "repo_init_import_force" {
		t.Fatalf("detail=%v", detail)
	}
	res := hh.mustOK("repo_import", map[string]any{
		"repo": "imp", "dir": p, "force": true})
	if res["imported"] != true {
		t.Fatalf("import=%v", res)
	}
}
