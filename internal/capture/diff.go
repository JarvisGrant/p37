package capture

import (
	"fmt"
	"sort"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/tree"
)

// Diff 由 (base, next) 两棵树推导 op 序列（§5.3、§6.5）。
//
// 产出的 op 是**确定性的**：同一对树总是给出同一序列。
// 目录本身不产生 op，目录的变化由其下的文件级 op 体现。
func Diff(wsID string, base, next *tree.Tree, ow ObjectWriter, opt Options) ([]op.Op, error) {
	if opt.ProbeBytes <= 0 {
		opt = DefaultOptions()
	}

	var changes []change
	if err := collect(ow, base, next, "", &changes); err != nil {
		return nil, err
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].path < changes[j].path })

	var ops []op.Op
	var deletes, creates []int
	seq := int64(0)
	nextSeq := func() int64 { seq++; return seq }

	for _, ch := range changes {
		switch {
		case ch.old == nil:
			ops = append(ops, op.Op{
				ID: newOpID(wsID, ch.path, seq, op.KindCreate), WsID: wsID,
				Seq: nextSeq(), Kind: op.KindCreate, Path: ch.path, NewFile: ch.new.ID,
				EntryKind: ch.new.Kind, Derived: true,
			})
			creates = append(creates, len(ops)-1)

		case ch.new == nil:
			ops = append(ops, op.Op{
				ID: newOpID(wsID, ch.path, seq, op.KindDelete), WsID: wsID,
				Seq: nextSeq(), Kind: op.KindDelete, Path: ch.path, BaseFile: ch.old.ID,
				EntryKind: ch.old.Kind, Derived: true,
			})
			deletes = append(deletes, len(ops)-1)

		case ch.old.Kind == codec.EntrySymlink && ch.new.Kind == codec.EntrySymlink:
			// 符号链接**目标变了**：用 blob_put 表达为一次修改（§6.5）。
			//
			// 早先这里走「删 + 建」，语义没错但会把一次改链接记成两个 op，
			// 而且插入侧的类型信息要靠 EntryKind 单独承载。blob_put 的重放规则
			// 恰好就是「目标 id == blob_before -> 直接换成 blob_after」，
			// 与改一个链接目标完全对应。
			if ch.old.ID == ch.new.ID {
				continue
			}
			ops = append(ops, op.Op{
				ID: newOpID(wsID, ch.path, seq, op.KindBlobPut), WsID: wsID,
				Seq: nextSeq(), Kind: op.KindBlobPut, Path: ch.path,
				BaseFile: ch.old.ID, NewFile: ch.new.ID,
				EntryKind: codec.EntrySymlink, Derived: true,
			})

		case ch.old.Kind != codec.EntryFile || ch.new.Kind != codec.EntryFile:
			// 文件↔符号链接之类的**类型**变化：按删 + 建处理。
			ops = append(ops, op.Op{
				ID: newOpID(wsID, ch.path, seq, op.KindDelete), WsID: wsID,
				Seq: nextSeq(), Kind: op.KindDelete, Path: ch.path, BaseFile: ch.old.ID,
				Derived: true,
			})
			deletes = append(deletes, len(ops)-1)
			ops = append(ops, op.Op{
				ID: newOpID(wsID, ch.path, seq, op.KindCreate), WsID: wsID,
				Seq: nextSeq(), Kind: op.KindCreate, Path: ch.path, NewFile: ch.new.ID,
				EntryKind: ch.new.Kind, Derived: true,
			})
			creates = append(creates, len(ops)-1)

		default:
			if ch.old.ID == ch.new.ID && ch.old.Mode == ch.new.Mode {
				continue
			}
			o, err := modifyOp(wsID, ch, ow, opt, nextSeq)
			if err != nil {
				return nil, err
			}
			ops = append(ops, o)
		}
	}

	ops = mergeRenames(ops, deletes, creates)

	// 重排：先按 path，再按**语义顺序**（同一路径必须先删后建），最后重编号
	// seq 保证严格递增且确定。
	//
	// 注意不能按 kind 的字典序排："create" < "delete"，会导致同一路径先建后删，
	// 于是 applyCreate 撞上仍存在的旧条目而误报重叠冲突。
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		oi, oj := kindOrder(ops[i].Kind), kindOrder(ops[j].Kind)
		if oi != oj {
			return oi < oj
		}
		return ops[i].DestPath < ops[j].DestPath
	})
	for i := range ops {
		ops[i].Seq = int64(i + 1)
		ops[i].ID = newOpID(wsID, ops[i].Path, ops[i].Seq, ops[i].Kind)
	}
	return ops, nil
}

