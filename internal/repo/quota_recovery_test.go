package repo_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// newEnvOpt 允许用自定义上限初始化仓库（newEnv 走默认值）。
func newEnvOpt(t *testing.T, opt repo.Options) *env {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()
	if opt.Materialize == "" {
		opt.Materialize = mountmgr.ModeAuto
	}
	r, err := repo.Init(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &env{t: t, root: root, src: src, r: r}
}

// TestQuotaCountsOnlyActive 钉住 A1：已落地（drop_workspace=false）的行
// 不再占用 max_active。
func TestQuotaCountsOnlyActive(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 4})
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		w := e.open("", "round")
		e.write(w.ID, "a.txt", "R"+string(rune('0'+i))+"\n")
		if _, err := e.r.LandTo(w.ID, repo.LandOptions{DropWorkspace: true}); err != nil {
			t.Fatal(err)
		}
		if n, _ := e.r.DB.CountVisibleWorkspaces(); n != 0 {
			t.Fatalf("第 %d 轮 land 后 active=%d，期望 0", i, n)
		}
	}

	// 保留工作区（只读 landed）不应挤占额度。
	w := e.open("", "keep")
	e.write(w.ID, "a.txt", "KEEP\n")
	if _, err := e.r.LandTo(w.ID, repo.LandOptions{DropWorkspace: false}); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.r.DB.CountVisibleWorkspaces(); n != 0 {
		t.Fatalf("落地保留的工作区仍占额度: active=%d", n)
	}
	if n, _ := e.r.DB.CountLandedWorkspaces(); n != 1 {
		t.Fatalf("landed 计数 = %d，期望 1", n)
	}
	// 额度没有泄漏：还能继续 open。
	if _, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "after-landed"}); err != nil {
		t.Fatalf("landed 工作区挤占了配额: %v", err)
	}
	if q, err := e.r.WS.Quota(); err != nil {
		t.Fatal(err)
	} else if q.ActiveUsed != 1 || q.ActiveAvailable != 3 || q.Landed != 1 {
		t.Fatalf("quota = %+v", q)
	}
}

// TestDropFreesQuotaWhenDirCleanupFails 钉住 A2：目录清理失败不得阻塞元数据
// 删除；错误降级为 ErrCleanupFailed。
func TestDropFreesQuotaWhenDirCleanupFails(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 2})
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	w := e.open("", "x")
	parentDir := filepath.Dir(e.r.WS.Dir(w.ID))
	if err := os.Chmod(parentDir, 0o555); err != nil {
		t.Skipf("无法构造只读目录: %v", err)
	}
	defer func() { _ = os.Chmod(parentDir, 0o755) }()

	_, _, err := e.r.WS.Drop(w.ID, false, false)
	if !errors.Is(err, workspace.ErrCleanupFailed) {
		t.Fatalf("Drop err = %v，期望 ErrCleanupFailed", err)
	}
	if _, gerr := e.r.DB.GetWorkspace(w.ID); !errors.Is(gerr, meta.ErrNotExist) {
		t.Fatalf("行仍在: %v", gerr)
	}
	if n, _ := e.r.DB.CountVisibleWorkspaces(); n != 0 {
		t.Fatalf("配额未回收: active=%d", n)
	}
	// 恢复权限后孤儿目录应能被兜底清理。
	if err := os.Chmod(parentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reaped, err := e.r.WS.ReapOrphanDirs()
	if err != nil {
		t.Fatalf("ReapOrphanDirs: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != w.ID {
		t.Fatalf("reaped = %v，期望 [%s]", reaped, w.ID)
	}
	if _, serr := os.Stat(e.r.WS.Dir(w.ID)); !os.IsNotExist(serr) {
		t.Fatalf("孤儿目录仍存在: %v", serr)
	}
}

// TestQuotaLimitErrorCarriesUsage 钉住 B1：配额的 used/limit 必须结构化返回。
func TestQuotaLimitErrorCarriesUsage(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 1})
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "one"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "two"})
	var le *workspace.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v，期望 *workspace.LimitError", err)
	}
	if le.Scope != workspace.ScopeActive || le.Used != 1 || le.Limit != 1 {
		t.Fatalf("LimitError = %+v", le)
	}
	if !errors.Is(err, workspace.ErrLimitExceeded) {
		t.Fatal("LimitError 必须仍匹配 ErrLimitExceeded")
	}
}

