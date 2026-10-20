package mcpsrv

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Metrics 返回 Prometheus 文本格式的仓库指标（附录 B 的 daemon.metrics_http）。
//
// 只暴露**只读**的规模信息，不暴露路径、任务描述等敏感内容。格式遵循
// Prometheus 的文本暴露协议（text/plain; version=0.0.4）。
func (s *Server) Metrics() (string, error) {
	if s.R == nil {
		if s.Hub == nil {
			return "# p37 multi-repo: no default repo\n", nil
		}
		es, err := s.Hub.List()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# HELP p37_repos_total 仓库总数。\n# TYPE p37_repos_total gauge\np37_repos_total %d\n", len(es))
		return b.String(), nil
	}
	st, err := s.R.Status()
	if err != nil {
		return "", err
	}
	wss, err := s.R.WS.List()
	if err != nil {
		return "", err
	}
	byState := map[string]int{}
	for _, w := range wss {
		byState[w.State]++
	}
	states := make([]string, 0, len(byState))
	for k := range byState {
		states = append(states, k)
	}
	sort.Strings(states)

	var b strings.Builder
	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	metric("p37_build_info", "构建信息，恒为 1。", "gauge")
	fmt.Fprintf(&b, "p37_build_info{version=%q,format_ver=\"%d\"} 1\n",
		Version, st.FormatVer)

	metric("p37_versions_total", "主干版本总数。", "gauge")
	fmt.Fprintf(&b, "p37_versions_total %d\n", st.VersionCount)

	metric("p37_epoch", "当前 epoch 序号。", "gauge")
	fmt.Fprintf(&b, "p37_epoch %d\n", st.Epoch)

	head := 0
	if st.HasHead {
		head = 1
	}
	metric("p37_mainline_head_present", "主干是否已初始化（1/0）。", "gauge")
	fmt.Fprintf(&b, "p37_mainline_head_present %d\n", head)

	metric("p37_last_land_timestamp_seconds", "最近一次落地的 Unix 时间（秒），无则 0。", "gauge")
	fmt.Fprintf(&b, "p37_last_land_timestamp_seconds %d\n", st.LastLandTS/1000)

	metric("p37_workspaces", "活跃工作区数量，按状态分。", "gauge")
	if len(states) == 0 {
		fmt.Fprintf(&b, "p37_workspaces{state=\"none\"} 0\n")
	}
	for _, stt := range states {
		fmt.Fprintf(&b, "p37_workspaces{state=%q} %d\n", stt, byState[stt])
	}
	metric("p37_workspaces_total", "活跃工作区总数。", "gauge")
	fmt.Fprintf(&b, "p37_workspaces_total %d\n", len(wss))
	return b.String(), nil
}

// MetricsHandler 返回 /metrics 的 HTTP handler。
func MetricsHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
			return
		}
		body, err := s.Metrics()
		if err != nil {
			http.Error(w, "p37d: 采集指标失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}
