package repo

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chenyiwen/p37/internal/op"
)

// TokenMismatchError 是 expected_* 乐观锁校验失败。
//
// 与 ErrParentMoved 的区别：这里是「调用方手里的令牌过期了」，父工作区在
// preview → land 之间变化；ErrParentMoved 是「读树之后、写盘之前」的最终校验。
// 两者都不落盘，调用方都应重读最新树后重试。
type TokenMismatchError struct {
	Field    string
	Expected string
	Current  string
}

func (e *TokenMismatchError) Error() string {
	return fmt.Sprintf("%s: %s 已变化（expected=%s current=%s）", ErrTargetMoved, e.Field, e.Expected, e.Current)
}

func (e *TokenMismatchError) Unwrap() error { return ErrTargetMoved }

// conflictPathSet 返回冲突涉及的路径集合。
func conflictPathSet(cs []op.Conflict) map[string]bool {
	out := make(map[string]bool, len(cs))
	for i := range cs {
		out[cs[i].Path] = true
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// buildPathStates 把三棵树 + 结果树的差异整理成路径级分类。
//
// 只输出「至少一侧改过」的路径；未变化路径不进入结果，避免大仓库把响应撑爆。
func buildPathStates(base, parent, child, result map[string]flatTreeEntry,
	conflicts map[string]bool, autoMerged []string) []PathState {
	auto := make(map[string]bool, len(autoMerged))
	for _, p := range autoMerged {
		auto[p] = true
	}
	childChanged := diffFlatPaths(base, child)
	parentChanged := diffFlatPaths(base, parent)
	childSet := make(map[string]bool, len(childChanged))
	for _, p := range childChanged {
		childSet[p] = true
	}
	parentSet := make(map[string]bool, len(parentChanged))
	for _, p := range parentChanged {
		parentSet[p] = true
	}
	set := map[string]bool{}
	for _, p := range childChanged {
		set[p] = true
	}
	for _, p := range parentChanged {
		set[p] = true
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]PathState, 0, len(paths))
	for _, p := range paths {
		st := PathState{
			Path:   p,
			Base:   entryID(base[p]),
			Parent: entryID(parent[p]),
			Child:  entryID(child[p]),
			Result: entryID(result[p]),
		}
		inChild, inParent := childSet[p], parentSet[p]
		switch {
		case conflicts[p]:
			st.State = "conflict"
		case inChild && inParent && auto[p]:
			st.State = "both_auto_merged"
		case inChild && inParent:
			st.State = "both_changed"
		case inChild:
			st.State = "child_only"
		default:
			st.State = "parent_only"
		}
		out = append(out, st)
	}
	return out
}

func entryID(e flatTreeEntry) string {
	if e.id.IsZero() {
		return ""
	}
	return e.id.String()
}

// outOfScopePaths 返回不在 allowed_paths / allowed_prefixes 里的路径。
func outOfScopePaths(paths, allowed, prefixes []string) []string {
	if len(allowed) == 0 && len(prefixes) == 0 {
		return nil
	}
	exact := make(map[string]bool, len(allowed))
	for _, p := range allowed {
		exact[normalizeAllowed(p)] = true
	}
	var out []string
	for _, p := range paths {
		if exact[p] {
			continue
		}
		ok := false
		for _, pre := range prefixes {
			pre = normalizeAllowed(pre)
			if pre == "" || p == pre || strings.HasPrefix(p, pre+"/") {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, p)
		}
	}
	return out
}

func normalizeAllowed(p string) string {
	return strings.TrimSuffix(strings.TrimSpace(p), "/")
}
