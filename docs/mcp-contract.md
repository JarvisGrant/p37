# p37 MCP 契约（v2 / format_ver=2）

所有工具返回统一信封：

```json
{ "ok": true, "data": {}, "error": null }
```

失败时 `ok=false`、`isError=true`，`error` 为：

```json
{ "code": "ws_not_found", "message": "...", "details": {} }
```

错误码统一小写 snake_case。`details` 可选字段：`repo`, `repo_id`, `line`,
`ws_id`, `version_id`, `target_head`, `current_head`, `conflicts`,
`retry_after_ms`, `limit`, `current`。

## 通用参数

- `repo`：仓库 id 或 name。多仓库模式必填；单仓库模式可省略。
- `ref`：`version:<id|前缀>` / `line:<name>` / `mainline`。
- `parent` / `target`：`{ "kind": "version|line|workspace|mainline", "id": "..." }`。
  旧字符串形式仍兼容。
- 可选字段在输出中返回 `null`，不省略。

## 多仓库

| 工具 | 说明 |
|---|---|
| `repo_list` | 列出 registry 中的仓库 |
| `repo_create` | `name`, `default_line?`, `remote_key?`, `remote_prefix?`, `remote?` |
| `repo_info` | id/name/path/default_line/lines/workspace_count/remote |
| `repo_delete` | `force`, `delete_remote`；有活跃工作区/租约时返回 `repo_busy` |
| `repo_disk_usage` | 磁盘占用 |
| `repo_import` | 空仓库导入目录为 v0 |
| `repo_export` | 把 line/version 物化到目录；`async=true` 返回 `job_id` |
| `repo_export_status` | 查询异步导出任务；任务只在当前 daemon 进程内有效 |
| `repo_set_remote` | 更新 per-repo `[remote]`；凭证只从环境变量读 |
| `ws_disk_usage` | 按工作区统计磁盘占用 |

## 命名线

| 工具 | 说明 |
|---|---|
| `line_create` | `name`, `from`(默认 mainline), `protected?` |
| `line_list` / `line_info` | 列表 / 详情（含相对 default line 的 ahead/behind） |
| `line_delete` | 默认/protected 需 `force`；有活跃工作区返回 `line_busy` |
| `line_protect` | `protected`, `force?` |
| `line_reset` | `version`, `force?`, `reason?`, `expected_head?` |
| `line_log` | line 祖先历史；`before_ts` + `before_id` 游标 |
| `line_compare` | `a`, `b` → ahead/behind/merge_base + `a_files`/`b_files` |
| `line_graph` / `version_graph` | DAG 节点/边 |
| `line_merge` | `source`, `target`, `preview?`, `no_ff?`, `merge_id?`, `expected_head?`, `expected_source_head?`, `task?`, `idempotency_key?` |

### line_merge 冲突流程

```text
line_merge(preview=true)  → merge_id + conflicts + target_head/base_version
conflict_list(ws_id=merge_id)
conflict_resolve(key, resolution)
conflict_resolve_all(ws_id=merge_id, policy)
line_merge(source, target, merge_id=merge_id, preview=false, expected_head=...)
```

merge preview workspace 隐藏，TTL 24h；过期返回 `merge_expired`。

## 工作区

