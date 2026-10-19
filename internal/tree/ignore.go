package tree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 本文件实现 §3.4 的忽略规则（self-written，不引入外部 ignore 库，以保证
// 跨平台一致）。
//
// 支持的语法（.gitignore 的子集）：
//
//	# 注释            空行忽略
//	!pattern          取反（重新纳入）
//	trailing/         仅匹配目录
//	*  ?  [..]        单段内的通配
//	**                跨段通配（可匹配任意多段，含 0 段）
//	无 '/' 的模式      匹配任意层级上的同名条目（按 basename）
//	含 '/' 的模式      相对仓库根锚定
//
// 只读取仓库根的 .p37ignore；**不读取子目录中的嵌套 ignore 文件**（§2.1）。

// Errors
var (
	// ErrIgnoreSyntax 表示 ignore 文件存在语法错误。
	ErrIgnoreSyntax = errors.New("tree: ignore 语法错误")
)

// Ignore 是一组已编译的忽略规则。
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	segs     []string // 模式按 '/' 切分后的段
	negate   bool     // '!' 前缀
	dirOnly  bool     // 尾随 '/'
	anywhere bool     // 无 '/'：匹配任意层级的 basename
}

// DefaultIgnorePatterns 是内置忽略集（§3.4）。用户不可删，只能追加。
var DefaultIgnorePatterns = []string{
	".git/",
	".p37/",
	"node_modules/",
	"__pycache__/",
	".venv/",
	"target/",
	"dist/",
	"build/",
	".idea/",
	".DS_Store",
}

// IgnoreFileName 是仓库根的忽略文件名（§3.4）。
const IgnoreFileName = ".p37ignore"

// LoadIgnoreFile 读取仓库根下的 .p37ignore，返回其中有效的行。
//
// 文件不存在时返回 (nil, nil)：忽略文件是可选的。语法错误留给 ParseIgnore
// 报出，以便调用方给出带行号的错误。
func LoadIgnoreFile(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, IgnoreFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw := strings.Split(string(b), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		lines = append(lines, strings.TrimRight(l, "\r"))
	}
	return lines, nil
}

// NewIgnore 编译规则集合。builtin 为真时在最前面加入内置集。
func NewIgnore(builtin bool, extra []string) (*Ignore, error) {
	var lines []string
	if builtin {
		lines = append(lines, DefaultIgnorePatterns...)
	}
	lines = append(lines, extra...)
	return ParseIgnore(lines)
}

// ParseIgnore 从行列表编译规则。
func ParseIgnore(lines []string) (*Ignore, error) {
	ig := &Ignore{}
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := parseRule(line)
		if err != nil {
			return nil, fmt.Errorf("%w（第 %d 行 %q）: %v", ErrIgnoreSyntax, i+1, raw, err)
		}
		ig.rules = append(ig.rules, r)
	}
	return ig, nil
}

func parseRule(line string) (ignoreRule, error) {
	var r ignoreRule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	line = strings.TrimPrefix(line, "/") // 前导 '/' 仅表示锚定，本实现一律锚定
	if line == "" {
		return r, errors.New("空模式")
	}
	r.segs = strings.Split(line, "/")
	for _, s := range r.segs {
		if s == "" {
			return r, errors.New("含空路径段")
		}
	}
	r.anywhere = len(r.segs) == 1
	return r, nil
}

// Match 报告仓库相对路径 path 是否应被忽略。isDir 表示该条目是否为目录。
//
// 规则按顺序应用，**最后一条匹配的规则决定结果**（与 gitignore 一致）。
func (ig *Ignore) Match(path string, isDir bool) bool {
	if ig == nil {
		return false
	}
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return false
	}
	segs := strings.Split(path, "/")

	ignored := false
	for i := range ig.rules {
		r := &ig.rules[i]
		if r.matches(segs, isDir) {
			ignored = !r.negate
		}
	}
	return ignored
}

func (r *ignoreRule) matches(segs []string, isDir bool) bool {
	// dir-only 规则也匹配其下的一切：'node_modules/' 必须同时忽略
	// 'node_modules/x.js'，否则直接调用 Match 的语义会自相矛盾。
	if r.dirOnly && !isDir {
		for i := 1; i < len(segs); i++ {
			if r.matchOnce(segs[:i]) {
				return true
			}
		}
		return false
	}
	return r.matchOnce(segs)
}

func (r *ignoreRule) matchOnce(segs []string) bool {
	if r.anywhere {
		// 单段模式：任一层级的 basename 命中即可。
		for _, s := range segs {
			if globMatch(r.segs[0], s) {
				return true
			}
		}
		return false
	}
	return matchSegs(r.segs, segs)
}

// matchSegs 让模式段序列与路径段序列匹配，支持 '**'。
func matchSegs(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		// '**' 匹配 0 段或多段。
		for i := 0; i <= len(path); i++ {
			if matchSegs(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	if !globMatch(pat[0], path[0]) {
		return false
	}
	return matchSegs(pat[1:], path[1:])
}

// globMatch 在单段内做 * / ? / [...] 匹配。
func globMatch(pat, s string) bool {
	// 迭代式回溯，避免递归深度问题。
	pi, si := 0, 0
	star, mark := -1, 0

	for si < len(s) {
		switch {
		case pi < len(pat) && pat[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(pat) && pat[pi] == '?':
			pi++
			si++
		case pi < len(pat) && pat[pi] == '[':
			ok, next := matchClass(pat, pi, s[si])
			if !ok {
				if star >= 0 {
					pi = star + 1
					mark++
					si = mark
					continue
				}
				return false
			}
			pi = next
			si++
		case pi < len(pat) && pat[pi] == s[si]:
			pi++
			si++
		default:
			if star >= 0 {
				pi = star + 1
				mark++
				si = mark
				continue
			}
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}

// matchClass 处理 [...] 字符类，返回 (是否匹配, 类结束后的下标)。
func matchClass(pat string, pi int, c byte) (bool, int) {
	i := pi + 1
	negate := false
	if i < len(pat) && (pat[i] == '!' || pat[i] == '^') {
		negate = true
		i++
	}
	matched := false
	first := true
	for i < len(pat) && (pat[i] != ']' || first) {
		first = false
		if i+2 < len(pat) && pat[i+1] == '-' && pat[i+2] != ']' {
			if pat[i] <= c && c <= pat[i+2] {
				matched = true
			}
			i += 3
			continue
		}
		if pat[i] == c {
			matched = true
		}
		i++
	}
	if i >= len(pat) {
		return false, pi + 1 // 未闭合的 '[' 视为普通字符
	}
	return matched != negate, i + 1
}
