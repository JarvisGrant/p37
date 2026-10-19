package tree

import "bytes"

// 本文件实现 §5.3 的第 1、2 趟：**块移动检测**。
//
// 背景（§5.3 原文）：派生 diff 会把「移动一段代码」拆成互不相干的一次删除与
// 一次插入。这在结果上没错，但会让重放的锚点质量变差 —— 插入那一侧往往只剩
// 极短的上下文可以锚定。
//
// 做法（与 §5.3 的四趟一致，但我们把它实现为**区域列表的后处理**而不是
// 重写 diff 内核，这样正确性仍然由已被大量测试覆盖的 histogram 保证）：
//
//	第 1 趟  精确块移动：删块与插块内容逐字节相同
//	第 2 趟  相似度移动：按行的多重集合相似度 >= move_similarity
//	第 3 趟  （其余区间）由 histogram 产出，本文件不动它
//	第 4 趟  重命名检测，在 capture 层（mergeRenames）
//
// 检测到一对之后，把「删块 + 插块」合并成**一个**覆盖两者之间的区间：
// 于是 hunk 的 before/remove/after 连成一条完整、精确的模式，锚点质量显著
// 好于原来「两个碎片」。合并本身不改变重建结果（区间边界仍精确覆盖原来的
// 两段），只是换了等价的切分方式。

// MinMoveLines 是「算得上一次块移动」的最小行数。
//
// 取 1：一行被搬到别处就是一次真实的移动（重排 import、挪一行初始化都是），
// 而 DetectMoves 只在「一删一插」两个**独立**区间之间配对，不会把普通的
// 「改了一行」（单个 replace 区间）误判成移动。
const MinMoveLines = 1

// Move 是一对「内容相同或相似、但位置变了」的块。
//
// 两个字段都是**完整的区间坐标**而不是「内容在另一侧的位置」：删除区间的
// BStart 与插入区间的 BStart 是两回事（前者是它在 B 上的对应点，后者才是内容
// 落点）。用内容位置去反查区间会找不到，这是实现时踩过的坑。
type Move struct {
	// Delete 是 A 侧的纯删除区间（ALen > 0，BLen == 0）。
	Delete Region
	// Insert 是 B 侧的纯插入区间（ALen == 0，BLen > 0）。
	Insert Region
	// Exact 为真表示两侧内容逐字节相同（第 1 趟）。
	Exact bool
	// Similarity 是两侧的行相似度（第 2 趟；Exact 时为 1）。
	Similarity float64
}

// ALen 是移动块的行数（取两侧较大者，便于展示）。
func (m Move) ALen() int { return m.Delete.ALen }

// BLen 是移动块在 B 侧的行数。
func (m Move) BLen() int { return m.Insert.BLen }

// AStart / BStart 是移动块在两侧的起点。
func (m Move) AStart() int { return m.Delete.AStart }
func (m Move) BStart() int { return m.Insert.BStart }

// DetectMoves 在 histogram 产出的区间列表里找出块移动（§5.3 第 1、2 趟）。
//
// similarity 为 0 时取 DefaultMoveSimilarity。返回顺序按 AStart 升序，
// 完全由输入决定（无随机、无 map 遍历）。
func DetectMoves(a, b [][]byte, regions []Region, similarity float64) []Move {
	if similarity <= 0 {
		similarity = DefaultMoveSimilarity
	}

	type block struct {
		region  Region
		content [][]byte
	}
	// 只考虑**纯**删除与**纯**插入区间：一个 replace 区间里既有删又有插，
	// 那是「改动了这几行」，不是移动。
	var dels, inss []block
	for _, r := range regions {
		if r.ALen >= MinMoveLines && r.BLen == 0 {
			dels = append(dels, block{r, sliceRange(a, r.AStart, r.ALen)})
		}
		if r.BLen >= MinMoveLines && r.ALen == 0 {
			inss = append(inss, block{r, sliceRange(b, r.BStart, r.BLen)})
		}
	}
	if len(dels) == 0 || len(inss) == 0 {
		return nil
	}

	used := make([]bool, len(inss))
	var out []Move
	for _, d := range dels {
		best := -1
		var bestSim float64
		for j, in := range inss {
			if used[j] {
				continue
			}
			if bytes.Equal(joinLines(d.content), joinLines(in.content)) {
				best, bestSim = j, 1
				break // 精确命中优先
			}
			if s := lineSimilarity(d.content, in.content); s >= similarity {
				// 取相似度最高者；并列时取更早出现的那个（确定性）。
				if best < 0 || s > bestSim {
					best, bestSim = j, s
				}
			}
		}
		if best < 0 {
			continue
		}
		used[best] = true
		in := inss[best]
		out = append(out, Move{
			Delete:     d.region,
			Insert:     in.region,
			Exact:      bestSim == 1,
			Similarity: bestSim,
		})
	}
	return out
}

