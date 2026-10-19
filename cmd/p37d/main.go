// Command p37d 是常驻 daemon：持有仓库排他锁，并通过 MCP 暴露能力
// （unix socket 默认开启；HTTP 需显式配置且强制 token，§11.2）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/mcpsrv"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/watch"
)

func main() {
	var (
		repoRoot  = flag.String("repo", "", "仓库容器目录（必填）")
		serveHTTP = flag.Bool("http", false, "额外监听 HTTP（需配置 auth_token）")
		version   = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()

	if *version {
		fmt.Printf("p37d %s\n", mcpsrv.Version)
		return
	}
	if *repoRoot == "" {
		fmt.Fprintln(os.Stderr, "用法: p37d --repo <root> [--http]")
		os.Exit(2)
	}
	if err := runDaemon(*repoRoot, *serveHTTP); err != nil {
		fmt.Fprintf(os.Stderr, "p37d: %v\n", err)
		os.Exit(1)
	}
}

func runDaemon(root string, serveHTTP bool) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}

	// 1. 取仓库排他锁（§11.1）。
	lockPath := filepath.Join(root, ".p37", "lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("打开锁文件失败: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := flockExclusive(lock); err != nil {
		return fmt.Errorf("已有 p37d 实例持有 %s（%w）", lockPath, err)
	}
	defer func() { _ = flockRelease(lock) }()

	// 2. 打开仓库（内部会做格式校验与 land 修复，§7.7）。
	repoOpt := cfg.RepoOptions()
	// 常驻 daemon 负责恢复活跃工作区的挂载（§7.7 步骤 3）。
	repoOpt.RemountWorkspaces = true
	r, err := repo.Open(root, repoOpt)
	if err != nil {
		return err
	}

	// 3. 启动自检（附录 G.4）——影响正确性的失败必须拒绝启动。
	if err := startupCheck(r, cfg); err != nil {
		_ = r.Close()
		return err
	}

	srv := mcpsrv.New(r)
	// 两个平面分开（§11.2）：
	//   unix socket —— 依赖文件权限（socket 本身 0600），不需要 token
	//   HTTP        —— 必须带 Bearer token，否则任何本机进程都能写仓库
	unixHandler := srv.Handler()
	httpHandler := http.Handler(unixHandler)

	logger := log.New(os.Stderr, "p37d ", log.LstdFlags|log.LUTC)

	// 3b. 启动采集触发源 a（fs 事件）与 c（周期扫描）（§5.2、§4.7）。
	w, err := watch.New(r, watch.Options{
		Debounce:       config.Duration(cfg.Workspace.Debounce, 500*time.Millisecond),
		ScanInterval:   config.Duration(cfg.Workspace.ScanInterval, 30*time.Second),
		FallbackToRoot: watchFallbackNeeded(),
		Logger:         logger,
		OnSync: func(wsID string, took time.Duration) {
			// 只有 info 及以上才逐条打印：debug 级别下采集很频繁。
			if cfg.LogLevel() == "info" || cfg.LogLevel() == "debug" {
				logger.Printf("自动采集 %s 完成（%s）", wsID, took.Round(time.Millisecond))
			}
		},
	})
	if err != nil {
		_ = r.Close()
		return err
	}
	w.Start()
	defer func() { _ = w.Close() }()
	logger.Printf("采集触发源已启动（debounce=%s，周期扫描=%s）",
		cfg.Workspace.Debounce, cfg.Workspace.ScanInterval)

	// 4. 监听。
	sockPath := cfg.Daemon.Socket
	if sockPath == "" {
		sockPath = filepath.Join(root, ".p37", "p37d.sock")
	}
	_ = os.Remove(sockPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		_ = r.Close()
		return err
	}
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = r.Close()
		return fmt.Errorf("监听 %s 失败: %w", sockPath, err)
	}
	defer func() { _ = os.Remove(sockPath) }()
	// 收紧 socket 权限：只有属主能连（A3）。
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = unixLn.Close()
		_ = r.Close()
		return fmt.Errorf("设置 %s 权限失败: %w", sockPath, err)
	}

	type listener struct {
		ln   net.Listener
		desc string
	}
	listeners := []listener{{unixLn, "unix://" + sockPath}}

	if serveHTTP || cfg.Daemon.ListenHTTP {
		if cfg.Daemon.AuthToken == "" {
			_ = unixLn.Close()
			_ = r.Close()
			return errors.New("HTTP 模式必须配置 daemon.auth_token（§11.2）")
		}
		addr := fmt.Sprintf("%s:%d", cfg.Daemon.HTTPAddr, cfg.Daemon.HTTPPort)
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			_ = unixLn.Close()
			_ = r.Close()
			return fmt.Errorf("监听 %s 失败: %w", addr, err)
		}
		httpHandler = mcpsrv.BearerAuth(httpHandler, cfg.Daemon.AuthToken)
		listeners = append(listeners, listener{tcpLn, "http://" + addr + "/mcp"})
		defer func() { _ = tcpLn.Close() }()
	}

	unixSrv := &http.Server{Handler: unixHandler, ReadHeaderTimeout: 10 * time.Second}
	httpSrv := &http.Server{Handler: httpHandler, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, len(listeners))
	for i, l := range listeners {
		ln := l.ln
		srvRef := unixSrv
		if i > 0 {
			srvRef = httpSrv
		}
		logger.Printf("监听 %s（repo=%s，materialize=%s）", l.desc, root, cfg.Workspace.Materialize)
		go func() {
			if err := srvRef.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	if len(listeners) > 1 {
		logger.Printf("HTTP 已启用并要求 Bearer token")
	}

	// 5. 优雅退出（§11.1）：停收新请求 → 等当前 land → umount 全部 → 放锁。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Printf("监听出错: %v", err)
		return err
	case sig := <-sigCh:
		logger.Printf("收到 %s，开始优雅退出", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := unixSrv.Shutdown(ctx); err != nil {
		logger.Printf("unix 监听关闭超时: %v", err)
	}
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Printf("HTTP 关闭超时: %v", err)
	}

	// 卸载全部 overlay 挂载点（不动 upper 层，§4.6）。
	all, err := r.WS.List()
	if err != nil {
		logger.Printf("列举工作区失败: %v", err)
	}
	for _, w := range all {
		if w.Materialize == "overlay" && w.Mounted {
			if err := r.MM.Unmount(w.ID); err != nil {
				logger.Printf("卸载 %s 失败: %v", w.ID, err)
			}
		}
	}

	if err := r.Close(); err != nil {
		logger.Printf("关闭仓库失败: %v", err)
	}
	logger.Printf("已退出")
	return nil
}

// startupCheck 是附录 G.4 的启动自检。
func startupCheck(r *repo.Repo, cfg config.Config) error {
	root := r.Root
	wsDir := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return fmt.Errorf("自检: workspaces/ 不可写: %w", err)
	}

	// 权限收紧（A3）：重复一次以防仓库是老版本创建的。
	for _, d := range []string{
		filepath.Join(root, ".p37"),
		wsDir,
	} {
		if err := os.Chmod(d, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("自检: 收紧 %s 权限失败: %w", d, err)
		}
	}
	if hf, err := os.OpenFile(filepath.Join(root, ".p37", "p37.db"), os.O_RDWR, 0o600); err == nil {
		_ = hf.Chmod(0o600)
		_ = hf.Close()
	}

	mode := cfg.MountMode()
	if mode == mountmgr.ModeOverlay {
		probe := filepath.Join(wsDir, ".p37-selfcheck")
		_ = os.RemoveAll(probe)
		_ = os.MkdirAll(probe, 0o755)
		err := r.MM.ProbeOverlay(probe)
		_ = os.RemoveAll(probe)
		if err != nil {
			return fmt.Errorf("自检: 配置要求 overlay 模式但挂载不可用（%w）；"+
				"请改用 copy，或按附录 G 配置权限", err)
		}
	}

	// inotify 限额只告警不阻断（附录 G.3）。
	if n, err := maxUserWatches(); err == nil {
		need := 64 * 8000
		if n < need {
			fmt.Fprintf(os.Stderr,
				"p37d 警告: fs.inotify.max_user_watches=%d，低于建议的 %d；"+
					"将降级为只监听工作区根目录（附录 G.3）\n", n, need)
		}
	}
	return nil
}

// watchFallbackNeeded 报告是否应降级为「只监听工作区根目录」（附录 G.3）。
//
// inotify 额度不足时，递归监听会在 64 个工作区上把额度耗尽；此时牺牲延迟
// 换取可用性，深层改动交给周期扫描兜底。
func watchFallbackNeeded() bool {
	n, err := maxUserWatches()
	if err != nil {
		return false
	}
	return n < 64*8000
}
