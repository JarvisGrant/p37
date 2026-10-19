package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/op"
)

// TestSymlinkTargetChangeIsModify 断言「改一个符号链接的目标」被记成**一次修改**
// 而不是「删 + 建」。
//
// 语义上两者都能重放正确，但 blob_put 的表达更贴切（重放规则就是
// 「目标 id == blob_before -> 换成 blob_after」），op 数也少一半。
func TestSymlinkTargetChangeIsModify(t *testing.T) {
	e := newEnv(t)
	if err := os.Symlink("target-a", filepath.Join(e.dir, "link")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	t0 := e.build()

	if err := os.Remove(filepath.Join(e.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-b", filepath.Join(e.dir, "link")); err != nil {
		t.Fatal(err)
	}
	t1 := e.build()

	ops, err := Diff("ws_1", t0, t1, e.ow, e.opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("期望 1 个 op，得到 %d: %+v", len(ops), ops)
	}
	o := ops[0]
	if o.Kind != op.KindBlobPut {
		t.Fatalf("kind = %s，期望 %s", o.Kind, op.KindBlobPut)
	}
	if o.BaseFile.IsZero() || o.NewFile.IsZero() || o.BaseFile == o.NewFile {
		t.Fatalf("blob_put 的 before/after 不对: %+v", o)
	}
	if o.EntryKind != 2 { // codec.EntrySymlink
		t.Fatalf("EntryKind = %d，期望符号链接(2)", o.EntryKind)
	}

	// 重放必须还原成新目标。
	res, err := op.Replay(t0, ops, e.ow, op.Options{
		WsID: "ws_1", MaxMatches: op.DefaultMaxMatches,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("重放产生冲突: %+v", res.Conflicts)
	}
	en, ok := res.Tree.Get("link")
	if !ok {
		t.Fatal("link 丢失")
	}
	got, err := e.store.ReadBlob(en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "target-b" {
		t.Fatalf("重放后目标 = %q，期望 target-b", got)
	}
	if res.Tree.ID() != t1.ID() {
		t.Fatalf("重放结果树 ≠ 目标树")
	}
}

// TestFileSymlinkTypeChangeStillDeleteCreate 断言**类型**变化仍然是删+建
// （不能用 blob_put 糊过去 —— 那会丢掉「类型变了」这个事实）。
func TestFileSymlinkTypeChangeStillDeleteCreate(t *testing.T) {
	e := newEnv(t)
	e.write("x", "regular file\n")
	t0 := e.build()

	if err := os.Remove(filepath.Join(e.dir, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(e.dir, "x")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	t1 := e.build()

	ops, err := Diff("ws_1", t0, t1, e.ow, e.opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].Kind != op.KindDelete || ops[1].Kind != op.KindCreate {
		t.Fatalf("类型变化应为「删 + 建」，得到 %+v", ops)
	}
}
