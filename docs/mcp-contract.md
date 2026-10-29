# p37 MCP 契约（v0.4.6 / format_ver=2）

本文是 MCP 工具的参数、返回字段和错误码参考。工具实现以
`internal/mcpsrv` 的注册为准；当前注册 65 个工具。

## 统一信封

所有工具返回：

```json
{ "ok": true, "data": {}, "error": null }
```

失败时 `ok=false`、`isError=true`，`error` 为：

```json
{ "code": "ws_not_found", "message": "...", "detail": {} }
```

注意字段名是 **`detail`（单数）**，不是 `details`。`detail` 是可选字段；
不同错误码的结构不同。成功时 `data` 为对象、数组或 `null`，失败时 `data` 为
`null`。

返回体中的可选字段可能因 `omitempty` 被省略；调用方应按“字段可选”处理，不要
假设所有键都存在。

## 通用参数

- `repo`：仓库 id 或 name。多仓库模式必填；单仓库模式可省略。
- `ref`：`version:<id|前缀>` / `line:<name>` / `mainline`。
- `parent` / `target`：`{ "kind": "version|line|workspace|mainline", "id": "..." }`。
  旧字符串形式仍兼容。
- 版本 id 支持唯一前缀（至少 4 位）；歧义返回 `ambiguous_id`。

### 幂等参数

以下工具的 `idempotency_key` 会写入/读取持久化幂等记录：

- `ws_open`（同一 key 返回同一个 `ws_id`，即使该工作区后来已回收）
- `ws_ensure`（同上；同时按 `name+parent` 做原子 open-or-get）
- `ws_land`
- `ws_land_into`
- `line_merge`
- `sync_push`

同一个 key 被用于不同的 `ws_id`/target 时返回 `idempotency_conflict`，不会静默
返回首次结果。`version_revert` 接受 `idempotency_key` 并写入版本元数据，但当前
不做前置查重。幂等/回收记录是本地仓库数据，`sync_restore` 不恢复；跨 VM 恢复
后应按 name/line 重新 `ws_ensure`。

## 工具总览

| 分类 | 工具 |
|---|---|
| 仓库只读 | `repo_status` `repo_list` `repo_info` `repo_disk_usage` `repo_health` |
| 仓库管理 | `repo_create` `repo_delete` `repo_import` `repo_export` `repo_export_status` `repo_set_remote` |
| 应用卷生命周期 | `repo_init` `repo_register` `repo_unregister` `repo_scan` |
| 命名线 | `line_create` `line_list` `line_info` `line_delete` `line_protect` `line_reset` `line_log` `line_compare` `line_graph` `line_merge` |
| 工作区 | `ws_open` `ws_ensure` `ws_list` `ws_info` `ws_sync` `ws_diff` `ws_rebase` `ws_drop` `ws_quota` `ws_reap` `ws_disk_usage` `ws_lease` `lease_query` |
| 整合 | `land_preview` `ws_land` `ws_land_into` `land_history` |
| 冲突 | `conflict_list` `conflict_show` `conflict_resolve` `conflict_resolve_all` |
| 历史读取 | `version_log` `version_show` `version_diff` `version_tree` `version_file` `version_archive` `version_compare` `version_graph` `version_revert` `blame` |
| 同步 | `sync_status` `sync_prepare` `sync_list` `sync_show` `sync_push` `sync_fetch` `sync_pull` `sync_restore` `sync_prune` |

## 仓库只读

| 工具 | 参数 | 说明 |
|---|---|---|
| `repo_status` | `ws?` | 仓库摘要 + 全部 lease；带 `ws` 时附带该工作区状态和 lease 违规 |
| `repo_list` | — | 列出 registry 中的仓库；单仓库模式返回合成的 `default` |
| `repo_info` | `repo` | id/name/path/default_line/lines/workspace_count/remote；不可用时返回 registry-only 信息 |
| `repo_disk_usage` | `repo` | 按实际块数统计仓库磁盘占用 |

`repo_list` / `repo_info` 的 Entry 字段：

```json
{
  "id": "repo_0123456789abcdef0123456789abcdef",
  "name": "app-a",
  "path": "/srv/repos/app-a",
  "default_line": "main",
  "mounted": true,
  "available": true,
  "degraded": false,
  "degraded_msg": "可选"
}
```

这些字段只做轻量 stat/mount 探测；列表不会为了展示去打开 repo。
`mounted=false` 只表示该目录不是独立 mount point，不代表数据丢失；看
`available`/`degraded`。

`repo_info` / `repo_status` 还返回：

```json
{
  "p37_version": "0.4.6",
  "persistence": {
    "truth_source": "local_repo",
    "local": {"root": "/srv/repos/app", "available": true, "format_ver": 2},
    "remote": {"configured": true, "remote_key": "rk_...", "last_push_ts": 0,
               "remote_head": "", "last_package_id": ""},
    "durable": false,
    "durable_scope": "local_only"
  }
}
```

本地 DB+CAS 是在线语义真相源；`sync_push` 成功后才可能 `durable=true`。


## 仓库管理

