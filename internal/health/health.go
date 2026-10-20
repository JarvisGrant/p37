// Package health 实现 p37 的部署健康检查。
//
// 与 p37 的版本控制语义无关：它只回答“这台机器/这个仓库现在能不能安全地
// 跑 p37d”。CLI 的 p37 doctor 与 p37d --check 共用这里的文件系统检查，
// 避免两套阈值各说各话。
package health

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chenyiwen/p37/internal/buildinfo"
	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repolock"
)

// Status 是单项检查结果。
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Item 是一项检查结果。
type Item struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Detail   string `json:"detail"`
	Required bool   `json:"required"`
}

// Report 是一次完整检查。
type Report struct {
	OK        bool   `json:"ok"`
	Repo      string `json:"repo"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	FormatVer int    `json:"formatVer"`
	Strict    bool   `json:"strict"`
	Checks    []Item `json:"checks"`
}

// Options 控制检查严格度。
type Options struct {
	// Strict 为真时把 P36 的部署硬要求（ext4）视为必需项。
	Strict bool
}

// Check 对 root 执行健康检查。它不获取仓库排他锁，可以在 daemon 运行时调用。
func Check(root string, opt Options) Report {
	rep := Report{
		Repo:      root,
		Version:   buildinfo.Version,
		Commit:    buildinfo.Commit,
		FormatVer: meta.FormatVer,
		Strict:    opt.Strict,
	}
	add := func(name string, st Status, detail string, required bool) {
		rep.Checks = append(rep.Checks, Item{
			Name: name, Status: st, Detail: detail, Required: required,
		})
	}

	p37 := filepath.Join(root, ".p37")
	wsDir := filepath.Join(root, "workspaces")

	repoOK := true
	if fi, err := os.Stat(p37); err != nil || !fi.IsDir() {
		repoOK = false
		detail := p37
		if err != nil {
			detail = err.Error()
		} else {
			detail = p37 + " 不是目录"
		}
		add("仓库目录", StatusFail, detail, true)
	} else {
		add("仓库目录", StatusOK, p37, true)
	}

	// 数据库与格式版本。
	dbPath := filepath.Join(p37, "p37.db")
	if _, err := os.Stat(dbPath); err != nil {
		add("元数据库", StatusFail, err.Error(), true)
	} else if db, err := meta.Open(root); err != nil {
		add("元数据库", StatusFail, err.Error(), true)
	} else {
		ver, _ := db.UserVersion()
		ferr := db.CheckFormat()
		_ = db.Close()
		switch {
		case ferr != nil:
			add("格式版本", StatusFail, ferr.Error(), true)
		case ver != meta.FormatVer:
			add("格式版本", StatusFail,
				fmt.Sprintf("user_version=%d，期望 %d", ver, meta.FormatVer), true)
		default:
			add("格式版本", StatusOK,
				fmt.Sprintf("format_ver=%d", ver), true)
		}
	}

	// 可写探测必须真的写一个文件：access(2) 在 root / ACL / 只读挂载下都
	// 可能骗人。
	if repoOK {
		checkWritable(add, ".p37/tmp 可写", filepath.Join(p37, "tmp"))
		checkWritable(add, "workspaces/ 可写", wsDir)
	} else {
		add(".p37/tmp 可写", StatusSkip, "仓库目录不存在", true)
		add("workspaces/ 可写", StatusSkip, "仓库目录不存在", true)
	}

	// OverlayFS 的 lower/upper/work 必须同文件系统。
	if repoOK {
		same, detail := sameFilesystem(p37, wsDir)
		if same {
			add("同一文件系统", StatusOK, detail, true)
		} else {
			add("同一文件系统", StatusFail, detail, true)
		}
	} else {
		add("同一文件系统", StatusSkip, "仓库目录不存在", true)
	}

	// 文件系统类型（P36 的目标是 ext4）。
	if repoOK {
		fs, err := fsType(p37)
		switch {
		case errors.Is(err, errNotApplicable):
			add("文件系统类型", StatusSkip, "仅 Linux 检查", opt.Strict)
		case err != nil:
			st := StatusWarn
			if opt.Strict {
				st = StatusFail
			}
			add("文件系统类型", st, "无法探测: "+err.Error(), opt.Strict)
		case fs == "ext4":
			add("文件系统类型", StatusOK, "ext4", opt.Strict)
		case opt.Strict:
			add("文件系统类型", StatusFail,
				fmt.Sprintf("%s（P36 要求 ext4）", fs), true)
		default:
			add("文件系统类型", StatusWarn,
				fmt.Sprintf("%s（P36 部署要求 ext4；开发环境可用非 strict）", fs),
				false)
		}
	} else {
		add("文件系统类型", StatusSkip, "仓库目录不存在", opt.Strict)
	}

	// OverlayFS 可用性。
	cfg, cfgErr := config.Load(root)
	switch {
	case !repoOK:
		add("配置", StatusSkip, "仓库目录不存在", true)
	case cfgErr != nil:
		add("配置", StatusFail, cfgErr.Error(), true)
	default:
		add("配置", StatusOK, config.Path(root), true)
	}
	checkOverlay(add, root, wsDir, cfg, repoOK)

	// inotify 限额。
	if w, inst, err := inotifyLimits(); err != nil {
		add("inotify 限额", StatusSkip, "仅 Linux 检查", false)
	} else {
		switch {
		case w < 1<<20 || inst < 4096:
			add("inotify 限额", StatusWarn,
				fmt.Sprintf("max_user_watches=%d max_user_instances=%d（推荐 ≥ %d / 4096）",
					w, inst, 1<<20), false)
		default:
			add("inotify 限额", StatusOK,
				fmt.Sprintf("max_user_watches=%d max_user_instances=%d", w, inst), false)
		}
	}

	// 磁盘可用空间（只告警，不影响正确性）。
	if repoOK {
		avail, err := diskAvailable(root)
		if err != nil {
			add("磁盘可用空间", StatusWarn, "无法探测: "+err.Error(), false)
		} else if avail < cfg.WarnDiskBytes() {
			add("磁盘可用空间", StatusWarn,
				fmt.Sprintf("%s（建议 ≥ %s）", humanBytes(avail), humanBytes(cfg.WarnDiskBytes())), false)
		} else {
			add("磁盘可用空间", StatusOK, humanBytes(avail), false)
		}
	}

	// daemon 状态：仅报告，不参与退出码。
	checkDaemon(add, root, cfg)

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == StatusFail {
			rep.OK = false
			break
		}
	}
	return rep
}

func checkWritable(add func(string, Status, string, bool), name, dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		add(name, StatusFail, err.Error(), true)
		return
	}
	f, err := os.CreateTemp(dir, ".p37-check-*")
	if err != nil {
		add(name, StatusFail, err.Error(), true)
		return
	}
	nameOnDisk := f.Name()
	_, werr := f.WriteString("ok\n")
	cerr := f.Close()
	_ = os.Remove(nameOnDisk)
	if werr != nil {
		add(name, StatusFail, werr.Error(), true)
		return
	}
	if cerr != nil {
		add(name, StatusFail, cerr.Error(), true)
		return
	}
	add(name, StatusOK, dir, true)
}

func checkOverlay(add func(string, Status, string, bool), root, wsDir string, cfg config.Config, repoOK bool) {
	if !repoOK {
		add("OverlayFS", StatusSkip, "仓库目录不存在", false)
		return
	}
	if !overlaySupported() {
		add("OverlayFS", StatusSkip, "仅 Linux 检查", false)
		return
	}
	overlayMode := cfg.MountMode() == mountmgr.ModeOverlay
	failStatus := StatusWarn
	if overlayMode {
		failStatus = StatusFail
	}
	mm, err := mountmgr.New(root, mountmgr.Options{
		BaseCacheBytes: cfg.Workspace.BaseCacheBytes,
	})
	if err != nil {
		add("OverlayFS", failStatus, err.Error(), overlayMode)
		return
	}
	probe := filepath.Join(wsDir, fmt.Sprintf(".p37-check-overlay-%d", os.Getpid()))
	_ = os.RemoveAll(probe)
	if err := os.MkdirAll(probe, 0o755); err != nil {
		add("OverlayFS", failStatus, err.Error(), overlayMode)
		return
	}
	err = mm.ProbeOverlay(probe)
	_ = os.RemoveAll(probe)

	switch cfg.MountMode() {
	case mountmgr.ModeOverlay:
		if err != nil {
			add("OverlayFS", StatusFail, err.Error(), true)
		} else {
			add("OverlayFS", StatusOK, "可用", true)
		}
	case mountmgr.ModeCopy:
		add("OverlayFS", StatusSkip, "配置为 copy 模式，不需要 overlay", false)
	default:
		if err != nil {
			add("OverlayFS", StatusWarn,
				err.Error()+"（auto 将回退 copy；20GB L2 不建议）", false)
		} else {
			add("OverlayFS", StatusOK, "可用", false)
		}
	}
}

func checkDaemon(add func(string, Status, string, bool), root string, cfg config.Config) {
	p37 := filepath.Join(root, ".p37")
	if _, err := os.Stat(p37); err != nil {
		add("p37d 状态", StatusSkip, "仓库目录不存在", false)
		return
	}
	lockPath := filepath.Join(p37, "lock")
	sockPath := config.SocketPath(root, cfg)

	// 没有 lock 文件时不要为了探测而创建它：check 应尽量无副作用。
	if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
		if _, serr := os.Stat(sockPath); serr == nil {
			add("p37d 状态", StatusWarn, "socket 存在但锁文件不存在（残留 socket）", false)
		} else {
			add("p37d 状态", StatusOK, "未运行", false)
		}
		return
	}

	lock, err := repolock.TryExclusive(lockPath)
	switch {
	case errors.Is(err, repolock.ErrBusy):
		if _, serr := os.Stat(sockPath); serr == nil {
			add("p37d 状态", StatusOK, "运行中（锁+socket）", false)
		} else {
			add("p37d 状态", StatusWarn,
				"锁被持有但 socket 不存在（daemon 正在启动或异常）", false)
		}
	case err != nil:
		add("p37d 状态", StatusWarn, "无法检测: "+err.Error(), false)
	default:
		_ = lock.Release()
		if _, serr := os.Stat(sockPath); serr == nil {
			add("p37d 状态", StatusWarn, "socket 存在但锁未被持有（可能是残留 socket）", false)
		} else {
			add("p37d 状态", StatusOK, "未运行", false)
		}
	}
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
