package op

import (
	"testing"

	"github.com/chenyiwen/p37/internal/tree"
)

// modifyOp 用 tree.Hunks 构造一个 modify op。
func (f *fixture) modifyOp(path, from, to string, seq int64, baseFile string) Op {
	f.t.Helper()
	hs := tree.Hunks(tree.SplitLines([]byte(from)), tree.SplitLines([]byte(to)), 3)
	out := make([]Hunk, len(hs))
	for i, h := range hs {
		out[i] = FromTreeHunk(h)
	}
	var bid = f.fileID(path, baseFile)
	return Op{
		ID: "op_x", WsID: "ws_1", Seq: seq, Kind: KindModify, Path: path,
		BaseFile: bid, Hunks: out,
	}
}

// TestAutoMergeNonOverlappingLines 覆盖 §6.3 第一条：
// 「overlapping_edit 且两侧改不重叠行 -> 自动都保留（多数情况直接消解）」。
//
// 构造：本侧改 l1，对侧改 l2 —— l2 恰好是本侧 hunk 的**下文锚点**，
// 因此完整模式 [l0 l1 l2] 匹配不上，place 会失败。但本侧要替换的 l1
// 仍然唯一存在，说明两侧没有改同一行，应当自动都保留。
func TestAutoMergeNonOverlappingLines(t *testing.T) {
	f := newFixture(t, "ws_1")
	const base = "l0\nl1\nl2\n"
	other := f.treeWith("a.txt", "l0\nl1\nL2\n") // 对侧把 l2 改成 L2

	o := f.modifyOp("a.txt", base, "l0\nL1\nl2\n", 1, base)
	res := f.replay(other, o)

	if len(res.Conflicts) != 0 {
		t.Fatalf("两侧改不重叠行不该冲突: %+v", res.Conflicts)
	}
	if res.AutoMerged != 1 {
		t.Fatalf("AutoMerged = %d，期望 1", res.AutoMerged)
	}
	if got, _ := f.read(res.Tree, "a.txt"); got != "l0\nL1\nL2\n" {
		t.Fatalf("两侧改动都应保留，得到 %q", got)
	}
}

// TestAutoMergeSameAnchorInsert 覆盖 §6.3：
// 「两侧在同锚点插入 -> 按 op seq 拼接（顺序确定、可解释）」。
func TestAutoMergeSameAnchorInsert(t *testing.T) {
	f := newFixture(t, "ws_1")
	const base = "a\nb\n"
	other := f.treeWith("a.txt", "a\nY\nb\n") // 对侧已在同一锚点插入 Y

	o := f.modifyOp("a.txt", base, "a\nX\nb\n", 7, base)
	res := f.replay(other, o)

	if len(res.Conflicts) != 0 {
		t.Fatalf("同锚点插入不该冲突: %+v", res.Conflicts)
	}
	if res.AutoMerged != 1 {
		t.Fatalf("AutoMerged = %d，期望 1", res.AutoMerged)
	}
	// 本侧 op 的 seq 晚于目标里已有的内容 -> 本侧拼在后面。
	if got, _ := f.read(res.Tree, "a.txt"); got != "a\nY\nX\nb\n" {
		t.Fatalf("拼接顺序 = %q，期望 %q（本侧应在后）", got, "a\nY\nX\nb\n")
	}

	// 确定性：同一输入重放两次必须逐字节相同。
	res2 := f.replay(other, o)
	if res2.Tree.ID() != res.Tree.ID() {
		t.Fatalf("同锚点插入不确定：%s vs %s", res.Tree.ID(), res2.Tree.ID())
	}
}

// TestOverlappingEditStillConflicts 断言**真正**改了同一行时仍然是硬冲突。
// 这是自动合并的边界：宁可报冲突，也不能猜。
func TestOverlappingEditStillConflicts(t *testing.T) {
	f := newFixture(t, "ws_1")
	const base = "a\nb\nc\n"
	other := f.treeWith("a.txt", "a\nY\nc\n")

	o := f.modifyOp("a.txt", base, "a\nX\nc\n", 1, base)
	res := f.replay(other, o)

	if len(res.Conflicts) != 1 {
		t.Fatalf("同一行的两侧改动必须冲突，得到 %d 个: %+v", len(res.Conflicts), res.Conflicts)
	}
	if res.AutoMerged != 0 {
		t.Fatalf("不该自动合并，AutoMerged = %d", res.AutoMerged)
	}
	if res.Conflicts[0].Kind != ConflictOverlappingEdit {
		t.Fatalf("kind = %s", res.Conflicts[0].Kind)
	}
}

// TestAutoMergeRefusesAmbiguous 断言「多处匹配」绝不自动合并。
func TestAutoMergeRefusesAmbiguous(t *testing.T) {
	f := newFixture(t, "ws_1")
	// dup 出现两次，本侧要改其中一处 —— 无法确定是哪一处。
	const base = "dup\nmid\ndup\n"
	other := f.treeWith("a.txt", "dup\nMID\ndup\n")

	// 手工构造一个 Remove 在目标里出现两次的 hunk。
	// StartHint 故意落在两处中间，使 §6.1 的「按 hint 原位试」落到空处，
	// 从而走全文搜索 -> 两处匹配且距离并列 -> place 失败。
	o := Op{
		ID: "op_amb", WsID: "ws_1", Seq: 1, Kind: KindModify, Path: "a.txt",
		BaseFile: f.fileID("a.txt", base),
		Hunks:    []Hunk{{Remove: lines("dup"), Insert: lines("DUP"), StartHint: 1}},
	}
	res := f.replay(other, o)
	if len(res.Conflicts) != 1 {
		t.Fatalf("歧义匹配必须冲突，得到 %d", len(res.Conflicts))
	}
	if res.AutoMerged != 0 {
		t.Fatalf("歧义时不该自动合并")
	}
}