| 工具 | 参数 | 说明 |
|---|---|---|
| `repo_create` | `name`, `path?`, `default_line?`, `remote_key?`, `remote_prefix?`, `remote?` | 创建空 v0 与默认 line。传 `path` 等价于 `repo_init(mode=empty)`；**不传 path 是 L2 scratch，不能用于应用卷** |
| `repo_delete` | `repo`, `force?`, `delete_remote?` | 有活跃工作区/租约时返回 `repo_busy`；挂载点返回 `repo_is_mountpoint` |
| `repo_import` | `repo`, `dir?`, `task?`, `force?` | 把目录导入空仓库作为 v0；未初始化的已注册仓库等价于 `repo_init(mode=import)` |
| `repo_export` | `repo`, `ref`, `dest_dir`, `async?` | 把 line/version 物化到目录；`async=true` 返回 `job_id` |
| `repo_export_status` | `job_id` | 查询异步导出任务；任务只在当前 daemon 进程内有效 |
| `repo_set_remote` | `repo`, `remote` | 更新 per-repo `[remote]`；凭证仍只从环境变量读 |

`repo_delete` 的 `delete_remote` 当前保留但未参与删除逻辑；不会删除远端包。
`repo_import` 的 `task` 当前只作为参数保留，不写 journal。

`repo_import.idempotency_key` 当前保留但未参与 root `init_idempotency` 表；
需要跨重启幂等请使用 `repo_init`。

`repo_import` 与 `repo_init(mode=import)` 共用同一个 v0 状态机：先采集目录并
构建完整对象树、flush，再比较 `currentTree` / `v0Tree`；相同只 repair，v0 为空
时直接导入，v0 非空且不同时要求 `force=true`。替换在同一个 SQLite 事务里完成
退休旧 v0/插入新 v0/更新 head 与 line，最后写 `refs/mainline`；任何 pre-commit
失败都会回滚，不会出现 “head 指向已删除版本”。`version_count>1`、非默认
line、活跃 workspace/lease 不允许 force。未初始化的已注册仓库等价于
`repo_init(mode=import)`（此时 `dir` 必须等于仓库路径）。

### repo_export

```json
repo_export(repo, ref, dest_dir, async=false)
→ { exported, ref, dest_dir, version_id, files, bytes, duration_ms }
```

- 不创建 workspace、不占 `ws_quota`、不出现在 `ws_list`，也不会被 fs watcher
  监听；导出目录里的构建产物不会进入 CAS/op/版本。
- `dest_dir` 必须是绝对路径、位于仓库目录之外。配置了 `[repo] export_root` 时
  还必须位于该目录之下（P36 模板默认 `/srv/repos/.p37/tmp/p34-deploy`）。
- `dest_dir` 必须不存在或为空；非空返回 `destination_not_empty`。
- 文件按分块流式写出，内存占用上界为单个 CAS 分块；`files` 统计普通文件与
  symlink，不统计目录；`bytes` 是普通文件字节数加 symlink 目标字符串长度。
- 保留 symlink、可执行位；不保留 mtime/uid/gid/xattr/硬链接。
- 导出目录由调用方清理；p37 不自动删除。
- `async=true` 立即返回 `{job_id, state:"running", dest_dir}`，用
  `repo_export_status(job_id)` 轮询；daemon 重启后任务失效。

## 应用卷生命周期

P36 把「每应用一个持久卷」挂到 guest 的 `/srv/repos/<app>`。p37d 看到的仍是
本地 ext4；不需要 NFS 专用模式。卷在运行中 attach/detach 时使用：

```text
attach: 宿主热加 qcow2 → guest mount /srv/repos/<app>
        → repo_init(mode=empty)（P36 旧镜像可继续用 p37 init --if-missing）
        → repo_register 只在仓库已完整时成功
detach: repo_unregister → guest umount → 宿主 remove-device
删除卷: repo_unregister → guest umount → 宿主删卷（不要用 repo_delete）
```

所有 registry 写操作都由运行中的 p37d 执行（Hub mutex + SQLite 事务）。CLI 在
daemon socket 可连接时通过 MCP 路由 init/register/unregister/scan。

### repo_init

在已挂载的持久目录上**原子**初始化一个完整仓库。这是应用卷的推荐入口；
P36 attach、P34 首次打开和“重试初始化”都走它。

```json
{
  "repo": "p34-59",
  "path": "/srv/repos/p34-59",
  "default_line": "main",
  "mode": "empty",
  "force": false,
  "idempotency_key": "p34-init-p34-59"
}
```

返回：

```json
{
  "repo": {"name":"p34-59","path":"/srv/repos/p34-59","default_line":"main",
           "mainline_head":"<v0>","version_count":1},
  "line": {"name":"main","head":"<v0>"},
  "version": {"id":"<v0>"},
  "created": true
}
```

- `worktree` = repo root 下排除 `.p37*`、`workspaces`、`lost+found` 的用户文件。
- `mode=empty`：只允许空 worktree；有用户文件返回 `repo_not_empty`，
  `detail.next_action=repo_init_import`，不改 DB、不动文件。
- `mode=repair`：只修复已有 DB 的 head/line/refs；有用户文件时同样返回
  `repo_not_empty`，不允许静默 repair。没有 DB 返回 `repo_not_initialized`。
- `mode=import` 的状态机：
  - 没有 DB / 没有 v0：完整导入；
  - `currentTree == v0Tree`：只 repair 元数据，`repaired=true, imported=false`；
  - v0 为空树、currentTree 非空：直接导入（不需要 force）；
  - v0 非空、currentTree 不同或为空：`force=false` 返回 `repo_not_empty`
    + `next_action=repo_init_import_force`；`force=true` 才替换 v0；
  - `version_count>1`、存在非默认 line：`repo_history_not_empty`，force 也不行；
  - 有活跃 workspace/lease：`repo_busy`，force 也不行；
  - 多个 root v0：`repo_db_corrupt` + `backup_path`，force 也不行。
