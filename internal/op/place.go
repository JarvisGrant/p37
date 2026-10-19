package op

import "bytes"

// 本文件实现 §6.1 的 place 算法。
//
// 冻结的规则（§6.1）：
//
//	匹配方式：原始字节精确匹配，不做空白归一化、不做模糊匹配
//	搜索范围：整个目标文本，无窗口限制
//	多匹配：取最接近 hint 者；并列最小则冲突（不猜）
//	匹配上限：maxMatches（默认 64），超过即视为「过多匹配」-> 冲突
//	hint 失效：回退到全文搜索
//
// 返回的 (patternStart, replStart, replEnd) 中，替换作用是
// target[replStart:replEnd] -> hunk.Insert。

// DefaultMaxMatches 是 place 的匹配上限（§6.1、附录 B 的 merge.max_matches）。
const DefaultMaxMatches = 64

// Locate 在 target 中定位一个 hunk。
//
// hint 是该 hunk 在新文件中的 0-based 起始行号（仅作提示）。
// ok 为 false 表示产生**冲突**（调用方应记录 Conflict）。
func Locate(h *Hunk, target [][]byte, hint, maxMatches int) (patternStart, replStart, replEnd int, ok bool) {
	if maxMatches <= 0 {
		maxMatches = DefaultMaxMatches
	}

	pat := h.Pattern()

	// 1. 按 hint 原位试。
	if len(pat) > 0 && hint >= 0 && hint+len(pat) <= len(target) && matchAt(target, pat, hint) {
		return startOf(h, hint)
	}

	// 2. 全文搜索完整模式。
	pos := matchPositions(target, pat, maxMatches)
	switch {
	case len(pos) > maxMatches:
		return 0, 0, 0, false // 过多匹配
	case len(pos) == 1:
		return startOf(h, pos[0])
	case len(pos) > 1:
		return pickNearest(h, pos, hint)
	}

	// 3. 纯插入：退化为搜索 (before ++ after) 作为锚点。
	if len(h.Remove) == 0 {
		anchor := make([][]byte, 0, len(h.Before)+len(h.After))
		anchor = append(anchor, h.Before...)
		anchor = append(anchor, h.After...)
		pos = matchPositions(target, anchor, maxMatches)
		switch {
		case len(pos) > maxMatches:
			return 0, 0, 0, false
		case len(pos) == 1:
			return startOf(h, pos[0])
		case len(pos) > 1:
			return pickNearest(h, pos, hint)
		}
	}

	// 4. 全部失败。
	return 0, 0, 0, false
}

// startOf 把模式起点换算为 (patternStart, replStart, replEnd)。
func startOf(h *Hunk, patternStart int) (int, int, int, bool) {
	replStart := patternStart + len(h.Before)
	return patternStart, replStart, replStart + len(h.Remove), true
}

// pickNearest 在多处匹配中取最接近 hint 者；并列最小视为冲突（不猜）。
func pickNearest(h *Hunk, positions []int, hint int) (int, int, int, bool) {
	best, bestDist, tie := -1, 0, false
	for _, p := range positions {
		d := abs(p - hint)
		switch {
		case best < 0 || d < bestDist:
			best, bestDist, tie = p, d, false
		case d == bestDist:
			tie = true
		}
	}
	if best < 0 || tie {
		return 0, 0, 0, false
	}
	return startOf(h, best)
}

// Place 在 target 上放置一个 hunk，返回替换后的**新切片**（target 不被修改）。
func Place(h *Hunk, target [][]byte, hint, maxMatches int) (out [][]byte, ok bool) {
	_, replStart, replEnd, ok := Locate(h, target, hint, maxMatches)
	if !ok {
		return nil, false
	}
	out = make([][]byte, 0, len(target)-(replEnd-replStart)+len(h.Insert))
	out = append(out, target[:replStart]...)
	out = append(out, h.Insert...)
	out = append(out, target[replEnd:]...)
	return out, true
}

// PlaceAll 依次放置全部 hunk，**原地推进**目标。返回第一个失败的 hunk 下标。
func PlaceAll(hunks []Hunk, target [][]byte, maxMatches int) (out [][]byte, failedAt int) {
	cur := target
	for i := range hunks {
		h := &hunks[i]
		next, ok := Place(h, cur, h.StartHint, maxMatches)
		if !ok {
			return cur, i
		}
		cur = next
	}
	return cur, -1
}

// matchAt 报告 pat 是否匹配 target[pos:pos+len(pat)]。
func matchAt(target, pat [][]byte, pos int) bool {
	if pos < 0 || pos+len(pat) > len(target) {
		return false
	}
	for i := range pat {
		if !bytes.Equal(target[pos+i], pat[i]) {
			return false
		}
	}
	return true
}

