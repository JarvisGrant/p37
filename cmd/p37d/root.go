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

	// 旧单仓库原地注册为 default（不移动目录）。
	if _, err := h.RegisterLegacy("/srv/repo"); err != nil {
		fmt.Fprintf(os.Stderr, "p37d: 注册旧仓库 /srv/repo 失败: %v\n", err)
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
		ID    string `json:"id"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		Lines int    `json:"lines"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	items := make([]item, 0, len(entries))
	fail := false
	for _, e := range entries {
		it := item{ID: e.ID, Name: e.Name, Path: e.Path, OK: true}
		if _, err := os.Stat(filepath.Join(e.Path, ".p37", "p37.db")); err != nil {
			it.OK = false
			it.Error = err.Error()
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
			fmt.Fprintf(out, "%s %s (%s) lines=%d %s\n", status, it.Name, it.ID, it.Lines, it.Error)
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