- 已完整初始化时：
  - refs 对齐：`mode=empty/repair` 幂等返回；
  - refs 落后（`refs_match=false`）：`mode=empty/repair` 做 refs-only repair，
    返回 `repaired=true`，不改版本/line；
  - `mode=import` 仍会按上面的 same/different 判断（same 只 repair，different
    需要 force）。
- 新仓库在 `<path>/.p37.tmp` 里完整建好后 `rename` 成 `.p37`；失败回滚，
  不留下半套 `.p37`。残留 `.p37.tmp` 会在下次初始化时清理。
- 并发初始化用 root 级文件锁串行化：一个创建，另一个返回已初始化。
- 幂等键：
  - root 级 `init_idempotency` 表（`<root>/.p37/registry.db`）记录
    `key/path/mode/name/default_line/force/request_hash/status/result_json`；
  - 同 key 同 request 返回原结果；同 key 不同 request 返回
    `idempotency_conflict`；同 key 正在处理返回 `idempotency_in_progress`
    （`retryable=true`）；
  - 每个 repo 的 `.p37/init.json` 保留最近 32 个 key 的 `requests` map，
    作为 root 索引丢失时的同 path 兜底；跨 path 冲突在 root 索引丢失后降级；
  - root 索引不可用/CLI 本地无 daemon 时记录 warning，按 path 级幂等。
- 生产环境要求 `mount_point=true`；开发/CI 可用 CLI
  `--allow-nonpersistent` 或 `P37_ALLOW_NONPERSISTENT=1` 放宽，MCP 不暴露该字段。
- 路径非法/不在 root/符号链接逃逸/非持久卷返回 `invalid_path`，
  `detail.code="repo_path_invalid"`、`detail.reason` 说明具体原因。

### repo_register

```json
{
  "repo": "app-a",
  "path": "/srv/repos/app-a",
  "default_line": "main",
  "init_if_missing": false,
  "force_rename": false,
  "rebind": false
}
```

- `path` 必须是 root 下的绝对路径，且不能是 root 自身、root/.p37、symlink 或
  逃逸路径。
- 0.4.3 起 **只注册完整仓库**。只读校验 repo 行、`mainline_head`、`version_count`、
  default line、`refs/mainline`、DB 可读与 root 一致；任一不满足：
  - 没有 `.p37/p37.db` → `repo_not_initialized`，
    `detail.next_action="repo_init"`；
  - 有 DB 但缺 line/version/refs → `repo_not_initialized` 或
    `repo_db_corrupt`，`detail.next_action="repo_init_repair"`。
- 同 path 已注册且完整时幂等返回已有 Entry。
- `init_if_missing=true` **保留但已 deprecated**：内部等价于
  `repo_init(mode=empty)`，失败完整回滚；新调用方应直接用 `repo_init`。
- `idempotency_key` 只在 `init_if_missing=true` 且确实触发初始化时传给
  `repo_init`；仓库已完整且 refs 对齐时按 strict register 处理，key 被忽略，
  不会因 key 冲突失败。`repo_init` 的 root 级 key 规则见上节。
- strict register 遇到 `initialized=true, refs_match=false` 返回
  `repo_not_initialized` + `detail.reason=refs_mismatch` +
  `next_action=repo_init_repair`，不注册一个 `/ready` 不通过的仓库；
  `init_if_missing=true` 会先做 refs-only repair。
- 同名但已注册在**不同路径**：默认返回 `repo_path_mismatch`，不会静默复用旧
  路径；只有显式 `rebind=true` 才把 registry 指针改到 `path`。`rebind` 只改控制
  面，不搬数据；目标必须已有完整 `p37.db`（缺 head/line/refs 时返回
  `repo_not_initialized`），有 `.p37/repo.json` 时 repo id 必须一致。
- `rebind=true` 会检查目标 repo 的活跃 workspace/lease；有则返回 `repo_busy`，
  `detail` 带 `workspaces`/`leases` 与 `next_action="stop_workspaces"`，registry
  不变。
- 有 `.p37/p37.db` 但没有 `.p37/repo.json`：生成 `repo_<32hex>` 身份并落盘。
- 已有 repo.json 时以其中的 id/name 为准；name 冲突返回 `repo_name_mismatch`，
  只有显式 `force_rename=true` 才会改 repo.json 的 name。
- 不移动目录、不复制数据。

### repo_unregister

```json
{ "repo": "app-a", "force": false }
```

- 按 id 或 name 查 repo；不存在时幂等成功并返回 `already_unregistered=true`。
- 关闭顺序：卸载该 repo 的 overlay workspace → 关闭 SQLite → 释放 per-repo
  flock → 写 registry tombstone（`repo_list` 不再返回）。
- 不删除 `.p37`、`workspaces` 或 repo 数据。
- 有 active/conflicted/rebasing/integrating workspace 或 lease 且 `force=false`
  → `repo_busy`，`detail` 带 `workspaces`/`leases`。
- `force=true` 会卸载 workspace 后注销，但仍不删数据。
- 注销是 tombstone：daemon 重启或 `repo_scan` 不会自动复活；重新挂载后用
  `repo_register` 显式恢复（id/name 保持不变）。
- 失败时 registry 行保持可见，可直接重试。

### repo_scan

```json
{ "reconcile": true, "prune_missing": false, "dry_run": false, "repair": false }
```

- `repair=true` 时对检测到的半初始化仓库执行 `repo_init(mode=repair)`；
  默认 false，daemon 启动 scan 保持非破坏性。返回 `uninitialized[]` 与
  `repaired[]`。
