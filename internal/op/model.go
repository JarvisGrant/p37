// Package op 实现 op 模型、place 引擎（§6.1）、ConflictKey（§6.4）与各类
// op 的重放语义（§6.5）。
//
// 关键性质：对同一 (ops, target)，重放结果必须**完全确定**（逐字节可复现）。
// 这是 §14.1.4（顺序无关）与 §14.1.5（land_preview 与 ws_land 一致）的基础。
package op

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
)

// Kind 是 op 的种类（§5.1）。
type Kind string

// op 种类。
const (
	KindCreate  Kind = "create"
	KindModify  Kind = "modify"
	KindDelete  Kind = "delete"
	KindRename  Kind = "rename"
	KindAttr    Kind = "attr"
	KindBlobPut Kind = "blob_put"
)

// Valid 报告 kind 是否为已知取值。
func (k Kind) Valid() bool {
	switch k {
	case KindCreate, KindModify, KindDelete, KindRename, KindAttr, KindBlobPut:
		return true
	}
	return false
}

// Hunk 是一个带上下文的编辑块（§5.1、附录 I）。
//
// 各字段都为**原始行字节**（不含 '\n'，可能含尾部 '\r'）。序列化时按 base64
// 编码，以免非法 UTF-8 被 JSON 替换而损坏内容。
type Hunk struct {
	Before    [][]byte
	Remove    [][]byte
	Insert    [][]byte
	After     [][]byte
	StartHint int
}

// FromTreeHunk 由 tree.Hunk 构造。
func FromTreeHunk(h tree.Hunk) Hunk {
	return Hunk{
		Before:    h.Before,
		Remove:    h.Remove,
		Insert:    h.Insert,
		After:     h.After,
		StartHint: h.StartHint,
	}
}

// RemovedHash 返回 remove 部分的哈希（§3.5、§5.3）：
// BLAKE3(join(remove, "\n"))，使用原始行字节。
func (h *Hunk) RemovedHash() hash.ID {
	size := max(len(h.Remove)-1, 0)
	for _, l := range h.Remove {
		size += len(l)
	}
	buf := make([]byte, 0, size)
	for i, l := range h.Remove {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, l...)
	}
	return hash.Sum(buf)
}

// Pattern 返回 (before ++ remove ++ after)，即用于定位的完整模式。
func (h *Hunk) Pattern() [][]byte {
	out := make([][]byte, 0, len(h.Before)+len(h.Remove)+len(h.After))
	out = append(out, h.Before...)
	out = append(out, h.Remove...)
	out = append(out, h.After...)
	return out
}

// EntryKindOf 返回归一化后的条目类型（0 视为常规文件）。
func (o *Op) EntryKindOf() uint8 {
	if o.EntryKind == codec.EntrySymlink {
		return codec.EntrySymlink
	}
	return codec.EntryFile
}

// Attr 是 kind=attr 的载荷。
type Attr struct {
	EOL uint8
}

// Op 是一次采集产生的单个改动（§5.1）。
//
// 与 §5.1 原文的字段对应关系（实现后明确化）：
//
//	BaseFile ≡ blob_before：期望的当前 FileContent id；全零表示「该路径不应存在」
//	NewFile  ≡ blob_after：create / attr / blob_put 的新 FileContent id
type Op struct {
	ID   string
	WsID string
	Seq  int64
	Kind Kind

	// Path 是源路径。对 rename 而言是被重命名的旧路径。
	Path string
	// DestPath 仅用于 kind=rename：目标路径。
	DestPath string

	// EntryKind 是条目类型：codec.EntryFile 或 codec.EntrySymlink。
	//
	// 必须持久化：否则「文件 ↔ 符号链接」的类型变化在重放后会退化成常规文件
	// （内容变成链接目标文本）。0 视为 EntryFile（老数据的默认）。
	EntryKind uint8

	Hunks []Hunk

	// BaseFile 是该路径在**采集时刻**的 FileContent id；create 时为全零。
	//
	// 它让 delete / attr / rename / blob_put 的重放可以精确校验「目标是否
	// 已被第三方改动」，而不必依赖模糊推断。
	BaseFile hash.ID

	// NewFile 是 create / attr / blob_put 产生的新 FileContent id。
	NewFile hash.ID

	// Attr 仅用于 kind=attr。
	Attr *Attr

	CapturedTS int64
	CapturedBy string
	Derived    bool
}

// --- 序列化 -------------------------------------------------------------
//
// op 不进哈希，因此用 JSON 存储；但行内容可能是非法 UTF-8（例如接近二进制的
// 文本），JSON 字符串会把非法字节替换为 U+FFFD 从而**损坏内容**。因此所有行
// 按 base64 存储。

type hunkWire struct {
	Before    []string `json:"before,omitempty"`
	Remove    []string `json:"remove,omitempty"`
	Insert    []string `json:"insert,omitempty"`
	After     []string `json:"after,omitempty"`
	StartHint int      `json:"start_hint"`
}

// MarshalJSON 实现 json.Marshaler。
func (h Hunk) MarshalJSON() ([]byte, error) {
	w := hunkWire{
		Before:    encodeLines(h.Before),
		Remove:    encodeLines(h.Remove),
		Insert:    encodeLines(h.Insert),
		After:     encodeLines(h.After),
		StartHint: h.StartHint,
	}
	return json.Marshal(w)
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (h *Hunk) UnmarshalJSON(b []byte) error {
	var w hunkWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	var err error
	if h.Before, err = decodeLines(w.Before); err != nil {
		return err
	}
	if h.Remove, err = decodeLines(w.Remove); err != nil {
		return err
	}
	if h.Insert, err = decodeLines(w.Insert); err != nil {
		return err
	}
	if h.After, err = decodeLines(w.After); err != nil {
		return err
	}
	h.StartHint = w.StartHint
	return nil
}

// MarshalHunks 序列化 hunk 列表（写入 hunks_json 列）。
func MarshalHunks(hs []Hunk) (string, error) {
	if len(hs) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(hs)
	if err != nil {
		return "", fmt.Errorf("op: 序列化 hunks: %w", err)
	}
	return string(b), nil
}

// UnmarshalHunks 反序列化 hunk 列表。
func UnmarshalHunks(s string) ([]Hunk, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var hs []Hunk
	if err := json.Unmarshal([]byte(s), &hs); err != nil {
		return nil, fmt.Errorf("op: 反序列化 hunks: %w", err)
	}
	return hs, nil
}

func encodeLines(ls [][]byte) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = base64.StdEncoding.EncodeToString(l)
	}
	return out
}

func decodeLines(ss []string) ([][]byte, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	out := make([][]byte, len(ss))
	for i, s := range ss {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("op: 行 base64 解码失败: %w", err)
		}
		out[i] = b
	}
	return out, nil
}
