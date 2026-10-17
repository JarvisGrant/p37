package cas

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/hash"
)

var update = flag.Bool("update", false, "重新生成并冻结黄金向量")

// gearSnapshot 是 GEAR 表的冻结快照（附录 H.3）。
//
// 它一经冻结不得更改：gear 表是 CDC 格式资产的一部分。
type gearSnapshot struct {
	DomainSep string `json:"domain_sep"`
	Formula   string `json:"formula"`
	Count     int    `json:"count"`
	First     string `json:"gear_0"`
	Second    string `json:"gear_1"`
	Last      string `json:"gear_255"`
	TableHash string `json:"table_blake3"`
}

const gearSnapshotPath = "testdata/cdc-gear/gear.json"

func buildGearSnapshot() gearSnapshot {
	var raw [2048]byte
	for i, v := range GearTable {
		binary.LittleEndian.PutUint64(raw[i*8:], v)
	}
	return gearSnapshot{
		DomainSep: string(gearPrefix),
		Formula:   "GEAR[i] = LE64( BLAKE3_256(\"p37-cdc-gear-v1\" || uint8(i))[0:8] )",
		Count:     len(GearTable),
		First:     fmt.Sprintf("0x%016x", GearTable[0]),
		Second:    fmt.Sprintf("0x%016x", GearTable[1]),
		Last:      fmt.Sprintf("0x%016x", GearTable[255]),
		TableHash: hash.Sum(raw[:]).String(),
	}
}

// TestGearTableFrozen 断言 gear 表与冻结快照一致（附录 H.6 不变量）。
func TestGearTableFrozen(t *testing.T) {
	got := buildGearSnapshot()

	if *update {
		if err := os.MkdirAll(filepath.Dir(gearSnapshotPath), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gearSnapshotPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已重新冻结 gear 快照: %s", gearSnapshotPath)
		return
	}

	b, err := os.ReadFile(gearSnapshotPath)
	if err != nil {
		t.Fatalf("读取冻结快照失败（首次运行请加 -update）: %v", err)
	}
	var want gearSnapshot
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("gear 表已偏离冻结值 —— 这是格式破坏（附录 H）\n got: %+v\nwant: %+v", got, want)
	}
	if got.Count != 256 {
		t.Fatalf("gear 表长度 = %d，期望 256", got.Count)
	}
}

// TestCutBoundaries 覆盖附录 H.4/H.5 的边界规则。
func TestCutBoundaries(t *testing.T) {
	cases := []struct {
		name string
		size int
		want func(n int) int
	}{
		{"空文件", 0, func(int) int { return 0 }},
		{"1 字节", 1, func(n int) int { return n }},
		{"min_size 整", MinSize, func(n int) int { return n }},
		{"min_size+1", MinSize + 1, func(n int) int { return n }},
		{"max_size 整", MaxSize, func(n int) int { return n }},
		{"max_size+1", MaxSize + 1, func(int) int { panic("unreachable") }},
	}

	for _, tc := range cases[:len(cases)-1] {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.size)
			got := cut(data)
			if got != tc.want(tc.size) {
				t.Fatalf("cut 长度 %d -> %d，期望 %d", tc.size, got, tc.want(tc.size))
			}
		})
	}

	// max_size+1：cut 只看前 MaxSize 字节，必须返回 <= MaxSize。
	data := make([]byte, MaxSize+1)
	if n := cut(data); n > MaxSize || n <= 0 {
		t.Fatalf("cut(MaxSize+1) = %d，应在 (0, %d]", n, MaxSize)
	}
}

// TestCutDeterministic 断言同一输入多次调用结果一致。
func TestCutDeterministic(t *testing.T) {
	data := make([]byte, 300_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	want := Split(data)
	for i := 0; i < 200; i++ {
		got := Split(data)
		if len(got) != len(want) {
			t.Fatalf("第 %d 次分块数不同: %d vs %d", i, len(got), len(want))
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("第 %d 次第 %d 个边界不同: %d vs %d", i, j, got[j], want[j])
			}
		}
	}
}