- 只扫 root 的直接子目录；跳过隐藏目录、非目录、symlink。
- 读取每个子目录的 `.p37/repo.json`；不打开 repo、不初始化、不做重操作。
- registry 为空或非空都能工作；未注册但 repo.json 有效的目录会加入 registry。
- 已注册但 path 不存在：
  - `prune_missing=true` → 删除 registry 行（不删数据）；
  - 否则标记 degraded。
- 已注册但 repo.json 缺失 → 标记 degraded，不删除 registry 行或数据。
- 已注销（tombstone）的 repo 不会自动加回；`skipped.reason=unregistered`。
- `dry_run=true` 或 `reconcile=false` 只返回计划，不写 registry。

返回：

```json
{
  "added": [],
  "removed": [],
  "degraded": [],
  "skipped": [{ "path": "/srv/repos/app-b", "reason": "unregistered" }],
  "reconciled": true,
  "dry_run": false
}
```

## 命名线

| 工具 | 参数 | 说明 |
|---|---|---|
| `line_create` | `name`, `from?`(默认 mainline), `protected?` | 创建命名线；同名幂等返回已有 line |
| `line_list` | — | 列出全部命名线 |
| `line_info` | `name` | 详情（含相对 default line 的 ahead/behind） |
| `line_delete` | `name`, `force?` | 删除线引用，不删版本；有活跃工作区返回 `ws_busy` |
| `line_protect` | `name`, `protected`, `force?` | 设置/取消保护 |
| `line_reset` | `name`, `version`, `force?`, `reason?`, `expected_head?` | 移动 head；默认只允许祖先 |
| `line_log` | `name`, `limit?`, `before_ts?`, `before_id?` | 线祖先历史；游标分页 |
| `line_compare` | `a`, `b` | ahead/behind/merge_base + `a_files`/`b_files` |
| `line_graph` | `lines?`, `limit?` | DAG 节点/边 |
| `line_merge` | `source`, `target`, `preview?`, `no_ff?`, `merge_id?`, `expected_head?`, `expected_source_head?`, `task?`, `idempotency_key?` | 合并 source 到 target |

### line_merge 冲突流程

```text
line_merge(preview=true)  → merge_id + conflicts + target_head/base_version
conflict_list(ws_id=merge_id)
conflict_resolve(key, resolution)
conflict_resolve_all(ws_id=merge_id, policy)
line_merge(source, target, merge_id=merge_id, preview=false, expected_head=...)
```

merge preview workspace 隐藏，TTL 24h；过期返回 `merge_expired`。默认
fast-forward；`no_ff=true` 产生双亲 merge 版本。

## 工作区

| 工具 | 参数 | 说明 |
|---|---|---|
| `ws_open` | `parent?`, `name?`, `task?`/`task_desc?`, `leases?`, `materialize?` | 打开并物化工作区；返回 `ws_id/dir/base_line/base_version/base_tree/ops_epoch/materialize/state` |
| `ws_list` | `include_hidden?` | 默认不含隐藏 merge/preview |
| `ws_info` | `ws_id` | base_line/base_version/base_tree/effective_tree/ops_epoch/parent_tree/parent/dirty/ops/conflicts/disk/state/base_stale |
| `ws_sync` | `ws_id` | 采集磁盘变更；见下方返回字段 |
| `ws_diff` | `ws_id` | 文件级 A/M/D/R/C；已 land 返回 `ok=true,state=already_landed,items=[]` |
| `ws_rebase` | `ws_id`, `to?` | `to` = version/line/workspace；默认父 |
| `ws_quota` | — | 配额用量；见下方 |
| `ws_drop` | `ws_id`, `cascade?`, `keep_dir?` | 幂等；`cascade=true` 把子工作区重挂到祖父，不是递归删除 |
| `ws_lease` | `ws_id`, `add?`, `release?`, `mode?` | 登记/解除路径租约；`mode` = shared/exclusive |
| `lease_query` | `paths?` | 查询若干路径上的租约 |
| `ws_reap` | `older_than?`, `state?`, `name_prefix?`, `include_orphan_dirs?`, `dry_run?` | 默认 dry-run；返回 `candidates[]/skipped[]`，有 lease/active 不删；`name_prefix` 只过滤 eligible 候选 |
| `ws_disk_usage` | — | 按工作区统计磁盘占用 |

### revision 字段（v0.4.2 起）

- `base_tree`：子工作区创建时冻结的不可变基准树。**workspace 父的子工作区必须
  用它做合并坐标**；`base_version` 是父的 line 基线，不能替代。
- `effective_tree`：当前 op-log 对应的树（只读计算，不触发采集）。
- `ops_epoch`：op-log 的单调版本号；内容没变时不会被周期扫描刷新，可以直接
  当乐观锁令牌。
- `parent_tree`：仅子工作区有，父工作区的当前有效树（不触发采集；需要「磁盘
  最新」时先 `ws_sync(parent)` 或直接用 `land_preview` 的 `parent_tree_before`）。
- `parent_tree_error`：父工作区有未决冲突/不可读时的原因，此时 `parent_tree`
  为空。

### ws_info / ws_sync / land_preview 对已回收工作区

- 已落地并被回收（`reason=landed_and_dropped`）：返回 `ok=true`，
  `state="already_landed"`、`already_landed=true`，带 `version_id/line/line_head`。
- 显式 drop / 过期回收：返回 `ws_gone` 错误，`detail` 带
  `repo/name/ws_id/reason/dropped_at/line/line_head`。
- 完全未知的 id：仍是 `ws_not_found`，`detail.reason=never_opened_or_tombstone_expired`。
- 存储/挂载异常：`ws_broken` / `ws_not_mounted`，不会伪装成 gone。

### ws_list tombstone 项

