package op

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
)

// op 携带的是**完整仓库路径**（如 "src/auth/token.go"），而树是嵌套结构。
// 因此重放引擎在扁平的 path→Entry 命名空间上工作，最后重建嵌套树。
//
// 重建技巧：按目录分组后自底向上写 Tree 对象，父目录用子目录的 Tree id 引用
// 之 —— 这与 §5.5 的子树复用同构，未变更的目录自然写出同一个 Tree id。

// flatten 把嵌套树展开为 path→Entry（不含目录条目）。
func flatten(cs ContentStore, t *tree.Tree) (map[string]tree.Entry, error) {
	out := make(map[string]tree.Entry)
	var walk func(dir string, tr *tree.Tree) error
	walk = func(dir string, tr *tree.Tree) error {
		for _, e := range tr.Entries {
			p := e.Name
			if dir != "" {
				p = dir + "/" + e.Name
			}
			if e.Kind == codec.EntryTree {
				sub, err := cs.GetTree(e.ID)
				if err != nil {
					return fmt.Errorf("op: 展开目录 %s: %w", p, err)
				}
				if err := walk(p, sub); err != nil {
					return err
				}
				continue
			}
			out[p] = tree.Entry{Name: e.Name, Mode: e.Mode, Kind: e.Kind, ID: e.ID}
		}
		return nil
	}
	if err := walk("", t); err != nil {
		return nil, err
	}
	return out, nil
}

// unflatten 由 path→Entry 重建嵌套树，并写入 CAS。
func unflatten(cs ContentStore, entries map[string]tree.Entry) (*tree.Tree, error) {
	type dirLevel struct {
		entries []tree.Entry
	}
	levels := map[string]*dirLevel{}

	ensure := func(d string) *dirLevel {
		if l, ok := levels[d]; ok {
			return l
		}
		l := &dirLevel{}
		levels[d] = l
		return l
	}
	ensure("")

	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		e := entries[p]
		dir, name := splitDir(p)
		l := ensure(dir)
		l.entries = append(l.entries, tree.Entry{
			Name: name, Mode: e.Mode, Kind: e.Kind, ID: e.ID,
		})
	}

	// 必须先补齐**所有祖先目录**再排定处理顺序。
	//
	// 曾经的 bug：先收集 levels 的键、再在循环里创建父目录 —— 对于
	// "pkg/mod5/f5.go" 这类两级嵌套，"pkg" 不在初始键集合里，于是永远不会
	// 被构建，最终根树为空（land 会静默清空整棵仓库树）。
	for _, p := range paths {
		dir, _ := splitDir(p)
		for dir != "" {
			ensure(dir)
			dir, _ = splitDir(dir)
		}
	}

	// 目录按深度降序处理，保证子目录先完成。
	dirs := make([]string, 0, len(levels))
	for d := range levels {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if di != dj {
			return di > dj
		}
		return dirs[i] > dirs[j]
	})

	ids := map[string]hash.ID{}
	for _, d := range dirs {
		t := &tree.Tree{Entries: levels[d].entries}
		t.Sort()
		id, err := cs.PutTree(t)
		if err != nil {
			return nil, err
		}
		ids[d] = id

		if d == "" {
			continue
		}
		parent, name := splitDir(d)
		pl := ensure(parent)
		pl.entries = append(pl.entries, tree.Entry{
			Name: name, Mode: codec.ModeNormal, Kind: codec.EntryTree, ID: id,
		})
	}

	root := &tree.Tree{Entries: levels[""].entries}
	root.Sort()
	return root, nil
}

// splitDir 把 "a/b/c" 拆成 ("a/b", "c")；顶层条目返回 ("", name)。
func splitDir(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}
