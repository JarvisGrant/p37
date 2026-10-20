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
| `repo_export` | 把 line/version 物化到目录 |
| `repo_set_remote` | 更新 per-repo `[remote]`；凭证只从环境变量读 |

## 命名线

| 工具 | 说明 |
|---|---|
| `line_create` | `name`, `from`(默认 mainline), `protected?` |
| `line_list` / `line_info` | 列表 / 详情（含相对 default line 的 ahead/behind） |
| `line_delete` | 默认/protected 需 `force`；有活跃工作区返回 `line_busy` |
| `line_protect` | `protected`, `force?` |
| `line_reset` | `version`, `force?`, `reason?`, `expected_head?` |
| `line_log` | line 祖先历史；`before_ts` + `before_id` 游标 |
| `line_compare` | `a`, `b` → ahead/behind/merge_base |
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
| `ws_drop` | 幂等；`cascade`, `force` |
| `ws_lease` / `lease_query` | 路径租约 |
| `ws_reap` | 默认 dry-run；有 lease/active 不删；隐藏工作区 TTL 24h |
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
p = land_preview(child, target={kind:"workspace", id:parent_ws})
ws_land_into(child, parent_ws,
             expected_parent_tree=p.target_head, preview_id=p.preview_id)
```

`ws_land_into` 只允许直接父；不产生版本、不推进 line。

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
响应上限）；更大内容用 `version_archive`。

## 同步

| 工具 | 说明 |
|---|---|
| `sync_status` | local/remote head、pending、ahead/behind、remote_key/prefix |
| `sync_prepare` | 仅生成本地 outbox 包 |
| `sync_list` / `sync_show` | 查看暂存包 |
| `sync_push` | 显式同步推送；成功后 `durable=true` |
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
not_dirty conflict invalid_path quota_exceeded land_queue_full
idempotency_key_conflict idempotency_expired ambiguous_id
sync_not_configured sync_in_progress sync_remote_moved
response_too_large archive_too_large file_too_large corrupt_object
invalid_argument internal
```
