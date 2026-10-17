package op

import (
	"testing"
)

func lines(ss ...string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}

func eqLines(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			return false
		}
	}
	return true
}

func strs(ls [][]byte) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l)
	}
	return out
}

// TestPlaceRule1HintHit 规则 1：hint 处命中即采用，不做全局搜索。
func TestPlaceRule1HintHit(t *testing.T) {
	// 模式出现两次；hint 分别指向它们时都必须直接命中。
	target := lines("a", "b", "X", "a", "b")
	h := &Hunk{Remove: lines("a", "b"), Insert: lines("A")}

	if _, replStart, _, ok := Locate(h, target, 0, DefaultMaxMatches); !ok || replStart != 0 {
		t.Fatalf("hint=0 应命中 replStart=0，得到 ok=%v replStart=%d", ok, replStart)
	}
	if _, replStart, _, ok := Locate(h, target, 3, DefaultMaxMatches); !ok || replStart != 3 {
		t.Fatalf("hint=3 应命中 replStart=3，得到 ok=%v replStart=%d", ok, replStart)
	}
	// hint 未命中时回退到全文搜索，取最接近者。
	if _, replStart, _, ok := Locate(h, target, 1, DefaultMaxMatches); !ok || replStart != 0 {
		t.Fatalf("hint=1 应回退到最近的 0，得到 ok=%v replStart=%d", ok, replStart)
	}
}

// TestPlaceRule2Unique 规则 2/5：全文唯一匹配。
func TestPlaceRule2Unique(t *testing.T) {
	target := lines("p", "q", "a", "b", "r", "s")
	h := &Hunk{Before: lines("q"), Remove: lines("a", "b"), After: lines("r"), Insert: lines("A", "B")}
	_, replStart, replEnd, ok := Locate(h, target, -1, DefaultMaxMatches)
	if !ok {
		t.Fatal("应唯一命中")
	}
	if replStart != 2 || replEnd != 4 {
		t.Fatalf("repl 区间 = [%d,%d)，期望 [2,4)", replStart, replEnd)
	}
}

// TestPlaceRule3PureInsertAnchor 规则 3：纯插入用 (before++after) 锚定。
func TestPlaceRule3PureInsertAnchor(t *testing.T) {
	target := lines("a", "b", "c")
	h := &Hunk{Before: lines("a"), After: lines("b"), StartHint: 1, Insert: lines("X")}
	out, ok := Place(h, target, h.StartHint, DefaultMaxMatches)
	if !ok {
		t.Fatal("纯插入应成功")
	}
	if !eqLines(out, lines("a", "X", "b", "c")) {
		t.Fatalf("结果 = %v", strs(out))
	}
}

// TestPlaceRule4NoMatch 规则 4：无匹配 → 冲突。
func TestPlaceRule4NoMatch(t *testing.T) {
	target := lines("a", "b")
	h := &Hunk{Remove: lines("zzz")}
	if _, _, _, ok := Locate(h, target, 0, DefaultMaxMatches); ok {
		t.Fatal("无匹配应冲突")
	}
}

// TestPlaceRule6Nearest 规则 6：多匹配取最接近 hint 者。
func TestPlaceRule6Nearest(t *testing.T) {
	target := lines("a", "x", "a", "x", "a")
	h := &Hunk{Remove: lines("a"), Insert: lines("A"), StartHint: 2}

	_, replStart, _, ok := Locate(h, target, h.StartHint, DefaultMaxMatches)
	if !ok {
		t.Fatal("应命中最近位置")
	}
	if replStart != 2 {
		t.Fatalf("replStart = %d，期望 2（最接近 hint）", replStart)
	}
}

// TestPlaceRule6TieConflict 规则 6：并列最小 → 冲突（不猜）。
func TestPlaceRule6TieConflict(t *testing.T) {
	// hint 位于两个匹配的正中间，距离相等。
	target := lines("a", "x", "a")
	h := &Hunk{Remove: lines("a"), StartHint: 1}
	if _, _, _, ok := Locate(h, target, h.StartHint, DefaultMaxMatches); ok {
		t.Fatal("并列最小距离时应冲突，而不是猜测")
	}
}

