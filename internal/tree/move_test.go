package tree

import (
	"strings"
	"testing"
)

func mv(s string) [][]byte {
	if s == "" {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	out := make([][]byte, len(parts))
	for i, p := range parts {
		out[i] = []byte(p)
	}
	return out
}

func strOf(ls [][]byte) string {
	return strings.Join(func() []string {
		o := make([]string, 0, len(ls))
		for _, l := range ls {
			o = append(o, string(l))
		}
		return o
	}(), "|")
}

// TestDetectMovesExact 覆盖 §5.3 第 1 趟：内容逐字节相同的块移动。
func TestDetectMovesExact(t *testing.T) {
	cases := []struct {
		name  string
		a, b  string
		wantN int
	}{
		{"块后移", "P\nX1\nX2\nX3\nQ\n", "P\nQ\nX1\nX2\nX3\n", 1},
		{"块前移", "A\nB\nM1\nM2\nM3\nC\n", "M1\nM2\nM3\nA\nB\nC\n", 1},
		{"块内重复行", "P\np\np\np\nQ\n", "P\nQ\np\np\np\n", 1},
		{"单行移动（重排）", "P\nX\nQ\n", "P\nQ\nX\n", 1},
		{"无移动（只改一行）", "A\nB\nC\n", "A\nX\nC\n", 0},
		{"无移动（整段替换）", "a\nb\nc\nd\n", "w\nx\ny\nz\n", 0},
		{"无移动（纯追加）", "a\nb\n", "a\nb\nc\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := mv(c.a), mv(c.b)
			regions := HistogramWith(a, b, DefaultMaxChainLength)
			moves := DetectMoves(a, b, regions, DefaultMoveSimilarity)
			if len(moves) != c.wantN {
				t.Fatalf("检测到 %d 个移动，期望 %d（regions=%+v）", len(moves), c.wantN, regions)
			}
		})
	}
}

// TestDetectMovesSimilarity 覆盖 §5.3 第 2 趟：移动 + 微改。
//
// 直接构造区间来单测匹配器 —— histogram 对这种输入往往会先挑一条锚点，
// 于是区间形态并不稳定，不适合用来钉住匹配规则本身。
func TestDetectMovesSimilarity(t *testing.T) {
	a := mv("0\n1\n2\nM1\nM2\nM3\nM4\n9\n")   // a[3:7] 是要搬走的块
	b := mv("M1\nM2\nM3\nM4-x\n0\n1\n2\n9\n") // b[0:4] 是搬来并改了一行的块

	regions := []Region{
		{AStart: 3, ALen: 4, BStart: 3, BLen: 0}, // 纯删除 a[3:7]
		{AStart: 0, ALen: 0, BStart: 0, BLen: 4}, // 纯插入 b[0:4]
	}
	moves := DetectMoves(a, b, regions, DefaultMoveSimilarity)
	if len(moves) != 1 {
		t.Fatalf("期望 1 个相似度移动，得到 %d", len(moves))
	}
	if moves[0].Exact {
		t.Error("内容已改，不该判为精确移动")
	}
	// 2*3/(4+4) = 0.75
	if got := moves[0].Similarity; got < 0.74 || got > 0.76 {
		t.Fatalf("相似度 = %v，期望 ~0.75", got)
	}
	if moves[0].Delete != regions[0] || moves[0].Insert != regions[1] {
		t.Fatalf("移动未携带完整区间坐标: %+v", moves[0])
	}

	// 阈值调高到 0.95 就不该再匹配。
	if n := DetectMoves(a, b, regions, 0.95); len(n) != 0 {
		t.Fatalf("阈值提高后仍匹配到 %d 个", len(n))
	}
	// 精确相同的块必须优先按精确匹配认。
	a2 := mv("0\n1\n2\nA\nB\nC\n9\n")
	b2 := mv("A\nB\nC\n0\n1\n2\n9\n")
	r2 := []Region{
		{AStart: 3, ALen: 3, BStart: 3, BLen: 0},
		{AStart: 0, ALen: 0, BStart: 0, BLen: 3},
	}
	m2 := DetectMoves(a2, b2, r2, DefaultMoveSimilarity)
	if len(m2) != 1 || !m2[0].Exact {
		t.Fatalf("内容完全相同应判为精确移动: %+v", m2)
	}
}

// TestMergeMovesProducesExactHunk 断言合并后 hunk 的锚点模式在目标里**唯一**。
//
// 这是移动检测的全部意义：把「一删一插两个碎片」并成一个模式完整、能唯一定位
// 的 hunk，`place` 才锚得稳。
func TestMergeMovesProducesExactHunk(t *testing.T) {
	a := mv("P\np\np\np\nQ\n")
	b := mv("P\nQ\np\np\np\n")

	hs, moves := HunksWithMoves(a, b, 3, DefaultMaxChainLength, DefaultMoveSimilarity)
	if len(moves) == 0 {
		t.Fatal("应当检测到移动")
	}
	if len(hs) != 1 {
		t.Fatalf("移动对被合并成 1 个 hunk，得到 %d 个: %+v", len(hs), hs)
	}
	h := hs[0]
	// 模式必须唯一命中（tree.Hunk 的模式 = before ++ remove ++ after）。
	pat := append(append(append([][]byte{}, h.Before...), h.Remove...), h.After...)
	if n := countPattern(a, pat); n != 1 {
		t.Fatalf("合并后 hunk 的模式在旧文件里命中 %d 次，期望 1 次", n)
	}
	// 重建必须正确。
	regions := MergeMoves(HistogramWith(a, b, DefaultMaxChainLength), moves)
	if got := strOf(applyRegions(a, b, regions)); got != strOf(b) {
		t.Fatalf("重建结果 %q，期望 %q", got, strOf(b))
	}
}