// MergeMoves 把检测到的移动对合并成单个区间。
//
// 只有「两个区间在列表里相邻」时才合并 —— 中间若还夹着别的变更，合并会把
// 无关改动一起吞进来，得不偿失。
func MergeMoves(regions []Region, moves []Move) []Region {
	if len(moves) == 0 || len(regions) == 0 {
		return regions
	}
	find := func(want Region) int {
		for i, r := range regions {
			if r == want {
				return i
			}
		}
		return -1
	}

	drop := make(map[int]bool)
	var merged []Region
	for _, m := range moves {
		di := find(m.Delete)
		ii := find(m.Insert)
		if di < 0 || ii < 0 || di == ii {
			continue
		}
		lo, hi := di, ii
		if lo > hi {
			lo, hi = hi, lo
		}
		if hi-lo != 1 {
			continue // 中间还有别的区间，不合并
		}
		if drop[lo] || drop[hi] {
			continue
		}
		d, ins := m.Delete, m.Insert
		// 合并后的区间覆盖两者之间的全部内容（两端都取并集）。
		c := Region{
			AStart: min(d.AStart, ins.AStart),
			BStart: min(d.BStart, ins.BStart),
		}
		c.ALen = max(d.AStart+d.ALen, ins.AStart+ins.ALen) - c.AStart
		c.BLen = max(d.BStart+d.BLen, ins.BStart+ins.BLen) - c.BStart
		drop[lo], drop[hi] = true, true
		merged = append(merged, c)
	}
	if len(merged) == 0 {
		return regions
	}

	out := make([]Region, 0, len(regions))
	for i, r := range regions {
		if !drop[i] {
			out = append(out, r)
		}
	}
	out = append(out, merged...)
	sortRegions(out)
	return out
}

// HunksWithMoves 产出 hunk，并把检测到的块移动合并成单个 hunk（§5.3）。
//
// 第二个返回值是检测到的移动，供上报与测试使用。
func HunksWithMoves(a, b [][]byte, ctx, maxChain int, similarity float64) ([]Hunk, []Move) {
	regions := HistogramWith(a, b, maxChain)
	moves := DetectMoves(a, b, regions, similarity)
	regions = MergeMoves(regions, moves)
	return hunksFromRegions(a, b, regions, ctx), moves
}

// sliceRange 返回 s[i:i+n] 的浅拷贝。
func sliceRange(s [][]byte, i, n int) [][]byte {
	if i < 0 || n <= 0 || i >= len(s) {
		return nil
	}
	if i+n > len(s) {
		n = len(s) - i
	}
	out := make([][]byte, n)
	copy(out, s[i:i+n])
	return out
}

// joinLines 把行按 '\n' 连接（用于内容相等判定）。
func joinLines(ls [][]byte) []byte {
	size := max(len(ls)-1, 0)
	for _, l := range ls {
		size += len(l)
	}
	buf := make([]byte, 0, size)
	for i, l := range ls {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, l...)
	}
	return buf
}

// lineSimilarity 是两侧内容的**行多重集合**相似度：
//
//	2 * |共有行（按重数计）| / (|x| + |y|)
//
// 用多重集合而不是最长公共子序列，是因为「移动 + 微改」场景下行的顺序基本
// 一致，而这个度量对少量行的增删改更宽容，且天然确定性（只依赖排序后的计数）。
func lineSimilarity(x, y [][]byte) float64 {
	if len(x) == 0 && len(y) == 0 {
		return 1
	}
	cx := map[string]int{}
	for _, l := range x {
		cx[string(l)]++
	}
	common := 0
	for _, l := range y {
		if cx[string(l)] > 0 {
			cx[string(l)]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(x)+len(y))
}
