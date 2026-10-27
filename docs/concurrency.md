# 并发模型与锁顺序

本文描述**代码里实际存在**的并发控制。旧版文档里的 `repo_registry` /
`repo_mutation` / `workspace` 三层锁并不存在，请以本文为准。

## 1. 锁与串行化机制

| 机制 | 位置 | 保护范围 |
|---|---|---|
| `Hub.mu` | `internal/hub/hub.go` | registry 读写、repo 缓存、打开/关闭 repo；打开期间持有，避免并发首次打开争抢 per-repo flock |
| per-repo flock | `internal/repolock` | daemon 生命周期内独占 `<repo>/.p37/lock`；CLI `gc` 用同一把锁拒绝在线 GC |
| `Repo.landMu` | `internal/repo/repo.go` | 整条 land 流水线、revert、line reset/delete/merge、`ws_land_into`、GC、pack；**这是 repo 级的粗粒度写串行化** |
| `Repo.landSem` | `internal/repo/repo.go` | 限制同时进入 land 的请求数；满时返回 `land_queue_full`。它只是准入限流，不提供正确性保证 |
| `workspace.Manager.wsLocks` | `internal/workspace/locks.go` | 按 `ws_id` 的读写锁：`Sync`/`Rebase`/`Drop`/`Apply*` 取写锁，`EffectiveTree` 取读锁；`ws_land_into` 在合并/校验/落盘期间持子+父写锁 |
| `Repo.syncMu` | `internal/repo/repo.go` | 同一 repo 的 `sync_prepare/push/fetch/pull/restore/prune` 互斥；并发返回 `sync_in_progress` |
| `meta.DB.wmu` | `internal/meta/db.go` | SQLite 写事务串行化；WAL 下读操作可并发 |
| `workspace.Manager.openMu` | `internal/workspace/workspace.go` | 工作区配额检查与名额预留，避免 open 的 TOCTOU |
| `workspace.Manager.unstableMu` | `internal/workspace/workspace.go` | `LastUnstable` 快照 |
| fs watcher goroutine | `internal/watch` | 所有调度在单 goroutine 推进；同一工作区同时最多一个在途采集 |

## 2. 实际顺序

```text
Hub.mu
  → per-repo flock（打开 repo 时）
    → Repo.landMu（写/整合/GC）
      → workspace wsLock(子) → wsLock(父)
        → meta.DB.wmu（SQLite 写事务）

workspace wsLock(单工作区)（Sync/Rebase/Drop）
  → meta.DB.wmu

Repo.syncMu（同步工具）
  → Repo.landMu（prepare 需要读稳定快照时）
    → meta.DB.wmu
```

规则：

- 不要在持有 `landMu` 时回调 `Hub` 或等待用户输入。
- 不要在持有 `Hub.mu` 时执行长时间的文件操作；`openLocked` 是例外，它需要保证
  同一 repo 的首次打开只发生一次。
- `landMu` 覆盖所有会推进 line head、删除版本/对象、或改变整合结果的操作：
  `ws_land`、`ws_land_into`、`version_revert`、`line_reset`、`line_delete`、
  `line_merge`、`gc`、`pack`。
- `wsLocks` 只在本进程内，保护「op-log ↔ 有效树 ↔ 工作区目录」三者的一致性：
  `Sync` 整体替换 op-log（同一事务推进 `ops_epoch` 并清缓存），`EffectiveTree`
  读 op 并写缓存，两者必须互斥，否则会把旧树写回新 epoch 的缓存。
- 同时持两把 ws 锁时，方向固定为**子 → 父**；工作区层级无环，不会死锁。
- 不要在同一 goroutine 里先持写锁再调用公开的 `Sync`/`EffectiveTree`（RWMutex
  不可重入）；持锁代码必须使用 `SyncLocked`/`EffectiveTreeLocked`/`ApplyTreeDeltaLocked`。
- `ws_sync` / `ws_diff` / `ws_rebase` / `ws_drop` 不取 `landMu`，但取各自工作区的
  ws 锁，因此不会与 land 的合并窗口交错。
- **没有按工作区的跨进程锁**；agent 直接写工作区文件不经过任何锁。`ws_land_into`
  用「写前逐路径内容校验 + 只写子改动路径」把这类并发的影响限制在最小范围，
  并在 `post_apply_mismatch` 里如实报告。

## 3. 跨进程

- daemon 打开 repo 后持有该 repo 的 flock，直到 `repo_unregister`、`repo_delete`
  或 daemon 退出。
- `p37 gc` 在入口尝试同一把 flock；daemon 持锁时返回非 0，`--force` 才跳过。
- 其他 CLI 写命令（`import`、`fsck`、`revert` 等）不会自动取这把锁，因此仍应
  在维护窗口停 daemon 后执行。
