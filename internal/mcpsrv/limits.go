package mcpsrv

import "net/http"

// LimitBodyBytes 限制请求体大小。
//
// HTTP 模式的 MCP 调用只传小段 JSON；不设上限时，任何能连上端口的进程都能
// 用一个超大 body 把 daemon 的内存打满。http.MaxBytesReader 会在超限时让
// handler 读到 *http.MaxBytesError，并尽量把响应置为 413
// （实现 http.ResponseWriter 的 requestTooLarge 时）。
//
// max <= 0 表示不限制（调用方显式选择关闭）。
func LimitBodyBytes(next http.Handler, max int64) http.Handler {
	if max <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}
