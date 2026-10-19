package capture

import (
	"testing"

	"github.com/chenyiwen/p37/internal/tree"
)

// TestDiffDetectsBlockMove 是 §5.3 第 1、2 趟的集成用例。
//
// 把一段代码整体搬走时，产出的 op 必须是**一个**模式完整的 hunk，
// 而不是互不相干的「删一处 + 插一处」两个碎片 —— 后者的插入侧只剩极短的
// 上下文可以锚定，重放时容易失手。
func TestDiffDetectsBlockMove(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "head\nM1\nM2\nM3\nM4\ntail\n")
	t0 := e.build()

	// 把 M1..M4 整块搬到文件开头（内容不变）。
	e.write("a.txt", "M1\nM2\nM3\nM4\nhead\ntail\n")
	t1 := e.build()

	var moves []string
	e.opt.OnMoves = func(path string, mv []tree.Move) {
		for range mv {
			moves = append(moves, path)
		}
	}
	ops, err := Diff("ws_1", t0, t1, e.ow, e.opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) == 0 {
		t.Fatal("没有检测到块移动")
	}
	if len(ops) != 1 {
		t.Fatalf("期望 1 个 op，得到 %d", len(ops))
	}
	op := ops[0]
	if len(op.Hunks) == 0 {
		t.Fatal("modify op 没有 hunk")
	}
	// 移动必须落在**一个** hunk 里 —— 这是与「两个碎片」的关键区别。
	if len(op.Hunks) != 1 {
		t.Fatalf("移动应产出 1 个 hunk，得到 %d：%+v", len(op.Hunks), op.Hunks)
	}
	t.Logf("单一 hunk: remove=%d 行 insert=%d 行", len(op.Hunks[0].Remove), len(op.Hunks[0].Insert))
}

// TestDiffMoveSimilarityHasEffect 断言 merge.move_similarity 真的影响产出。
func TestDiffMoveSimilarityHasEffect(t *testing.T) {
	body := "s1\ns2\ns3\ns4\ns5\ns6\n"
	e := newEnv(t)
	e.write("b.txt", "head\n"+body+"tail\n")
	t0 := e.build()

	// 整块搬到前面，并改掉其中一行（相似度 2*5/(6+6) ≈ 0.833）。
	b := "s1\ns2\nS3-changed\ns4\ns5\ns6\nhead\ntail\n"
	e.write("b.txt", b)
	t1 := e.build()

	count := func(sim float64) int {
		e.opt.MoveSimilarity = sim
		n := 0
		e.opt.OnMoves = func(string, []tree.Move) { n++ }
		if _, err := Diff("ws_1", t0, t1, e.ow, e.opt); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if got := count(0.6); got == 0 {
		t.Fatal("阈值 0.6 时应当认出这次相似度移动")
	}
	if got := count(0.999); got != 0 {
		t.Fatalf("阈值 0.999 时不应当认出，却得到 %d", got)
	}
}
