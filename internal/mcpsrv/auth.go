package mcpsrv

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerAuth 包一层 HTTP handler，要求 `Authorization: Bearer <token>`。
//
// 规格 §11.2：HTTP 模式**必须**带 token（TCP 端口可被本机任意进程访问，
// 而 MCP 工具具备写仓库、推进 head 的能力）。unix socket 依赖文件权限，
// 不走这里。
//
// token 为空即拒绝启动意义上的「不可用」——调用方应在此之前就拒绝配置。
func BearerAuth(next http.Handler, token string) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.Error(w, "p37d: HTTP 未配置 auth_token，拒绝服务", http.StatusServiceUnavailable)
			return
		}
		got := extractBearer(r.Header.Get("Authorization"))
		// 定长比较，避免时序侧信道。
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="p37"`)
			http.Error(w, "p37d: 未授权", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractBearer 从 Authorization 头中取出 bearer 值。
func extractBearer(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
