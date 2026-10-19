package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/chenyiwen/p37/internal/repo"
)

// cmdBlame 实现 `p37 blame <root> <path> [--range a-b] [--version <id>] [--hunk]`。
//
// 这是 MCP `blame` 的运维等价物；产品路径仍然是 MCP。
func cmdBlame(root string, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errw, "blame 需要 <path>")
		return 2
	}
	path := args[0]
	opt := repo.BlameOptions{Granularity: "line"}
	versionArg := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--hunk":
			opt.Granularity = "hunk"
		case "--version":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--version 需要参数")
				return 2
			}
			i++
			versionArg = args[i]
		case "--range":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--range 需要 a-b")
				return 2
			}
			i++
			a, b, err := parseRange(args[i])
			if err != nil {
				fmt.Fprintf(errw, "--range 非法: %v\n", err)
				return 2
			}
			opt.From, opt.To = a, b
		default:
			fmt.Fprintf(errw, "未知参数 %q\n", args[i])
			return 2
		}
	}

	r, err := repo.Open(root, repo.Options{})
	if err != nil {
		fmt.Fprintf(errw, "打开仓库失败: %v\n", err)
		return 1
	}
	defer func() { _ = r.Close() }()

	if versionArg != "" {
		v, err := resolveVersionArg(r, versionArg)
		if err != nil {
			fmt.Fprintf(errw, "版本 id 非法: %v\n", err)
			return 2
		}
		opt.VersionID = v
	}

	recs, err := r.Blame(path, opt)
	if err != nil {
		if errors.Is(err, repo.ErrPathNotFound) {
			fmt.Fprintf(errw, "路径不存在: %s\n", path)
			return 1
		}
		fmt.Fprintf(errw, "blame 失败: %v\n", err)
		return 1
	}
	for _, rec := range recs {
		who := rec.Agent
		if who == "" {
			who = "-"
		}
		task := rec.Task
		if task == "" {
			task = "-"
		}
		tag := ""
		if rec.Originated {
			tag = " [诞生]"
		}
		if !rec.Available {
			tag += "[无血缘记录]"
		}
		fmt.Fprintf(out, "%-9s %-16s %-12s %s%s\n",
			rec.Range, rec.VersionID[:16], who, task, tag)
	}
	return 0
}

func parseRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("期望 a-b，得到 %q", s)
	}
	from, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil {
		return 0, 0, err
	}
	to, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil {
		return 0, 0, err
	}
	return from, to, nil
}