| 工具 | 说明 |
|---|---|
| `ws_open` | `parent?`, `name?`, `task`/`task_desc`, `materialize?` |
| `ws_list` | 默认不含隐藏 merge/preview；`include_hidden=true` 才显示 |
| `ws_info` | base_line/base_version/parent/dirty/ops/conflicts/disk/state/base_stale |
| `ws_sync` | 返回 ops/files/conflicts/dirty/skipped_unstable |
| `ws_diff` | 文件级 A/M/D/R/C |
| `ws_rebase` | `to` = version/line/workspace |
| `ws_quota` | 配额用量：`max_active`/`active_used`/`active_available`/`hidden_*`/`max_depth`/`max_fanout`/`landed`/`broken` |
| `ws_drop` | 幂等；`cascade`, `keep_dir`。**`cascade=true` 是把子工作区重挂到祖父，不是递归删除** |
| `ws_lease` / `lease_query` | 路径租约 |
| `ws_reap` | 默认 dry-run；有 lease/active 不删；隐藏工作区 TTL 24h；`state=broken` 可回收；`include_orphan_dirs=true` 顺带清理孤儿目录 |
| `ws_disk_usage` | 按工作区统计 |
| `land_preview` | `target`；返回 `target_head`, `ws_tree`, `ws_ops_epoch`, conflicts |
| `ws_land` | `target`；`fold=self|stack`, `drop_workspace=true`, `expected_head`, `expected_ws_tree`, `idempotency_key` |
| `ws_land_into` | `parent_ws_id`, `expected_parent_tree`, `preview_id`, `idempotency_key` |

### 会话模型

```text
line_create("p34/session/<id>", from="line:<app>")
ws = ws_open(parent={kind:"line", id:"p34/session/<id>"})
... 改文件 ...
ws_sync(ws)
p = land_preview(ws, target={kind:"line", id:"p34/session/<id>"})
... P34 验收 ...
ws_land(ws, target={kind:"line", id:"p34/session/<id>"},
        expected_head=p.target_head, expected_ws_tree=p.ws_tree,
        drop_workspace=true, idempotency_key=...)
```

### 子任务

```text
child = ws_open(parent={kind:"workspace", id:parent_ws})
r = ws_land_into(child, parent_ws)          # 首次不必带 preview_id
if r.state == "conflict" or !r.can_land:
    conflict_list(ws_id=child)              # 冲突挂在子工作区上
    conflict_resolve_all(ws_id=child, policy="prefer_upper")
    r = ws_land_into(child, parent_ws,
                     expected_parent_tree=r.target_head,
                     preview_id=r.preview_id)   # = child id
```

`ws_land_into` 只允许直接父；不产生版本、不推进 line。冲突规则：

- 冲突行挂在**子工作区**上，`conflict_list(ws_id=child)` / `conflict_resolve*`
  直接可用；返回的 `preview_id` 等于子工作区 id（历史 `mg_*` preview 仍接受）。
- 直接 `ws_land_into` 冲突时子工作区置 `conflicted`；`land_preview` 只记录
  冲突、不改状态。
- 冲突 key 包含工作区 id，但同一子工作区的每次重试都用同一个 id，因此
  `resolve` 后重试会生效（不要每次都新建子工作区）。
- 子工作区处于 `conflicted` 时父工作区 `ws_land(fold=self)` 返回
  `ws_has_active_children`，`detail.blocked_by_children` 给出
  `[{ws_id,state}]`；调用方必须显式 `conflict_resolve*` 或 `ws_drop` 子工作区。
- `ws_drop(child)` 后配额立即回收；`ws_drop(parent, cascade=true)` 只会把子
  工作区重挂到祖父，不会释放它们的额度。

## 历史读取

| 工具 | 说明 |
|---|---|
| `version_tree` | `ref`, `path`, `recursive`, `limit` → items + truncated |
| `version_file` | `ref`, `path`, `max_bytes` → base64 + binary/truncated/mode |
| `version_archive` | `ref`, `path`, `format=tar.gz|zip` → VM 内临时文件 |
| `version_log` | `ref`(line/mainline), `limit`, `before_ts`, `before_id` |
| `version_show` | 含 `parents[]` |
| `version_diff` | `a`, `b?`, `parent_index`(默认 0), `path`, `stat_only` |
| `version_compare` | 两个 ref 的 ahead/behind |

`version_file.max_bytes` 默认 4 MiB、最大 8 MiB（base64 膨胀后仍低于 16 MiB
响应上限）；更大内容用 `version_archive`。实现只读取前 `max_bytes`，不会把
整文件读进内存。

