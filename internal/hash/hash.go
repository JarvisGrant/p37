// Package hash 提供 BLAKE3-256 的内容寻址对象标识。
//
// 规格依据：附录 A.1（哈希函数 = BLAKE3，输出 32 字节原始二进制）。
package hash

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"
)

// Size 是对象标识的字节长度（BLAKE3-256）。
const Size = 32

// HexLen 是标识的十六进制字符串长度。
const HexLen = Size * 2

// ID 是一个内容寻址标识：BLAKE3-256 的原始 32 字节输出。
type ID [Size]byte

// Zero 是全零标识。用于表示 Version 的「首版本」parent（附录 A.3）。
var Zero ID

// Sum 计算 b 的 BLAKE3-256。
func Sum(b []byte) ID { return SumParts(b) }

// SumParts 按顺序拼接多段后计算 BLAKE3-256，不产生额外的拼接缓冲。
func SumParts(parts ...[]byte) ID {
	h := blake3.New()
	for _, p := range parts {
		if len(p) > 0 {
			_, _ = h.Write(p)
		}
	}
	var id ID
	copy(id[:], h.Sum(nil))
	return id
}

// String 返回 64 字符的小写十六进制。
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// IsZero 报告 id 是否为全零。
func (id ID) IsZero() bool { return id == Zero }

// Parse 解析 64 字符小写十六进制标识。
func Parse(s string) (ID, error) {
	var id ID
	if len(s) != HexLen {
		return id, fmt.Errorf("hash: 非法长度 %d，期望 %d", len(s), HexLen)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("hash: 非法十六进制: %w", err)
	}
	copy(id[:], b)
	return id, nil
}

// MustParse 解析标识，失败则 panic。仅用于测试与冻结常量。
func MustParse(s string) ID {
	id, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

// MarshalJSON 实现 json.Marshaler（小写十六进制字符串）。
func (id ID) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, HexLen+2)
	buf = append(buf, '"')
	buf = hex.AppendEncode(buf, id[:])
	buf = append(buf, '"')
	return buf, nil
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (id *ID) UnmarshalJSON(b []byte) error {
	if len(b) != HexLen+2 || b[0] != '"' || b[len(b)-1] != '"' {
		return fmt.Errorf("hash: 期望 %d 字符的 JSON 字符串", HexLen)
	}
	parsed, err := Parse(string(b[1 : len(b)-1]))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// MarshalText 实现 encoding.TextMarshaler。
func (id ID) MarshalText() ([]byte, error) { return hex.AppendEncode(nil, id[:]), nil }

// UnmarshalText 实现 encoding.TextUnmarshaler。
func (id *ID) UnmarshalText(b []byte) error {
	parsed, err := Parse(string(b))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
