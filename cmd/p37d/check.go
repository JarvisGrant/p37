package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/chenyiwen/p37/internal/health"
)

// runCheck 执行部署健康检查，返回 0/1（用法错误由 main 返回 2）。
func runCheck(root string, jsonOut, strict bool, out, errw io.Writer) int {
	rep := health.Check(root, health.Options{Strict: strict})

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(errw, "p37d: 输出 JSON 失败: %v\n", err)
			return 1
		}
		if !rep.OK {
			return 1
		}
		return 0
	}

	for _, c := range rep.Checks {
		mark := "OK  "
		switch c.Status {
		case health.StatusWarn:
			mark = "WARN"
		case health.StatusFail:
			mark = "FAIL"
		case health.StatusSkip:
			mark = "SKIP"
		}
		fmt.Fprintf(out, "[%s] %-28s %s\n", mark, c.Name, c.Detail)
	}
	if rep.OK {
		fmt.Fprintln(out, "\n健康检查通过。")
		return 0
	}
	fmt.Fprintln(out, "\n存在未通过项，见上。")
	return 1
}
