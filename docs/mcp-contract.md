# p37 MCP 契约（v0.3.3 / format_ver=2）

本文是 MCP 工具的参数、返回字段和错误码参考。工具实现以
`internal/mcpsrv` 的注册为准；当前注册 61 个工具。

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

只有以下工具的 `idempotency_key` 会写入/读取幂等缓存：

- `ws_land`
- `line_merge`
- `sync_push`

`version_revert` 接受 `idempotency_key` 并写入版本元数据，但当前不做前置查重。
其他工具即使 schema 接受 `idempotency_key`，目前也只是保留参数，不会做幂等
去重。`ws_land_into` 的幂等只保证同一 VM 生命周期（restore 不恢复 workspace）。

## 工具总览

| 分类 | 工具 |
|---|---|
| 仓库只读 | `repo_status` `repo_list` `repo_info` `repo_disk_usage` |
| 仓库管理 | `repo_create` `repo_delete` `repo_import` `repo_export` `repo_export_status` `repo_set_remote` |
| 应用卷生命周期 | `repo_register` `repo_unregister` `repo_scan` |
| 命名线 | `line_create` `line_list` `line_info` `line_delete` `line_protect` `line_reset` `line_log` `line_compare` `line_graph` `line_merge` |
| 工作区 | `ws_open` `ws_list` `ws_info` `ws_sync` `ws_diff` `ws_rebase` `ws_drop` `ws_quota` `ws_reap` `ws_disk_usage` `ws_lease` `lease_query` |
| 整合 | `land_preview` `ws_land` `ws_land_into` |
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

## 仓库管理

| 工具 | 参数 | 说明 |
|---|---|---|
| `repo_create` | `name`, `default_line?`, `remote_key?`, `remote_prefix?`, `remote?` | 创建空 v0 与默认 line；`remote` 是 `[remote]` 配置对象 |
| `repo_delete` | `repo`, `force?`, `delete_remote?` | 有活跃工作区/租约时返回 `repo_busy`；挂载点返回 `repo_is_mountpoint` |
| `repo_import` | `repo`, `dir`, `task?` | 把目录导入空仓库作为 default line 的 v0 |
| `repo_export` | `repo`, `ref`, `dest_dir`, `async?` | 把 line/version 物化到目录；`async=true` 返回 `job_id` |
| `repo_export_status` | `job_id` | 查询异步导出任务；任务只在当前 daemon 进程内有效 |
| `repo_set_remote` | `repo`, `remote` | 更新 per-repo `[remote]`；凭证仍只从环境变量读 |

`repo_delete` 的 `delete_remote` 当前保留但未参与删除逻辑；不会删除远端包。
`repo_import` 的 `task` 当前只作为参数保留，不写 journal。

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
        → repo_register（或 repo_scan）
