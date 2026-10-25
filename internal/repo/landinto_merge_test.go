package repo_test

import (
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// readTreeFile 按仓库相对路径读取一棵树里的文件内容。
func readTreeFileAt(t *testing.T, store *cas.Store, root string, rel string) string {
	t.Helper()
	id, err := hash.Parse(root)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := store.GetTree(id)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		e, ok := cur.Get(p)
		if !ok {
			t.Fatalf("树里缺少 %s", rel)
		}
		if i == len(parts)-1 {
			if e.Kind != codec.EntryFile {
				t.Fatalf("%s 不是普通文件", rel)
			}
			b, err := store.ReadFileData(e.ID)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		if e.Kind != codec.EntryTree {
			t.Fatalf("%s 的中间段不是目录", rel)
		}
		if cur, err = store.GetTree(e.ID); err != nil {
			t.Fatal(err)
		}
	}
	return ""
}

// TestEffectiveTreeCacheInvalidatedOnSameOpCount 是会话 #90 的回归测试。
//
// 父工作区只有 1 个被修改的文件；先读一次有效树把缓存填上，再把同一个文件
// 改成新内容。op 条数仍是 1，旧实现用 len(ops) 当缓存键，会命中上一次的旧树。
func TestEffectiveTreeCacheInvalidatedOnSameOpCount(t *testing.T) {
	e := newEnv(t)
	e.seed("tests/levelForRate.test.ts", "v1\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")

	e.write(parent.ID, "tests/levelForRate.test.ts", "v2\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	t1, err := e.r.WS.EffectiveTree(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTreeFileAt(t, e.r.Store, t1.String(), "tests/levelForRate.test.ts"); got != "v2\n" {
		t.Fatalf("首次有效树 = %q，期望 v2", got)
	}

	// 第二次修改：op 路径集合不变，条数仍是 1。
	e.write(parent.ID, "tests/levelForRate.test.ts", "v3\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	n, _ := e.r.DB.CountOps(parent.ID)
	if n != 1 {
		t.Fatalf("op 条数 = %d，期望 1（本用例要求条数不变）", n)
	}
	t2, err := e.r.WS.EffectiveTree(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTreeFileAt(t, e.r.Store, t2.String(), "tests/levelForRate.test.ts"); got != "v3\n" {
		t.Fatalf("第二次有效树 = %q，期望 v3（缓存命中了旧树）", got)
	}
	if t1 == t2 {
		t.Fatal("内容变了，有效树 id 不应相同")
	}

	// 内容没变时，周期扫描/ws_sync 不应推进 ops_epoch：否则 preview → land
	// 之间会被无意义的版本刷新打断（target_moved 假阳性）。
	w, err := e.r.WS.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	epoch := w.OpsEpoch
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	w2, err := e.r.WS.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w2.OpsEpoch != epoch {
		t.Fatalf("无改动 Sync 不应推进 epoch: %d -> %d", epoch, w2.OpsEpoch)
	}
}

// TestLandIntoDoesNotRevertParentEdit 复现 #90 的完整链路：
//
//	父改测试文件 → 派子会话 → 父又改一次（同一文件）→ 子只改别的文件并 land。
//	父的第二次修改必须原样保留。
func TestLandIntoDoesNotRevertParentEdit(t *testing.T) {
	e := newEnv(t)
	e.seed("tests/levelForRate.test.ts", "v1\n")
	e.seed("src/a.ts", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")

	// 第一次修改 + 读有效树（污染缓存的机会点）。
	e.write(parent.ID, "tests/levelForRate.test.ts", "v2\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.WS.EffectiveTree(parent.ID); err != nil {
		t.Fatal(err)
	}

	// 子会话创建前父再做一次修改（op 条数不变）。
	e.write(parent.ID, "tests/levelForRate.test.ts", "v3\n")
	if err := e.r.WS.Sync(parent.ID); err != nil {
		t.Fatal(err)
	}

	child := e.open(parent.ID, "child")
	e.write(child.ID, "b.txt", "B\n")
	if _, err := e.r.LandInto(child.ID, parent.ID, "", "", ""); err != nil {
		t.Fatal(err)
	}

	if got := e.read(parent.ID, "tests/levelForRate.test.ts"); got != "v3\n" {
		t.Fatalf("父的测试修正被回退: %q，期望 v3", got)
	}
	if got := e.read(parent.ID, "b.txt"); got != "B\n" {
		t.Fatalf("子的改动没有并入: %q", got)
	}
}
