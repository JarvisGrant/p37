// Package version 实现 Version 对象的编解码与血缘。
//
// 规格依据：附录 A.3。Version id 是 Merkle 链（含 parent），因此 **version
// 一旦产生永不改写**：压缩与回收只能作用于对象与包，绝不能重写 version 身份
// （§3.1）。
//
// v2 增加多父（merge）版本：线性版本继续使用 TypeVersion，编码与 id 完全不变；
// merge 版本使用 TypeVersionMerge，id 覆盖全部 parents。
package version

import (
	"fmt"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// Meta 是 Version 的血缘与描述信息。
//
// 纳入 id 的字段（meta_core）= (Parents, Tree, Epoch, Agent, TaskHash, TS, Msg)。
// 不纳入 id 的字段 = (JournalID, WsID)。
//
// Parents 的第一个元素是 first parent；Parent 保留为兼容字段，始终等于
// Parents[0]（或零值）。
type Meta struct {
	Parent   hash.ID   // first parent（首版本为 hash.Zero）
	Parents  []hash.ID // 全部父；线性版本为空或长度为 1
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

// normalizeParents 返回用于编码的父列表。
func (v *Version) normalizeParents() []hash.ID {
	if len(v.Parents) > 0 {
		return v.Parents
	}
	if v.Parent.IsZero() {
		return nil
	}
	return []hash.ID{v.Parent}
}

// IsMerge 报告是否为多父版本。
func (v *Version) IsMerge() bool { return len(v.normalizeParents()) > 1 }

// Encode 返回参与 id 计算的确定性编码（meta_core）。
//
// 线性版本（0 或 1 个父）沿用 v1 编码，保证老仓库 id 不变。
func (v *Version) Encode() []byte {
	parents := v.normalizeParents()
	e := codec.NewEncoder()
	if len(parents) <= 1 {
		var p hash.ID
		if len(parents) == 1 {
			p = parents[0]
		}
		e.U8(codec.TypeVersion)
		e.Hash(p)
		e.Hash(v.Tree)
		e.Uvarint(v.Epoch)
		e.Str(v.Agent)
		e.Hash(v.TaskHash)
		e.U64(uint64(v.TS))
		e.Str(v.Msg)
		return e.Out()
	}
	e.U8(codec.TypeVersionMerge)
	e.Uvarint(uint64(len(parents)))
	for _, p := range parents {
		e.Hash(p)
	}
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

// New 构造一个线性 Version 并计算 TaskHash。
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

// NewMerge 构造一个多父 Version。parents[0] 是 first parent。
func NewMerge(parents []hash.ID, treeID hash.ID, epoch uint64, agent, taskDesc string, tsMillis int64, msg string) *Version {
	ps := append([]hash.ID{}, parents...)
	v := New(hash.Zero, treeID, epoch, agent, taskDesc, tsMillis, msg)
	if len(ps) > 0 {
		v.Parent = ps[0]
	}
	v.Parents = ps
	return v
}

// IsFirst 报告是否为首版本（parent 为全零）。
func (v *Version) IsFirst() bool {
	ps := v.normalizeParents()
	return len(ps) == 0 || ps[0].IsZero()
}

// Decode 解析 Version 编码。TaskDesc 与 JournalID/WsID 不在编码内，
// 需由调用方从元数据库补全。
func Decode(b []byte) (*Version, error) {
	d := codec.NewDecoder(b)
	typ := d.U8()
	var v Version
	switch typ {
	case codec.TypeVersion:
		v.Parent = d.Hash()
		v.Tree = d.Hash()
		v.Epoch = d.Uvarint()
		v.Agent = d.Str()
		v.TaskHash = d.Hash()
		v.TS = int64(d.U64())
		v.Msg = d.Str()
	case codec.TypeVersionMerge:
		n := d.Uvarint()
		if n == 0 || n > 1024 {
			return nil, fmt.Errorf("version: 非法 parent 数 %d", n)
		}
		v.Parents = make([]hash.ID, 0, n)
		for i := uint64(0); i < n; i++ {
			v.Parents = append(v.Parents, d.Hash())
		}
		v.Parent = v.Parents[0]
		v.Tree = d.Hash()
		v.Epoch = d.Uvarint()
		v.Agent = d.Str()
		v.TaskHash = d.Hash()
		v.TS = int64(d.U64())
		v.Msg = d.Str()
	default:
		return nil, fmt.Errorf("version: 期望 Version(0x%02x/0x%02x)，得到 0x%02x",
			codec.TypeVersion, codec.TypeVersionMerge, typ)
	}
	if err := d.Finish(); err != nil {
		return nil, err
	}
	return &v, nil
}
