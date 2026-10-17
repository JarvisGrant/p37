package op

import (
	"testing"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
)

// fixture 提供一套可用的 CAS + 内容存储。
type fixture struct {
	t   *testing.T
	cs  *cas.Store
	opt Options
}

func newFixture(t *testing.T, wsID string) *fixture {
	t.Helper()
	bs, err := cas.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := cas.NewStore(bs, cas.NewMemIndex())
	return &fixture{
		t:  t,
		cs: store,
		opt: Options{
			WsID:       wsID,
			MaxMatches: DefaultMaxMatches,
			Resolver:   NewMemResolver(),
			Candidates: true,
		},
	}
}

// treeWith 构造只含一个文件的树。
func (f *fixture) treeWith(path, content string) *tree.Tree {
	f.t.Helper()
	id, err := f.cs.PutFileData([]byte(content))
	if err != nil {
		f.t.Fatal(err)
	}
	return &tree.Tree{Entries: []tree.Entry{
		{Name: path, Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: id},
	}}
}

func (f *fixture) fileID(path, content string) hash.ID {
	f.t.Helper()
	id, err := f.cs.PutFileData([]byte(content))
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) replay(base *tree.Tree, ops ...Op) *ReplayResult {
	f.t.Helper()
	res, err := Replay(base, ops, f.cs, f.opt)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) read(t *tree.Tree, path string) (string, bool) {
	f.t.Helper()
	e, ok := t.Get(path)
	if !ok {
		return "", false
	}
	data, err := f.cs.ReadFileData(e.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data), true
}

// TestReplayModify 覆盖 modify 的正常路径。
func TestReplayModify(t *testing.T) {
	f := newFixture(t, "ws_1")
	base := f.treeWith("a.txt", "l0\nl1\nl2\n")

	hunks := tree.Hunks(tree.SplitLines([]byte("l0\nl1\nl2\n")),
		tree.SplitLines([]byte("l0\nL1\nl2\n")), 3)

	baseID, _ := f.cs.PutFileData([]byte("l0\nl1\nl2\n"))
	res := f.replay(base, Op{
		ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindModify, Path: "a.txt",
		BaseFile: baseID,
		Hunks: func() []Hunk {
			o := make([]Hunk, len(hunks))
			for i, h := range hunks {
				o[i] = FromTreeHunk(h)
			}
			return o
		}(),
	})
	if len(res.Conflicts) != 0 {
		t.Fatalf("不应有冲突: %+v", res.Conflicts)
	}
	got, ok := f.read(res.Tree, "a.txt")
	if !ok || got != "l0\nL1\nl2\n" {
		t.Fatalf("内容 = %q", got)
	}
}

// TestReplayCreateConflict 覆盖 create 遇到同名文件时的重叠冲突。
func TestReplayCreateConflict(t *testing.T) {
	f := newFixture(t, "ws_1")
	base := f.treeWith("a.txt", "old\n")
	newID := f.fileID("a.txt", "new\n")
	res := f.replay(base, Op{
		ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindCreate, Path: "a.txt", NewFile: newID,
	})
	if len(res.Conflicts) != 1 || res.Conflicts[0].Kind != ConflictOverlappingEdit {
		t.Fatalf("期望 overlapping_edit，得到 %+v", res.Conflicts)
	}
}

// TestReplayDelete 覆盖 delete 的成功与冲突两条路径。
func TestReplayDelete(t *testing.T) {
	f := newFixture(t, "ws_1")
	const content = "bye\n"
	base := f.treeWith("a.txt", content)
	id := f.fileID("a.txt", content)

	// 成功。
	res := f.replay(base, Op{ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindDelete, Path: "a.txt", BaseFile: id})
	if len(res.Conflicts) != 0 {
		t.Fatalf("不应冲突: %+v", res.Conflicts)
	}
	if _, ok := res.Tree.Get("a.txt"); ok {
		t.Fatal("文件未被删除")
	}

	// 已被第三方改动 → delete_vs_edit。
	other := f.treeWith("a.txt", "changed\n")
	res2 := f.replay(other, Op{ID: "op_2", WsID: "ws_1", Seq: 1, Kind: KindDelete, Path: "a.txt", BaseFile: id})
	if len(res2.Conflicts) != 1 || res2.Conflicts[0].Kind != ConflictDeleteVsEdit {
		t.Fatalf("期望 delete_vs_edit，得到 %+v", res2.Conflicts)
	}

	// 目标不存在 → 幂等 no-op。
	res3 := f.replay(&tree.Tree{}, Op{ID: "op_3", WsID: "ws_1", Seq: 1, Kind: KindDelete, Path: "a.txt", BaseFile: id})
	if len(res3.Conflicts) != 0 {
		t.Fatalf("不存在时应为 no-op，得到 %+v", res3.Conflicts)
	}
}