// TestRenameFollowsWhenContentChanged 覆盖 §6.3：
// 「rename_vs_edit 且内容未分叉 -> 跟随重命名，在新路径应用」。
func TestRenameFollowsWhenContentChanged(t *testing.T) {
	f := newFixture(t, "ws_1")
	const base = "a\nb\nc\n"
	// 对侧把 b 改成 B（内容被第三方改过）。
	other := f.treeWith("old.txt", "a\nB\nc\n")

	// 本侧：重命名 old.txt -> new.txt，同时把 c 改成 C。
	o := f.modifyOp("old.txt", base, "a\nb\nC\n", 1, base)
	o.Kind = KindRename
	o.DestPath = "new.txt"

	res := f.replay(other, o)

	if len(res.Conflicts) != 0 {
		t.Fatalf("内容未分叉时应跟随重命名，不该冲突: %+v", res.Conflicts)
	}
	if _, ok := res.Tree.Get("old.txt"); ok {
		t.Fatal("旧路径仍然存在")
	}
	// 对侧的 B 与本侧的 C 都在 —— 两个改动都保留。
	if got, ok := f.read(res.Tree, "new.txt"); !ok || got != "a\nB\nC\n" {
		t.Fatalf("新路径内容 = %q ok=%v，期望 %q", got, ok, "a\nB\nC\n")
	}
}

// TestRenameFollowsPureMove 覆盖重命名本身（本侧没改内容）遇到对侧改动内容。
func TestRenameFollowsPureMove(t *testing.T) {
	f := newFixture(t, "ws_1")
	const base = "x\ny\n"
	other := f.treeWith("old.txt", "x\nY\n")

	baseID := f.fileID("old.txt", base)
	res := f.replay(other, Op{
		ID: "op_r", WsID: "ws_1", Seq: 1, Kind: KindRename, Path: "old.txt",
		DestPath: "new.txt", BaseFile: baseID,
	})
	if len(res.Conflicts) != 0 {
		t.Fatalf("不该冲突: %+v", res.Conflicts)
	}
	if got, ok := f.read(res.Tree, "new.txt"); !ok || got != "x\nY\n" {
		t.Fatalf("新路径内容 = %q ok=%v（应保留对侧的改动）", got, ok)
	}
}

// TestCandidatesPerKind 断言各类冲突给出的候选解（§6.3）。
func TestCandidatesPerKind(t *testing.T) {
	f := newFixture(t, "ws_1")

	// delete_vs_edit：候选第一项必须是「取删除」。
	{
		const c = "bye\n"
		id := f.fileID("d.txt", c)
		other := f.treeWith("d.txt", "changed\n")
		res := f.replay(other, Op{ID: "op_d", WsID: "ws_1", Seq: 1,
			Kind: KindDelete, Path: "d.txt", BaseFile: id})
		if len(res.Conflicts) != 1 {
			t.Fatalf("期望 1 个冲突，得到 %d", len(res.Conflicts))
		}
		cs := res.Conflicts[0].Candidat
		if len(cs) == 0 || cs[0].Kind != ResolveTakeLower {
			t.Fatalf("delete_vs_edit 首选候选应是取删除，得到 %+v", cs)
		}
		if cs[0].Note == "" {
			t.Fatal("候选必须带可解释的说明")
		}
	}

	// attr_conflict：候选第一项必须是 take_upper（§6.3）。
	{
		oldID := f.fileID("e.txt", "a\nb\n")
		newID := f.fileID("e.txt", "a\r\nb\r\n")
		other := f.treeWith("e.txt", "a\nB\n")
		res := f.replay(other, Op{ID: "op_e", WsID: "ws_1", Seq: 1,
			Kind: KindAttr, Path: "e.txt", BaseFile: oldID, NewFile: newID,
			Attr: &Attr{EOL: tree.EOLcrlf}})
		if len(res.Conflicts) != 1 {
			t.Fatalf("期望 1 个冲突，得到 %d", len(res.Conflicts))
		}
		cs := res.Conflicts[0].Candidat
		if len(cs) == 0 || cs[0].Kind != ResolveTakeUpper {
			t.Fatalf("attr 冲突首选候选应是取 upper，得到 %+v", cs)
		}
	}

	// binary：两侧都变了 -> 只能二选一，不提供 both。
	{
		oldID := f.fileID("b.dat", "\x00old")
		newID := f.fileID("b.dat", "\x00new")
		other := f.treeWith("b.dat", "\x00other")
		res := f.replay(other, Op{ID: "op_b", WsID: "ws_1", Seq: 1,
			Kind: KindBlobPut, Path: "b.dat", BaseFile: oldID, NewFile: newID})
		if len(res.Conflicts) != 1 {
			t.Fatalf("期望 1 个冲突，得到 %d", len(res.Conflicts))
		}
		cs := res.Conflicts[0].Candidat
		if len(cs) != 2 {
			t.Fatalf("binary 冲突应恰好 2 个候选，得到 %+v", cs)
		}
		for _, c := range cs {
			if c.Kind == ResolveBoth {
				t.Fatalf("binary 冲突永不自动合并，不该出现 both: %+v", cs)
			}
		}
	}
}
