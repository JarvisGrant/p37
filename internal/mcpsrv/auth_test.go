package mcpsrv

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBearerAuth 覆盖 A1：HTTP 模式必须真的校验 token。
func TestBearerAuth(t *testing.T) {
	const token = "s3cr3t-token-value"

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := BearerAuth(next, token)

	cases := []struct {
		name   string
		header string
		want   int
		hits   bool
	}{
		{"无 Authorization", "", http.StatusUnauthorized, false},
		{"空 Bearer", "Bearer ", http.StatusUnauthorized, false},
		{"错误 token", "Bearer wrong", http.StatusUnauthorized, false},
		{"前缀正确但更长", "Bearer " + token + "x", http.StatusUnauthorized, false},
		{"token 正确", "Bearer " + token, http.StatusOK, true},
		{"大小写不敏感前缀", "bearer " + token, http.StatusOK, true},
		{"多余空白", "Bearer   " + token + "  ", http.StatusOK, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Fatalf("状态码 = %d，期望 %d", rec.Code, c.want)
			}
			if reached != c.hits {
				t.Fatalf("下游是否被调用 = %v，期望 %v", reached, c.hits)
			}
		})
	}
}

// TestBearerAuthEmptyTokenFailsClosed 断言未配置 token 时关闭而不是放行。
func TestBearerAuthEmptyTokenFailsClosed(t *testing.T) {
	reached := false
	h := BearerAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}), "")

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（fail closed）", rec.Code)
	}
	if reached {
		t.Fatal("未配置 token 时不应放行")
	}
}

// TestExtractBearer 覆盖解析边界。
func TestExtractBearer(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"Bearer":       "",
		"Bearer ":      "",
		"Bearer abc":   "abc",
		"bearer abc":   "abc",
		"BEARER abc":   "abc",
		"Basic abc":    "",
		"Bearer  a b ": "a b",
	}
	for in, want := range cases {
		if got := extractBearer(in); got != want {
			t.Errorf("extractBearer(%q) = %q，期望 %q", in, got, want)
		}
	}
}
