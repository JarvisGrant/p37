package op

import (
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
)

// treeReader 只依赖展开嵌套树所需的最小能力。
type treeReader interface {
	GetTree(id hash.ID) (*tree.Tree, error)
}

// RenameFollows 计算 base 树到 target 树之间的**文件级重命名**映射：
// base 里的旧路径 -> target 里的新路径。
//
// 判据刻意保守：只匹配「内容 id 相同、base 中唯一、且在 target 中是新增路径」
// 的条目；任何歧义（同一内容对应多个候选）一律不映射，交由冲突流程处理，
// 绝不猜测路径。
//
// 用途：工作区 W 基于旧基准 B 采集改动后，主干前进到了 T（其中某文件被
// 重命名）。落地/对齐时把 W 的 op 从 B 重放到 T 上，需要把旧路径改道到新路径，
// 才能像 git 一样「跟随重命名」（§6.3 rename_vs_edit）。
func RenameFollows(store treeReader, base, target *tree.Tree) (map[string]string, error) {
	be := map[string]tree.Entry{}
	te := map[string]tree.Entry{}
	if err := flattenEntries(store, base, "", be); err != nil {
		return nil, err
	}
	if err := flattenEntries(store, target, "", te); err != nil {
		return nil, err
	}

	// target 里新增的文件路径，按内容 id 归组。
	added := map[hash.ID][]string{}
	for p, e := range te {
		if e.Kind != codec.EntryFile {
			continue
		}
		if _, ok := be[p]; ok {
			continue
		}
		added[e.ID] = append(added[e.ID], p)
	}

	out := map[string]string{}
	for p, e := range be {
		if e.Kind != codec.EntryFile {
			continue
		}
		if _, ok := te[p]; ok {
			continue // 旧路径仍在，不是重命名
		}
		cands := added[e.ID]
		if len(cands) != 1 {
			continue // 无候选或多候选 -> 不猜
		}
		out[p] = cands[0]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// flattenEntries 递归收集树的扁平视图（路径 -> 条目，含目录）。
func flattenEntries(store treeReader, t *tree.Tree, prefix string, out map[string]tree.Entry) error {
	for _, e := range t.Entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		out[p] = e
		if e.Kind == codec.EntryTree {
			sub, err := store.GetTree(e.ID)
			if err != nil {
				return err
			}
			if err := flattenEntries(store, sub, p, out); err != nil {
				return err
			}
		}
	}
	return nil
}