```json
{
  "ws_id": "ws_...", "name": "p34s-sess-42", "state": "tombstone",
  "reason": "landed_and_dropped", "dropped_at": 1792831200000,
  "line": "p34/session/s1", "line_head": "v...",
  "landed_version": "v...", "target_kind": "line",
  "target_id": "p34/session/s1", "tombstone_expires_at": 1793436000000
}
```

`reason` 枚举：`landed_and_dropped | expired | abandoned | unknown`。
默认保留 7 天（`[workspace] tombstone_ttl = "168h"`）；`include_recently_dropped`
默认只看最近 1 小时。

### ws_sync 返回

```json
{
  "captured_ops": 3,
  "new_ops": 2,
  "ops_count": 3,
  "files_touched": 2,
  "conflicts": 0,
  "dirty": true,
  "skipped_unstable": []
}
```

`captured_ops` / `ops_count` 是当前工作区 op 总数，`new_ops` 是本次新增数；
`skipped_unstable` 是连续多轮仍在写入、本轮被跳过的路径。

### ws_quota 返回

```json
{
  "max_active": 8,
  "active_used": 3,
  "active_available": 5,
  "hidden_limit": 32,
  "hidden_used": 0,
  "max_depth": 32,
  "max_fanout": 16,
  "landed": 0,
  "broken": 0,
  "workspaces": [
    {"ws_id": "ws_...", "name": "root-1", "state": "active", "dirty": true,
     "ops": 3, "age_ms": 120000, "leases": 0, "counts_to_quota": true}
  ]
}
```

`max_active` 只统计 `active/conflicted/rebasing`；`landed`、`integrating`、
`hidden`、`broken` 不占额度。`hidden_limit` 是隐藏 merge/preview 的上限
（默认 32）。`workspaces[]` 逐项给出 `ws_id/name/state/ops/dirty/age_ms/leases`，
便于上层提示“是谁占着配额”。

### ws_drop 返回

```json
{
  "dropped": ["ws_x"],
  "reparented": [],
  "conflicts": [],
  "dirty_ops_discarded": 2,
  "dir_cleanup_warning": "可选"
}
```

元数据已删除但目录清理失败时返回 `ok=true` 并带 `dir_cleanup_warning`；
`cascade=true` 只把子工作区重挂到祖父，不释放它们的额度。

## 冲突

| 工具 | 参数 | 说明 |
|---|---|---|
| `conflict_list` | `ws_id`, `include_resolved?` | 列出冲突 |
| `conflict_show` | `key` | 冲突详情：两侧任务描述、hunks、候选解 |
| `conflict_resolve` | `key`, `resolution` | `resolution.kind` = take_lower/take_upper/both/custom；custom 必须带 `content` |
| `conflict_resolve_all` | `ws_id`, `policy` | `policy` = prefer_lower/prefer_upper/abort |

同一 key 重复写入视为成功；`abort` 只返回 `{resolved:0, aborted:true}`，不写
resolution。

## 整合

| 工具 | 参数 | 说明 |
|---|---|---|
| `land_preview` | `ws_id`, `target?`, `fold?`, `expected_head?`, `expected_ws_tree?`, `expected_parent_tree?`, `expected_child_tree?`, `expected_base_tree?`, `allowed_paths?`, `allowed_prefixes?`, `merge_policy?` | 纯只读预演；workspace 目标会列出版本坐标与路径分类；冲突会持久化供 `conflict_resolve` |
| `ws_land` | `ws_id`, `target?`, `fold?`, `drop_workspace?`, `expected_head?`, `expected_ws_tree?`, `expected_ws_ops_epoch?`, `expected_parent_tree?`, `preview_id?`, `idempotency_key?`, `task?` | 落地到 line 或父工作区 |
| `ws_land_into` | `ws_id`, `parent_ws_id`, `expected_parent_tree?`, `expected_child_tree?`, `expected_base_tree?`, `allowed_paths?`, `allowed_prefixes?`, `merge_policy?`(`auto`\|`strict`), `preview?`, `preview_id?`, `idempotency_key?` | 子工作区并入直接父（三方合并） |
| `land_history` | `ws_id?`, `limit?` | 读取最近 30 天 land 审计记录 |

`fold` = `self`（默认）或 `stack`。`target` 省略时落到 default line；
`target.kind=workspace` 时等价于 `ws_land_into`。

**合并语义（v0.4.2 起）**：以 `child.base_tree` 为 base、当前父有效树为
parent、当前子有效树为 child 做三方重放。只把 `child` 相对 base 的真实改动
写进父目录；父在子运行期间改过、子没改的路径原样保留。同一路径双方都改时：
`merge_policy=auto`（默认）按 hunk 自动合并并在 `auto_merged_paths` 里报告，
放不下则冲突；`strict` 直接出冲突、不自动选边。

### 整合返回

```json
{
  "ops_count": 3,
  "files": [],
  "can_land": true,
  "auto_merged": 0,
  "target_head": "...",
  "ws_tree": "...",
  "ws_ops_epoch": 7,
  "preview_id": "...",
  "already_landed": false,
  "conflicts": [],

  "base_tree": "子工作区创建时冻结的基准树",
  "child_tree": "本次 land 的子有效树",
  "parent_tree_before": "合并前父有效树（= preview 的 target_head）",
  "parent_tree_after": "落盘并重新采集后的父有效树",
  "applied_paths": ["src/b.ts"],
  "child_only_changed_paths": ["src/b.ts"],
  "parent_only_changed_paths": ["tests/x.test.ts"],
  "both_changed_paths": ["src/a.ts"],
  "auto_merged_paths": ["src/a.ts"],
  "conflict_paths": [],
  "out_of_scope_paths": [],
  "paths": [
    {"path":"src/a.ts","state":"both_auto_merged",
     "base":"…","parent":"…","child":"…","result":"…"}
  ],
  "post_apply_mismatch": [],

  "version_id": "可选",
  "state": "可选",
  "warnings": []
}
```