## 导出（部署 / 迁移）

```text
repo_export(repo, ref, dest_dir, async=false)
→ { exported, ref, dest_dir, version_id, files, bytes, duration_ms }
```

- 不创建 workspace、不占 `ws_quota`、不出现在 `ws_list`，也不会被 fs watcher
  监听；导出目录里的构建产物不会进入 CAS/op/版本。
- `dest_dir` 必须是绝对路径、位于仓库目录之外。配置了
  `[repo] export_root` 时还必须位于该目录之下（P36 模板默认
  `/srv/repos/.p37/tmp/p34-deploy`）。
- `dest_dir` 必须不存在或为空；非空返回 `destination_not_empty`。
- 文件按分块流式写出，内存占用上界为单个 CAS 分块（64 KiB），不会因为大文件
  整份进内存。`files` 统计普通文件与 symlink，不统计目录；`bytes` 是普通文件
  字节数加 symlink 目标字符串长度。
- 保留 symlink、可执行位；不保留 mtime/uid/gid/xattr/硬链接。
- 导出目录的清理完全由调用方负责；P37 不自动删除导出目录。
- `async=true` 时立即返回 `{job_id, state:"running", dest_dir}`，用
  `repo_export_status(job_id)` 轮询。任务不持久化，daemon 重启后失效。

## 同步

| 工具 | 说明 |
|---|---|
| `sync_status` | local/remote head、pending、ahead/behind、remote_key/prefix |
| `sync_prepare` | 仅生成本地 outbox 包 |
| `sync_list` / `sync_show` | 查看暂存包 |
| `sync_push` | 显式同步推送；成功后 `durable=true`；同 repo 并发返回 `sync_in_progress` |
| `sync_fetch` | 下载对象/refs，不移动 head |
| `sync_pull` | 下载并快进 line；分叉保留本地 |
| `sync_restore` | 从完整 `remote_prefix` 恢复（可自建 repo） |
| `sync_prune` | 回收远端旧包（保留最新 checkpoint + 后续 delta） |

`sync_push` 的提交点是 `refs/lines.json` 的条件更新。部分上传的包是内容寻址
的，可安全重试。`auto_push` 为异步便利功能，不承担 durable 语义。

## 错误码

```text
repo_required repo_not_found repo_exists repo_busy repo_not_initialized
line_not_found line_exists line_protected line_busy
target_invalid target_moved source_moved target_not_ancestor
parent_moved preview_expired preview_mismatch merge_expired merge_base_ambiguous
ws_not_found ws_not_mounted ws_broken ws_busy ws_has_active_children
already_landed already_merged already_dropped already_deleted
not_dirty conflict invalid_path destination_not_empty quota_exceeded land_queue_full
idempotency_key_conflict idempotency_expired ambiguous_id
sync_not_configured sync_in_progress sync_remote_moved
response_too_large archive_too_large file_too_large corrupt_object
invalid_argument internal
```

`quota_exceeded` 的 `error.detail` 带结构化用量：

```json
{ "scope": "workspace_active", "used": 4, "limit": 4 }
```

`scope` ∈ `workspace_active|workspace_hidden|workspace_depth|workspace_fanout`。
事前限流用 `ws_quota`，不要从错误文本里解析。

`ws_has_active_children` 的 `error.detail` 在 `ws_land(fold=self)` 时带：

```json
{ "ws_id": "ws_parent", "blocked_by_children": [{"ws_id":"ws_child","state":"conflicted"}] }
```

`ws_drop` 在元数据已删除、但目录清理失败时返回 `ok=true` 且带
`dir_cleanup_warning`；land 路径把同类问题放在结果的 `warnings[]`。
工作区上限的可用量只统计 `active/conflicted/rebasing`；`landed`、`integrating`、
`hidden`、`broken` 不占 `max_active`（`broken` 在 `ws_quota.broken` 单独暴露）。
