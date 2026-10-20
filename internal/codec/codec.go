// Package codec 实现附录 A 的确定性二进制编码原语。
//
// 硬要求（§3.2）：同一逻辑对象在任何端、任何顺序、任何平台下必须算出同一个
// id。因此仅使用：定长小端整数、LEB128 无符号变长整数、带长度前缀的字节串。
// 不得使用 JSON / MessagePack / protobuf 的 map 语义。
package codec

import (
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/chenyiwen/p37/internal/hash"
)

// 对象类型号（附录 A.2）。
const (
	TypeBlob        uint8 = 0x01
	TypeFileContent uint8 = 0x02
	TypeTree        uint8 = 0x03
	TypeVersion     uint8 = 0x04
	// TypeVersionMerge 是多父（merge）版本对象。线性版本仍使用 TypeVersion，
	// 保证老仓库的 version id 不变。
	TypeVersionMerge uint8 = 0x05
)

// EOL 取值（附录 A.3）。
const (
	EOLlf    uint8 = 0
	EOLcrlf  uint8 = 1
	EOLmixed uint8 = 2
)

// EOLName 返回可读名称。
func EOLName(v uint8) string {
	switch v {
	case EOLlf:
		return "lf"
	case EOLcrlf:
		return "crlf"
	case EOLmixed:
		return "mixed"
	default:
		return fmt.Sprintf("eol(%d)", v)
	}
}

// 目录条目种类（附录 A.3）。
const (
	EntryFile    uint8 = 0
	EntryTree    uint8 = 1
	EntrySymlink uint8 = 2
)

// 文件模式（附录 A.3）。
const (
	ModeNormal     uint64 = 0 // 0644
	ModeExecutable uint64 = 1 // 0755
)

// NFC 返回 s 的 NFC 归一化形式。
//
// 路径规则（§3.3）：进入系统前必须做 NFC 归一化，否则 macOS(NFD) 与
// Linux(NFC) 会对同一路径算出不同的哈希。
func NFC(s string) string {
	if norm.NFC.IsNormalString(s) {
		return s
	}
	return norm.NFC.String(s)
}

// Encoder 构建一段确定性字节串。
//
// 用法：按字段顺序调用方法，最后取 Bytes()。
type Encoder struct{ buf []byte }

// NewEncoder 返回空编码器。
func NewEncoder() *Encoder { return &Encoder{} }

// U8 写入单字节。
func (e *Encoder) U8(v uint8) { e.buf = append(e.buf, v) }

// Uvarint 写入 LEB128 无符号变长整数。
func (e *Encoder) Uvarint(v uint64) { e.buf = binary.AppendUvarint(e.buf, v) }

// U64 写入 8 字节小端整数（仅定长字段使用）。
func (e *Encoder) U64(v uint64) { e.buf = binary.LittleEndian.AppendUint64(e.buf, v) }

// Bytes 写入 uvarint 长度前缀 + 原始字节。
func (e *Encoder) Bytes(b []byte) {
	e.Uvarint(uint64(len(b)))
	e.buf = append(e.buf, b...)
}

// Str 写入 uvarint 长度前缀 + NFC 归一化后的 UTF-8。
func (e *Encoder) Str(s string) { e.Bytes([]byte(NFC(s))) }

// Hash 写入 32 字节标识（无长度前缀，固定长度）。
func (e *Encoder) Hash(h hash.ID) { e.buf = append(e.buf, h[:]...) }

// Out 返回已编码内容。返回的切片与内部缓冲共享，调用方不得修改。
func (e *Encoder) Out() []byte { return e.buf }

// Len 返回已编码字节数。
func (e *Encoder) Len() int { return len(e.buf) }

// Decoder 解析确定性字节串。
//
// 所有方法在出错后返回零值并把错误记录在 Err() 中，因此可以在若干次调用之后
// 统一检查一次。最后应调用 Done() 确认没有多余字节。
type Decoder struct {
	buf []byte
	off int
	err error
}

// NewDecoder 返回针对 b 的解码器。b 不会被修改。
func NewDecoder(b []byte) *Decoder { return &Decoder{buf: b} }

// Err 返回首个解码错误，若无则返回 nil。
func (d *Decoder) Err() error { return d.err }

func (d *Decoder) fail(format string, args ...any) {
	if d.err == nil {
		d.err = fmt.Errorf("codec: "+format, args...)
	}
}