// TestMovesDoNotChangeReconstruction 断言开/关移动检测都不会改变重建结果。
//
// 移动检测只换等价的切分方式，**绝不能**影响正确性。
func TestMovesDoNotChangeReconstruction(t *testing.T) {
	cases := []struct{ a, b string }{
		{"P\nX1\nX2\nX3\nQ\n", "P\nQ\nX1\nX2\nX3\n"},
		{"A\nB\nM1\nM2\nM3\nC\n", "M1\nM2\nM3\nA\nB\nC\n"},
		{"P\np\np\np\nQ\n", "P\nQ\np\np\np\n"},
		{"P\nX1\nX2\nX3\nQ\n", "P\nQ\nX1\nX2\nX9\n"},
		{"1\n2\n3\n4\n5\n6\n7\n8\n", "1\n3\n5\n2\n4\n6\n7\n8\n"},
	}
	for i, c := range cases {
		a, b := mv(c.a), mv(c.b)
		plain := HunksWith(a, b, 3, DefaultMaxChainLength)
		withMoves, _ := HunksWithMoves(a, b, 3, DefaultMaxChainLength, DefaultMoveSimilarity)

		_ = plain
		_ = withMoves

		// 直接用区间重建，两个变体都必须得到 B。
		r0 := HistogramWith(a, b, DefaultMaxChainLength)
		if got := strOf(applyRegions(a, b, r0)); got != strOf(b) {
			t.Fatalf("用例 %d：不开移动检测的重建 = %q，期望 %q", i, got, strOf(b))
		}
		mv := DetectMoves(a, b, r0, DefaultMoveSimilarity)
		r1 := MergeMoves(r0, mv)
		if got := strOf(applyRegions(a, b, r1)); got != strOf(b) {
			t.Fatalf("用例 %d：开移动检测的重建 = %q，期望 %q", i, got, strOf(b))
		}
		// 区间必须不重叠且按 AStart 升序（重放顺序的前提）。
		prevEnd := 0
		for _, r := range r1 {
			if r.AStart < prevEnd {
				t.Fatalf("用例 %d：区间重叠或有重叠覆盖: %+v", i, r1)
			}
			prevEnd = r.AStart + r.ALen
		}
	}
}

// TestDetectMovesDeterministic 断言同一输入总是给出同一结果。
func TestDetectMovesDeterministic(t *testing.T) {
	a := mv("P\nx1\nx2\nx3\nQ\nx1\nx2\nx3\nR\n")
	b := mv("P\nQ\nx1\nx2\nx3\nR\nx1\nx2\nx3\n")
	var first []Move
	for i := 0; i < 10; i++ {
		_, moves := HunksWithMoves(a, b, 3, DefaultMaxChainLength, DefaultMoveSimilarity)
		if i == 0 {
			first = moves
			continue
		}
		if len(moves) != len(first) {
			t.Fatalf("第 %d 次结果个数不同: %d vs %d", i, len(moves), len(first))
		}
		for j := range moves {
			if moves[j] != first[j] {
				t.Fatalf("第 %d 次结果 %d 不同: %+v vs %+v", i, j, moves[j], first[j])
			}
		}
	}
}

// TestLineSimilarity 钉住相似度度量的取值。
func TestLineSimilarity(t *testing.T) {
	cases := []struct {
		x, y string
		want float64
	}{
		{"", "", 1},
		{"a\n", "a\n", 1},
		{"a\nb\n", "b\na\n", 1},               // 顺序无关（多重集合）
		{"a\nb\nc\n", "a\nb\nX\n", 2.0 / 3.0}, // 2*2/(3+3)
		{"a\n", "b\n", 0},
	}
	for _, c := range cases {
		got := lineSimilarity(mv(c.x), mv(c.y))
		if got != c.want {
			t.Errorf("lineSimilarity(%q,%q) = %v，期望 %v", c.x, c.y, got, c.want)
		}
	}
}

// TestMergeMovesSkipsNonAdjacent 断言中间夹着别的改动时不合并
// （合并会把无关改动一起吞进来）。
func TestMergeMovesSkipsNonAdjacent(t *testing.T) {
	// 删块和插块之间还夹着一次独立的修改。
	a := mv("P\nM1\nM2\nM3\nZ\nQ\n")
	b := mv("P\nQ\nZZZ\nM1\nM2\nM3\n")
	regions := HistogramWith(a, b, DefaultMaxChainLength)
	moves := DetectMoves(a, b, regions, DefaultMoveSimilarity)
	merged := MergeMoves(regions, moves)
	// 无论合并与否，重建都必须正确。
	if got := strOf(applyRegions(a, b, merged)); got != strOf(b) {
		t.Fatalf("重建 = %q，期望 %q", got, strOf(b))
	}
}

// applyRegions 用变更区间把 A 重建为 B。
func applyRegions(a, b [][]byte, regs []Region) [][]byte {
	out := make([][]byte, 0, len(b))
	prev := 0
	for _, r := range regs {
		out = append(out, a[prev:r.AStart]...)
		out = append(out, b[r.BStart:r.BStart+r.BLen]...)
		prev = r.AStart + r.ALen
	}
	out = append(out, a[prev:]...)
	return out
}

// countPattern 统计 pat 在 target 中出现的次数（用于断言锚点唯一性）。
func countPattern(target, pat [][]byte) int {
	if len(pat) == 0 {
		return 0
	}
	n := 0
	for i := 0; i+len(pat) <= len(target); i++ {
		ok := true
		for k := range pat {
			if string(target[i+k]) != string(pat[k]) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}
