package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mcpsrv"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// runRootDaemon 以单进程多仓库模式运行。
func runRootDaemon(root string, serveHTTP bool) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	h, err := hub.New(root, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	for _, w := range h.RecoverWarnings() {
		fmt.Fprintf(os.Stderr, "p37d: registry 重建警告: %s\n", w)
	}

	// 旧单仓库原地注册为 default（不移动目录）。
	if _, err := h.RegisterLegacy("/srv/repo"); err != nil {
		fmt.Fprintf(os.Stderr, "p37d: 注册旧仓库 /srv/repo 失败: %v\n", err)
	}
	// 启动时按实际挂载的 repo 做一次轻量 reconcile：新挂上的应用卷会被
	// 加入 registry；已注册但卷不在的只标记 degraded，不自动删除数据。
	if res, err := h.Scan(hub.ScanOptions{Reconcile: true}); err != nil {
		fmt.Fprintf(os.Stderr, "p37d: registry scan 失败: %v\n", err)
	} else {
		if len(res.Added)+len(res.Degraded)+len(res.Removed) > 0 {
			fmt.Fprintf(os.Stderr, "p37d: registry reconciled added=%d degraded=%d removed=%d\n",
				len(res.Added), len(res.Degraded), len(res.Removed))
		}
		for _, sk := range res.Skipped {
			fmt.Fprintf(os.Stderr, "p37d: registry 跳过 %s（%s）\n", sk.Path, sk.Reason)
		}
		for _, is := range res.Uninitialized {
			fmt.Fprintf(os.Stderr, "p37d: repo 未完整初始化 %s（reason=%s next_action=%s）\n",
				is.Path, is.Reason, is.NextAction)
		}
	}
	// 启动时打开现有仓库：恢复 workspace 挂载，并让维护命令能看到
	// per-repo flock。单个 repo 失败只标记 degraded，不影响其它 repo。
	if es, err := h.List(); err == nil {
		for i := range es {
			if !es[i].Available {
				continue
			}
			if _, err := h.Open(es[i].ID); err != nil {
				fmt.Fprintf(os.Stderr, "p37d: 打开 repo %s 失败: %v\n", es[i].Name, err)
			}
		}
	}

	srv := mcpsrv.NewWithResolver(nil, func(ref string) (*repo.Repo, error) {
		return h.Open(ref)
	})
	srv.Hub = h
	srv.SetSyncOptions(cfg.RemoteOptions(), cfg.Remote.Enabled)

	logger := log.New(os.Stderr, "p37d ", log.LstdFlags|log.LUTC)
	unixHandler := srv.Handler()
	httpHandler := http.Handler(unixHandler)

	if serveHTTP || cfg.Daemon.ListenHTTP {
		if cfg.Daemon.AuthToken == "" {
			return errors.New("HTTP 模式必须配置 daemon.auth_token")
		}
		addr := fmt.Sprintf("%s:%d", cfg.Daemon.HTTPAddr, cfg.Daemon.HTTPPort)
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("监听 %s 失败: %w", addr, err)
		}
		defer func() { _ = tcpLn.Close() }()
		if cfg.Daemon.MetricsHTTP {
			mux := http.NewServeMux()
			mux.Handle("/metrics", mcpsrv.MetricsHandler(srv))
			mux.Handle("/", httpHandler)
			httpHandler = mux
		}
		httpHandler = mcpsrv.BearerAuth(httpHandler, cfg.Daemon.AuthToken)
		httpSrv := &http.Server{
			Handler:           mcpsrv.LimitBodyBytes(httpHandler, httpMaxBodyBytes),
			ReadHeaderTimeout: httpReadHeaderTimeout,
			ReadTimeout:       httpReadTimeout,
			IdleTimeout:       httpIdleTimeout,
			MaxHeaderBytes:    httpMaxHeaderBytes,
		}
		go func() {
			if err := httpSrv.Serve(tcpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("HTTP 监听失败: %v", err)
			}
		}()
		logger.Printf("HTTP 监听 http://%s/mcp", addr)
	}

	sockPath := h.SocketPath()
	_ = os.Remove(sockPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", sockPath, err)
	}
	defer func() { _ = os.Remove(sockPath) }()
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return err
	}
	unixSrv := &http.Server{
		Handler:           mcpsrv.LimitBodyBytes(unixHandler, httpMaxBodyBytes),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
	go func() {
		if err := unixSrv.Serve(unixLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("unix 监听失败: %v", err)
		}
	}()
	logger.Printf("多仓库 daemon 已启动 root=%s socket=%s", root, sockPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Printf("收到 %s，开始优雅退出", sig)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = unixSrv.Shutdown(ctx)
	return nil
}

// runRootCheck 是多仓库模式的 --check。
func runRootCheck(root string, jsonOut, strict bool, out, errw io.Writer) int {
	cfg, err := config.Load(root)
	if err != nil {
		fmt.Fprintf(errw, "p37d: 读取配置失败: %v\n", err)
		return 1
	}
	_ = cfg
	h, err := hub.New(root, cfg)
	if err != nil {
		fmt.Fprintf(errw, "p37d: 打开 registry 失败: %v\n", err)
		return 1
	}
	defer func() { _ = h.Close() }()
	entries, err := h.List()
	if err != nil {
		fmt.Fprintf(errw, "p37d: 读取 repo 列表失败: %v\n", err)
		return 1
	}
	type item struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Path        string `json:"path"`
		Lines       int    `json:"lines"`
		Initialized bool   `json:"initialized"`
		NextAction  string `json:"next_action,omitempty"`
		OK          bool   `json:"ok"`
		Error       string `json:"error,omitempty"`
	}
	items := make([]item, 0, len(entries))
	fail := false
	for _, e := range entries {
		it := item{ID: e.ID, Name: e.Name, Path: e.Path, OK: true}
		in, _ := repo.CheckIntegrity(e.Path)
		if in != nil {
			it.Initialized = in.Initialized
			it.NextAction = in.NextAction
		}
		if _, err := os.Stat(filepath.Join(e.Path, ".p37", "p37.db")); err != nil {
			it.OK = false
			it.Error = err.Error()
		} else if in == nil || !in.Initialized {
			switch {
			case in != nil && in.FormatVer > 0 && in.FormatVer < meta.FormatVer:
				// v1 老仓库允许启动后由 Open 自动迁移；--check 本身保持只读。
				it.OK = true
				it.NextAction = "migrate_on_open"
			case !strict && in != nil && in.RepoDBOK:
				// 非 strict 是 daemon 启动前门禁：允许 daemon 起来，让
				// /ready 暴露 initialized=false，并允许 repo_init(repair)。
				it.OK = true
				it.NextAction = in.NextAction
				it.Error = "repo 未完整初始化（允许启动，等待 repair）"
				if in.Reason != "" {
					it.Error += ": " + in.Reason
				}
			default:
				it.OK = false
				it.Error = "repo 未完整初始化"
				if in != nil && in.Reason != "" {
					it.Error += ": " + in.Reason
				}
			}
		}
		if it.OK && strict {
			r, err := h.Open(e.ID)
			if err != nil {
				it.OK = false
				it.Error = err.Error()
			} else {
				if ls, err := r.Lines(); err == nil {
					it.Lines = len(ls)
				}
				for _, l := range mustLines(r) {
					if err := r.WalkHead(l.Head, nil); err != nil {
						it.OK = false
						it.Error = err.Error()
						break
					}
				}
			}
		}
		if !it.OK {
			fail = true
		}
		items = append(items, it)
	}
	if jsonOut {
		b, _ := json.MarshalIndent(map[string]any{"ok": !fail, "root": root, "repos": items}, "", "  ")
		fmt.Fprintln(out, string(b))
	} else {
		for _, it := range items {
			status := "[OK]  "
			if !it.OK {
				status = "[FAIL]"
			}
			fmt.Fprintf(out, "%s %s (%s) lines=%d initialized=%v next=%s %s\n",
				status, it.Name, it.ID, it.Lines, it.Initialized, it.NextAction, it.Error)
		}
	}
	if fail {
		return 1
	}
	return 0
}

func mustLines(r *repo.Repo) []meta.Line {
	ls, _ := r.Lines()
	return ls
}