`paths[].state` ∈ `child_only|parent_only|both_changed|both_auto_merged|conflict`。
`parent_only_changed_paths` 就是「子会话没改、但父工作区已经变化」的清单；
`applied_paths` 是结果树相对父树的差异路径（preview=计划写入，land 成功=实际
写入，冲突时仅供定位），保证是 `child_only + both_*` 的子集。

冲突时 `can_land=false`、`state="conflict"`，冲突挂在被 land 的工作区上；
`preview_id` 等于该工作区 id（历史 `mg_*` preview 仍接受）。冲突解决后带
最新 `expected_parent_tree` 重试，不要每次新建工作区。

### 会话与子任务

```text
line_create("p34/session/<id>", from="line:<app>")
ws = ws_open(parent={kind:"line", id:"p34/session/<id>"})
... 改文件 ...
ws_sync(ws)
p = land_preview(ws, target={kind:"line", id:"p34/session/<id>"})
ws_land(ws, target={kind:"line", id:"p34/session/<id>"},
        expected_head=p.target_head, expected_ws_tree=p.ws_tree,
        drop_workspace=true, idempotency_key=...)
```

```text
child = ws_open(parent={kind:"workspace", id:parent_ws})
# 记录 child.base_tree 与 parent_ws；它们是本次任务的不可变血缘坐标。
r = ws_land_into(child, parent_ws)           # 默认 auto
if r.state == "conflict" or !r.can_land:
    conflict_list(ws_id=child)
    conflict_resolve_all(ws_id=child, policy="prefer_upper")
    r = ws_land_into(child, parent_ws,
                     expected_parent_tree=r.parent_tree_before,
                     preview_id=r.preview_id,
                     idempotency_key=<同一个 key>)
```

推荐的生产调用（先 preview 拿父树令牌，再 land；工作卡范围交给 P37 兜底）：

```text
p = land_preview(child, target={kind:"workspace", id:parent_ws},
                 allowed_prefixes=<工作卡目录>)
r = ws_land_into(child, parent_ws,
                 expected_parent_tree=p.parent_tree_before,
                 expected_base_tree=child_base_tree,
                 allowed_prefixes=<工作卡目录>,
                 merge_policy="strict",
                 idempotency_key=<child task id>)
# 遇到 target_moved / parent_moved：重读 p 后重试；遇到 path_not_allowed：
# 不要重试，直接把 out_of_scope_paths 交给主管裁决。
```

父工作区有冲突/未落地的子工作区时，`ws_land(fold=self)` 返回
`ws_has_active_children`，`detail.blocked_by_children` 形如
`[{"ws_id":"ws_child","state":"conflicted"}]`。此时必须显式解决冲突或
`ws_drop(child)`，不要无限重试。

## 启动自检

`repo_health` 是 P36 `/ready` 的廉价入口，也是 P34 的聚合只读视图：

```json
repo_health { "repo": "my-app" }
repo_health { "path": "/srv/repos/my-app", "with_quota": false }
repo_health { "repo": "my-app", "deep": true }
```

参数：

- `repo`：已注册仓库的 id/name。`path` 优先于 `repo`，用于 attach 失败、
  registry 还没有行时直接探测挂载点。
- `with_quota=false`：P36 `/ready` 用。不打开 repo、不跑 EnsureSchema/migrate，
  只做 repo.json + 只读 SQLite + 短 TTL 缓存。
- `deep=true`：doctor/attach 用，额外校验 head（或唯一 root v0）的对象树存在。

未初始化时仍返回 `ok=true`（HTTP 200），不能当成 p37d down：

```json
{
  "initialized": false,
  "reason": "mainline_head_missing",
  "next_action": "repo_init_repair",
  "path": "/srv/repos/p34-59",
  "default_line": "main",
  "default_line_ready": false,
  "mainline_head": "0000...",
  "version_count": 0,
  "data_ok": false,
  "repo_db_ok": true,
  "repo_json_ok": false,
  "root_ok": true,
  "refs_ok": false,
  "refs_match": false,
  "user_files": 0,
  "root_count": 1,
  "root_tree_empty": true,
  "history_not_empty": false,
  "active_workspaces": 0,
  "leases": 0,
  "format_ver": 2,
  "p37_version": "0.4.6",
  "warnings": ["mainline_head_missing"]
}
```

已初始化时在原有字段上增加：

```json
{
  "initialized": true,
  "reason": "",
  "next_action": "",
  "default_line_ready": true,
  "refs_ok": true,
  "refs_match": true,
  "data_ok": true,
  "repo_db_ok": true,
  "p37_version": "0.4.6",
  "quota": {"max_active":8,"active_used":2,"active_available":6,
            "hidden_limit":32,"hidden_used":0,"max_depth":32,"max_fanout":16,
            "landed":0,"broken":0,"workspaces":[]},
  "workspaces": {"total":2,"active":2,"conflicted":0,"landed":0,
                 "broken":0,"hidden":0,"tombstones":1},
  "lines": {"count":2,"default_line":"main","heads":{"main":"v..."}},
  "persistence": {"durable": false, "durable_scope": "local_only",
                  "pending_packages": 0, "ahead": 0, "behind": 0, "diverged": false},
  "last_land_ts": 1792831200000,
  "warnings": []
}
```