// TestReplayMissingBase 覆盖 missing_base。
func TestReplayMissingBase(t *testing.T) {
	f := newFixture(t, "ws_1")
	res := f.replay(&tree.Tree{}, Op{
		ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindModify, Path: "gone.txt",
		Hunks: []Hunk{{Remove: lines("x")}},
	})
	if len(res.Conflicts) != 1 || res.Conflicts[0].Kind != ConflictMissingBase {
		t.Fatalf("期望 missing_base，得到 %+v", res.Conflicts)
	}
}

// TestReplayRename 覆盖 rename 成功与目标被占用。
func TestReplayRename(t *testing.T) {
	f := newFixture(t, "ws_1")
	const content = "data\n"
	base := f.treeWith("old.txt", content)
	id := f.fileID("old.txt", content)

	res := f.replay(base, Op{
		ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindRename, Path: "old.txt",
		DestPath: "new.txt", BaseFile: id,
	})
	if len(res.Conflicts) != 0 {
		t.Fatalf("不应冲突: %+v", res.Conflicts)
	}
	if _, ok := res.Tree.Get("old.txt"); ok {
		t.Fatal("旧路径仍存在")
	}
	if got, ok := f.read(res.Tree, "new.txt"); !ok || got != content {
		t.Fatalf("新路径内容 = %q ok=%v", got, ok)
	}

	// 目标被占用 → rename_vs_edit。
	base2 := f.treeWith("old.txt", content)
	otherID := f.fileID("new.txt", "taken\n")
	base2.Upsert(tree.Entry{Name: "new.txt", Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: otherID})
	res2 := f.replay(base2, Op{
		ID: "op_2", WsID: "ws_1", Seq: 1, Kind: KindRename, Path: "old.txt",
		DestPath: "new.txt", BaseFile: id,
	})
	if len(res2.Conflicts) != 1 || res2.Conflicts[0].Kind != ConflictRenameVsEdit {
		t.Fatalf("期望 rename_vs_edit，得到 %+v", res2.Conflicts)
	}
}

// TestReplayAttr 覆盖 EOL-only 变化的 attr 路径。
func TestReplayAttr(t *testing.T) {
	f := newFixture(t, "ws_1")
	oldID := f.fileID("a.txt", "a\nb\n")
	newID := f.fileID("a.txt", "a\r\nb\r\n")
	base := f.treeWith("a.txt", "a\nb\n")

	res := f.replay(base, Op{
		ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindAttr, Path: "a.txt",
		BaseFile: oldID, NewFile: newID, Attr: &Attr{EOL: tree.EOLcrlf},
	})
	if len(res.Conflicts) != 0 {
		t.Fatalf("不应冲突: %+v", res.Conflicts)
	}
	got, _ := f.read(res.Tree, "a.txt")
	if got != "a\r\nb\r\n" {
		t.Fatalf("EOL 未更新: %q", got)
	}
}

// TestReplayBlobPut 覆盖 blob_put 的三条分支（§6.5）。
func TestReplayBlobPut(t *testing.T) {
	f := newFixture(t, "bin.dat")
	const oldC, newC, otherC = "\x00old", "\x00new", "\x00other"
	oldID := f.fileID("bin.dat", oldC)
	newID := f.fileID("bin.dat", newC)

	// 快路径：目标 == BaseFile → 替换。
	base := f.treeWith("bin.dat", oldC)
	res := f.replay(base, Op{ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindBlobPut,
		Path: "bin.dat", BaseFile: oldID, NewFile: newID})
	if len(res.Conflicts) != 0 {
		t.Fatalf("快路径不应冲突: %+v", res.Conflicts)
	}
	if got, _ := f.read(res.Tree, "bin.dat"); got != newC {
		t.Fatalf("内容 = %q", got)
	}

	// 幂等：目标 == NewFile → no-op。
	already := f.treeWith("bin.dat", newC)
	res2 := f.replay(already, Op{ID: "op_2", WsID: "ws_1", Seq: 1, Kind: KindBlobPut,
		Path: "bin.dat", BaseFile: oldID, NewFile: newID})
	if len(res2.Conflicts) != 0 {
		t.Fatalf("幂等路径不应冲突: %+v", res2.Conflicts)
	}

	// 其余：第三方改动 → binary 冲突，永不自动消解。
	third := f.treeWith("bin.dat", otherC)
	res3 := f.replay(third, Op{ID: "op_3", WsID: "ws_1", Seq: 1, Kind: KindBlobPut,
		Path: "bin.dat", BaseFile: oldID, NewFile: newID})
	if len(res3.Conflicts) != 1 || res3.Conflicts[0].Kind != ConflictBinary {
		t.Fatalf("期望 binary 冲突，得到 %+v", res3.Conflicts)
	}
}

