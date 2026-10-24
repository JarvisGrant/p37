package mcpsrv_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocsCoverRegisteredTools 防止 MCP 工具面与文档漂移。
//
// 新增工具时必须同步更新 docs/mcp-contract.md 与 README.md 的工具表；
// 否则这个测试会失败。
func TestDocsCoverRegisteredTools(t *testing.T) {
	h := newHarness(t)
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	docs := map[string]string{
		"docs/mcp-contract.md": "../../docs/mcp-contract.md",
		"README.md":            "../../README.md",
	}
	bodies := map[string]string{}
	for name, path := range docs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		bodies[name] = string(b)
	}

	for _, tool := range res.Tools {
		needle := "`" + tool.Name + "`"
		for name, body := range bodies {
			if !strings.Contains(body, needle) {
				t.Errorf("%s 未覆盖工具 %s（期望出现 %s）", name, tool.Name, needle)
			}
		}
	}

	count := fmt.Sprintf("当前注册 %d 个工具", len(res.Tools))
	for name, body := range bodies {
		if !strings.Contains(body, count) {
			t.Errorf("%s 的工具数描述未更新，期望包含 %q", name, count)
		}
	}
}

// TestContractErrorCodesMatchCode 断言文档错误码表与 mcpsrv 源码一致。
func TestContractErrorCodesMatchCode(t *testing.T) {
	doc, err := os.ReadFile("../../docs/mcp-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	start := strings.Index(text, "## 错误码")
	if start < 0 {
		t.Fatal("docs/mcp-contract.md 缺少错误码章节")
	}
	section := text[start:]
	fence := strings.Index(section, "```text")
	if fence < 0 {
		t.Fatal("错误码章节缺少 text 代码块")
	}
	section = section[fence+len("```text"):]
	end := strings.Index(section, "```")
	if end < 0 {
		t.Fatal("错误码代码块未闭合")
	}
	docCodes := map[string]bool{}
	for _, c := range strings.Fields(section[:end]) {
		docCodes[c] = true
	}

	codeRe := regexp.MustCompile(`coded\("([a-z_]+)"|return "([a-z_]+)", err\.Error\(\)`)
	srcCodes := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range codeRe.FindAllStringSubmatch(string(b), -1) {
			if m[1] != "" {
				srcCodes[m[1]] = true
			}
			if m[2] != "" {
				srcCodes[m[2]] = true
			}
		}
	}

	for c := range srcCodes {
		if !docCodes[c] {
			t.Errorf("错误码 %s 在代码中存在，但 docs/mcp-contract.md 未列出", c)
		}
	}
	for c := range docCodes {
		if !srcCodes[c] {
			t.Errorf("docs/mcp-contract.md 列出了代码中不存在的错误码 %s", c)
		}
	}
}

func TestContractUsesSingularDetail(t *testing.T) {
	b, err := os.ReadFile("../../docs/mcp-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	if !strings.Contains(doc, "`detail`（单数）") {
		t.Error("docs/mcp-contract.md 应明确错误字段名为 detail（单数）")
	}
	if strings.Contains(doc, `"details"`) {
		t.Error("docs/mcp-contract.md 仍出现 JSON 字段 details；实际字段是 detail")
	}
}

// TestDocsRelativeLinksExist 检查公开文档里的相对链接是否都指向真实文件。
func TestDocsRelativeLinksExist(t *testing.T) {
	files := []string{
		"../../README.md", "../../PROGRESS.md", "../../VERIFY.md",
		"../../CONTRIBUTING.md", "../../packaging/p36/README.md",
	}
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, docs...)

	linkRe := regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range linkRe.FindAllStringSubmatch(string(b), -1) {
			link := m[1]
			if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") ||
				strings.HasPrefix(link, "#") {
				continue
			}
			target := strings.SplitN(link, "#", 2)[0]
			if target == "" {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s 的链接 %q 不存在（解析为 %s）", path, link, resolved)
			}
		}
	}
}