// U8 读取单字节。
func (d *Decoder) U8() uint8 {
	if d.err != nil {
		return 0
	}
	if d.off >= len(d.buf) {
		d.fail("U8 越界 (off=%d, len=%d)", d.off, len(d.buf))
		return 0
	}
	v := d.buf[d.off]
	d.off++
	return v
}

// Uvarint 读取 LEB128 无符号变长整数。
func (d *Decoder) Uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.buf[d.off:])
	if n <= 0 {
		d.fail("Uvarint 解码失败 (off=%d)", d.off)
		return 0
	}
	d.off += n
	return v
}

// U64 读取 8 字节小端整数。
func (d *Decoder) U64() uint64 {
	if d.err != nil {
		return 0
	}
	if d.off+8 > len(d.buf) {
		d.fail("U64 越界 (off=%d, len=%d)", d.off, len(d.buf))
		return 0
	}
	v := binary.LittleEndian.Uint64(d.buf[d.off:])
	d.off += 8
	return v
}

// Bytes 读取 uvarint 长度前缀 + 原始字节。
func (d *Decoder) Bytes() []byte {
	if d.err != nil {
		return nil
	}
	n := d.Uvarint()
	if d.err != nil {
		return nil
	}
	if n > uint64(len(d.buf)-d.off) {
		d.fail("Bytes 越界: 需要 %d，剩余 %d", n, len(d.buf)-d.off)
		return nil
	}
	v := d.buf[d.off : d.off+int(n)]
	d.off += int(n)
	return v
}

// Str 读取 uvarint 长度前缀 + UTF-8 字符串。
func (d *Decoder) Str() string { return string(d.Bytes()) }

// Hash 读取固定的 32 字节标识。
func (d *Decoder) Hash() hash.ID {
	var id hash.ID
	if d.err != nil {
		return id
	}
	if d.off+hash.Size > len(d.buf) {
		d.fail("Hash 越界 (off=%d, len=%d)", d.off, len(d.buf))
		return id
	}
	copy(id[:], d.buf[d.off:d.off+hash.Size])
	d.off += hash.Size
	return id
}

// Rest 返回剩余未解码字节。
func (d *Decoder) Rest() []byte {
	if d.err != nil {
		return nil
	}
	return d.buf[d.off:]
}

// Done 报告是否已消费完所有字节。
func (d *Decoder) Done() bool { return d.err == nil && d.off == len(d.buf) }

// Finish 在解码结束时调用：确认无错误且无多余字节。
func (d *Decoder) Finish() error {
	if d.err != nil {
		return d.err
	}
	if d.off != len(d.buf) {
		return fmt.Errorf("codec: 尾部有 %d 字节多余数据", len(d.buf)-d.off)
	}
	return nil
}

// PathError 描述一条非法路径的原因（§3.3、§13）。
type PathError struct {
	Path   string
	Reason string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("codec: 非法路径 %q: %s", e.Path, e.Reason)
}

// ValidatePath 校验并归一化一条仓库内路径（§3.3）。
//
// 规则：UTF-8、NFC、以 "/" 分隔、无前导 "/"、无空段、无 "."、无 ".."、
// 总长 ≤ MaxPathBytes、单段 ≤ MaxSegmentBytes。
func ValidatePath(p string) (string, error) {
	const (
		MaxPathBytes    = 4096
		MaxSegmentBytes = 255
	)
	if p == "" {
		return "", &PathError{p, "空路径"}
	}
	if !norm.NFC.IsNormalString(p) {
		// 允许但归一化；若归一化后与原文不同，调用方应使用返回值。
		p = norm.NFC.String(p)
	}
	if len(p) > MaxPathBytes {
		return "", &PathError{p, fmt.Sprintf("超过 %d 字节", MaxPathBytes)}
	}
	if strings.HasPrefix(p, "/") {
		return "", &PathError{p, "不允许前导 /"}
	}
	if strings.HasSuffix(p, "/") {
		return "", &PathError{p, "不允许尾随 /"}
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "":
			return "", &PathError{p, "含空路径段"}
		case ".", "..":
			return "", &PathError{p, "含 . 或 .. 路径段"}
		}
		if len(seg) > MaxSegmentBytes {
			return "", &PathError{p, fmt.Sprintf("单段超过 %d 字节", MaxSegmentBytes)}
		}
	}
	return p, nil
}
