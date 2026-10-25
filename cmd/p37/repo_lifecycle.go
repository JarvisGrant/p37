package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/daemonclient"
	"github.com/chenyiwen/p37/internal/hub"
)

// cmdRepoRegister 支持：
//
//	repo register <name> <path> [--init-if-missing] [--default-line main]
//	                             [--force-rename] [--rebind] [--json]
func cmdRepoRegister(root string, args []string, out, errw io.Writer) int {
	var (
		name, path, defLine         string
		initMissing, rename, rebind bool
		jsonOut                     bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--init-if-missing":
			initMissing = true
		case a == "--force-rename":
			rename = true
		case a == "--rebind":
			rebind = true
		case a == "--default-line" && i+1 < len(args):
			defLine = args[i+1]
			i++
		case strings.HasPrefix(a, "--default-line="):
			defLine = strings.TrimPrefix(a, "--default-line=")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		default:
			if name == "" {
				name = a
			} else if path == "" {
				path = a
			} else {
				fmt.Fprintf(errw, "repo register 只接受 <name> <path>，多余参数 %q\n", a)
				return 2
			}
		}
	}
	if name == "" || path == "" {
		fmt.Fprintln(errw, "repo register 需要 <name> <path>")
		return 2
	}
	argsMap := map[string]any{
		"repo": name, "path": path, "default_line": defLine,
		"init_if_missing": initMissing, "force_rename": rename, "rebind": rebind,
	}
	if data, routed, err := routeRepoCommand(root, "repo_register", argsMap); err != nil {
		return repoFail(errw, err)
	} else if routed {
		return printRegisterData(data, jsonOut, out, errw)
	}

	h, err := openRepoHub(root)
	if err != nil {
		return repoFail(errw, err)
	}
	defer func() { _ = h.Close() }()
	e, err := h.RegisterPath(name, path, hub.RegisterOptions{
		DefaultLine: defLine, InitIfMissing: initMissing, ForceRename: rename, Rebind: rebind,
	})
	if err != nil {
		return repoFail(errw, err)
	}
	b, _ := json.Marshal(e)
	return printRegisterData(b, jsonOut, out, errw)
}

// cmdRepoUnregister 支持：
//
//	repo unregister <id|name> [--force] [--json]
func cmdRepoUnregister(root string, args []string, out, errw io.Writer) int {
	var ref string
	var force, jsonOut bool
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		default:
			if ref != "" {
				fmt.Fprintf(errw, "repo unregister 只接受一个 <id|name>，多余参数 %q\n", a)
				return 2
			}
			ref = a
		}
	}
	if ref == "" {
		fmt.Fprintln(errw, "repo unregister 需要 <id|name>")
		return 2
	}
	argsMap := map[string]any{"repo": ref, "force": force}
	if data, routed, err := routeRepoCommand(root, "repo_unregister", argsMap); err != nil {
		return repoFail(errw, err)
	} else if routed {
		return printUnregisterData(data, jsonOut, out, errw)
	}

	h, err := openRepoHub(root)
	if err != nil {
		return repoFail(errw, err)
	}
	defer func() { _ = h.Close() }()
	res, err := h.Unregister(ref, force)
	if err != nil {
		return repoFail(errw, err)
	}
	payload := map[string]any{"unregistered": true, "already_unregistered": res.Already, "repo": ref}
	if res.Entry != nil {
		payload["id"] = res.Entry.ID
		payload["name"] = res.Entry.Name
		payload["path"] = res.Entry.Path
	}
	b, _ := json.Marshal(payload)
	return printUnregisterData(b, jsonOut, out, errw)
}

// cmdRepoScan 支持：
//
//	repo scan [--no-reconcile] [--prune-missing] [--dry-run] [--json]
func cmdRepoScan(root string, args []string, out, errw io.Writer) int {
	reconcile := true
	var prune, dry, jsonOut bool
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "--no-reconcile":
			reconcile = false
		case "--prune-missing":
			prune = true
		case "--dry-run":
			dry = true
		default:
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		}
	}
	argsMap := map[string]any{"reconcile": reconcile, "prune_missing": prune, "dry_run": dry}
	if data, routed, err := routeRepoCommand(root, "repo_scan", argsMap); err != nil {
		return repoFail(errw, err)
	} else if routed {
		return printScanData(data, jsonOut, out, errw)
	}

	h, err := openRepoHub(root)
	if err != nil {
		return repoFail(errw, err)
	}
	defer func() { _ = h.Close() }()
	res, err := h.Scan(hub.ScanOptions{Reconcile: reconcile, PruneMissing: prune, DryRun: dry})
	if err != nil {
		return repoFail(errw, err)
	}
	b, _ := json.Marshal(res)
	return printScanData(b, jsonOut, out, errw)
}

func openRepoHub(root string) (*hub.Hub, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	return hub.New(root, cfg)
}