// matchPositions 返回 pat 在 target 中的所有出现位置，最多收集 limit+1 个
// （超过即足以判定「过多匹配」，无需继续扫描）。
func matchPositions(target, pat [][]byte, limit int) []int {
	if len(pat) == 0 {
		return nil
	}
	var out []int
	for i := 0; i+len(pat) <= len(target); i++ {
		if matchAt(target, pat, i) {
			out = append(out, i)
			if len(out) > limit {
				return out
			}
		}
	}
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// autoMerge 在「两侧改动不重叠」的前提下把一个放不下的 hunk 自动合并掉（§6.3）。
//
// 返回 false 表示无法确定 —— 调用方必须退回冲突，**绝不猜**。所有判据都要求
// **唯一匹配**。
//
//   - Remove 非空：本侧要替换的那段原文在目标里仍唯一存在，说明对侧没有碰
//     这些行，直接把 Remove 换成 Insert。
//   - Remove 为空（纯插入）：对侧在同一锚点插入过内容。本侧 op 的 seq 必然
//     晚于目标里已有的内容，因此把本侧内容拼在**后面**（即紧挨 After 之前），
//     这就是 §6.3 的「按 op seq 拼接」。
//
// 锚点定位允许**逐级放宽上下文**：从完整的 Before/After 开始，找不到就在
// 目标里试更短的后缀/前缀，直到能唯一匹配。这样「在文件末尾追加一行」不会
// 因为紧邻的上一行被对侧改过就退化成一个假冲突 —— 这是最常见的误报来源。
// 每一级都要求唯一匹配，因此放宽不会把改动落到错误的位置。
func autoMerge(h *Hunk, target [][]byte) ([][]byte, bool) {
	if len(h.Remove) > 0 {
		pos := matchPositions(target, h.Remove, 1)
		if len(pos) != 1 {
			return nil, false
		}
		return spliceAt(target, pos[0], len(h.Remove), h.Insert), true
	}
	return autoMergeInsert(h, target)
}

// autoMergeInsert 处理纯插入 hunk 的自动合并。
//
// 判定顺序（每一步都要求**唯一匹配**，都不成立就退回冲突）：
//
//  1. After 可定位 -> 插在它前面，即对侧已插入内容的**后面**（§6.3 的 seq 拼接）。
//  2. After 为空 -> 本侧是**在文件末尾追加**。追加位置不依赖紧邻的上文，
//     因此即使上一行刚被对侧改过也应当合并（这是最常见的误报来源：
//     父工作区改了某行，子工作区在文件尾部追加）。
//  3. Before 可定位 -> 插在它后面。
func autoMergeInsert(h *Hunk, target [][]byte) ([][]byte, bool) {
	if len(h.Before) == 0 && len(h.After) == 0 {
		// 无锚点（例如向空文件插入）。只有目标为空时才没有歧义。
		if len(target) == 0 {
			return append([][]byte(nil), h.Insert...), true
		}
		return nil, false
	}

	from, haveFrom := 0, false
	if len(h.Before) > 0 {
		if end, ok := anchorAfter(target, h.Before); ok {
			from, haveFrom = end, true
		}
	}

	if len(h.After) > 0 {
		if at, ok := anchorBefore(target, from, h.After); ok {
			return spliceAt(target, at, 0, h.Insert), true
		}
	} else {
		// 末尾追加。注意行切分把「文件以换行结尾」表示为一个尾随空元素，
		// 因此追加点是 len-1 而不是 len。
		return spliceAt(target, appendIndex(target), 0, h.Insert), true
	}

	if haveFrom {
		return spliceAt(target, from, 0, h.Insert), true
	}
	return nil, false
}

// appendIndex 返回「在文件末尾追加一行」的插入下标。
//
// 行切分把「以换行结尾」表示成尾随的空元素，所以有尾随空元素时插入点要
// 退一格，否则会多出一个空行。
func appendIndex(target [][]byte) int {
	if n := len(target); n > 0 && len(target[n-1]) == 0 {
		return n - 1
	}
	return len(target)
}

// anchorAfter 返回 before 的**最长唯一可定位后缀**在 target 中匹配结束的位置。
func anchorAfter(target, before [][]byte) (int, bool) {
	for k := len(before); k >= 1; k-- {
		suf := before[len(before)-k:]
		if pos := matchPositions(target, suf, 1); len(pos) == 1 {
			return pos[0] + k, true
		}
	}
	return 0, false
}

// anchorBefore 返回 after 的**最长唯一可定位前缀**在 target[from:] 中的起始位置。
func anchorBefore(target [][]byte, from int, after [][]byte) (int, bool) {
	if from < 0 {
		from = 0
	}
	if from > len(target) {
		return 0, false
	}
	tail := target[from:]
	for k := len(after); k >= 1; k-- {
		if pos := matchPositions(tail, after[:k], 1); len(pos) == 1 {
			return from + pos[0], true
		}
	}
	return 0, false
}
