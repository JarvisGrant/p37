// Package version 实现 Version 对象的编解码与血缘。
//
// 规格依据：附录 A.3。Version id 是 Merkle 链（含 parent），因此 **version
// 一旦产生永不改写**：压缩与回收只能作用于对象与包，绝不能重写 version 身份
// （§3.1）。
package version

import (
	"fmt"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// Meta 是 Version 的血缘与描述信息。
//
// 纳入 id 的字段（meta_core）= (Parent, Tree, Epoch, Agent, TaskHash, TS, Msg)。
// 不纳入 id 的字段 = (JournalID, WsID)。
type Meta struct {
	Parent   hash.ID // 首版本为 hash.Zero
	Tree     hash.ID
	Epoch    uint64
	Agent    string
	TaskHash hash.ID // BLAKE3(TaskDesc)
	TaskDesc string  // 不参与 id，仅存储
	TS       int64   // Unix 毫秒
	Msg      string
}

// Version 是一个不可变的版本。
type Version struct {
	Meta
	JournalID int64
	WsID      string
}

// Encode 返回参与 id 计算的确定性编码（meta_core）。
//
// 注意：JournalID 与 WsID 不参与编码，因此改变它们不会改变 version id。
func (v *Version) Encode() []byte {
	e := codec.NewEncoder()
	e.U8(codec.TypeVersion)
	e.Hash(v.Parent)
	e.Hash(v.Tree)
	e.Uvarint(v.Epoch)
	e.Str(v.Agent)
	e.Hash(v.TaskHash)
	e.U64(uint64(v.TS))
	e.Str(v.Msg)
	return e.Out()
}

// ID 返回 version 的内容寻址标识。
func (v *Version) ID() hash.ID { return hash.Sum(v.Encode()) }

// New 构造一个 Version 并计算 TaskHash。
func New(parent, treeID hash.ID, epoch uint64, agent, taskDesc string, tsMillis int64, msg string) *Version {
	return &Version{Meta: Meta{
		Parent:   parent,
		Tree:     treeID,
		Epoch:    epoch,
		Agent:    agent,
		TaskHash: hash.Sum([]byte(taskDesc)),
		TaskDesc: taskDesc,
		TS:       tsMillis,
		Msg:      msg,
	}}
}

// IsFirst 报告是否为首版本（parent 为全零）。
func (v *Version) IsFirst() bool { return v.Parent.IsZero() }

// Decode 解析 Version 编码。TaskDesc 与 JournalID/WsID 不在编码内，
// 需由调用方从元数据库补全。
func Decode(b []byte) (*Version, error) {
	d := codec.NewDecoder(b)
	if typ := d.U8(); typ != codec.TypeVersion {
		return nil, fmt.Errorf("version: 期望 Version(0x%02x)，得到 0x%02x", codec.TypeVersion, typ)
	}
	var v Version
	v.Parent = d.Hash()
	v.Tree = d.Hash()
	v.Epoch = d.Uvarint()
	v.Agent = d.Str()
	v.TaskHash = d.Hash()
	v.TS = int64(d.U64())
	v.Msg = d.Str()
	if err := d.Finish(); err != nil {
		return nil, err
	}
	return &v, nil
}
