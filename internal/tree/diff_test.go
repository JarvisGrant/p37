package tree

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// applyViaA 在**原始 A** 的坐标里逐个定位 hunk，再据此重建 B。
//
// 这比「就地改 cur」更能检验锚点是否自洽：要求每个 hunk 的模式在 A 中
// 从上一 hunk 末尾起**唯一**匹配。
func applyViaA(a, b [][]byte, hunks []Hunk) ([][]byte, bool) {
	var out [][]byte
	pos := 0
	for _, h := range hunks {
		lo, _, ok := locateSpan(a, pos, h, true)
		if !ok {
			return nil, false
		}
		// 模式 = before ++ remove ++ after；before 与 after 都要保留。
		out = append(out, a[pos:lo]...)
		out = append(out, h.Before...)
		out = append(out, h.Insert...)
		pos = lo + len(h.Before) + len(h.Remove)
	}
	out = append(out, a[pos:]...)
	return out, true
}

func toLines(s []string) [][]byte {
	out := make([][]byte, len(s))
	for i, l := range s {
		out[i] = []byte(l)
	}
	return out
}

func fromLines(ls [][]byte) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l)
	}
	return out
}

// TestHistogramIdentical 是不变量 1：histogram(A,A) 为空。
func TestHistogramIdentical(t *testing.T) {
	for _, n := range []int{0, 1, 2, 10, 100} {
		a := make([][]byte, n)
		for i := range a {
			a[i] = []byte(fmt.Sprintf("line-%d", i))
		}
		if r := Histogram(a, a); len(r) != 0 {
			t.Fatalf("n=%d：相同序列应无变更，得到 %v", n, r)
		}
	}
}

// TestHistogramDeterministic 是不变量 3：输出完全由输入决定。
//
// 注意：附录 I 曾列出「互为镜像」为不变量，实测该性质**不成立** —— 锚点
// 选择规则（candidates 依赖 A 侧计数）本质上是方向相关的，JGit 的 histogram
// 同样不对称。管线中没有任何环节依赖镜像性（revert 采用显式取逆而非重算
// 反向 diff），因此该不变量已从规格中移除，改为断言确定性。
func TestHistogramDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 300; iter++ {
		a := dupLines(rng, rng.Intn(40))
		b := dupLines(rng, rng.Intn(40))
		first := Histogram(a, b)
		for k := 0; k < 5; k++ {
			got := Histogram(a, b)
			if len(got) != len(first) {
				t.Fatalf("结果不稳定")
			}
			for i := range got {
				if got[i] != first[i] {
					t.Fatalf("第 %d 次第 %d 个区间不同", k, i)
				}
			}
		}
	}
}

// dupLines 生成重复度很高的行序列（用于压锚点逻辑）。
func dupLines(rng *rand.Rand, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("L%d", rng.Intn(8)))
	}
	return out
}

// uniqLines 生成基本唯一的行序列，并保留少量重复。
func uniqLines(rng *rand.Rand, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		if rng.Intn(5) == 0 && i > 0 {
			out[i] = out[rng.Intn(i)] // 少量重复
			continue
		}
		out[i] = []byte(fmt.Sprintf("unique-line-%d-%d", i, rng.Intn(1<<20)))
	}
	return out
}

// TestHistogramRoundTrip 是不变量：按区间重建 B。
func TestHistogramRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 500; iter++ {
		a := dupLines(rng, rng.Intn(50))
		b := dupLines(rng, rng.Intn(50))
		regions := Histogram(a, b)

		var got [][]byte
		ai, bi := 0, 0
		for _, r := range regions {
			if r.AStart < ai || r.BStart < bi {
				t.Fatalf("区间非单调: %+v (prev a=%d b=%d)", r, ai, bi)
			}
			got = append(got, a[ai:r.AStart]...)              // 未变更部分
			got = append(got, b[r.BStart:r.BStart+r.BLen]...) // 替换为 B 的内容
			ai = r.AStart + r.ALen
			bi = r.BStart + r.BLen
		}
		got = append(got, a[ai:]...)

		if !equalLines(got, b) {
			t.Fatalf("重建结果与 B 不同\na=%v\nb=%v\ngot=%v", fromLines(a), fromLines(b), fromLines(got))
		}
	}
}