// TestPlaceMaxMatches 匹配上限：超过上限 → 冲突。
func TestPlaceMaxMatches(t *testing.T) {
	var target [][]byte
	for i := 0; i < 100; i++ {
		target = append(target, []byte("same"))
	}
	h := &Hunk{Remove: lines("same"), StartHint: -1}
	if _, _, _, ok := Locate(h, target, -1, DefaultMaxMatches); ok {
		t.Fatal("超过匹配上限应冲突")
	}
	// 放宽上限且给出明确 hint 时应能命中。
	if _, _, _, ok := Locate(h, target, 10, 1000); !ok {
		t.Fatal("放宽上限后应命中")
	}
}

// TestPlaceExactBytes 断言不做空白归一化（§6.1）。
func TestPlaceExactBytes(t *testing.T) {
	target := lines("  a", "b")
	h := &Hunk{Remove: lines("a", "b")} // 少了前导空格
	if _, _, _, ok := Locate(h, target, 0, DefaultMaxMatches); ok {
		t.Fatal("空白不同不应匹配（必须精确字节比较）")
	}
}

// TestPlaceAllSequential 断言 hunk 顺序放置且目标原地推进。
func TestPlaceAllSequential(t *testing.T) {
	target := lines("l0", "l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9")
	hunks := []Hunk{
		{Before: lines("l0", "l1"), Remove: lines("l2"), After: lines("l3", "l4"), StartHint: 2, Insert: lines("A")},
		{Before: lines("l5", "l6"), Remove: lines("l7"), After: lines("l8", "l9"), StartHint: 7, Insert: lines("B")},
	}
	out, failed := PlaceAll(hunks, target, DefaultMaxMatches)
	if failed >= 0 {
		t.Fatalf("第 %d 个 hunk 放置失败", failed)
	}
	want := lines("l0", "l1", "A", "l3", "l4", "l5", "l6", "B", "l8", "l9")
	if !eqLines(out, want) {
		t.Fatalf("结果 = %v，期望 %v", strs(out), strs(want))
	}
}

// TestConflictKeyStableAcrossOpID 是 G7 的核心断言：
// op_id / hunk_index 变化后 ConflictKey 必须**不变**。
func TestConflictKeyStableAcrossOpID(t *testing.T) {
	mk := func(opID string, hunkIdx int) *Side {
		return &Side{OpID: opID, Hunks: []Hunk{{Remove: lines("a", "b")}}}
	}
	mkUpper := func(opID string, hunkIdx int) *Side {
		return &Side{OpID: opID, Hunks: []Hunk{{Insert: lines("X")}}}
	}

	// 同一两侧内容，但 op_id 与 hunk_index 完全不同（模拟 op-log 重建）。
	lowerA := mk("op_1", 0)
	upperA := mkUpper("op_1", 0)
	lowerB := mk("op_999", 7)
	upperB := mkUpper("op_999", 7)

	ka := ConflictKey("ws_1", "src/a.go", lowerA.ContentHash(), upperA.ContentHash())
	kb := ConflictKey("ws_1", "src/a.go", lowerB.ContentHash(), upperB.ContentHash())
	if ka != kb {
		t.Fatalf("op_id 变化后 key 变了：%s vs %s（G7 回归）", ka, kb)
	}

	// 任一侧内容变化必须改变 key。
	lowerC := &Side{OpID: "op_1", Hunks: []Hunk{{Remove: lines("a", "c")}}}
	kc := ConflictKey("ws_1", "src/a.go", lowerC.ContentHash(), upperA.ContentHash())
	if kc == ka {
		t.Fatal("内容变化后 key 未变，自动失效规则失效")
	}

	// 路径变化必须改变 key。
	kd := ConflictKey("ws_1", "src/b.go", lowerA.ContentHash(), upperA.ContentHash())
	if kd == ka {
		t.Fatal("路径变化后 key 未变")
	}

	// 工作区变化必须改变 key。
	ke := ConflictKey("ws_2", "src/a.go", lowerA.ContentHash(), upperA.ContentHash())
	if ke == ka {
		t.Fatal("ws_id 变化后 key 未变")
	}
}