判定规则：repo 行存在、`root` 与 path 一致、`mainline_head` 非零、
`version_count>0`、default line 存在且 head 指向存在的 version、
`refs/mainline` 存在且指向存在的 version、DB 可读、format_ver 匹配。任一不满足
`initialized=false`。

`next_action` 的口径：

- 缺 DB / 空 worktree：`repo_init`；
- 数据完整、只缺 refs/root（`data_ok=true`）：`repo_init_repair`，即使有用户文件
  也允许（只修元数据，不导入）；
- v0 为空 + 有用户文件：`repo_init_import`；
- v0 非空 + 有用户文件：`repo_init_import`；`repo_init(mode=import, force=false)`
  会先比较 currentTree/v0Tree，相同则 repair，不同则返回
  `repo_not_empty + next_action=repo_init_import_force`；health 不在线做完整
  worktree 建树，避免 `/ready` 变重；
- 有历史版本/非默认 line：health 给 `repo_init_repair`（修元数据安全），
  `repo_init(mode=import)` 会返回 `repo_history_not_empty`；
- 有活跃 workspace/lease：`stop_workspaces`。

`refs_ok=true` 表示 ref 存在且指向存在的 version；`refs_match=true` 表示 ref
等于 DB head。0.4.5 起 default line 的 land/merge/reset/revert/sync 都会同步
`refs/mainline`，P36 `/ready` 应要求两者都为 true。

refs 落后（指向仍然存在的旧 ancestor）时，`repo_health` 返回：

```json
{
  "initialized": true,
  "reason": "refs_mismatch",
  "next_action": "repo_init_repair",
  "refs_ok": true,
  "refs_match": false,
  "data_ok": true,
  "warnings": ["refs/mainline does not match DB head; repo_init(mode=repair) will align it"]
}
```

`initialized` 仍为 true（数据和 line 完整，仓库可用），但 P36 `/ready` 不能放行；
调用 `repo_init(mode=repair)` / `p37 init --if-missing` /
`repo_register(init_if_missing=true)` 做 refs-only repair。有活跃 workspace/lease
时返回 `repo_busy` + `next_action=stop_workspaces`。

## 历史读取

| 工具 | 参数 | 说明 |
|---|---|---|
| `version_log` | `ref?`(默认 mainline), `limit?`(1–500，默认 50), `before_ts?`, `before_id?` | 版本摘要列表 |
| `version_show` | `version_id`(空=head，支持前缀) | 版本详情，含 `parents[]` |
| `version_diff` | `a`, `b?`, `parent_index?`(默认 0), `path?`, `stat_only?` | B 省略时相对指定 parent |
| `version_tree` | `ref`, `path?`, `recursive?`, `limit?` | 目录树 items + truncated |
| `version_file` | `ref`, `path`, `max_bytes?` | base64 + binary/truncated/mode |
| `version_archive` | `ref`, `path?`, `format?`(tar.gz/zip) | VM 内临时文件，1h 过期 |
| `version_compare` | `a`, `b` | 两个 ref 的 ahead/behind |
| `version_graph` | `lines?`, `limit?` | `line_graph` 的别名 |
| `version_revert` | `version_id`, `target_line?`, `expected_head?`, `idempotency_key?` | 产生反向版本 |
| `blame` | `path`, `range?`([from,to]), `version_id?`, `granularity?`(line/hunk) | 行/hunk 归因 |

`version_file.max_bytes` 默认 4 MiB、最大 8 MiB（base64 膨胀后仍低于 16 MiB
响应上限）；更大内容用 `version_archive`。实现只读取前 `max_bytes`，不会把
整文件读进内存。

`version_archive` 返回 `{path,size,sha256,expires_at,format}`；临时文件在
`<repo>/.p37/tmp/archive/`，1 小时后可被清理。

## 同步

| 工具 | 参数 | 说明 |
|---|---|---|
| `sync_status` | — | local/remote head、pending、ahead/behind、remote_key/prefix |
| `sync_prepare` | `force?`, `full?` | 只生成到本地 outbox；`force` 忽略阈值，`full` 强制 checkpoint |
| `sync_list` | — | 查看暂存包 |
| `sync_show` | `id?`, `verify?` | 查看清单；`verify=true` 校验分片与对象 |
| `sync_push` | `all?`, `full?`, `lines?`, `expected_remote_generation?`, `idempotency_key?`, `async?` | 显式推送；成功 `durable=true` |
| `sync_fetch` | — | 下载对象/refs，不移动 head |
| `sync_pull` | — | 下载并快进 line；分叉保留本地 |
| `sync_restore` | `repo`, `remote_prefix`, `name?`, `remote?`, `force?` | 从远端前缀恢复（可自建 repo） |
| `sync_prune` | `dry_run?`, `remote?`, `local?` | 回收远端旧包 / 本地已上传包 |

`sync_push` 的提交点是 `refs/lines.json` 的条件更新；部分上传的包是内容寻址
的，可安全重试。`async=true` 只返回 `{durable:false, async:true}` 并在后台
推送；删 VM 前必须使用同步模式并确认 `durable=true`。`all` / `lines` 目前
保留但未参与包选择；当前实现按 line 可达版本生成包。

同一 repo 的 `sync_prepare/push/fetch/pull/restore/prune` 互斥；并发返回
`sync_in_progress`。`expected_remote_generation` 不匹配返回
`sync_remote_moved`。

## 错误码

以下为 MCP 层实际会返回的错误码：