// kindOrder 给出同一路径上各 kind 的必然执行顺序。
//
//	delete 必须在 create 之前；rename 视作「先移走」，排在 modify 之前。
func kindOrder(k op.Kind) int {
	switch k {
	case op.KindDelete:
		return 0
	case op.KindRename:
		return 1
	case op.KindCreate:
		return 2
	case op.KindModify:
		return 3
	case op.KindAttr:
		return 4
	case op.KindBlobPut:
		return 5
	default:
		return 6
	}
}

// change 描述一个文件级条目在两棵树之间的变化。
type change struct {
	path string
	old  *tree.Entry
	new  *tree.Entry
}

// collect 递归合并遍历两棵树，只收集**文件级**变化。
func collect(ow ObjectWriter, base, next *tree.Tree, prefix string, out *[]change) error {
	i, j := 0, 0
	for i < len(base.Entries) || j < len(next.Entries) {
		switch {
		case i >= len(base.Entries):
			e := next.Entries[j]
			if err := expandNew(ow, e, join(prefix, e.Name), out); err != nil {
				return err
			}
			j++

		case j >= len(next.Entries):
			e := base.Entries[i]
			if err := expandOld(ow, e, join(prefix, e.Name), out); err != nil {
				return err
			}
			i++

		default:
			a, b := base.Entries[i], next.Entries[j]
			switch {
			case a.Name < b.Name:
				if err := expandOld(ow, a, join(prefix, a.Name), out); err != nil {
					return err
				}
				i++
			case a.Name > b.Name:
				if err := expandNew(ow, b, join(prefix, b.Name), out); err != nil {
					return err
				}
				j++
			default:
				p := join(prefix, a.Name)
				i++
				j++
				switch {
				case a.Kind == codec.EntryTree && b.Kind == codec.EntryTree:
					if a.ID == b.ID {
						continue // 整棵子树未变：无需下钻（§5.5 的复用在此变现）
					}
					as, err := loadTree(ow, a.ID)
					if err != nil {
						return err
					}
					bs, err := loadTree(ow, b.ID)
					if err != nil {
						return err
					}
					if err := collect(ow, as, bs, p, out); err != nil {
						return err
					}

				case a.Kind == codec.EntryTree:
					as, err := loadTree(ow, a.ID)
					if err != nil {
						return err
					}
					if err := collect(ow, as, &tree.Tree{}, p, out); err != nil {
						return err
					}
					if err := expandRaw(b, p, out, false); err != nil {
						return err
					}

				case b.Kind == codec.EntryTree:
					bs, err := loadTree(ow, b.ID)
					if err != nil {
						return err
					}
					if err := collect(ow, &tree.Tree{}, bs, p, out); err != nil {
						return err
					}
					if err := expandRaw(a, p, out, true); err != nil {
						return err
					}

				default:
					ca, cb := a, b
					if ca.ID == cb.ID && ca.Mode == cb.Mode && ca.Kind == cb.Kind {
						continue
					}
					*out = append(*out, change{path: p, old: &ca, new: &cb})
				}
			}
		}
	}
	return nil
}

// expandOld 记录「基准有而目标无」的条目。
func expandOld(ow ObjectWriter, e tree.Entry, path string, out *[]change) error {
	if e.Kind != codec.EntryTree {
		ce := e
		*out = append(*out, change{path: path, old: &ce})
		return nil
	}
	sub, err := loadTree(ow, e.ID)
	if err != nil {
		return err
	}
	return collect(ow, sub, &tree.Tree{}, path, out)
}

// expandNew 记录「目标有而基准无」的条目。
func expandNew(ow ObjectWriter, e tree.Entry, path string, out *[]change) error {
	if e.Kind != codec.EntryTree {
		ce := e
		*out = append(*out, change{path: path, new: &ce})
		return nil
	}
	sub, err := loadTree(ow, e.ID)
	if err != nil {
		return err
	}
	return collect(ow, &tree.Tree{}, sub, path, out)
}

// expandRaw 把一个非目录条目按「删」或「建」记录。
func expandRaw(e tree.Entry, path string, out *[]change, asOld bool) error {
	if e.Kind == codec.EntryTree {
		return fmt.Errorf("capture: expandRaw 收到目录条目 %s", path)
	}
	ce := e
	if asOld {
		*out = append(*out, change{path: path, old: &ce})
	} else {
		*out = append(*out, change{path: path, new: &ce})
	}
	return nil
}

