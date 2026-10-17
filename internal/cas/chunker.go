// Package cas 实现内容寻址存储：CDC 分块、对象存储、BlobStore 接口。
package cas

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/chenyiwen/p37/internal/hash"
)

// FastCDC 参数与 gear 表构成**格式资产**，一经冻结不得更改（附录 H）。
//
// 修改其中任何一项都会改变 FileContent id → Tree id → Version id，
// 使全部既有历史与 Merkle 链失效。变更必须升 format_ver 并提供迁移。
const (
	// MinSize 是最小分块长度（允许尾部残块更短）。
	MinSize = 2048
	// AvgSize 是目标平均分块长度。
	AvgSize = 16384
	// MaxSize 是最大分块长度。
	MaxSize = 65536

	gearBits = 14
	// maskS 是阶段 1 的严格掩码：0x7FFF（15 个 1）。
	maskS uint64 = (1 << (gearBits + 1)) - 1
	// maskL 是阶段 2 的宽松掩码：0x1FFF（13 个 1）。
	maskL uint64 = (1 << (gearBits - 1)) - 1
)

// gearPrefix 是 gear 表派生所用的域分隔前缀（附录 H.3）。
var gearPrefix = []byte("p37-cdc-gear-v1")

// GearTable 是 256 项的分块哈希表，由 BLAKE3 确定性派生（附录 H.3）：
//
//	GEAR[i] = LE64( BLAKE3_256("p37-cdc-gear-v1" || uint8(i))[0:8] )
//
// 使用派生而非照抄魔数表：可复现、无转录错误、避免病态碰撞。
var GearTable [256]uint64

func init() {
	for i := range GearTable {
		sum := hash.SumParts(gearPrefix, []byte{byte(i)})
		GearTable[i] = binary.LittleEndian.Uint64(sum[0:8])
	}
}

// cut 返回 data 的第一个分块长度（附录 H.4）。
//
// 前置条件由调用方保证：data 长度 <= MaxSize，或已到流末尾。
// 当 len(data) > MinSize 时，返回值总是 <= len(data)。
func cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}

	var h uint64
	i := MinSize

	// 阶段 1：严格掩码，倾向更小的块。
	lo := min(n, AvgSize)
	for ; i < lo; i++ {
		h = (h << 1) + GearTable[data[i]]
		if h&maskS == 0 {
			return i + 1
		}
	}

	// 阶段 2：宽松掩码，避免超大块。
	hi := min(n, MaxSize)
	for ; i < hi; i++ {
		h = (h << 1) + GearTable[data[i]]
		if h&maskL == 0 {
			return i + 1
		}
	}

	return hi
}

// Split 把整个字节切片切成若干分块的**结束偏移**（附录 H.5）。
//
// 偏移严格递增，最后一个等于 len(data)。空输入返回空切片（0 个分块）。
func Split(data []byte) []int {
	if len(data) == 0 {
		return nil
	}
	var ends []int
	for off := 0; off < len(data); {
		n := cut(data[off:])
		if n <= 0 {
			n = len(data) - off
		}
		off += n
		ends = append(ends, off)
	}
	return ends
}

// ChunkReader 按附录 H 对字节流分块，内存占用上界为 MaxSize*2。
type ChunkReader struct {
	r    io.Reader
	buf  []byte
	off  int
	eof  bool
	done bool
}

// NewChunkReader 包装 r。
func NewChunkReader(r io.Reader) *ChunkReader {
	return &ChunkReader{r: r, buf: make([]byte, 0, MaxSize*2)}
}

// Next 返回下一个分块。分块内容复用内部缓冲，**仅在下次调用 Next 之前有效**；
// 需要保留请自行复制。流结束时返回 io.EOF。
func (cr *ChunkReader) Next() ([]byte, error) {
	if cr.done {
		return nil, io.EOF
	}

	// 先压缩已消费的前缀，再切片。若在切片之后压缩，会覆盖上一次返回的
	// 分块内容（该切片仍指向数组前部）。
	cr.compact()

	// 保证缓冲区至少能容纳一个 MaxSize 的完整分块，或已读到 EOF。
	for !cr.eof && len(cr.buf)-cr.off < MaxSize {
		if err := cr.fill(); err != nil {
			return nil, err
		}
	}

	avail := len(cr.buf) - cr.off
	if avail == 0 {
		cr.done = true
		return nil, io.EOF
	}

	n := cut(cr.buf[cr.off : cr.off+min(avail, MaxSize)])
	if n <= 0 {
		n = avail
	}
	chunk := cr.buf[cr.off : cr.off+n]
	cr.off += n
	return chunk, nil
}

// compact 把已消费的前缀移到缓冲头部。
func (cr *ChunkReader) compact() {
	if cr.off == 0 {
		return
	}
	// 保留少量已消费数据没有意义：直接前移剩余部分。
	cr.buf = append(cr.buf[:0], cr.buf[cr.off:]...)
	cr.off = 0
}

func (cr *ChunkReader) fill() error {
	// 确保有可写空间。
	if len(cr.buf) == cap(cr.buf) {
		grown := make([]byte, len(cr.buf), cap(cr.buf)*2)
		copy(grown, cr.buf)
		cr.buf = grown
	}
	start := len(cr.buf)
	cr.buf = cr.buf[:cap(cr.buf)]
	n, err := cr.r.Read(cr.buf[start:])
	cr.buf = cr.buf[:start+n]
	if errors.Is(err, io.EOF) {
		cr.eof = true
		return nil
	}
	if err != nil {
		return err
	}
	if n == 0 {
		cr.eof = true
	}
	return nil
}