- 多仓库 daemon 中，不同 repo 的 `landMu` / `syncMu` / flock 互不影响；同一 repo
  的写操作串行。

## 4. 工作区状态与并发

`active` / `conflicted` / `rebasing` / `integrating` / `landed` / `merging` /
`reaping` / `broken`。

- `landed` 用 `landed_ts` 表达（state 仍存 active），`ws_info` / `ws_list` 对外
  显示 `landed`。
- `max_active` 只统计 `active/conflicted/rebasing`；`landed`、`integrating`、
  `hidden`、`broken` 不占额度，`broken` 在 `ws_quota.broken` 单独暴露。
- 隐藏 merge/preview 工作区 `hidden=1`，不占用户 `max_active`，单独受
  `max_hidden`（默认 32）限制，默认不出现在 `ws_list`。
- `ws_drop` 先释放元数据（配额立即回收），目录清理失败只返回
  `dir_cleanup_warning`；daemon 启动会清掉没有行引用的孤儿目录。
- `ws_reap` 不删除有 lease 的工作区；用户 active 工作区不自动回收；隐藏
  merge/preview TTL 24h；`landed` 可在 24h 后回收；显式 `state=broken` 可回收
  broken 工作区；`include_orphan_dirs=true` 顺带清理孤儿目录；`name_prefix`
  只过滤已符合回收条件的候选，不扩大回收范围。
- dry-run 返回 `candidates[]`（name/ops/dirty/age/reason/will_drop）与
  `skipped[]`（lease_held/not_old_enough），解释“为什么回收/为什么不动”。

## 5. 幂等

- `ws_open` / `ws_ensure` 用 `idempotency_result` 做持久化 claim：name claim
  按 `(name, parent)` 唯一，idem claim 按调用方 key 唯一；并发 / 多进程都安全。
- pending claim 租约 15 分钟；loser 最多等 30 秒，超时返回
  `ws_open_in_progress`（可重试）。daemon 启动会清理过期 claim。
- `ws_land` / `ws_land_into` 的 key 写 `idempotency_result`（30 天），同时按
  `ws_id` 保留 `landed_ws` 兜底；工作区已 drop 后仍能返回首次落地结果。
- 同一个 key 用于不同 `ws_id`/target 返回 `idempotency_conflict`。
- `line_merge` / `sync_push` 继续使用 7 天 TTL。
- 回收记录（tombstone）存 `idempotency_result` 的内部命名空间，默认 7 天，
  `sync_restore` 不恢复；跨 VM 后按 name/line 重新 `ws_ensure`。

## 6. 初始化锁

- `repo_init` / `p37 init` 先拿 root 级 `<path>/.p37.init.lock`（flock），
  再在 `.p37` 已存在时拿 daemon 同款 `.p37/lock`；并发初始化只会有一个创建，
  另一个等待后走幂等返回。
- 新仓库在 `<path>/.p37.tmp` 构建，rename 成 `.p37` 后再 fsync 父目录；
  失败/崩溃留下的 `.p37.tmp` 由下一次初始化在锁内清理。
- 已注册仓库若被 p37d 打开，CLI 会向上查找 `.p37/p37d.sock` 并把 init/repair
  路由到 daemon，避免和 daemon 的 `.p37/lock` 打架。
- `repo_register(rebind=true)` 会检查目标 repo 的活跃 workspace/lease；
  有活跃项时返回 `repo_busy`，不改 registry 指针。

## 7. 初始化幂等

- `repo_init` / `repo_register(init_if_missing)` 的 key 在 root registry 的
  `init_idempotency` 表中原子 claim：
  - 同 key + 同 `request_hash` + done → 返回 `result_json`；
  - 同 key + 不同 request → `idempotency_conflict`；
  - 同 key + in_progress（未过期）→ `idempotency_in_progress`；
  - failed/过期 lease 的同 request → 允许重试。
- 每个 repo 的 `.p37/init.json` 保留最近 32 个 key 的 requests map，作为
  root 索引丢失时的同 path 兜底；跨 path 冲突在索引丢失后降级并记录 warning。
- `p37 init` 默认不传 key；只有显式 `--idempotency-key` 才参与。strict
  `repo_register` 忽略 key。

## 8. 已知边界

- `repo_unregister` 不等待已经进入执行中的 MCP 调用结束；detach 前由上层确认
  应用空闲。
- `p37d --stdio` 在启动时快照 daemon 工具清单；daemon 升级后客户端应重连。
- `landMu` 只在本进程内有效，跨进程依靠 per-repo flock；不要假设 CLI 与 daemon
  之间有共享的 land 锁。
