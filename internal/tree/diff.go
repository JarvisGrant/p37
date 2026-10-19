package tree

import "bytes"

// 本文件实现附录 I 的 histogram diff 与带上下文的 hunk 生成。
//
// 冻结的决策（附录 I.3）：
//   - 锚点 = 出现次数最少、且两侧都存在的行
//   - 并列时先取在 B 中出现次数最小者，再取在 A 中最早出现者
//   - 链长上限 = maxChainLength（默认 64）
//   - 无锚点区间**直接输出 replace，不回退 Myers**（D25）
//   - 行比较为原始字节精确比较
//
// 行为完全确定：无随机、无 map 遍历顺序依赖。

// DefaultMaxChainLength 是 histogram diff 的链长上限（附录 I.3）。
const DefaultMaxChainLength = 64

// Region 表示一个变更区间：A[AStart:AStart+ALen] 被 B[BStart:BStart+BLen] 替换。
type Region struct {
	AStart, ALen int
	BStart, BLen int
}

// Histogram 用附录 I 的算法求出 A→B 的变更区间，按位置升序返回。
func Histogram(a, b [][]byte) []Region {
	var out []Region
	hist(a, b, 0, 0, DefaultMaxChainLength, &out)
	return out
}

// HistogramWith 允许指定链长上限。
func HistogramWith(a, b [][]byte, maxChain int) []Region {
	var out []Region
	hist(a, b, 0, 0, maxChain, &out)
	return out
}

func equalLine(x, y []byte) bool { return bytes.Equal(x, y) }

// hist 在 a/b 上递归求解，aOff/bOff 是它们在原序列中的偏移。
func hist(a, b [][]byte, aOff, bOff, maxChain int, out *[]Region) {
	// 1. 剥除公共前缀。
	i := 0
	for i < len(a) && i < len(b) && equalLine(a[i], b[i]) {
		i++
	}
	// 剥除公共后缀（不与前缀重叠）。
	j := 0
	for j < len(a)-i && j < len(b)-i && equalLine(a[len(a)-1-j], b[len(b)-1-j]) {
		j++
	}

	a2 := a[i : len(a)-j]
	b2 := b[i : len(b)-j]

	// 2. 边界：一侧为空。
	switch {
	case len(a2) == 0 && len(b2) == 0:
		return
	case len(a2) == 0:
		*out = append(*out, Region{AStart: aOff + i, ALen: 0, BStart: bOff + i, BLen: len(b2)})
		return
	case len(b2) == 0:
		*out = append(*out, Region{AStart: aOff + i, ALen: len(a2), BStart: bOff + i, BLen: 0})
		return
	}

	// 3. 锚点。
	ai, bi, ok := findAnchor(a2, b2, maxChain)
	if !ok {
		// 6. 无锚点：整体作为一个 replace。
		*out = append(*out, Region{AStart: aOff + i, ALen: len(a2), BStart: bOff + i, BLen: len(b2)})
		return
	}

	// 5. 递归两侧；锚点行本身视为未变更。
	hist(a2[:ai], b2[:bi], aOff+i, bOff+i, maxChain, out)
	hist(a2[ai+1:], b2[bi+1:], aOff+i+ai+1, bOff+i+bi+1, maxChain, out)
}

// findAnchor 返回锚点在 a2 / b2 中最早出现的位置。ok 为 false 表示无锚点。
func findAnchor(a2, b2 [][]byte, maxChain int) (int, int, bool) {
	cntA := make(map[string]int, len(a2))
	for _, l := range a2 {
		cntA[string(l)]++
	}
	minCnt := len(a2) + 1
	for _, l := range a2 {
		if c := cntA[string(l)]; c < minCnt {
			minCnt = c
		}
	}
	if minCnt > maxChain {
		return 0, 0, false
	}

	cntB := make(map[string]int, len(b2))
	for _, l := range b2 {
		cntB[string(l)]++
	}
	firstB := make(map[string]int, len(b2))
	for i, l := range b2 {
		if _, seen := firstB[string(l)]; !seen {
			firstB[string(l)] = i
		}
	}

	bestLine := ""
	bestB := 0
	bestA := -1
	for i, l := range a2 {
		s := string(l)
		if cntA[s] != minCnt {
			continue
		}
		cb, inB := cntB[s]
		if !inB {
			continue
		}
		if bestA < 0 || cb < bestB {
			bestLine, bestB, bestA = s, cb, i
		}
		// 相同 cntB 时保留更早出现者（即不覆盖）。
	}
	if bestA < 0 {
		return 0, 0, false
	}
	return bestA, firstB[bestLine], true
}

