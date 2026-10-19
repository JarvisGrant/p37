package repo_test

import (
	"errors"
	"testing"

	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestStackThreeLevelCollapsesToOneVersion 覆盖三层栈的端到端语义。
//
// 栈的核心承诺：一个整合事件（land）把**整条链**折叠成**一个**版本。
// 中间层的「过程记录」不产生版本，落地后焚毁。
//
// main(v1) ─ w1 ─ w2 ─ w3        land(w3) ⇒ v2，且 w1/w2/w3 全部消失
func TestStackThreeLevelCollapsesToOneVersion(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	e.seed("dir/b.txt", "B\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head1, _, _ := e.r.Head()

	w1 := e.open("", "一层")
	e.write(w1.ID, "w1.txt", "one\n")

	w2 := e.open(w1.ID, "二层")
	e.write(w2.ID, "w2.txt", "two\n")
	e.write(w2.ID, "dir/deep/w2deep.txt", "two-deep\n")

	w3 := e.open(w2.ID, "三层")
	e.write(w3.ID, "w3.txt", "three\n")

	// 折叠集合必须是 w1 → w2 → w3（深度升序）。
	fold, err := e.r.WS.FoldSet(w3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fold) != 3 {
		t.Fatalf("折叠集合大小 = %d，期望 3", len(fold))
	}
	for i, want := range []string{w1.ID, w2.ID, w3.ID} {
		if fold[i].ID != want {
			t.Fatalf("折叠集合[%d] = %s，期望 %s", i, fold[i].ID, want)
		}
	}

	res, err := e.r.Land(w3.ID)
	if err != nil {
		t.Fatal(err)
	}
	head2, _, _ := e.r.Head()
	if head2 == head1 {
		t.Fatal("head 未推进")
	}
	if res.VersionID != head2 {
		t.Fatalf("落地版本 %s != head %s", res.VersionID, head2)
	}

	// ★ 一次整合事件只产生一个版本。
	v1, _ := e.r.DB.GetVersion(head1)
	v2, _ := e.r.DB.GetVersion(head2)
	if v2.Parent != v1.ID() {
		t.Fatalf("新版本的父 = %s，期望 %s（必须是一条主线的单步推进）", v2.Parent, v1.ID())
	}

	// ★ 整条链的内容都在同一棵新树里。
	tr, err := e.r.Store.GetTree(v2.Tree)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"a.txt":               "A\n",
		"dir/b.txt":           "B\n",
		"w1.txt":              "one\n",
		"w2.txt":              "two\n",
		"dir/deep/w2deep.txt": "two-deep\n",
		"w3.txt":              "three\n",
	} {
		got, err := readTreeFile(e, tr, rel)
		if err != nil {
			t.Fatalf("落地后 %s 丢失: %v", rel, err)
		}
		if got != want {
			t.Fatalf("%s 内容 = %q，期望 %q", rel, got, want)
		}
	}

	// ★ 折叠集合里的工作区全部焚毁。
	for _, w := range fold {
		if _, err := e.r.DB.GetWorkspace(w.ID); err == nil {
			t.Fatalf("工作区 %s 落地后仍存在 —— 折叠集合必须被焚毁", w.ID)
		}
	}
}

// TestStackIntermediateChangesVisibleToChild 验证 D23：
// 子工作区的基底是父**当时有效树**的冻结快照。
//
// 也就是说：父在工作区里未落地的改动，开子工作区时**应当可见**；
// 但子开完之后父再改，子看不见（直到 rebase）。
func TestStackIntermediateChangesVisibleToChild(t *testing.T) {
	e := newEnv(t)
	e.seed("base.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w1 := e.open("", "父")
	e.write(w1.ID, "shared.txt", "from-parent\n")

	// 开子之前，父的改动必须已经被采集（Open 会同步父）。
	w2 := e.open(w1.ID, "子")
	if got := e.read(w2.ID, "shared.txt"); got != "from-parent\n" {
		t.Fatalf("子工作区看不到父的未落地改动: %q", got)
	}

	// 子开完之后父再改 —— 子不该看见（冻结快照语义，不是 live 叠加）。
	e.write(w1.ID, "shared.txt", "parent-changed-later\n")
	if got := e.read(w2.ID, "shared.txt"); got != "from-parent\n" {
		t.Fatalf("子工作区看到了父的**后续**改动 %q —— 说明基底是 live 叠加，"+
			"这正是 D23 明令禁止的（会静默掩盖父的改动）", got)
	}
}

// TestStackForkSiblingsLandIndependently 覆盖分叉：
// 两个兄弟工作区各自改不同文件，先后落地互不影响。
func TestStackForkSiblingsLandIndependently(t *testing.T) {
	e := newEnv(t)
	e.seed("base.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	a := e.open("", "兄弟A")
	b := e.open("", "兄弟B")

	e.write(a.ID, "a.txt", "A\n")
	e.write(b.ID, "b.txt", "B\n")

	if _, err := e.r.Land(a.ID); err != nil {
		t.Fatalf("A 落地失败: %v", err)
	}
	// B 的基底是导入版本（冻结快照），head 已前进；重放应把 B 的 op
	// 叠到新 head 上，而不是回退掉 A。
	if _, err := e.r.Land(b.ID); err != nil {
		t.Fatalf("B 落地失败: %v", err)
	}

	head, _, _ := e.r.Head()
	v, _ := e.r.DB.GetVersion(head)
	tr, _ := e.r.Store.GetTree(v.Tree)
	for rel, want := range map[string]string{"base.txt": "base\n", "a.txt": "A\n", "b.txt": "B\n"} {
		if got, err := readTreeFile(e, tr, rel); err != nil || got != want {
			t.Fatalf("%s = %q err=%v，期望 %q（兄弟落地互相覆盖了）", rel, got, err, want)
		}
	}
}

// TestStackSurviving childrenReparentAfterLand 覆盖落地后血缘重挂：
// 若叶子还有子工作区，它们的父应从被焚毁的工作区改为新版本。
func TestStackSurvivingChildrenReparentAfterLand(t *testing.T) {
	e := newEnv(t)
	e.seed("base.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w1 := e.open("", "将被焚毁")
	e.write(w1.ID, "one.txt", "1\n")

	// w1 落地前，w1 上再开一个子 —— 它不在 w1 的折叠集合里（折叠集合只到叶子），
	// 但 w1 落地后它必须被重挂到新版本上，否则会悬空。
	survivor := e.open(w1.ID, "幸存者")

	if _, err := e.r.Land(w1.ID); err != nil {
		t.Fatalf("落地失败: %v", err)
	}

	s, err := e.r.DB.GetWorkspace(survivor.ID)
	if err != nil {
		t.Fatalf("幸存工作区消失了: %v", err)
	}
	head, _, _ := e.r.Head()
	if s.ParentKind != meta.ParentVersion {
		t.Fatalf("幸存者 parent_kind = %s，期望 %s", s.ParentKind, meta.ParentVersion)
	}
	if s.ParentID != head.String() {
		t.Fatalf("幸存者 parent_id = %s，期望新 head %s", s.ParentID, head)
	}
	// 幸存者应当能看到被焚毁工作区的内容（因为它成了新版本的祖先）。
	if got := e.read(survivor.ID, "one.txt"); got != "1\n" {
		t.Fatalf("幸存者看不到已落地的内容: %q", got)
	}
}

// TestStackDropCascade 覆盖 ws_drop 的 cascade 语义（§4.4、附录 K）。
//
// 注意：`cascade` **不是**「递归删除后代」，而是「允许删除一个有子的工作区」——
// 子被**重挂到祖父**上，不随之消失。`cascade:false` 时有子则拒绝。
func TestStackDropCascade(t *testing.T) {
	e := newEnv(t)
	e.seed("base.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	root := e.open("", "根")
	mid := e.open(root.ID, "中")
	leaf := e.open(mid.ID, "叶")

	// cascade:false + 有子 -> 必须拒绝。
	if _, _, err := e.r.WS.Drop(mid.ID, false, false); err == nil {
		t.Fatal("cascade:false 却删掉了有子的工作区，应返回 WS_HAS_CHILDREN")
	} else if !errors.Is(err, workspace.ErrHasChildren) {
		t.Fatalf("错误类型 = %v，期望 ErrHasChildren", err)
	}

	dropped, reparented, err := e.r.WS.Drop(mid.ID, true, false)
	if err != nil {
		t.Fatalf("cascade:true 删除失败: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != mid.ID {
		t.Fatalf("dropped = %v，期望 [%s]", dropped, mid.ID)
	}
	if len(reparented) != 1 || reparented[0] != leaf.ID {
		t.Fatalf("reparented = %v，期望 [%s]", reparented, leaf.ID)
	}

	// 叶必须存活，且父变成祖父 root。
	lw, err := e.r.DB.GetWorkspace(leaf.ID)
	if err != nil {
		t.Fatalf("级联把子工作区也删了 —— cascade 语义搞错了: %v", err)
	}
	if lw.ParentKind != meta.ParentWorkspace || lw.ParentID != root.ID {
		t.Fatalf("叶的父 = (%s,%s)，期望 (%s,%s)", lw.ParentKind, lw.ParentID,
			meta.ParentWorkspace, root.ID)
	}
	if _, err := e.r.DB.GetWorkspace(mid.ID); err == nil {
		t.Fatal("被删的工作区仍在库里")
	}
	if _, err := e.r.DB.GetWorkspace(root.ID); err != nil {
		t.Fatalf("非目标 %s 被误删: %v", root.ID, err)
	}
}

// TestStackFoldSetRootFirst 是 FoldSet 顺序的回归测试。
//
// 曾经的缺陷：FoldSet 里有一句对**下标**排序的 sort 调用，和随后的显式反转
// 相互抵消，折叠集合以 [叶子…根] 返回。后果是 CollectOps 把叶子的 op 拼在
// 父的前面，而叶子的 op 建立在父的改动之上 —— 重放顺序颠倒；版本归属也会
// 记到根而不是叶子。
func TestStackFoldSetRootFirst(t *testing.T) {
	e := newEnv(t)
	e.seed("base.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	w1 := e.open("", "一")
	w2 := e.open(w1.ID, "二")
	w3 := e.open(w2.ID, "三")
	w4 := e.open(w3.ID, "四")

	fold, err := e.r.WS.FoldSet(w4.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{w1.ID, w2.ID, w3.ID, w4.ID}
	if len(fold) != len(want) {
		t.Fatalf("折叠集合大小 = %d，期望 %d", len(fold), len(want))
	}
	for i := range want {
		if fold[i].ID != want[i] {
			t.Fatalf("折叠集合[%d] = %s，期望 %s（必须根在前、叶在后）",
				i, fold[i].ID, want[i])
		}
	}
}

// TestStackLeafOwnsVersion 验证版本归属到叶子工作区，且 op 按根→叶顺序重放。
func TestStackLeafOwnsVersion(t *testing.T) {
	e := newEnv(t)
	e.seed("base.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	w1 := e.open("", "根任务")
	e.write(w1.ID, "one.txt", "1\n")
	// 名字带标识，便于断言版本记在谁头上。
	leaf := e.open(w1.ID, "叶任务-唯一标记")
	e.write(leaf.ID, "two.txt", "2\n")

	res, err := e.r.Land(leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.r.DB.GetVersion(res.VersionID)
	// §7.2：agent / task_desc 取自**叶子**工作区。
	if v.TaskDesc != "叶任务-唯一标记" {
		t.Fatalf("版本 task_desc = %q，期望来自叶子的 %q", v.TaskDesc, "叶任务-唯一标记")
	}
	// 两层的内容都要在。
	tr, _ := e.r.Store.GetTree(v.Tree)
	for rel, want := range map[string]string{"base.txt": "base\n", "one.txt": "1\n", "two.txt": "2\n"} {
		if got, err := readTreeFile(e, tr, rel); err != nil || got != want {
			t.Fatalf("%s = %q err=%v，期望 %q", rel, got, err, want)
		}
	}
}

// TestStackParentAndChildEditSameFile 是 FoldSet 顺序错误的**数据级**回归。
//
// 关键构造：**子修改的那一行是父新增的**。
// 子的 hunk 锚点只存在于「父的改动已经应用之后」的树里。若折叠集合顺序反了
// （叶→根），重放会拿这个锚点去匹配**父改动之前**的树，要么报冲突，要么把
// 改动落到错误的位置。
func TestStackParentAndChildEditSameFile(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\nB\nC\nD\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w1 := e.open("", "父新增一行")
	e.write(w1.ID, "a.txt", "A\nB\nNEW\nC\nD\n")

	// 子是在父改动之后开的，它看到 NEW。
	w2 := e.open(w1.ID, "子改父新增的那行")
	if got := e.read(w2.ID, "a.txt"); got != "A\nB\nNEW\nC\nD\n" {
		t.Fatalf("子没看到父的改动: %q", got)
	}
	e.write(w2.ID, "a.txt", "A\nB\nNEW2\nC\nD\n")

	res, err := e.r.Land(w2.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.r.DB.GetVersion(res.VersionID)
	tr, _ := e.r.Store.GetTree(v.Tree)
	got, err := readTreeFile(e, tr, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := "A\nB\nNEW2\nC\nD\n"
	if got != want {
		t.Fatalf("父子合并结果 = %q\n期望 %q —— 子的锚点只存在于父改动之后，"+
			"说明折叠集合的重放顺序错了", got, want)
	}
}
