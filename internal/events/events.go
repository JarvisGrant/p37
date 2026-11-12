// Package events 实现 P37 → 本机消费方（P36 guest agent）的语义事件上报。
//
// 设计约束：
//   - 只做 best-effort：socket 不存在、对端慢或断开都不能影响 repo 操作；
//   - 单条 JSON line，便于消费方用 json.Decoder 流式读取；
//   - 事件只带“哪个 repo 的什么状态变了”，不带数据；消费方再调用 repo_usage 取准确值。
package events

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Version 是事件格式版本。
const Version = 1

// 事件名（Event.Event 取值）。
const (
	// WorkspaceChanged 是工作区文件发生变化（未提交内容），用于刷新 workspace_bytes。
	WorkspaceChanged = "workspace_changed"
	// VersionLanded 是 ws_land / ws_land_into 成功落地。
	VersionLanded = "version_landed"
	// VersionReverted 是 version_revert / line reset 成功。
	VersionReverted = "version_reverted"
	// SyncPulled 是 sync_pull 快进本地 head。
	SyncPulled = "sync_pulled"
	// RepoInitialized / RepoImported 是仓库初始化/导入。
	RepoInitialized = "repo_initialized"
	RepoImported    = "repo_imported"
	// GCCompleted 是 GC/打包完成。
	GCCompleted = "gc_completed"
)

// Event 是写入 unix socket 的一条事件。
type Event struct {
	V         int    `json:"v"`
	Event     string `json:"event"`
	Repo      string `json:"repo,omitempty"`
	Root      string `json:"root"`
	Head      string `json:"head,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	WSID      string `json:"ws_id,omitempty"`
	TS        int64  `json:"ts"`
}

type asyncEvent struct {
	socket string
	ev     Event
}

var asyncCh = make(chan asyncEvent, 256)

func init() {
	go func() {
		for e := range asyncCh {
			Emit(e.socket, e.ev)
		}
	}()
}

// EmitAsync 把事件放进有界队列，由单 goroutine 串行写 socket。
//
// 用于采集/land 等热路径：调用方不再为 150ms dial/write 阻塞；队列满时
// 丢弃本次事件（事件是 best-effort，P36 侧有周期兜底）。
func EmitAsync(socket string, ev Event) {
	if socket == "" {
		return
	}
	select {
	case asyncCh <- asyncEvent{socket: socket, ev: ev}:
	default:
	}
}

// Emit 把事件写入 socket。socket 为空或不可达时静默返回。
//
// 同步版本保留给测试/显式调用；业务热路径应使用 EmitAsync。
func Emit(socket string, ev Event) {
	socket = filepath.Clean(socket)
	if socket == "" || socket == "." {
		return
	}
	if ev.V == 0 {
		ev.V = Version
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	conn, err := net.DialTimeout("unix", socket, 150*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(150 * time.Millisecond))
	_ = json.NewEncoder(conn).Encode(ev)
}

// EmitTo 是测试/工具入口：socket 不存在时不创建。
func EmitTo(socket string, ev Event) error {
	if socket == "" {
		return os.ErrInvalid
	}
	conn, err := net.DialTimeout("unix", socket, 150*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	if ev.V == 0 {
		ev.V = Version
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	return json.NewEncoder(conn).Encode(ev)
}
