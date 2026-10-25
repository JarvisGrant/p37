package repo_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
)

func mustLandInto(t *testing.T, e *env, child, parent string) *repo.LandResult {
	t.Helper()
	res, err := e.r.LandInto(child, parent, "", "", "")
	if err != nil {
		t.Fatalf("LandInto: %v", err)
	}
	return res
}

// TestLandIntoPreservesParentOnlyChanges 覆盖验收用例 1：
// 父改 A、子只改 B，子运行期间父把 A 改成新版本；land 后 A 必须是新版本。
func TestLandIntoPreservesParentOnlyChanges(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A-v1\n")
	e.seed("b.txt", "B\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	// 子工作区在父的 A-v1 上分叉。
	e.write(parent.ID, "a.txt", "A-v1\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	child := e.open(parent.ID, "child")
	e.write(child.ID, "b.txt", "B-child\n")

	// 父在子运行期间改 A。
	e.write(parent.ID, "a.txt", "A-v2\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(filepath.Join(e.r.WS.Dir(parent.ID), "a.txt"))
	if err != nil {
		t.Fatal(err)
	}

	res := mustLandInto(t, e, child.ID, parent.ID)
	if got := e.read(parent.ID, "a.txt"); got != "A-v2\n" {
		t.Fatalf("父的 A 被覆盖: %q，期望 A-v2", got)
	}
	afterStat, err := os.Stat(filepath.Join(e.r.WS.Dir(parent.ID), "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatalf("父独占路径被无谓重写: mtime %v -> %v", beforeStat.ModTime(), afterStat.ModTime())
	}
	if got := e.read(parent.ID, "b.txt"); got != "B-child\n" {
		t.Fatalf("子的 B 没有并入: %q", got)
	}
	if want := []string{"a.txt"}; !equalStrings(res.ParentOnlyChangedPaths, want) {
		t.Fatalf("parent_only_changed_paths = %v，期望 %v", res.ParentOnlyChangedPaths, want)
	}
	if want := []string{"b.txt"}; !equalStrings(res.ChildOnlyChangedPaths, want) {
		t.Fatalf("child_only_changed_paths = %v，期望 %v", res.ChildOnlyChangedPaths, want)
	}
	if want := []string{"b.txt"}; !equalStrings(res.AppliedPaths, want) {
		t.Fatalf("applied_paths = %v，期望 %v（不能写 a.txt）", res.AppliedPaths, want)
	}
}

// TestLandIntoDisjointHunksAutoMerge 覆盖验收用例 2：
// 父子改同一文件的不同位置，必须给出明确的合并结果，并列出 both_changed/auto_merged。
func TestLandIntoDisjointHunksAutoMerge(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "l1\nl2\nl3\nl4\nl5\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child") // 子基于 l1..l5
	e.write(child.ID, "a.txt", "l1\nl2\nl3\nl4\nl5-child\n")
	e.write(parent.ID, "a.txt", "l1-parent\nl2\nl3\nl4\nl5\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}

	res := mustLandInto(t, e, child.ID, parent.ID)
	if !res.CanLand {
		t.Fatalf("不重叠改动应可自动合并，conflicts=%+v", res.Conflicts)
	}
	got := e.read(parent.ID, "a.txt")
	if !strings.Contains(got, "l1-parent") || !strings.Contains(got, "l5-child") {
		t.Fatalf("自动合并结果 = %q，应同时包含父子改动", got)
	}
	if want := []string{"a.txt"}; !equalStrings(res.BothChangedPaths, want) {
		t.Fatalf("both_changed_paths = %v，期望 %v", res.BothChangedPaths, want)
	}
	// 不重叠行的结果既可能是 place 直接定位，也可能走 autoMerge；
	// 两条路都必须把父子改动都保留（已在上面断言），这里只要求路径被标记为
	// both_changed。auto_merged_paths 是可选的可观测字段。
	for _, p := range res.AutoMergedPaths {
		if p != "a.txt" {
			t.Fatalf("auto_merged_paths 出现了意外路径: %v", res.AutoMergedPaths)
		}
	}
}

// TestLandIntoSameLineConflict 覆盖验收用例 3：同一行双方都改必须 conflict。
func TestLandIntoSameLineConflict(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "l1\nl2\nl3\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "a.txt", "l1\nchild\nl3\n")
	e.write(parent.ID, "a.txt", "l1\nparent\nl3\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}

	res := mustLandInto(t, e, child.ID, parent.ID)
	if res.CanLand || res.State != "conflict" {
		t.Fatalf("同一行修改应冲突，res=%+v", res)
	}
	if len(res.Conflicts) == 0 || res.Conflicts[0].Path != "a.txt" {
		t.Fatalf("冲突必须带 path=a.txt: %+v", res.Conflicts)
	}
	if want := []string{"a.txt"}; !equalStrings(res.ConflictPaths, want) {
		t.Fatalf("conflict_paths = %v，期望 %v", res.ConflictPaths, want)
	}
	// 冲突不落盘。
	if got := e.read(parent.ID, "a.txt"); got != "l1\nparent\nl3\n" {
		t.Fatalf("冲突时父文件被改动: %q", got)
	}
	// paths[] 里应有三方 blob 坐标。
	var conflictPath *repo.PathState
	for i := range res.Paths {
		if res.Paths[i].Path == "a.txt" {
			conflictPath = &res.Paths[i]
		}
	}
	if conflictPath == nil || conflictPath.State != "conflict" ||
		conflictPath.Base == "" || conflictPath.Parent == "" || conflictPath.Child == "" {
		t.Fatalf("paths[] 缺少三方坐标: %+v", conflictPath)
	}
}

// TestLandIntoStrictMergeConflictsOnBothChanged 验证 merge_policy=strict。
func TestLandIntoStrictMergeConflictsOnBothChanged(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "l1\nl2\nl3\nl4\nl5\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "a.txt", "l1\nl2\nl3\nl4\nchild\n")
	e.write(parent.ID, "a.txt", "parent\nl2\nl3\nl4\nl5\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.LandIntoWithOptions(child.ID, repo.LandIntoOptions{
		ParentWsID: parent.ID, StrictMerge: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CanLand {
		t.Fatalf("strict 模式下同路径双改必须冲突: %+v", res)
	}
	if want := []string{"a.txt"}; !equalStrings(res.BothChangedPaths, want) {
		t.Fatalf("both_changed_paths = %v", res.BothChangedPaths)
	}
}

// TestLandIntoAllowedPaths 覆盖验收用例 4：越界路径在 preview 里可见、land 拒绝。
func TestLandIntoAllowedPaths(t *testing.T) {
	e := newEnv(t)
	e.seed("src/b.ts", "B\n")
	e.seed("secret/c.ts", "C\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "src/b.ts", "B-child\n")
	e.write(child.ID, "secret/c.ts", "C-child\n")
	opt := repo.LandIntoOptions{ParentWsID: parent.ID, AllowedPrefixes: []string{"src"}}

	pv, err := e.r.PreviewIntoWithOptions(child.ID, opt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"secret/c.ts"}; !equalStrings(pv.OutOfScopePaths, want) {
		t.Fatalf("out_of_scope_paths = %v，期望 %v", pv.OutOfScopePaths, want)
	}
	if _, err := e.r.LandIntoWithOptions(child.ID, opt); !errors.Is(err, repo.ErrPathNotAllowed) {
		t.Fatalf("land 应拒绝越界路径，err=%v", err)
	}
	if got := e.read(parent.ID, "secret/c.ts"); got != "C\n" {
		t.Fatalf("越界文件被写入: %q", got)
	}
	// 去掉限制后可以正常落地。
	if _, err := e.r.LandIntoWithOptions(child.ID, repo.LandIntoOptions{ParentWsID: parent.ID}); err != nil {
		t.Fatal(err)
	}
	if got := e.read(parent.ID, "secret/c.ts"); got != "C-child\n" {
		t.Fatalf("解除限制后应并入: %q", got)
	}
}

// TestLandIntoIdempotentReplay 覆盖验收用例 5：同一 key 重复 land 不重复应用。
func TestLandIntoIdempotentReplay(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "b.txt", "B\n")
	opt := repo.LandIntoOptions{ParentWsID: parent.ID, IdempotencyKey: "child-1"}

	if _, err := e.r.LandIntoWithOptions(child.ID, opt); err != nil {
		t.Fatal(err)
	}
	res2, err := e.r.LandIntoWithOptions(child.ID, opt)
	if err != nil {
		t.Fatalf("重放应成功: %v", err)
	}
	if !res2.Replayed || !res2.AlreadyLanded {
		t.Fatalf("第二次应为幂等重放: %+v", res2)
	}
	if got := e.read(parent.ID, "b.txt"); got != "B\n" {
		t.Fatalf("重复 land 改坏了父文件: %q", got)
	}
}

// TestLandIntoExpectedParentTreeMoved 覆盖验收用例 6：
// 父在 preview 之后变化，expected_parent_tree 必须拒绝，且不写盘。
func TestLandIntoExpectedParentTreeMoved(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	e.seed("b.txt", "B\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "c.txt", "C\n")

	pv, err := e.r.PreviewInto(child.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 父在 preview 之后被改。
	e.write(parent.ID, "a.txt", "A-new\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}

	_, err = e.r.LandIntoWithOptions(child.ID, repo.LandIntoOptions{
		ParentWsID: parent.ID, ExpectedParentTree: pv.TargetHead.String(),
	})
	if !errors.Is(err, repo.ErrTargetMoved) {
		t.Fatalf("expected_parent_tree 失配应返回 target_moved，err=%v", err)
	}
	var te *repo.TokenMismatchError
	if !errors.As(err, &te) || te.Field != "parent_tree" {
		t.Fatalf("应带 TokenMismatchError: %v", err)
	}
	if got := e.read(parent.ID, "a.txt"); got != "A-new\n" {
		t.Fatalf("失配时不应写盘: %q", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