// TestFanoutIgnoresLandedAndHidden 钉住 A5：已落地/隐藏工作区不占 fanout。
func TestFanoutIgnoresLandedAndHidden(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 8, MaxFanout: 1})
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	// 一个隐藏 preview 子工作区（历史遗留形态）不应占 fanout。
	if err := e.r.DB.InsertWorkspace(meta.Workspace{
		ID: "ws_00000000000000aa", Name: "preview", ParentKind: meta.ParentWorkspace,
		ParentID: parent.ID, BaseTree: parent.BaseTree, BaseVersion: parent.BaseVersion,
		State: meta.StateConflicted, Materialize: meta.MaterializeCopy,
		Hidden: true, Kind: "preview", CreatedTS: 1, UpdatedTS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	child := e.open(parent.ID, "child")
	// 落地并保留 child 后，它也不应占 fanout。
	e.write(child.ID, "b.txt", "B\n")
	if _, err := e.r.LandInto(child.ID, parent.ID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// child 已删除；再开一个子工作区必须成功（fanout=1 已被隐藏行占满也不该失败）。
	if _, err := e.r.WS.Open(workspace.OpenRequest{Parent: parent.ID, TaskDesc: "second"}); err != nil {
		t.Fatalf("fanout 把隐藏/已落地工作区算进去了: %v", err)
	}
}

// TestLandIntoConflictMarksChildAndResolves 覆盖 C + D：
//   - 冲突挂在子工作区上（conflict_list(child) 可查）；
//   - 直接 land_into 把子工作区置为 conflicted；
//   - 解决后带 preview_id 重试必须成功（key 稳定）；
//   - 成功后子工作区行/目录删除、配额回收。
func TestLandIntoConflictMarksChildAndResolves(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 4})
	e.seed("a.txt", "A\n")
	e.seed("keep.txt", "K\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "a.txt", "CHILD\n")
	e.rm(parent.ID, "a.txt")

	res, err := e.r.LandInto(child.ID, parent.ID, "", "", "")
	if err != nil {
		t.Fatalf("冲突不应返回 error: %v", err)
	}
	if res.CanLand || len(res.Conflicts) == 0 || res.State != "conflict" {
		t.Fatalf("res = %+v", res)
	}
	if res.PreviewID != child.ID {
		t.Fatalf("preview_id = %q，期望子工作区 id %q", res.PreviewID, child.ID)
	}
	cw, _ := e.r.DB.GetWorkspace(child.ID)
	if cw == nil || cw.State != meta.StateConflicted {
		t.Fatalf("子工作区状态 = %+v，期望 conflicted", cw)
	}
	rows, err := e.r.Conflicts(child.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("conflict_list(child) 查不到冲突")
	}
	// 冲突中父工作区必须被挡住，并带结构化列表。
	_, err = e.r.LandTo(parent.ID, repo.LandOptions{})
	var ace *repo.ActiveChildrenError
	if !errors.As(err, &ace) {
		t.Fatalf("父 land err = %v，期望 ActiveChildrenError", err)
	}
	if len(ace.Children) != 1 || ace.Children[0].WSID != child.ID ||
		ace.Children[0].State != meta.StateConflicted {
		t.Fatalf("blocked_by_children = %+v", ace.Children)
	}

	key, err := hash.Parse(rows[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Resolve(key, op.ResolveTakeUpper, nil, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.r.ReopenIfNoConflicts(child.ID); err != nil {
		t.Fatal(err)
	}
	// 关键：带 preview_id（此时 == child）重试，冲突 key 必须保持一致。
	res2, err := e.r.LandInto(child.ID, parent.ID, res.TargetHead.String(), res.PreviewID, "")
	if err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	if !res2.CanLand || len(res2.Conflicts) != 0 {
		t.Fatalf("解决后仍冲突（key 不稳定）: %+v", res2.Conflicts)
	}
	if _, err := e.r.DB.GetWorkspace(child.ID); !errors.Is(err, meta.ErrNotExist) {
		t.Fatalf("成功后子工作区行仍在: %v", err)
	}
	if n, _ := e.r.DB.CountVisibleWorkspaces(); n != 1 {
		t.Fatalf("成功后 active=%d，期望只剩父工作区", n)
	}
}

// TestLandIntoConflictCyclesDoNotLeakQuota 验收：反复「冲突 → 解决 → 再 land」
// 不减少可用额度。
func TestLandIntoConflictCyclesDoNotLeakQuota(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 4})
	for _, f := range []string{"f0.txt", "f1.txt", "f2.txt"} {
		e.seed(f, f+"\n")
	}
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	for _, f := range []string{"f0.txt", "f1.txt", "f2.txt"} {
		child := e.open(parent.ID, "child")
		e.write(child.ID, f, "CHILD\n")
		e.rm(parent.ID, f)
		res, err := e.r.LandInto(child.ID, parent.ID, "", "", "")
		if err != nil || res.CanLand || res.State != "conflict" {
			t.Fatalf("%s 首轮: err=%v res=%+v", f, err, res)
		}
		rows, err := e.r.Conflicts(child.ID, true)
		if err != nil || len(rows) == 0 {
			t.Fatalf("%s conflict_list: err=%v rows=%d", f, err, len(rows))
		}
		key, err := hash.Parse(rows[0].Key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.r.Resolve(key, op.ResolveTakeUpper, nil, "test"); err != nil {
			t.Fatal(err)
		}
		if err := e.r.ReopenIfNoConflicts(child.ID); err != nil {
			t.Fatal(err)
		}
		res2, err := e.r.LandInto(child.ID, parent.ID, res.TargetHead.String(), res.PreviewID, "")
		if err != nil || !res2.CanLand {
			t.Fatalf("%s 重试: err=%v res=%+v", f, err, res2)
		}
		if n, _ := e.r.DB.CountVisibleWorkspaces(); n != 1 {
			t.Fatalf("%s 之后 active=%d，期望只剩父工作区", f, n)
		}
	}
	// 父工作区一定能 land，不会被子工作区死锁。
	e.write(parent.ID, "keep.txt", "PARENT\n")
	if _, err := e.r.LandTo(parent.ID, repo.LandOptions{DropWorkspace: true}); err != nil {
		t.Fatalf("父工作区无法 land: %v", err)
	}
}

// TestConcurrentOpenDoesNotExceedQuota 验证并发 open 的准入是原子的：
// 8 个并发请求 + max_active=2 只能成功 2 个。
func TestConcurrentOpenDoesNotExceedQuota(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 2})
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "concurrent"}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("并发 open 成功 %d 个，期望 2（errs=%v）", ok, errs)
	}
	if n, _ := e.r.DB.CountVisibleWorkspaces(); n != 2 {
		t.Fatalf("active=%d，期望 2", n)
	}
}

// TestPreviewIntoKeepsChildActiveAndListsConflicts 预览不应改变子工作区状态，
// 但冲突必须挂在子工作区上（D-b）。
func TestPreviewIntoKeepsChildActiveAndListsConflicts(t *testing.T) {
	e := newEnvOpt(t, repo.Options{MaxActive: 4})
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "a.txt", "CHILD\n")
	e.rm(parent.ID, "a.txt")

	res, err := e.r.PreviewInto(child.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.PreviewID != child.ID || res.State != "conflict" {
		t.Fatalf("res = %+v", res)
	}
	cw, _ := e.r.DB.GetWorkspace(child.ID)
	if cw.State != meta.StateActive {
		t.Fatalf("preview 不应改状态，得到 %s", cw.State)
	}
	rows, err := e.r.Conflicts(child.ID, true)
	if err != nil || len(rows) == 0 {
		t.Fatalf("预览冲突不可见: err=%v rows=%d", err, len(rows))
	}
}