// routeRepoCommand 在 daemon 可连接时通过 unix socket 的 MCP 路由执行。
//
// 返回 (data, true, nil) 表示已由 daemon 处理；(_, false, nil) 表示 daemon
// 不在，调用方应直连 registry。
func routeRepoCommand(root, tool string, args any) (json.RawMessage, bool, error) {
	sock := filepath.Join(root, ".p37", "p37d.sock")
	fi, err := os.Stat(sock)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// 普通文件不是 daemon socket（可能是遗留物），按 daemon 不在处理。
	if fi.Mode()&os.ModeSocket == 0 {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := daemonclient.Connect(ctx, sock)
	if err != nil {
		if daemonAbsent(err) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("daemon_running: socket %s 存在但无法连接: %w", sock, err)
	}
	defer c.Close()
	data, err := c.Call(ctx, tool, args)
	if err != nil {
		return nil, true, err
	}
	return data, true, nil
}

func daemonAbsent(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ENOTSOCK) ||
		errors.Is(err, syscall.ECONNRESET)
}

func repoFail(errw io.Writer, err error) int {
	return fail(errw, withRepoErrorCode(err))
}

// withRepoErrorCode 让 CLI 直连路径也输出与 MCP 一致的错误码。
func withRepoErrorCode(err error) error {
	switch {
	case errors.Is(err, hub.ErrRepoBusy):
		return fmt.Errorf("repo_busy: %w", err)
	case errors.Is(err, hub.ErrRepoIsMountpoint):
		return fmt.Errorf("repo_is_mountpoint: %w", err)
	case errors.Is(err, hub.ErrInvalidPath):
		return fmt.Errorf("invalid_path: %w", err)
	case errors.Is(err, hub.ErrRepoNotInitialized):
		return fmt.Errorf("repo_not_initialized: %w", err)
	case errors.Is(err, hub.ErrRepoNameMismatch):
		return fmt.Errorf("repo_name_mismatch: %w", err)
	case errors.Is(err, hub.ErrRepoPathExists):
		return fmt.Errorf("repo_path_exists: %w", err)
	case errors.Is(err, hub.ErrRegistryBusy):
		return fmt.Errorf("registry_busy: %w", err)
	case errors.Is(err, hub.ErrRepoNotFound):
		return fmt.Errorf("repo_not_found: %w", err)
	}
	return err
}

func printRegisterData(data json.RawMessage, jsonOut bool, out, errw io.Writer) int {
	if jsonOut {
		return writeRawJSON(out, errw, data)
	}
	var e hub.Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "registered %s (%s) path=%s mounted=%v available=%v\n",
		e.Name, e.ID, e.Path, e.Mounted, e.Available)
	return 0
}

func printUnregisterData(data json.RawMessage, jsonOut bool, out, errw io.Writer) int {
	if jsonOut {
		return writeRawJSON(out, errw, data)
	}
	var r struct {
		Already bool   `json:"already_unregistered"`
		Repo    string `json:"repo"`
		ID      string `json:"id"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fail(errw, err)
	}
	if r.Already {
		fmt.Fprintf(out, "already unregistered %s\n", r.Repo)
		return 0
	}
	fmt.Fprintf(out, "unregistered %s (%s)\n", r.Name, r.ID)
	return 0
}

func printScanData(data json.RawMessage, jsonOut bool, out, errw io.Writer) int {
	if jsonOut {
		return writeRawJSON(out, errw, data)
	}
	var r hub.ScanResult
	if err := json.Unmarshal(data, &r); err != nil {
		return fail(errw, err)
	}
	for i := range r.Added {
		fmt.Fprintf(out, "added    %s (%s) %s\n", r.Added[i].Name, r.Added[i].ID, r.Added[i].Path)
	}
	for i := range r.Removed {
		fmt.Fprintf(out, "removed  %s (%s) %s\n", r.Removed[i].Name, r.Removed[i].ID, r.Removed[i].Path)
	}
	for i := range r.Degraded {
		fmt.Fprintf(out, "degraded %s (%s) %s: %s\n", r.Degraded[i].Name,
			r.Degraded[i].ID, r.Degraded[i].Path, r.Degraded[i].DegradedMsg)
	}
	for i := range r.Skipped {
		fmt.Fprintf(out, "skipped  %s: %s\n", r.Skipped[i].Path, r.Skipped[i].Reason)
	}
	fmt.Fprintf(out, "added=%d removed=%d degraded=%d skipped=%d dry_run=%v reconciled=%v\n",
		len(r.Added), len(r.Removed), len(r.Degraded), len(r.Skipped), r.DryRun, r.Reconciled)
	return 0
}

func writeRawJSON(out, errw io.Writer, data json.RawMessage) int {
	if len(data) == 0 {
		fmt.Fprintln(out, "null")
		return 0
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fail(errw, err)
	}
	return writeJSON(out, errw, v)
}
