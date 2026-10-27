package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Meta 是 .p37/repo.json 的仓库身份锚。
type Meta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DefaultLine string `json:"default_line"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName 报告 repo 名是否合法（与 registry 的约束一致）。
func ValidName(name string) bool { return nameRE.MatchString(name) }

// SanitizeName 把目录名收敛成合法 repo 名（小写、非 [a-z0-9._-] 替换为 '-'）。
func SanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "repo"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	if !nameRE.MatchString(out) {
		out = "repo-" + out
		if len(out) > 64 {
			out = out[:64]
		}
	}
	return out
}

// ReadMeta 读取仓库根的 .p37/repo.json。
//
// 返回 os.ErrNotExist 表示文件不存在；文件存在但非法时返回格式错误。
func ReadMeta(root string) (*Meta, error) {
	b, err := os.ReadFile(filepath.Join(root, ".p37", "repo.json"))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.ID == "" || m.Name == "" {
		return nil, errors.New("repo.json 缺少 id/name")
	}
	if !ValidName(m.Name) {
		return nil, fmt.Errorf("repo.json name 非法: %q", m.Name)
	}
	if m.DefaultLine == "" {
		m.DefaultLine = "main"
	}
	return &m, nil
}

// WriteMeta 写入仓库根的 .p37/repo.json（0600）。
func WriteMeta(controlDir string, m Meta) error {
	if m.DefaultLine == "" {
		m.DefaultLine = "main"
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(controlDir, "repo.json"), b, 0o600)
}
