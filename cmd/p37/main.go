// Command p37 是 p37 的运维 CLI（初始化、导入、检查、调试）。
//
// 日常的版本操作走 MCP（§11.3），本 CLI 只覆盖初始化与运维。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/buildinfo"
	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/repolock"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/workspace"
)

const usage = `p37 —— AI 原生版本控制系统

用法：
  p37 <命令> [参数]
  p37 --root <root> repo <list|create|info|delete|register|unregister|scan> ...   多仓库维护
  p37 --root <root> --repo <id|name> <命令> [参数]       对指定 repo 执行

初始化与运维：
  init <root> [--if-missing] [--mode empty|import|repair] [--default-line main]
              [--name <name>] [--force] [--idempotency-key <key>]
              [--config <file>] [--allow-nonpersistent]
                                  原子初始化/修复仓库；--if-missing 校验+修复
  init <root> --strict | --no-repair
                                  只校验 / 诊断，不修复
  import <root> <srcdir>          把目录树作为 v0 导入
  status <root>                   打印仓库摘要
  doctor <root>                   运行部署自检（附录 G.4）
  fsck <root>                     校验对象完整性与封闭性
  gc <root> [--prune] [--scrub] [--pack] [--report-roots] [--workspaces] [--force]
                                  保留策略 + 对象回收 + 抽样校验；
                                  daemon 持锁时拒绝（--force 才能跳过）
  migrate <root>                  向前迁移 format_ver
  ignore-check <root> <path>      检查某路径是否被忽略

调试用版本操作（产品路径是 MCP）：
  ws-open <root> <task> [--materialize auto|overlay|copy] [--parent <id>]
          [--name <name>] [--idempotency-key <key>] [--json]
  ws-ensure <root> --name <name> [--parent <id>] [--task <desc>]
            [--materialize auto|overlay|copy] [--idempotency-key <key>] [--json]
  ws-list <root> [--include-dropped] [--include-recently-dropped] [--json]
  ws-info <root> <ws_id>
  ws-sync <root> <ws_id>
  ws-diff <root> <ws_id>          预览将落地的改动
  ws-land <root> <ws_id>
  ws-drop <root> <ws_id> [--cascade=false]
  log <root> [limit]
  show <root> <version_id>
  revert <root> <version_id>
  blame <root> <path> [--range a-b] [--version <id>] [--hunk]

远端同步（P3，S3 兼容对象存储）：
  sync <root> prepare [--full] [--force]  生成增量包到本地 outbox（不上传）
  sync <root> list                        列出本地暂存包
  sync <root> show <id|last> [--verify]   查看/校验包
  sync <root> status [--remote]           同步状态
  sync <root> push [--all|<id>]           上传暂存包并更新远端 ref
  sync <root> fetch                       下载远端包并导入对象（不移动 head）
  sync <root> pull                        fetch + 仅快进本地 head
  sync <root> prune [--local] [--remote]  清理已上传的本地包 / 远端旧包

多仓库维护：
  p37 --root /srv/repos repo list [--json]
  p37 --root /srv/repos repo init <name> <path> [--mode empty|import|repair]
              [--force] [--idempotency-key <key>] [--json]
  p37 --root /srv/repos repo create <name> [--default-line main] [--json]
  p37 --root /srv/repos repo info <id|name> [--json]
  p37 --root /srv/repos repo delete <id|name> [--force]
  p37 --root /srv/repos repo register <name> <path> [--init-if-missing] [--default-line main] [--rebind] [--json]
  p37 --root /srv/repos repo unregister <id|name> [--force] [--json]
  p37 --root /srv/repos repo scan [--no-reconcile] [--prune-missing] [--dry-run] [--repair] [--json]

其它：
  version
  help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errw io.Writer) int {
	rootMode, rootDir, repoRef, rest, err := parseGlobal(args)
	if err != nil {
		fmt.Fprintf(errw, "%v\n\n%s", err, usage)
		return 2
	}
	if len(rest) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}
	cmd := rest[0]
	cmdArgs := rest[1:]

	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return 0
	case "version", "-v", "--version":
		printVersion(out)
		return 0
	}

	if rootMode {
		if cmd == "repo" {
			return cmdRepo(rootDir, cmdArgs, out, errw)
		}
		if cmd == "init" {
			fmt.Fprintln(errw, "多仓库模式请用: p37 --root <root> repo create <name>")
			return 2
		}
		if repoRef == "" {
			fmt.Fprintln(errw, "多仓库模式必须提供 --repo <id|name>")
			return 2
		}
		path, err := resolveRepoPath(rootDir, repoRef)
		if err != nil {
			return fail(errw, err)
		}
		return dispatch(cmd, path, cmdArgs, out, errw)
	}

	if cmd == "init" {
		return cmdInit(cmdArgs, out, errw)
	}
	if len(cmdArgs) == 0 {
		fmt.Fprintf(errw, "命令 %s 缺少 <root>\n\n%s", cmd, usage)
		return 2
	}
	return dispatch(cmd, cmdArgs[0], cmdArgs[1:], out, errw)
}

// parseGlobal 解析命令前的 --root / --repo。其余参数原样返回。
func parseGlobal(args []string) (rootMode bool, rootDir, repoRef string, rest []string, err error) {
	rest = append([]string{}, args...)
	for len(rest) > 0 {
		a := rest[0]
		switch {
		case a == "--root" || a == "--repo":
			if len(rest) < 2 {
				return false, "", "", nil, fmt.Errorf("%s 需要参数", a)
			}
			if a == "--root" {
				rootDir = rest[1]
				rootMode = true
			} else {
				repoRef = rest[1]
			}
			rest = rest[2:]
		case strings.HasPrefix(a, "--root="):
			rootDir = strings.TrimPrefix(a, "--root=")
			rootMode = true
			rest = rest[1:]
		case strings.HasPrefix(a, "--repo="):
			repoRef = strings.TrimPrefix(a, "--repo=")
			rest = rest[1:]
		default:
			return rootMode, rootDir, repoRef, rest, nil
		}
	}
	return rootMode, rootDir, repoRef, rest, nil
}

func resolveRepoPath(rootDir, ref string) (string, error) {
	cfg, err := config.Load(rootDir)
	if err != nil {
		return "", err
	}
	h, err := hub.New(rootDir, cfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = h.Close() }()
	e, err := h.Get(ref)
	if err != nil {
		return "", err
	}
	return e.Path, nil
}

func dispatch(cmd, root string, args []string, out, errw io.Writer) int {
	switch cmd {
	case "import":
		if len(args) < 1 {
			fmt.Fprintln(errw, "import 需要 <srcdir>")
			return 2
		}
		return cmdImport(root, args[0], out, errw)
	case "status":
		return cmdStatus(root, out, errw)
	case "doctor":
		return cmdDoctor(root, out, errw)
	case "fsck":
		return cmdFsck(root, out, errw)
	case "gc":
		return cmdGC(root, args, out, errw)
	case "migrate":
		return cmdMigrate(root, out, errw)
	case "ignore-check":
		if len(args) < 1 {
			fmt.Fprintln(errw, "ignore-check 需要 <path>")
			return 2
		}
		return cmdIgnoreCheck(root, args[0], out, errw)
	case "ws-open":
		return cmdWSOpen(root, args, out, errw)
	case "ws-ensure":
		return cmdWSEnsure(root, args, out, errw)
	case "ws-list":
		return cmdWSList(root, args, out, errw)
	case "ws-info":
		return cmdWSInfo(root, args, out, errw)
	case "ws-sync":
		return cmdWSSync(root, args, out, errw)
	case "ws-diff":
		return cmdWSDiff(root, args, out, errw)
	case "ws-land":
		return cmdWSLand(root, args, out, errw)
	case "ws-drop":
		return cmdWSDrop(root, args, out, errw)
	case "log":
		return cmdLog(root, args, out, errw)
	case "show":
		return cmdShow(root, args, out, errw)
	case "revert":
		return cmdRevert(root, args, out, errw)
	case "blame":
		return cmdBlame(root, args, out, errw)
	case "sync":
		return cmdSync(root, args, out, errw)
	default:
		fmt.Fprintf(errw, "未知命令 %q\n\n%s", cmd, usage)
		return 2
	}
}

const formatVer = meta.FormatVer

// printVersion 输出稳定、可解析的一行构建信息。
func printVersion(out io.Writer) {
	fmt.Fprintf(out, "p37 version=%s commit=%s buildTime=%s goVersion=%s formatVer=%d\n",
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime, buildinfo.GoVersion, formatVer)
}

// openRepo 按 root 的 config.toml 打开仓库。
func openRepo(root string) (*repo.Repo, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	return repo.Open(root, cfg.RepoOptions())
}

func fail(errw io.Writer, err error) int {
	fmt.Fprintf(errw, "错误: %v\n", err)
	return 1
}

// cmdInit 支持：
//
//	init <root>                      创建空仓库
//	init <root> --if-missing         仓库已存在时返回 0，且绝不改写 config
//	init <root> --config <file>      首次初始化使用配置模板
//
// flag 可以出现在 root 前后，方便引导脚本按需拼装。
func cmdInit(args []string, out, errw io.Writer) int {
	var (
		ifMissing, strict, noRepair, allowNonPersistent, jsonOut, modeSet, nameSet, force bool
		template, root, name, defLine, mode, idemKey                                      string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--if-missing":
			ifMissing = true
		case a == "--strict":
			strict = true
		case a == "--no-repair":
			noRepair = true
		case a == "--allow-nonpersistent":
			allowNonPersistent = true
		case a == "--json":
			jsonOut = true
		case a == "--force":
			force = true
		case a == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--config 需要 <file>")
				return 2
			}
			template = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			template = strings.TrimPrefix(a, "--config=")
		case a == "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--name 需要 <name>")
				return 2
			}
			name = args[i+1]
			nameSet = true
			i++
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
			nameSet = true
		case a == "--default-line":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--default-line 需要 <name>")
				return 2
			}
			defLine = args[i+1]
			i++
		case strings.HasPrefix(a, "--default-line="):
			defLine = strings.TrimPrefix(a, "--default-line=")
		case a == "--mode":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--mode 需要 empty|import|repair")
				return 2
			}
			mode = args[i+1]
			modeSet = true
			i++
		case strings.HasPrefix(a, "--mode="):
			mode = strings.TrimPrefix(a, "--mode=")
			modeSet = true
		case a == "--idempotency-key":
			if i+1 >= len(args) {
				fmt.Fprintln(errw, "--idempotency-key 需要 <key>")
				return 2
			}
			idemKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idemKey = strings.TrimPrefix(a, "--idempotency-key=")
		case a == "-h" || a == "--help":
			fmt.Fprint(out, usage)
			return 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		default:
			if root != "" {
				fmt.Fprintf(errw, "init 只接受一个 <root>，多余参数 %q\n", a)
				return 2
			}
			root = a
		}
	}
	if root == "" {
		fmt.Fprintln(errw, "init 需要 <root>")
		return 2
	}
	root = filepath.Clean(root)
	if name == "" {
		name = filepath.Base(root)
	}
	if !nameSet {
		// 已有 repo.json 时以身份锚为准，避免目录 basename 与 repo 名不同
		// 时把 --if-missing 变成 repo_name_mismatch。
		if m, merr := repo.ReadMeta(root); merr == nil && m != nil && m.Name != "" {
			name = m.Name
		} else {
			name = repo.SanitizeName(name)
		}
	}
	if defLine == "" {
		defLine = "main"
	}
	if mode == "" {
		mode = string(repo.InitEmpty)
	}
	if os.Getenv("P37_ALLOW_NONPERSISTENT") == "1" {
		allowNonPersistent = true
	}
	if strict && noRepair {
		fmt.Fprintln(errw, "--strict 与 --no-repair 只能二选一")
		return 2
	}
	if mode != string(repo.InitEmpty) && mode != string(repo.InitImport) && mode != string(repo.InitRepair) {
		fmt.Fprintf(errw, "--mode 必须是 empty|import|repair\n")
		return 2
	}

	// --strict / --no-repair 是诊断路径：只读，不路由到 daemon（避免触发 repair）。
	if strict || noRepair {
		in, err := repo.CheckIntegrity(root)
		if err != nil {
			return fail(errw, err)
		}
		if jsonOut {
			if code := writeJSON(out, errw, in); code != 0 {
				return code
			}
		} else {
			printIntegrity(out, in)
		}
		if in.Initialized {
			return 0
		}
		if noRepair {
			return fail(errw, fmt.Errorf("repo_not_initialized: %s（reason=%s next_action=%s）",
				root, in.Reason, in.NextAction))
		}
		return fail(errw, fmt.Errorf("repo_not_initialized: %s（reason=%s next_action=%s）",
			root, in.Reason, in.NextAction))
	}

	// 已有完整仓库：保持旧语义（无 --if-missing 报 already initialized）。
	// 但 refs 落后时，--if-missing/显式 mode 要继续走 Ensure 做 refs repair。
	if in, _ := repo.CheckIntegrity(root); in != nil && in.Initialized &&
		(in.RefsMatch || (!ifMissing && !modeSet)) {
		if in.RefsMatch {
			if jsonOut {
				printJSONIntegrity(out, errw, in)
			} else {
				fmt.Fprintf(out, "仓库已完整初始化，跳过初始化: %s\n  mainline_head %s\n  version_count %d\n",
					root, short(in.MainlineHead), in.VersionCount)
			}
			if ifMissing {
				return 0
			}
			return fail(errw, fmt.Errorf("%w: %s", repo.ErrAlreadyInitialized, root))
		}
		return fail(errw, fmt.Errorf(
			"repo_not_initialized: %s（reason=refs_mismatch；用 --if-missing 或 --mode=repair 对齐 refs）", root))
	}
	// 半初始化且显式 --no-repair 已在上面拦住；没有 --if-missing 时保持旧语义：
	// 只要 p37.db 存在就报 already initialized，避免误改用户仓库。
	if !ifMissing && !modeSet && fileExists(filepath.Join(root, ".p37", "p37.db")) {
		return fail(errw, fmt.Errorf("%w: %s", repo.ErrAlreadyInitialized, root))
	}

	// 允许旧 P36 镜像在“已有半成品”时自动修复；新代码统一走 repo.Ensure。
	cfg := config.Default()
	if cfgPath := config.Path(root); template == "" {
		if _, err := os.Stat(cfgPath); err == nil {
			if loaded, lerr := config.Load(root); lerr == nil {
				cfg = loaded
			}
		}
	}
	if template != "" {
		loaded, err := config.LoadTemplate(template)
		if err != nil {
			return fail(errw, fmt.Errorf("读取配置模板 %s: %w", template, err))
		}
		cfg = loaded
	}
	if cfg.Repo.FormatVer != 0 && cfg.Repo.FormatVer != formatVer {
		return fail(errw, fmt.Errorf(
			"配置模板 format_ver=%d，本程序支持 %d", cfg.Repo.FormatVer, formatVer))
	}
	cfg.Repo.FormatVer = formatVer
	switch {
	case cfg.Daemon.Socket == "":
		cfg.Daemon.Socket = filepath.Join(root, ".p37", "p37d.sock")
	case !filepath.IsAbs(cfg.Daemon.Socket):
		cfg.Daemon.Socket = filepath.Join(root, cfg.Daemon.Socket)
	}

	// 如果上层 root 有 p37d，路由到 daemon，避免 CLI 与 daemon 抢 .p37/lock。
	if res, routed, rerr := routeInit(root, name, defLine, mode, idemKey, force); routed {
		if rerr != nil {
			return repoFail(errw, rerr)
		}
		if jsonOut {
			return writeJSON(out, errw, res)
		}
		printInitResult(out, res)
		return 0
	}

	res, err := repo.Ensure(root, repo.EnsureOptions{
		Mode: repo.InitMode(mode), DefaultLine: defLine, Name: name,
		IdempotencyKey: idemKey, Force: force,
		RepoOptions: cfg.RepoOptions(),
		WriteConfig: func(controlDir string) error {
			return config.SaveControl(controlDir, cfg)
		},
		MountProbe: mountmgr.IsMountPoint, EnforcePersistent: !allowNonPersistent,
	})
	if err != nil {
		return repoFail(errw, err)
	}
	if idemKey != "" {
		res.Warnings = append(res.Warnings,
			"no root idempotency index (daemon/--root not used); key is path-scoped only")
	}
	if jsonOut {
		if code := writeJSON(out, errw, res); code != 0 {
			return code
		}
	} else {
		printInitResult(out, res)
	}
	return 0
}

func printInitResult(out io.Writer, res *repo.EnsureResult) {
	state := "已初始化"
	switch {
	case res.Repaired:
		state = "已修复"
	case res.Imported:
		state = "已导入"
	case res.Created:
		state = "已初始化"
	}
	fmt.Fprintf(out, "%s仓库 %s\n  mainline_head %s\n  version_count %d\n  default_line  %s\n",
		state, res.Repo.Path, short(res.Repo.MainlineHead), res.Repo.VersionCount, res.Repo.DefaultLine)
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
}

func printIntegrity(out io.Writer, in *repo.Integrity) {
	status := "未初始化"
	if in.Initialized {
		status = "已初始化"
	}
	fmt.Fprintf(out, "仓库 %s\n  状态           %s\n  reason         %s\n  next_action    %s\n  mainline_head  %s\n  version_count  %d\n  default_line   %s (ready=%v)\n",
		in.Path, status, in.Reason, in.NextAction, short(in.MainlineHead), in.VersionCount,
		in.DefaultLine, in.DefaultLineReady)
}

func printJSONIntegrity(out, errw io.Writer, in *repo.Integrity) {
	_ = writeJSON(out, errw, in)
}

func cmdImport(root, src string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	start := time.Now()
	vid, err := r.Import(src)
	if err != nil {
		return fail(errw, err)
	}
	v, err := r.DB.GetVersion(vid)
	if err != nil {
		return fail(errw, err)
	}
	files, err := countTreeFiles(r, v.Tree)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已导入 %s\n  版本   %s\n  文件数 %d\n  耗时   %s\n",
		src, short(vid.String()), files, time.Since(start).Round(time.Millisecond))
	return 0
}

func cmdStatus(root string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	st, err := r.Status()
	if err != nil {
		return fail(errw, err)
	}
	nObj, err := r.DB.ObjectIndex().Count()
	if err != nil {
		return fail(errw, err)
	}
	nCache, err := r.DB.StatCache().CountAll()
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "仓库 %s\n", st.Root)
	fmt.Fprintf(out, "  mainline_head  %s\n", st.MainlineHead.String())
	fmt.Fprintf(out, "  version_count  %d（epoch %d）\n", st.VersionCount, st.Epoch)
	fmt.Fprintf(out, "  工作区         %d\n", st.Workspaces)
	fmt.Fprintf(out, "  CAS 对象       %d\n", nObj)
	fmt.Fprintf(out, "  stat 缓存       %d 行\n", nCache)
	fmt.Fprintf(out, "  format_ver     %d\n", st.FormatVer)
	if st.LastLandTS > 0 {
		fmt.Fprintf(out, "  最近落地       %s\n",
			time.UnixMilli(st.LastLandTS).Format(time.RFC3339))
	}
	fmt.Fprintf(out, "  物化管理器     基底缓存 %s\n", humanBytes(r.MM.BaseCacheSize()))
	return 0
}

func cmdDoctor(root string, out, errw io.Writer) int {
	// 自检读配置：磁盘阈值与其它建议值都来自配置，不能在这里另写一套常量
	// （那正是「配置改了不生效」的来源）。
	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	ok := true
	report := func(name string, good bool, detail string) {
		mark := "OK  "
		if !good {
			mark = "FAIL"
			ok = false
		}
		fmt.Fprintf(out, "[%s] %-28s %s\n", mark, name, detail)
	}

	// 1. 目录与文件系统。
	p37 := filepath.Join(root, ".p37")
	wsDir := filepath.Join(root, "workspaces")
	if _, err := os.Stat(p37); err != nil {
		report("仓库目录", false, err.Error())
	} else {
		report("仓库目录", true, p37)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		report("workspaces/ 可写", false, err.Error())
	} else {
		report("workspaces/ 可写", true, wsDir)
	}

	// 1b. 初始化完整性（repo/head/line/refs/对象树）。
	if in, ierr := repo.CheckIntegrityDeep(root); ierr != nil {
		report("仓库初始化", false, ierr.Error())
	} else {
		detail := fmt.Sprintf("mainline_head=%s version_count=%d default_line=%s",
			short(in.MainlineHead), in.VersionCount, in.DefaultLine)
		if !in.Initialized {
			detail += fmt.Sprintf(" reason=%s next_action=%s", in.Reason, in.NextAction)
		}
		report("仓库初始化", in.Initialized, detail)
		if in.Initialized {
			report("line/refs 一致性", in.DefaultLineReady && in.RefsOK,
				fmt.Sprintf("default_line_ready=%v refs_ok=%v refs_match=%v",
					in.DefaultLineReady, in.RefsOK, in.RefsMatch))
		}
	}

	// 2. 同一文件系统（OverlayFS 的 lower/upper/work 必须同 FS）。
	same, detail := sameFilesystem(p37, wsDir)
	report("同一文件系统", same, detail)

	// 3. OverlayFS 可用性（非 Linux 平台不适用，开发机走 copy）。
	if !isLinux {
		report("OverlayFS", true, "不适用（非 Linux，将使用 copy 模式）")
	} else {
		mm, err := mountmgr.New(root, mountmgr.Options{})
		if err != nil {
			report("OverlayFS", false, err.Error())
		} else {
			probe := filepath.Join(wsDir, ".p37-doctor-probe")
			_ = os.RemoveAll(probe)
			_ = os.MkdirAll(probe, 0o755)
			err := mm.ProbeOverlay(probe)
			_ = os.RemoveAll(probe)
			if err != nil {
				report("OverlayFS", false, err.Error()+"（将回退 copy 模式）")
			} else {
				report("OverlayFS", true, "可用")
			}
		}
	}

	// 4. 大小写敏感性：两种结果都合法，只影响是否多做一次冲突检查。
	sensitive := mountmgr.CaseSensitiveProbe(wsDir)
	report("大小写敏感性", true, boolDetail(sensitive))

	// 5. inotify 限额（仅 Linux 有意义）。
	watches, err := maxUserWatches()
	if err != nil {
		report("inotify 限额", true, "不适用（非 Linux）")
	} else {
		recommended := 1 << 20
		good := watches >= recommended
		report("inotify 限额", good,
			fmt.Sprintf("max_user_watches=%d（推荐 ≥ %d）", watches, recommended))
	}

	// 6. 磁盘可用空间。
	avail, err2 := diskAvailable(root)
	if err2 != nil {
		report("磁盘可用空间", true, "无法探测")
	} else {
		// 磁盘空间是**建议值**，不是硬性错误：空间偏小只是意味着并发工作区
		// 或仓库规模要相应收敛，不影响正确性。阈值取配置的
		// workspace.warn_disk_bytes（默认 50 GiB）。
		warnBytes := cfg.WarnDiskBytes()
		if avail >= warnBytes {
			report("磁盘可用空间", true, humanBytes(avail))
		} else {
			fmt.Fprintf(out, "[WARN] %-28s %s（建议 ≥ %s；空间偏小只影响并发度，不影响正确性）\n",
				"磁盘可用空间", humanBytes(avail), humanBytes(warnBytes))
		}
	}

	// 6b. 抽样校验（§10.4）：默认按 gc.scrub_sample 抽，能立刻暴露静默损坏。
	if cfg.GC.ScrubSample > 0 {
		r0, err := openRepo(root)
		if err == nil {
			if res, serr := r0.Scrub(cfg.GC.ScrubSample); serr == nil {
				if len(res.Corrupt) > 0 {
					report("对象抽样校验", false,
						fmt.Sprintf("%d 个损坏（共抽 %d 个）", len(res.Corrupt), res.Checked))
					for _, c := range res.Corrupt {
						fmt.Fprintf(out, "        损坏: %s\n", c)
					}
				} else {
					report("对象抽样校验", true,
						fmt.Sprintf("抽 %d 个，全部完好（比例 %g）", res.Checked, cfg.GC.ScrubSample))
				}
			}
			_ = r0.Close()
		}
	}

	// 7. 仓库可打开且格式匹配。
	r, err := openRepo(root)
	if err != nil {
		report("打开仓库", false, err.Error())
	} else {
		report("打开仓库", true, "format_ver 匹配")
		_ = r.Close()
	}

	if ok {
		fmt.Fprintln(out, "\n全部检查通过，可以进入验证。")
		return 0
	}
	fmt.Fprintln(out, "\n存在未通过项，见上。")
	return 1
}

func cmdFsck(root string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	// 索引一致性：扫描 objects/ 与 object_index 对比。
	stats, err := r.Store.BlobStore().ListPrefix("objects/")
	if err != nil {
		return fail(errw, err)
	}
	onDisk := int64(len(stats))
	inIndex, err := r.DB.ObjectIndex().Count()
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "对象总数（松散+打包）%d\nobject_index 行数 %d\n", onDisk, inIndex)

	// 重建索引并比对（索引是纯缓存，可由对象自描述形态重建）。
	idx := r.DB.ObjectIndex()
	if err := idx.Clear(); err != nil {
		return fail(errw, err)
	}
	n, err := r.Store.RebuildIndex()
	if err != nil {
		return fail(errw, fmt.Errorf("重建索引失败: %w", err))
	}
	fmt.Fprintf(out, "重建后对象数     %d\n", n)
	if int64(n) != onDisk {
		fmt.Fprintf(out, "警告: 有 %d 个对象文件无法解析为合法对象\n", onDisk-int64(n))
		return 1
	}

	// 头部可达性：遍历全部命名线。
	lines, err := r.Lines()
	if err != nil {
		return fail(errw, err)
	}
	if len(lines) == 0 {
		head, ok, err := r.DB.Head()
		if err != nil {
			return fail(errw, err)
		}
		if !ok {
			fmt.Fprintln(out, "主干尚未初始化")
			return 1
		}
		lines = append(lines, meta.Line{Name: "(head)", Head: head})
	}
	for i := range lines {
		if err := r.WalkHead(lines[i].Head, func(id string) error { return nil }); err != nil {
			return fail(errw, fmt.Errorf("line %s 不可达: %w", lines[i].Name, err))
		}
	}
	fmt.Fprintln(out, "全部 line head 链完整")
	fmt.Fprintln(out, "fsck 通过")
	return 0
}

func cmdGC(root string, args []string, out, errw io.Writer) int {
	prune, scrub, workspacesOnly, rootsOnly, pack, force := false, false, false, false, false, false
	for _, a := range args {
		switch a {
		case "--prune":
			prune = true
		case "--scrub":
			scrub = true
		case "--workspaces":
			workspacesOnly = true
		case "--report-roots":
			rootsOnly = true
		case "--pack":
			pack = true
		case "--force":
			force = true
		default:
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		}
	}

	// GC 会删对象，而 daemon 的 landMu 只在本进程内有效。CLI 与 daemon
	// 同时跑时没有共享的 landMu，必须在入口处拒绝。锁要一直持到 GC 结束，
	// 这样 daemon 也不能在 GC 中途启动。
	if force {
		fmt.Fprintln(errw,
			"警告: --force 跳过了仓库锁检查；daemon 同时运行时可能误删对象。")
	} else if _, err := os.Stat(filepath.Join(root, ".p37")); err == nil {
		lock, err := repolock.TryExclusive(filepath.Join(root, ".p37", "lock"))
		if errors.Is(err, repolock.ErrBusy) {
			fmt.Fprintln(errw,
				"错误: 检测到同仓库的 p37d 正在运行（.p37/lock 被占用），GC 已拒绝。")
			fmt.Fprintln(errw,
				"      请先停止 p37d（维护窗口），或确认风险后使用 --force。")
			return 1
		}
		if err != nil {
			return fail(errw, fmt.Errorf("获取仓库锁失败: %w", err))
		}
		defer func() { _ = lock.Release() }()
	}

	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	if workspacesOnly {
		return gcWorkspaces(r, root, prune, out, errw)
	}

	// 保留 + mark-sweep（§10）。默认只报告（dry-run），--prune 才真的删。
	rep, err := r.GC(!prune || rootsOnly)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "版本: 共 %d，保留 %d，%s %d\n",
		rep.VersionTotal, rep.VersionKeep,
		map[bool]string{true: "将剪枝", false: "已剪枝"}[rep.DryRun], rep.VersionDrop)
	fmt.Fprintf(out, "对象: 共 %d，可达 %d，%s %d（%s）\n",
		rep.ObjectTotal, rep.ObjectKeep,
		map[bool]string{true: "可回收", false: "已删除"}[rep.DryRun],
		rep.ObjectDrop, humanBytes(rep.DropBytes))
	fmt.Fprintf(out, "根（谁钉住了什么）:\n")
	for _, rt := range rep.Roots {
		fmt.Fprintf(out, "  %-16s %-20s %4d 个对象  %s\n",
			rt.Kind, rt.ID, rt.Count, rt.Note)
	}
	if !rep.DryRun {
		fmt.Fprintf(out, "已剪枝版本 %d，删除对象 %d，回收 %s\n",
			rep.VersionsPruned, rep.ObjectsDeleted, humanBytes(rep.BytesReclaimed))
		fmt.Fprintf(out, "已清空 stat 缓存 %d 行（避免悬空对象引用，下一轮采集会重建）\n",
			rep.StatCacheCleared)
	}

	// 基底缓存 LRU（§4.8）。
	if freed, err := r.MM.Evict(); err == nil && freed > 0 {
		fmt.Fprintf(out, "已淘汰 overlay 基底缓存 %s\n", humanBytes(freed))
	}

	// 打包（§9 P2）：把松散小对象合并进 pack 文件。与其它回收一致，
	// 只有 --prune 才真的执行；否则只报告。
	if pack {
		if !prune {
			fmt.Fprintln(out, "（--pack 需要配合 --prune 才会真的打包；当前为预演）")
		} else {
			pr, err := r.Pack(cas.DefaultPackOptions())
			if err != nil {
				return fail(errw, err)
			}
			fmt.Fprintf(out, "打包: 新建 %d 个 pack，纳入对象 %d（%s），删除松散文件 %d，跳过超大对象 %d\n",
				pr.PacksCreated, pr.ObjectsPacked, humanBytes(pr.BytesPacked),
				pr.LooseRemoved, pr.SkippedLarge)
		}
	}

	if scrub {
		res, err := r.Scrub(r.GCOptions().ScrubSample)
		if err != nil {
			return fail(errw, err)
		}
		fmt.Fprintf(out, "scrub: 抽样校验 %d 个对象（%s），损坏 %d 个\n",
			res.Checked, humanBytes(res.Bytes), len(res.Corrupt))
		for _, c := range res.Corrupt {
			fmt.Fprintf(errw, "  损坏: %s\n", c)
		}
		if len(res.Corrupt) > 0 {
			return 1
		}
	}

	if rep.DryRun {
		fmt.Fprintln(out, "（这是预演；加 --prune 才会真的删除）")
	}
	return 0
}

// gcWorkspaces 处理孤儿工作区目录（§4.8）。
func gcWorkspaces(r *repo.Repo, root string, prune bool, out, errw io.Writer) int {
	all, err := r.WS.List()
	if err != nil {
		return fail(errw, err)
	}
	known := map[string]bool{}
	for _, w := range all {
		known[w.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(root, "workspaces"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(errw, err)
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		orphans = append(orphans, e.Name())
	}
	sort.Strings(orphans)
	active, hidden, landed, broken := 0, 0, 0, 0
	for i := range all {
		w := &all[i]
		switch {
		case w.Hidden:
			hidden++
		case w.LandedTS != 0:
			landed++
		case w.State == meta.StateBroken:
			broken++
		default:
			active++
		}
	}
	fmt.Fprintf(out, "工作区 %d 个（active %d / landed %d / hidden %d / broken %d），孤儿目录 %d 个\n",
		len(all), active, landed, hidden, broken, len(orphans))
	for _, o := range orphans {
		fmt.Fprintf(out, "  孤儿 %s\n", o)
	}
	if prune {
		for _, o := range orphans {
			if err := os.RemoveAll(filepath.Join(root, "workspaces", o)); err != nil {
				return fail(errw, err)
			}
		}
		fmt.Fprintf(out, "已删除 %d 个孤儿目录\n", len(orphans))
	} else if len(orphans) > 0 {
		fmt.Fprintln(out, "（加 --prune 才会删除）")
	}
	return 0
}

func cmdMigrate(root string, out, errw io.Writer) int {
	r, err := repo.Open(root, repo.Options{})
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	st, err := r.Status()
	if err != nil {
		return fail(errw, err)
	}
	if st.FormatVer == formatVer {
		fmt.Fprintf(out, "已是当前格式版本 %d，无需迁移。\n", formatVer)
		return 0
	}
	fmt.Fprintf(errw, "不支持的迁移路径: %d -> %d\n", st.FormatVer, formatVer)
	return 1
}

func cmdIgnoreCheck(root, p string, out, errw io.Writer) int {
	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	ig, err := tree.NewIgnore(true, cfg.Repo.IgnoreExtra)
	if err != nil {
		return fail(errw, err)
	}
	if lines, err := tree.LoadIgnoreFile(root); err != nil {
		return fail(errw, err)
	} else if len(lines) > 0 {
		if ig, err = tree.NewIgnore(true, append(append([]string{}, cfg.Repo.IgnoreExtra...), lines...)); err != nil {
			return fail(errw, err)
		}
	}
	norm, err := tree.NormalizePath(p)
	if err != nil {
		return fail(errw, err)
	}
	ignored := ig.Match(norm, false)
	fmt.Fprintf(out, "%s -> %v\n", norm, ignored)
	return 0
}

// --- 调试用版本操作 -----------------------------------------------------

func cmdWSOpen(root string, args []string, out, errw io.Writer) int {
	task, parent, mode, name, key := "", "", "auto", "", ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--parent":
			if i+1 < len(args) {
				parent = args[i+1]
				i++
			}
		case "--materialize":
			if i+1 < len(args) {
				mode = args[i+1]
				i++
			}
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--idempotency-key":
			if i+1 < len(args) {
				key = args[i+1]
				i++
			}
		case "--json":
			asJSON = true
		default:
			if task == "" {
				task = args[i]
			}
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := r.WS.OpenWithKey(workspace.OpenRequest{
		Parent: parent, Name: name, TaskDesc: task, Mode: mountmgr.Mode(mode),
		IdempotencyKey: key,
	})
	if err != nil {
		return fail(errw, err)
	}
	if res.Workspace == nil {
		if asJSON {
			return printJSON(out, errw, wsLifecycleCLI(r, res.WSID))
		}
		fmt.Fprintf(out, "ws_id       %s\n状态        已回收/已落地\n", res.WSID)
		return 0
	}
	if asJSON {
		return printJSON(out, errw, map[string]any{
			"ws_id": res.Workspace.ID, "dir": r.WS.Dir(res.Workspace.ID),
			"name": res.Workspace.Name, "materialize": res.Workspace.Materialize,
			"state": res.Workspace.State, "created": res.Created,
			"reused": res.Reused, "idempotent_replay": res.Replayed,
		})
	}
	w := res.Workspace
	fmt.Fprintf(out, "ws_id       %s\n目录        %s\n物化模式    %s\n父          %s:%s\n",
		w.ID, r.WS.Dir(w.ID), w.Materialize, w.ParentKind, short(w.ParentID))
	return 0
}

func wsLifecycleCLI(r *repo.Repo, wsID string) map[string]any {
	life, err := r.WSLifecycleOf(wsID)
	if err != nil {
		return map[string]any{"ws_id": wsID, "state": "unknown", "error": err.Error()}
	}
	out := map[string]any{"ws_id": wsID, "state": string(life.State)}
	if life.Tombstone != nil {
		out["reason"] = life.Tombstone.Reason
		out["dropped_at"] = life.Tombstone.DroppedAt
		out["line"] = life.Tombstone.Line
		out["line_head"] = life.Tombstone.LineHead
		out["landed_version"] = life.Tombstone.LandedVersion
	}
	if life.CurrentLineHead != "" {
		out["current_line_head"] = life.CurrentLineHead
	}
	return out
}

func cmdWSEnsure(root string, args []string, out, errw io.Writer) int {
	name, parent, task, mode, key := "", "", "", "auto", ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--parent":
			if i+1 < len(args) {
				parent = args[i+1]
				i++
			}
		case "--task":
			if i+1 < len(args) {
				task = args[i+1]
				i++
			}
		case "--materialize":
			if i+1 < len(args) {
				mode = args[i+1]
				i++
			}
		case "--idempotency-key":
			if i+1 < len(args) {
				key = args[i+1]
				i++
			}
		case "--json":
			asJSON = true
		}
	}
	if name == "" {
		fmt.Fprintln(errw, "ws-ensure 需要 --name")
		return 2
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := r.WS.Ensure(workspace.OpenRequest{
		Parent: parent, Name: name, TaskDesc: task, Mode: mountmgr.Mode(mode),
		IdempotencyKey: key,
	})
	if err != nil {
		return fail(errw, err)
	}
	if res.Workspace == nil {
		if asJSON {
			return printJSON(out, errw, wsLifecycleCLI(r, res.WSID))
		}
		fmt.Fprintf(out, "ws_id       %s\n状态        已回收/已落地\n", res.WSID)
		return 0
	}
	if asJSON {
		return printJSON(out, errw, map[string]any{
			"ws_id": res.Workspace.ID, "dir": r.WS.Dir(res.Workspace.ID),
			"name": res.Workspace.Name, "materialize": res.Workspace.Materialize,
			"state": res.Workspace.State, "created": res.Created,
			"reused": res.Reused, "idempotent_replay": res.Replayed,
		})
	}
	w := res.Workspace
	fmt.Fprintf(out, "ws_id       %s\n目录        %s\n物化模式    %s\n父          %s:%s\n创建        %v\n",
		w.ID, r.WS.Dir(w.ID), w.Materialize, w.ParentKind, short(w.ParentID), res.Created)
	return 0
}

func cmdWSList(root string, args []string, out, errw io.Writer) int {
	includeDropped, includeRecent, asJSON := false, false, false
	for _, a := range args {
		switch a {
		case "--include-dropped":
			includeDropped = true
		case "--include-recently-dropped":
			includeRecent = true
		case "--json":
			asJSON = true
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	all, err := r.WS.List()
	if err != nil {
		return fail(errw, err)
	}
	now := time.Now().UnixMilli()
	items := make([]map[string]any, 0, len(all))
	for _, w := range all {
		n, _ := r.DB.CountOps(w.ID)
		state := w.State
		if w.LandedTS != 0 {
			state = "landed"
		}
		items = append(items, map[string]any{
			"ws_id": w.ID, "name": w.Name, "state": state,
			"materialize": w.Materialize, "captured_ops": n,
			"task": w.TaskDesc, "dir": r.WS.Dir(w.ID),
			"created_ts": w.CreatedTS, "updated_ts": w.UpdatedTS,
			"age_ms": now - w.CreatedTS, "landed_ts": w.LandedTS,
		})
	}
	if includeDropped || includeRecent {
		since := int64(0)
		if includeRecent && !includeDropped {
			since = now - int64(time.Hour/time.Millisecond)
		}
		ts, err := r.DB.ListWSTombstones(since, now, 500)
		if err != nil {
			return fail(errw, err)
		}
		for _, t := range ts {
			items = append(items, map[string]any{
				"ws_id": t.WSID, "name": t.Name, "state": "tombstone",
				"reason": t.Reason, "why": t.Why, "dropped_at": t.DroppedAt,
				"line": t.Line, "line_head": t.LineHead,
				"landed_version": t.LandedVersion, "target_kind": t.TargetKind,
				"target_id": t.TargetID, "dirty_ops": t.DirtyOps,
			})
		}
	}
	if asJSON {
		return printJSON(out, errw, items)
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "（无工作区）")
		return 0
	}
	fmt.Fprintf(out, "%-24s %-12s %-10s %-6s %s\n", "WS_ID", "STATE", "MATERIAL", "OPS", "TASK")
	for _, it := range items {
		ops, _ := it["captured_ops"].(int64)
		if ops == 0 {
			if v, ok := it["captured_ops"].(int); ok {
				ops = int64(v)
			}
		}
		task, _ := it["task"].(string)
		if it["state"] == "tombstone" {
			task = "reason=" + fmt.Sprint(it["reason"])
		}
		fmt.Fprintf(out, "%-24v %-12v %-10v %-6d %s\n",
			it["ws_id"], it["state"], it["materialize"], ops, task)
	}
	return 0
}

func printJSON(out, errw io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintln(out, string(b))
	return 0
}

func cmdWSInfo(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-info 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	info, err := r.WS.Info(args[0])
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "ws_id        %s\nstate        %s\ndir          %s\nmaterialize  %s\nparent       %s:%s\nbase_version %s\ndirty        %v\ncaptured_ops %d\nconflicts    %d\n",
		info.ID, info.State, info.Dir, info.Materialize, info.ParentKind, short(info.ParentID),
		short(info.BaseVersion.String()), info.Dirty, info.CapturedOps, info.ConflictCount)
	return 0
}

func cmdWSSync(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-sync 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	start := time.Now()
	if err := r.WS.Sync(args[0]); err != nil {
		return fail(errw, err)
	}
	n, _ := r.DB.CountOps(args[0])
	fmt.Fprintf(out, "采集完成：%d 个 op，耗时 %s\n", n, time.Since(start).Round(time.Millisecond))
	return 0
}

func cmdWSDiff(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-diff 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := r.Preview(args[0])
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "op 数 %d，可落地 %v\n", res.OpsCount, res.CanLand)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %-40s %-10s +%d -%d\n", f.Path, f.Kind, f.Added, f.Deleted)
	}
	for _, c := range res.Conflicts {
		fmt.Fprintf(out, "  冲突 %s %s (%s)\n", c.Key.String()[:16], c.Path, c.Kind)
	}
	return 0
}

func cmdWSLand(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-land 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := r.Land(args[0])
	if err != nil {
		if errors.Is(err, repo.ErrLandConflict) {
			fmt.Fprintf(out, "落地被冲突阻断，共 %d 个：\n", len(res.Conflicts))
			for _, c := range res.Conflicts {
				fmt.Fprintf(out, "  %s  %s  %s\n", c.Key.String(), c.Path, c.Kind)
			}
			fmt.Fprintln(out, "用 MCP 的 conflict_show / conflict_resolve 处理后重试。")
			return 1
		}
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已落地为 %s（op 数 %d）\n", res.VersionID.String(), res.OpsCount)
	return 0
}

func cmdWSDrop(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-drop 需要 <ws_id>"))
	}
	cascade := true
	for _, a := range args[1:] {
		if a == "--cascade=false" {
			cascade = false
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	dropped, reparented, err := r.WS.Drop(args[0], cascade, false)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已放弃 %v；重挂子工作区 %v\n", dropped, reparented)
	return 0
}

func cmdLog(root string, args []string, out, errw io.Writer) int {
	limit := 20
	if len(args) > 0 {
		_, _ = fmt.Sscanf(args[0], "%d", &limit)
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	vs, err := r.Log(limit, 0)
	if err != nil {
		return fail(errw, err)
	}
	for _, v := range vs {
		fmt.Fprintf(out, "%s  %s  %-12s  %s\n",
			short(v.ID.String()),
			time.UnixMilli(v.TS).Format("2006-01-02 15:04:05"),
			v.Agent, v.Task)
	}
	return 0
}

func cmdShow(root string, args []string, out, errw io.Writer) int {
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	vid, err := resolveVersionArg(r, id)
	if err != nil {
		return fail(errw, err)
	}
	d, err := r.VersionShow(vid)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "版本   %s\n父     %s\n时间   %s\nagent  %s\n任务   %s\n",
		d.ID.String(), d.Parent.String(),
		time.UnixMilli(d.TS).Format(time.RFC3339), d.Agent, d.Task)
	if len(d.Files) > 0 {
		fmt.Fprintln(out, "文件：")
		for _, f := range d.Files {
			fmt.Fprintf(out, "  %-40s %-10s +%d -%d\n", f.Path, f.Kind, f.Added, f.Deleted)
		}
	}
	return 0
}

func cmdRevert(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("revert 需要 <version_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	vid, err := resolveVersionArg(r, args[0])
	if err != nil {
		return fail(errw, err)
	}
	rv, err := r.Revert(vid)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已回退，新版本 %s\n", rv.String())
	return 0
}

// --- 小工具 -------------------------------------------------------------

// mustVersionArg 解析版本参数，支持 `head` / `HEAD` / 空（= head）。
func mustVersionArg(s string) hash.ID {
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "head") {
		return hash.ID{}
	}
	return mustHash(t)
}

// resolveVersionArg 解析版本参数，支持 head / 空 / 完整 id / **唯一前缀**。
//
// `p37 log` 打印的是 16 位短 id，用户理应用它回喂给 show / revert / blame。
func resolveVersionArg(r *repo.Repo, s string) (hash.ID, error) {
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "head") {
		head, ok, err := r.Head()
		if err != nil {
			return hash.ID{}, err
		}
		if !ok {
			return hash.ID{}, errors.New("无 head")
		}
		return head, nil
	}
	if id, err := hash.Parse(t); err == nil {
		return id, nil
	}
	if !isHexPrefix(t) {
		return hash.ID{}, fmt.Errorf("非法版本 id %q", s)
	}
	id, err := r.DB.FindVersionByPrefix(t)
	switch {
	case errors.Is(err, meta.ErrNotExist):
		return hash.ID{}, fmt.Errorf("找不到版本（前缀 %q）", s)
	case errors.Is(err, meta.ErrAmbiguous):
		return hash.ID{}, fmt.Errorf("前缀 %q 匹配到多个版本，请给更长前缀", s)
	}
	return id, err
}

// isHexPrefix 报告 s 是否为一段十六进制字符（不要求偶数长度）。
func isHexPrefix(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func mustHash(s string) hash.ID {
	id, err := hash.Parse(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "非法 id %q: %v\n", s, err)
		os.Exit(2)
	}
	return id
}

func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func boolDetail(b bool) string {
	if b {
		return "是"
	}
	return "否（物化前会检查大小写冲突）"
}

// countTreeFiles 递归统计树中的文件条目数。
func countTreeFiles(r *repo.Repo, root hash.ID) (int, error) {
	t, err := r.Store.GetTree(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range t.Entries {
		if e.Kind == codec.EntryTree {
			sub, err := countTreeFiles(r, e.ID)
			if err != nil {
				return 0, err
			}
			n += sub
			continue
		}
		n++
	}
	return n, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