detach: repo_unregister → guest umount → 宿主 remove-device
删除卷: repo_unregister → guest umount → 宿主删卷（不要用 repo_delete）
```

所有 registry 写操作都由运行中的 p37d 执行（Hub mutex + SQLite 事务）。CLI 在
daemon socket 可连接时通过 MCP 路由 register/unregister/scan。

### repo_register

```json
{
  "repo": "app-a",
  "path": "/srv/repos/app-a",
  "default_line": "main",
  "init_if_missing": false,
  "force_rename": false
}
```

- `path` 必须是 root 下的绝对路径，且不能是 root 自身、root/.p37、symlink 或
  逃逸路径。
- `path` 必须已经存在（应用卷应已挂载）；本工具不会创建目录。
- 同名或同 path 已注册时幂等返回已有 Entry。
- path 下没有 `.p37/p37.db`：
  - `init_if_missing=false` → `repo_not_initialized`；
  - `init_if_missing=true` → 使用 root 配置模板初始化，并写 `.p37/repo.json`。
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
{ "reconcile": true, "prune_missing": false, "dry_run": false }
```

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
| `ws_open` | `parent?`, `name?`, `task?`/`task_desc?`, `leases?`, `materialize?` | 打开并物化工作区；返回 `ws_id/dir/base_line/base_version/materialize/state` |
| `ws_list` | `include_hidden?` | 默认不含隐藏 merge/preview |
| `ws_info` | `ws_id` | base_line/base_version/parent/dirty/ops/conflicts/disk/state/base_stale |
| `ws_sync` | `ws_id` | 采集磁盘变更；见下方返回字段 |
| `ws_diff` | `ws_id` | 文件级 A/M/D/R/C |
| `ws_rebase` | `ws_id`, `to?` | `to` = version/line/workspace；默认父 |
| `ws_quota` | — | 配额用量；见下方 |
| `ws_drop` | `ws_id`, `cascade?`, `keep_dir?` | 幂等；`cascade=true` 把子工作区重挂到祖父，不是递归删除 |
| `ws_lease` | `ws_id`, `add?`, `release?`, `mode?` | 登记/解除路径租约；`mode` = shared/exclusive |
| `lease_query` | `paths?` | 查询若干路径上的租约 |
| `ws_reap` | `older_than?`, `state?`, `include_orphan_dirs?`, `dry_run?` | 默认 dry-run；有 lease/active 不删；隐藏 TTL 24h |
| `ws_disk_usage` | — | 按工作区统计磁盘占用 |

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
  "broken": 0
}
```

`max_active` 只统计 `active/conflicted/rebasing`；`landed`、`integrating`、
`hidden`、`broken` 不占额度。`hidden_limit` 是隐藏 merge/preview 的上限
（默认 32）。

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
| `land_preview` | `ws_id`, `target?`, `fold?`, `expected_head?`, `expected_ws_tree?` | 纯只读预演；冲突会持久化供 `conflict_resolve` |
| `ws_land` | `ws_id`, `target?`, `fold?`, `drop_workspace?`, `expected_head?`, `expected_ws_tree?`, `expected_ws_ops_epoch?`, `expected_parent_tree?`, `preview_id?`, `idempotency_key?`, `task?` | 落地到 line 或父工作区 |
| `ws_land_into` | `ws_id`, `parent_ws_id`, `expected_parent_tree?`, `preview_id?`, `idempotency_key?` | `ws_land(target=workspace)` 的显式别名 |

`fold` = `self`（默认）或 `stack`。`target` 省略时落到 default line；
`target.kind=workspace` 时等价于 `ws_land_into`。

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
  "version_id": "可选",
  "state": "可选",
  "warnings": []
}
```

冲突时 `can_land=false`、`state="conflict"`，冲突挂在被 land 的工作区上；
`preview_id` 等于该工作区 id（历史 `mg_*` preview 仍接受）。冲突解决后带
最新 `expected_head`/`expected_parent_tree` 重试，不要每次新建工作区。

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
r = ws_land_into(child, parent_ws)
if r.state == "conflict" or !r.can_land:
    conflict_list(ws_id=child)
    conflict_resolve_all(ws_id=child, policy="prefer_upper")
    r = ws_land_into(child, parent_ws,
                     expected_parent_tree=r.target_head,
                     preview_id=r.preview_id,
                     idempotency_key=<同一个 key>)
```

父工作区有冲突/未落地的子工作区时，`ws_land(fold=self)` 返回
`ws_has_active_children`，`detail.blocked_by_children` 形如
`[{"ws_id":"ws_child","state":"conflicted"}]`。此时必须显式解决冲突或
`ws_drop(child)`，不要无限重试。

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
repo_name_mismatch repo_path_exists repo_is_mountpoint
registry_busy
line_not_found line_exists line_protected
target_invalid target_moved source_moved target_not_ancestor
merge_base_ambiguous merge_expired preview_expired
ws_not_found ws_not_mounted ws_broken ws_busy ws_has_active_children
ws_cleanup_warning ws_exists quota_exceeded
not_dirty conflict land_queue_full invalid_path
destination_not_empty path_not_found
overlay_unavailable path_case_conflict
version_not_found ambiguous_id
sync_not_configured sync_in_progress sync_remote_moved
corrupt_object file_too_large
invalid_argument internal not_found
```

补充：

- `daemon_running` 是 CLI 的本地错误文本，不由 MCP 返回。
- `already_landed`、`already_merged`、`already_dropped`、`already_deleted`、
  `already_unregistered` 是返回体里的布尔字段，不是错误码。
- `repo_busy` 的 `detail` 带 `workspaces`/`leases`。
- `quota_exceeded` 的 `detail` 带 `scope/used/limit`，`scope` ∈
  `workspace_active|workspace_hidden|workspace_depth|workspace_fanout`。
- `ws_has_active_children` 的 `detail` 带 `ws_id` 和
  `blocked_by_children`。
- `ws_drop` 的目录清理失败不返回错误，而是在成功结果里带
  `dir_cleanup_warning`；land 路径把同类问题放在 `warnings[]`。