// Hunk 是一个带上下文的变更块，也是 op 的基本单位（§5.1）。
type Hunk struct {
	Before    [][]byte
	Remove    [][]byte
	Insert    [][]byte
	After     [][]byte
	StartHint int // 该 hunk 在**新文件**中的起始行号（0-based）
}

// DefaultContextLines 是 §5.1 默认的上下文行数。
const DefaultContextLines = 3

// Hunks 把 A→B 的行级差异转成带上下文的 hunk 列表。
//
// **与 §5.3 的一处有意偏离（实现后修正规格）**：规格原文是「两个改动区块间距
// < 2*ctx 时合并为一个 hunk」。字面实现会把两区间之间的未变更行塞进
// remove/insert，既降低 place 命中率，又无必要。这里改为：
//
//  1. **仅在 A 侧完全相邻（gap == 0）时合并** —— 此时合并是无损的（没有中间行）。
//  2. 其余情况下**把上下文裁剪到不越过邻近区间**：
//     before 起于 max(上一区间末尾, 本区间起点-ctx)，
//     after  止于 min(下一区间起点, 本区间末尾+ctx)。
//
// 这样既不会出现两个 hunk 的锚点区间重叠（重叠会让顺序重放时后一个 hunk
// 匹配失败），又保留了最长的有效锚点 —— 直接服务于 M2 的 place 命中率指标。
//
// 面向展示的「合并大块」由 HunksMerged 提供，不影响 op 的内容。
func Hunks(a, b [][]byte, ctx int) []Hunk {
	return HunksWith(a, b, ctx, DefaultMaxChainLength)
}

// HunksWith 与 Hunks 相同，但可指定 histogram 的链长上限（附录 I.3）。
func HunksWith(a, b [][]byte, ctx, maxChain int) []Hunk {
	if ctx < 0 {
		ctx = 0
	}
	regions := mergeAdjacent(HistogramWith(a, b, maxChain))
	if len(regions) == 0 {
		return nil
	}

	hunks := make([]Hunk, 0, len(regions))
	for i, r := range regions {
		prevEnd := 0
		if i > 0 {
			p := regions[i-1]
			prevEnd = p.AStart + p.ALen
		}
		nextStart := len(a)
		if i+1 < len(regions) {
			nextStart = regions[i+1].AStart
		}
		if r.AStart > nextStart {
			nextStart = r.AStart // 防御：保持区间单调
		}
		from := max(prevEnd, r.AStart-ctx)
		to := min(nextStart, r.AStart+r.ALen+ctx)
		if from > r.AStart {
			from = r.AStart
		}
		if to < r.AStart+r.ALen {
			to = r.AStart + r.ALen
		}

		hunks = append(hunks, Hunk{
			Before:    sliceCopy(a, from, r.AStart),
			Remove:    sliceCopy(a, r.AStart, r.AStart+r.ALen),
			Insert:    sliceCopy(b, r.BStart, r.BStart+r.BLen),
			After:     sliceCopy(a, r.AStart+r.ALen, to),
			StartHint: r.BStart,
		})
	}
	return hunks
}

// mergeAdjacent 只合并 A 侧完全相邻（无中间行）的区间。
func mergeAdjacent(rs []Region) []Region {
	if len(rs) == 0 {
		return nil
	}
	out := []Region{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		aEnd := last.AStart + last.ALen
		bEnd := last.BStart + last.BLen
		if r.AStart == aEnd && r.BStart == bEnd {
			last.ALen = r.AStart + r.ALen - last.AStart
			last.BLen = r.BStart + r.BLen - last.BStart
			continue
		}
		out = append(out, r)
	}
	return out
}

func sliceCopy(src [][]byte, from, to int) [][]byte {
	if from >= to || from < 0 {
		return nil
	}
	to = min(to, len(src))
	from = min(from, to)
	out := make([][]byte, 0, to-from)
	for _, l := range src[from:to] {
		out = append(out, append([]byte(nil), l...))
	}
	return out
}
