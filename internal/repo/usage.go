package repo

import (
	"os"
	"path/filepath"
	"time"

	"github.com/chenyiwen/p37/internal/events"
)

// Usage 是仓库的只读用量快照。
//
// 口径：
//   - objects.stored_bytes 是 CAS 对象的存储字节（object_index.size 之和）；
//   - objects.raw_bytes 是对象解压后的原始字节（object_index.raw_size 之和）；
//   - durable_stored_bytes = objects.stored_bytes + db/refs/config；
//   - workspaces.logical_bytes 是未提交工作区的文件逻辑大小；
//   - workspaces.disk_bytes 是工作区实际块占用（来自 workspace.Status）。
//
// 它不读 objects/ 目录本身，也不依赖卷外 mount：object_index 是缓存，
// 可由 RebuildIndex 重建，但测量路径应保持 O(1) SQL + 少量 stat。
type Usage struct {
	Repo               string          `json:"repo"`
	Root               string          `json:"root"`
	Head               string          `json:"head"`
	VersionCount       int64           `json:"version_count"`
	Epoch              uint64          `json:"epoch"`
	LastLandTS         int64           `json:"last_land_ts"`
	Objects            UsageObjects    `json:"objects"`
	DBBytes            int64           `json:"db_bytes"`
	RefsBytes          int64           `json:"refs_bytes"`
	ConfigBytes        int64           `json:"config_bytes"`
	DurableStoredBytes int64           `json:"durable_stored_bytes"`
	DurableRawBytes    int64           `json:"durable_raw_bytes"`
	Workspaces         UsageWorkspaces `json:"workspaces"`
	MeasuredAt         string          `json:"measured_at"`
}

// UsageObjects 是 CAS 对象统计。
type UsageObjects struct {
	Count       int64 `json:"count"`
	StoredBytes int64 `json:"stored_bytes"`
	RawBytes    int64 `json:"raw_bytes"`
}

// UsageWorkspaces 是未提交工作区统计。
type UsageWorkspaces struct {
	Count        int   `json:"count"`
	LogicalBytes int64 `json:"logical_bytes"`
	DiskBytes    int64 `json:"disk_bytes"`
}

// Usage 返回仓库的只读用量快照。
func (r *Repo) Usage() (*Usage, error) {
	st, err := r.Status()
	if err != nil {
		return nil, err
	}
	count, stored, raw, err := r.DB.ObjectIndex().Stats()
	if err != nil {
		return nil, err
	}
	dbBytes := statSum(
		filepath.Join(r.Root, ".p37", "p37.db"),
		filepath.Join(r.Root, ".p37", "p37.db-wal"),
		filepath.Join(r.Root, ".p37", "p37.db-shm"),
	)
	refsBytes := walkLogicalBytes(filepath.Join(r.Root, ".p37", "refs"))
	configBytes := statSum(
		filepath.Join(r.Root, ".p37", "config.toml"),
		filepath.Join(r.Root, ".p37", "repo.json"),
		filepath.Join(r.Root, ".p37", "init.json"),
	)
	wsLogical := walkLogicalBytes(filepath.Join(r.Root, "workspaces"))
	wsCount, wsDisk := 0, int64(0)
	if all, err := r.WS.List(); err == nil {
		wsCount = len(all)
		for i := range all {
			info, ierr := r.WS.Info(all[i].ID)
			if ierr == nil && info != nil {
				wsDisk += info.DiskBytes
			}
		}
	}
	head := ""
	if st.HasHead {
		head = st.MainlineHead.String()
	}
	u := &Usage{
		Repo:         filepath.Base(r.Root),
		Root:         r.Root,
		Head:         head,
		VersionCount: st.VersionCount,
		Epoch:        st.Epoch,
		LastLandTS:   st.LastLandTS,
		Objects: UsageObjects{
			Count: count, StoredBytes: stored, RawBytes: raw,
		},
		DBBytes:            dbBytes,
		RefsBytes:          refsBytes,
		ConfigBytes:        configBytes,
		DurableStoredBytes: stored + dbBytes + refsBytes + configBytes,
		DurableRawBytes:    raw + dbBytes + refsBytes + configBytes,
		Workspaces: UsageWorkspaces{
			Count: wsCount, LogicalBytes: wsLogical, DiskBytes: wsDisk,
		},
		MeasuredAt: time.Now().UTC().Format(time.RFC3339),
	}
	return u, nil
}

// EmitWorkspaceChanged 在工作区采集完成后上报一次 workspace_changed。
// 只用于 workspace_bytes 刷新；durable 数据要等 land/sync 事件。
func (r *Repo) EmitWorkspaceChanged(wsID string) {
	r.emit(events.WorkspaceChanged, "", wsID)
}

// EmitSyncPulled 在 sync_pull 推进 head 后上报。
func (r *Repo) EmitSyncPulled() { r.emit(events.SyncPulled, "", "") }

// EmitGCCompleted 在 GC/打包完成后上报。
func (r *Repo) EmitGCCompleted() { r.emit(events.GCCompleted, "", "") }

func (r *Repo) emit(event, versionID, wsID string) {
	if r == nil || r.eventsSocket == "" {
		return
	}
	head := ""
	if id, ok, err := r.DB.Head(); err == nil && ok {
		head = id.String()
	}
	events.EmitAsync(r.eventsSocket, events.Event{
		Event: event, Repo: filepath.Base(r.Root), Root: r.Root,
		Head: head, VersionID: versionID, WSID: wsID,
	})
}

func statSum(paths ...string) int64 {
	var total int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
	}
	return total
}

func walkLogicalBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