func equalLines(x, y [][]byte) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !bytes.Equal(x[i], y[i]) {
			return false
		}
	}
	return true
}

// TestHunksApplyRoundTrip 是核心不变量：apply(A, hunks) == B（对应 §附录 L.2 #15）。
func TestHunksApplyRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for iter := 0; iter < 10000; iter++ {
		a := uniqLines(rng, rng.Intn(60))
		b := uniqLines(rng, rng.Intn(60))
		hunks := Hunks(a, b, DefaultContextLines)
		got, ok := applyViaA(a, b, hunks)
		if !ok {
			continue // 合成数据里的重复行使锚点不唯一，跳过（见下一个测试）
		}
		if !equalLines(got, b) {
			t.Fatalf("应用结果 != B (iter=%d)\na=%v\nb=%v\ngot=%v",
				iter, fromLines(a), fromLines(b), fromLines(got))
		}
	}
}

// TestHunksApplyRoundTripStrict 断言在**锚点唯一**时可严格重建 B。
//
// 这是更强的一版：要求全部 10000 组都成功。
func TestHunksApplyRoundTripStrict(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	for iter := 0; iter < 10000; iter++ {
		a := uniqueOnlyLines(rng, rng.Intn(60))
		b := uniqueOnlyLines(rng, rng.Intn(60))
		hunks := Hunks(a, b, DefaultContextLines)
		got, ok := applyViaA(a, b, hunks)
		if !ok {
			t.Fatalf("锚点唯一时仍应用失败 (iter=%d)\na=%v\nb=%v\nhunks=%+v",
				iter, fromLines(a), fromLines(b), hunks)
		}
		if !equalLines(got, b) {
			t.Fatalf("应用结果 != B (iter=%d)", iter)
		}
	}
}

// uniqueOnlyLines 生成完全唯一的行序列。
func uniqueOnlyLines(rng *rand.Rand, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("u-%d-%d", i, rng.Int63()))
	}
	return out
}

// TestHunksNoOverlappingContext 断言相邻 hunk 的锚点区间不重叠。
//
// 这一点很关键：锚点重叠会让顺序重放时后一个 hunk 匹配失败。
func TestHunksNoOverlappingContext(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	for iter := 0; iter < 2000; iter++ {
		a := uniqueOnlyLines(rng, rng.Intn(80))
		b := uniqueOnlyLines(rng, rng.Intn(80))
		hunks := Hunks(a, b, DefaultContextLines)

		type span struct{ lo, hi int }
		var spans []span
		pos := 0
		for _, h := range hunks {
			lo, hi, ok := locateSpan(a, pos, h, true)
			if !ok {
				t.Fatalf("定位失败: a=%v hunk=%+v", fromLines(a), h)
			}
			spans = append(spans, span{lo, hi})
			pos = hi
		}
		for i := 1; i < len(spans); i++ {
			if spans[i].lo < spans[i-1].hi {
				t.Fatalf("锚点重叠: %v 与 %v", spans[i-1], spans[i])
			}
		}
	}
}

// locateSpan 在 a 中从 from 起找到 hunk 的 (before++remove++after) 位置。
// unique 为真时要求在该后缀中唯一匹配。
func locateSpan(a [][]byte, from int, h Hunk, unique bool) (int, int, bool) {
	pat := append(append(append([][]byte{}, h.Before...), h.Remove...), h.After...)
	found, foundAt := -1, 0
	for i := from; i+len(pat) <= len(a); i++ {
		match := true
		for k := range pat {
			if !bytes.Equal(a[i+k], pat[k]) {
				match = false
				break
			}
		}
		if match {
			if found >= 0 && unique {
				return 0, 0, false
			}
			if found < 0 {
				found, foundAt = i, i
			}
		}
	}
	if found < 0 {
		return 0, 0, false
	}
	return foundAt, foundAt + len(pat), true
}

