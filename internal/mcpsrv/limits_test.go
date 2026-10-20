package mcpsrv

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLimitBodyBytes 断言超过上限的请求体读不完整，且会给出 MaxBytesError。
func TestLimitBodyBytes(t *testing.T) {
	const limit = 8

	var (
		readErr error
		readLen int
	)
	h := LimitBodyBytes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		readErr = err
		readLen = len(b)
		w.WriteHeader(http.StatusOK)
	}), limit)

	body := strings.Repeat("x", limit*4)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if readErr == nil {
		t.Fatal("超限请求体应返回错误")
	}
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Fatalf("错误类型 = %T，期望 *http.MaxBytesError", readErr)
	}
	if readLen > limit {
		t.Fatalf("实际读到 %d 字节，超过上限 %d", readLen, limit)
	}
}

// TestLimitBodyBytesAllowsNormalRequest 断言正常大小的请求不受影响。
func TestLimitBodyBytesAllowsNormalRequest(t *testing.T) {
	const limit = 32

	var got string
	h := LimitBodyBytes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		got = string(b)
	}), limit)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"ok":true}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != `{"ok":true}` {
		t.Fatalf("请求体 = %q，期望原样透传", got)
	}
}

// TestLimitBodyBytesDisabled 断言 max<=0 时是直通，不改变行为。
func TestLimitBodyBytesDisabled(t *testing.T) {
	reached := false
	h := LimitBodyBytes(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}), 0)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !reached {
		t.Fatal("关闭限额时下游应被调用")
	}
}