// TestSplitReassemble 是不变量：任意随机文件的分块重组后等于原文。
func TestSplitReassemble(t *testing.T) {
	for i := 0; i < 500; i++ {
		n := 0
		if i == 0 {
			n = 0
		} else {
			n = i*997 + 1
		}
		data := make([]byte, n)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}

		ends := Split(data)
		if len(data) == 0 {
			if len(ends) != 0 {
				t.Fatalf("空输入应产生 0 个分块，得到 %d", len(ends))
			}
			continue
		}
		if ends[len(ends)-1] != len(data) {
			t.Fatalf("最后一个边界 %d != 文件长度 %d", ends[len(ends)-1], len(data))
		}

		var rebuilt []byte
		prev := 0
		for _, e := range ends {
			if e <= prev {
				t.Fatalf("边界非递增: %d 后出现 %d", prev, e)
			}
			rebuilt = append(rebuilt, data[prev:e]...)
			prev = e
		}
		if !bytes.Equal(rebuilt, data) {
			t.Fatalf("重组结果与原文不同 (n=%d)", n)
		}
	}
}

// TestChunkReaderMatchesSplit 断言流式与切片式分块完全一致。
func TestChunkReaderMatchesSplit(t *testing.T) {
	data := make([]byte, 500_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}

	ends := Split(data)
	var fromReader [][]byte
	cr := NewChunkReader(bytes.NewReader(data))
	for {
		c, err := cr.Next()
		if err != nil {
			break
		}
		fromReader = append(fromReader, append([]byte(nil), c...))
	}

	if len(fromReader) != len(ends) {
		t.Fatalf("流式分块数 %d != 切片式 %d", len(fromReader), len(ends))
	}
	prev := 0
	for i, e := range ends {
		if !bytes.Equal(fromReader[i], data[prev:e]) {
			t.Fatalf("第 %d 个分块内容不同", i)
		}
		prev = e
	}
}

// TestDedupAfterSingleEdit 断言单点修改只影响少量分块（§附录 L.1 #12）。
func TestDedupAfterSingleEdit(t *testing.T) {
	const n = 4 << 20 // 4 MiB
	orig := make([]byte, n)
	// splitmix64：确定性且统计性质良好，避免周期性数据掩盖去重问题。
	var s uint64 = 0x9E3779B97F4A7C15
	for i := range orig {
		s += 0x9E3779B97F4A7C15
		z := s
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		orig[i] = byte(z)
	}

	// 每种修改方式都必须只新增少量分块。
	edits := []struct {
		name string
		at   int
		text string
	}{
		{"尾部追加", n, "hello world"},
		{"中段插入", n / 2, "INSERTED-CONTENT"},
		{"开头替换", 100, "X"},
	}

	for _, e := range edits {
		t.Run(e.name, func(t *testing.T) {
			mod := make([]byte, 0, n+len(e.text))
			mod = append(mod, orig[:e.at]...)
			mod = append(mod, e.text...)
			mod = append(mod, orig[e.at:]...)

			cnt := func(data []byte) map[hash.ID]int {
				m := make(map[hash.ID]int)
				prev := 0
				for _, end := range Split(data) {
					m[hash.Sum(data[prev:end])]++
					prev = end
				}
				return m
			}

			base := cnt(orig)
			after := cnt(mod)
			newChunks := 0
			for id, c := range after {
				if base[id] < c {
					newChunks += c - base[id]
				}
			}
			// 单点修改后，内容定义分块应在 ~2 个平均块长内重新对齐。
			if maxNew := 16; newChunks > maxNew {
				t.Fatalf("单点修改产生 %d 个新分块，超过上限 %d（去重失效）", newChunks, maxNew)
			}
			t.Logf("%s: 新增分块 %d，总块数 %d", e.name, newChunks, len(after))
		})
	}
}

// TestChunkSizesWithinBounds 断言所有非尾块都在 [MinSize, MaxSize] 内。
func TestChunkSizesWithinBounds(t *testing.T) {
	for _, n := range []int{MinSize + 1, 100_000, 1 << 20} {
		data := make([]byte, n)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		ends := Split(data)
		prev := 0
		for i, e := range ends {
			size := e - prev
			last := i == len(ends)-1
			if size > MaxSize {
				t.Fatalf("n=%d 第 %d 块 %d 字节 > MaxSize", n, i, size)
			}
			if !last && size < MinSize {
				t.Fatalf("n=%d 第 %d 块 %d 字节 < MinSize", n, i, size)
			}
			prev = e
		}
	}
}
