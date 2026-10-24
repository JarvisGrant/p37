package buildinfo

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDefaultVersionMatchesDocs 防止默认版本号与 Makefile / README 漂移。
//
// 发布版本号由 -X 注入；这里校验的是源码默认值，保证本地 go build
// 与 `make release` 的默认版本一致。
func TestDefaultVersionMatchesDocs(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^VERSION \?= (\S+)\s*$`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("Makefile 缺少 VERSION ?= ...")
	}
	if got := string(m[1]); got != Version {
		t.Errorf("Makefile VERSION=%s，buildinfo.Version=%s", got, Version)
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "make release VERSION="+Version) {
		t.Errorf("README 未出现 make release VERSION=%s", Version)
	}
}