func loadTree(ow ObjectWriter, id hash.ID) (*tree.Tree, error) {
	return ow.GetTree(id)
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// modifyOp 由一对同路径文件条目推导出 modify / attr / blob_put。
func modifyOp(wsID string, ch change, ow ObjectWriter, opt Options, nextSeq func() int64) (op.Op, error) {
	newData, err := ow.ReadFileData(ch.new.ID)
	if err != nil {
		return op.Op{}, err
	}
	oldData, err := ow.ReadFileData(ch.old.ID)
	if err != nil {
		return op.Op{}, err
	}

	// 纯换行差异 → attr（§3.5、附录 L.2 #8）。
	if tree.IsEOLOnlyChange(oldData, newData) {
		return op.Op{
			ID: newOpID(wsID, ch.path, nextSeq(), op.KindAttr), WsID: wsID,
			Kind: op.KindAttr, Path: ch.path,
			BaseFile: ch.old.ID, NewFile: ch.new.ID,
			Attr:    &op.Attr{EOL: tree.DetectEOL(newData)},
			Derived: true,
		}, nil
	}

	// 二进制或大文件 → blob_put（§3.5、附录 L.2 #9/#10）。
	if tree.IsBinary(oldData, opt.ProbeBytes) || tree.IsBinary(newData, opt.ProbeBytes) ||
		int64(len(oldData)) > opt.MaxTextSize || int64(len(newData)) > opt.MaxTextSize {
		return op.Op{
			ID: newOpID(wsID, ch.path, nextSeq(), op.KindBlobPut), WsID: wsID,
			Kind: op.KindBlobPut, Path: ch.path,
			BaseFile: ch.old.ID, NewFile: ch.new.ID, Derived: true,
		}, nil
	}

	hs, moves := tree.HunksWithMoves(tree.SplitLines(oldData), tree.SplitLines(newData),
		opt.ContextLines, opt.MaxChain, opt.MoveSimilarity)
	if len(moves) > 0 && opt.OnMoves != nil {
		opt.OnMoves(ch.path, moves)
	}
	if len(hs) > 0 {
		out := make([]op.Hunk, len(hs))
		for i, h := range hs {
			out[i] = op.FromTreeHunk(h)
		}
		return op.Op{
			ID: newOpID(wsID, ch.path, nextSeq(), op.KindModify), WsID: wsID,
			Kind: op.KindModify, Path: ch.path,
			BaseFile: ch.old.ID, Hunks: out, Derived: true,
		}, nil
	}

	// 兜底：无法产出行级 hunk 时按整文件替换。
	return op.Op{
		ID: newOpID(wsID, ch.path, nextSeq(), op.KindBlobPut), WsID: wsID,
		Kind: op.KindBlobPut, Path: ch.path,
		BaseFile: ch.old.ID, NewFile: ch.new.ID, Derived: true,
	}, nil
}

// mergeRenames 把「内容相同的一删一建」合并为 rename op（§5.3 第 4 趟）。
func mergeRenames(ops []op.Op, deletes, creates []int) []op.Op {
	dropped := make(map[int]bool, len(creates))
	replaced := make(map[int]op.Op, len(deletes))

	byID := make(map[string][]int, len(deletes))
	for _, di := range deletes {
		k := ops[di].BaseFile.String()
		byID[k] = append(byID[k], di)
	}

	for _, ci := range creates {
		k := ops[ci].NewFile.String()
		cand := byID[k]
		if len(cand) == 0 {
			continue
		}
		di := cand[0]
		byID[k] = cand[1:]

		o := ops[di]
		o.Kind = op.KindRename
		o.DestPath = ops[ci].Path
		o.NewFile = ops[ci].NewFile
		if ops[ci].EntryKind != 0 {
			o.EntryKind = ops[ci].EntryKind
		}
		replaced[di] = o
		dropped[ci] = true
	}

	out := make([]op.Op, 0, len(ops))
	for i := range ops {
		if dropped[i] {
			continue
		}
		if o, ok := replaced[i]; ok {
			out = append(out, o)
			continue
		}
		out = append(out, ops[i])
	}
	return out
}

// newOpID 生成一个**确定性**的 op id。
//
// ops 表被定义为可重建缓存（§5.4），因此 id 不稳定也无妨；这里让 id 只依赖
// (ws, path, seq, kind)，便于测试与差异对比。
func newOpID(wsID, path string, seq int64, kind op.Kind) string {
	h := hash.SumParts([]byte("p37-opid-v1"), []byte(wsID), []byte{0},
		[]byte(path), []byte{0}, []byte(fmt.Sprintf("%d", seq)), []byte{0}, []byte(kind))
	return fmt.Sprintf("op_%s_%d", h.String()[:16], seq)
}