// TestReplayDeterministic 断言同一输入重放两次结果完全相同（逐字节）。
func TestReplayDeterministic(t *testing.T) {
	f := newFixture(t, "ws_1")
	const baseC = "l0\nl1\nl2\nl3\n"
	base := f.treeWith("a.txt", baseC)
	baseID := f.fileID("a.txt", baseC)
	newID := f.fileID("a.txt", "l0\nlZ\nl2\n")

	ops := []Op{
		{ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindBlobPut, Path: "a.txt", BaseFile: baseID, NewFile: newID},
		{ID: "op_2", WsID: "ws_1", Seq: 2, Kind: KindCreate, Path: "b.txt", NewFile: f.fileID("b.txt", "new\n")},
	}
	r1 := f.replay(base, ops...)
	r2 := f.replay(base, ops...)

	if r1.Tree.ID() != r2.Tree.ID() {
		t.Fatalf("重放不确定：%s vs %s", r1.Tree.ID(), r2.Tree.ID())
	}
	if len(r1.Conflicts) != len(r2.Conflicts) {
		t.Fatalf("冲突数不同：%d vs %d", len(r1.Conflicts), len(r2.Conflicts))
	}
	for i := range r1.Conflicts {
		if r1.Conflicts[i].Key != r2.Conflicts[i].Key {
			t.Fatalf("第 %d 个冲突 key 不同", i)
		}
	}
}

// TestReplayResolutionApplied 断言已记录的 resolution 会被自动采用，
// 且再次重放结果一致（§6.4、§14.1.5 的微观版）。
func TestReplayResolutionApplied(t *testing.T) {
	f := newFixture(t, "ws_1")
	const baseC = "a\nb\nc\n"

	// 两个工作区都改第二行 → 后落者与先落者冲突。
	other := f.treeWith("t.txt", "a\nX\nc\n") // 主干上的现状
	baseID := f.fileID("t.txt", baseC)        // 采集时的基准
	_ = f.fileID("t.txt", "a\nY\nc\n")

	hunks := tree.Hunks(tree.SplitLines([]byte(baseC)), tree.SplitLines([]byte("a\nY\nc\n")), 3)
	hs := make([]Hunk, len(hunks))
	for i, h := range hunks {
		hs[i] = FromTreeHunk(h)
	}
	o := Op{ID: "op_1", WsID: "ws_1", Seq: 1, Kind: KindModify, Path: "t.txt",
		BaseFile: baseID, Hunks: hs}

	r1 := f.replay(other, o)
	if len(r1.Conflicts) != 1 {
		t.Fatalf("期望 1 个冲突，得到 %d", len(r1.Conflicts))
	}
	c := r1.Conflicts[0]

	// 记录一条 resolution 并重新重放。
	mem := f.opt.Resolver.(*MemResolver)
	if !mem.Put(Resolution{Key: c.Key, WsID: "ws_1", Path: "t.txt",
		LowerHash: c.Lower.ContentHash(), UpperHash: c.Upper.ContentHash(),
		Kind: ResolveTakeUpper}) {
		t.Fatal("写入 resolution 失败")
	}

	r2 := f.replay(other, o)
	if len(r2.Conflicts) != 0 {
		t.Fatalf("应用 resolution 后不应有冲突: %+v", r2.Conflicts)
	}
	if r2.Resolved != 1 {
		t.Fatalf("Resolved = %d，期望 1", r2.Resolved)
	}
	if got, _ := f.read(r2.Tree, "t.txt"); got != "a\nY\nc\n" {
		t.Fatalf("take_upper 结果 = %q", got)
	}

	// take_lower 的记录方式不同 → 不同 key，应重新冲突。
	mem2 := NewMemResolver()
	mem2.Put(Resolution{Key: hash.Sum([]byte("different")), Kind: ResolveTakeLower})
	f.opt.Resolver = mem2
	r3 := f.replay(other, o)
	if len(r3.Conflicts) != 1 {
		t.Fatalf("无关 resolution 不应消解冲突，得到 %d", len(r3.Conflicts))
	}
}