```text
repo_required repo_not_found repo_exists repo_busy repo_not_initialized
repo_not_empty repo_history_not_empty repo_db_corrupt
idempotency_in_progress
repo_name_mismatch repo_path_exists repo_is_mountpoint
registry_busy
line_not_found line_exists line_protected
target_invalid target_moved source_moved target_not_ancestor
parent_moved path_not_allowed ws_unstable land_invariant_violation
merge_base_ambiguous merge_expired preview_expired
ws_not_found ws_gone ws_not_mounted ws_broken ws_busy ws_has_active_children
repo_path_mismatch
ws_cleanup_warning ws_exists quota_exceeded ws_open_in_progress idempotency_conflict
not_dirty conflict land_queue_full invalid_path
destination_not_empty path_not_found
overlay_unavailable path_case_conflict
version_not_found ambiguous_id
sync_not_configured sync_in_progress sync_remote_moved
corrupt_object file_too_large
invalid_argument internal not_found
```

补充：

- `repo_not_empty`：`repo_init(mode=empty)` 遇到用户文件（排除
  `lost+found/workspaces/.p37*`）或 `repo_import` 遇到已有版本。`detail` 带
  `path`、`user_files_count`、`retryable=false`、`next_action=repo_init_import`。
- `repo_db_corrupt`：DB 可打开但结构不可恢复（多个 root/no unique head/
  head tree 缺失/format 不匹配），或 `.p37` 存在但 DB 缺失且目录非空。
  `detail` 带 `path`、`backup_path`、`retryable=false`、
  `next_action=repo_init_repair`。0.4.5 不自动重建，先备份再要求显式修复。
- `repo_history_not_empty`：`repo_init(mode=import)` / `repo_import` 发现
  `version_count>1` 或存在非默认 line，不能覆盖 v0；`detail` 带
  `version_count`/`lines`，`next_action=repo_init_repair`。force 也无效。
- `idempotency_in_progress`：同一个 init key 正在被另一个请求处理；
  `detail.idempotency_key`，`retryable=true`，`retry_after_ms=1000`。
- `repo_not_initialized`：`detail.reason` 至少是
  `repo_db_missing_or_corrupt`/`mainline_head_missing`/`default_line_missing`/
  `refs_missing_or_invalid`，`detail.next_action` 是 `repo_init` 或
  `repo_init_repair`；`retryable=false`。
- `invalid_path` 在初始化场景的 `detail.code="repo_path_invalid"`，
  `detail.reason` ∈ `outside_root|not_mounted|symlink_escape|not_persistent`。
- `repo_busy` 在 rebind/import 场景的 `detail.next_action="stop_workspaces"`；
  初始化锁冲突是 `retryable=true`、`step="lock"`。
- `daemon_running` 是 CLI 的本地错误文本，不由 MCP 返回。
- `already_landed`、`already_merged`、`already_dropped`、`already_deleted`、
  `already_unregistered` 是返回体里的布尔字段，不是错误码。
- `repo_busy` 的 `detail` 带 `workspaces`/`leases`。
- `repo_path_mismatch` 表示同名 repo 已在旧路径；`detail` 带
  `existing_path/requested_path/reason`。迁移时先 unregister 旧路径，或显式
  `repo_register(rebind=true)`（只改 registry，不搬数据）。
- `quota_exceeded` 现在按 scope 给出 `retryable/retry_after_ms/next_action`：
  `workspace_active` → 可重试、`retry_or_reclaim`；
  `workspace_hidden` → 可重试、`ws_reap`；
  `workspace_depth/fanout` → 不可重试、`restructure`。
- `ws_diff` 对已落地/已回收工作区返回 `ok=true`（`already_landed`，空 items）；
  对 abandoned/expired 返回 `ws_gone`。
- `quota_exceeded` 的 `detail` 带 `scope/used/limit`，`scope` ∈
  `workspace_active|workspace_hidden|workspace_depth|workspace_fanout`。
- `ws_has_active_children` 的 `detail` 带 `ws_id` 和
  `blocked_by_children`。
- `ws_drop` 的目录清理失败不返回错误，而是在成功结果里带
  `dir_cleanup_warning`；land 路径把同类问题放在 `warnings[]`。
- 所有 `ws_*` / `repo_*` / `line_*` 错误的 `detail` 至少带 `repo`、标识符
  （`ws_id` 或 `name`）和 `reason`；需要时附 `line/line_head`。
- `ws_gone` 的 `detail.legacy_code="ws_not_found"`，便于旧调用方兜底。
- `ws_open_in_progress` 表示并发创建正在物化，`detail.retry_after_ms` 可重试。
- `already_landed` / `already_dropped` 是成功结果里的布尔字段，不是错误码。
- `ws_land` / `ws_land_into` 返回 `durable=false` 与
  `durability.local_committed=true`：本地真相源已提交，但跨 VM 仍需
  `sync_push`（`durable=true`）。
- `target_moved` 的 `detail` 带 `field/expected/current`（字段可能是
  `parent_tree` / `child_tree` / `base_tree`）：调用方重读最新 revision 后重试。
- `parent_moved` 是「读树之后、写盘之前」的最终校验失败，`detail.paths` 列出
  被并发改动的路径；P37 一个字节都没写，重试是安全的。
- `path_not_allowed` 表示子工作区改动了 `allowed_paths/allowed_prefixes` 之外
  的路径，`detail.paths` 是越界清单。
- `ws_unstable` 表示采集时有文件正在被写（`detail.paths`），
  `detail.retryable=true`；这不是冲突，稍后重试即可。
- `land_invariant_violation` 是内部护栏：合并结果触碰了子工作区没改的路径，
  直接拒绝而不是写盘。遇到它请带上完整结果反馈给 P37。