// TestHunksHandcrafted 覆盖手写样例（§附录 L.2 #5 的缩减版）。
func TestHunksHandcrafted(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
	}{
		{"纯插入", []string{"a", "b"}, []string{"a", "X", "b"}},
		{"纯删除", []string{"a", "X", "b"}, []string{"a", "b"}},
		{"替换一行", []string{"a", "b", "c"}, []string{"a", "B", "c"}},
		{"全替换", []string{"a", "b"}, []string{"c", "d"}},
		{"空到有", []string{}, []string{"x"}},
		{"有到空", []string{"x"}, []string{}},
		{"追加末尾", []string{"a"}, []string{"a", "b"}},
		{"删除开头", []string{"a", "b"}, []string{"b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := toLines(tc.a), toLines(tc.b)
			hunks := Hunks(a, b, DefaultContextLines)
			got, ok := applyViaA(a, b, hunks)
			if !ok {
				t.Fatalf("应用失败: %+v", hunks)
			}
			if !equalLines(got, b) {
				t.Fatalf("结果 %v != %v", fromLines(got), tc.b)
			}
		})
	}
}

// TestLineSplitRoundTrip 断言行切分与拼接严格互逆（§3.5）。
func TestLineSplitRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("a"),
		[]byte("a\n"),
		[]byte("a\nb"),
		[]byte("a\nb\n"),
		[]byte("a\r\nb\r\n"),
		[]byte("\n\n\n"),
		[]byte("a\xff\xfeb"), // 非法 UTF-8 也必须原样往返
	}
	for _, c := range cases {
		lines := SplitLines(c)
		back := JoinLines(lines)
		if !bytes.Equal(c, back) {
			t.Fatalf("往返失败: %q -> %q", c, back)
		}
	}
}

// TestDetectEOL 覆盖换行风格判定（§3.5）。
func TestDetectEOL(t *testing.T) {
	cases := []struct {
		in   string
		want uint8
	}{
		{"", EOLlf},
		{"abc", EOLlf},
		{"a\nb\n", EOLlf},
		{"a\r\nb\r\n", EOLcrlf},
		{"a\r\nb\n", EOLmixed},
	}
	for _, c := range cases {
		if got := DetectEOL([]byte(c.in)); got != c.want {
			t.Errorf("DetectEOL(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestIsEOLOnlyChange 覆盖「纯换行差异不产生内容 op」（§3.5、附录 L.2 #8）。
func TestIsEOLOnlyChange(t *testing.T) {
	if !IsEOLOnlyChange([]byte("a\nb\n"), []byte("a\r\nb\r\n")) {
		t.Fatal("纯 CRLF/LF 差异应被识别")
	}
	if IsEOLOnlyChange([]byte("a\nb\n"), []byte("a\nc\n")) {
		t.Fatal("内容差异不应被识别为换行差异")
	}
	if IsEOLOnlyChange([]byte("a\n"), []byte("a\n")) {
		t.Fatal("完全相同不应算作变化")
	}
}

// TestIsBinary 覆盖二进制判定（§3.5）。
func TestIsBinary(t *testing.T) {
	if IsBinary([]byte("hello world"), DefaultBinaryProbeBytes) {
		t.Fatal("纯文本被误判为二进制")
	}
	if !IsBinary([]byte("he\x00llo"), DefaultBinaryProbeBytes) {
		t.Fatal("含 NUL 未被判为二进制")
	}
	// NUL 出现在探测窗口之外时不算二进制。
	long := bytes.Repeat([]byte("a"), DefaultBinaryProbeBytes+100)
	long = append(long, 0)
	if IsBinary(long, DefaultBinaryProbeBytes) {
		t.Fatal("探测窗口之外的 NUL 不应触发判定")
	}
}
