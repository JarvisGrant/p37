# 当前状态（v0.4.5）

> 这是当前状态的唯一入口。按时间倒序的实现记录、缺陷复盘和设计修正见
> [`../PROGRESS.md`](../PROGRESS.md)；发布说明见 `packaging/p36/RELEASE_NOTES.md`。

## 版本

| 项 | 值 |
|---|---|
| 发布版本 | `v0.4.5` |
| 仓库格式 | `format_ver = 2` |
| MCP 工具 | 65 个（以 `internal/mcpsrv` 注册为准） |
| 发布平台 | `linux/amd64` 静态发布物；`darwin/amd64`、`linux/amd64`、`linux/arm64` 做编译校验 |
| 测试 | `go test ./...` 全绿；`make cross` 通过 |

`format_ver=2` 相对 v1 增加多父 merge 版本、命名线、幂等和工作区扩展字段；
线性版本的编码与 id 不变。v1 仓库首次被新二进制打开时会自动迁移并备份，见
[`format-v2.md`](format-v2.md)。

**v0.4.5 重点（不改 `format_ver`）**：把 `refs/mainline` 落后变成可执行的
repair 状态：`repo_health` 返回 `initialized=true, refs_ok=true,
refs_match=false, reason=refs_mismatch, next_action=repo_init_repair`；
`repo_init(mode=repair)` / `p37 init --if-missing` / `repo_register(init_if_missing)`
对 complete+refs-mismatch 的仓库执行 refs-only repair（有活跃 workspace/lease
返回 `repo_busy`）；strict `repo_register` 拒绝 refs mismatch，避免注册一个
`/ready` 不通过的仓库。`p37d --check --root` 非 strict 允许 refs mismatch 启动并
交给 `/ready`/repair，strict 仍失败。详见 [`mcp-contract.md`](mcp-contract.md)。

**v0.4.4 重点（不改 `format_ver`）**：补齐“半初始化 + 用户文件”的导入状态机
（empty/repair 拒绝静默 repair，import 按 v0 空/非空、same/different 决定
repair/force）；`repo_init`/`repo_import` 增加 `force`；root 级
`init_idempotency` + per-repo `init.json` 幂等校验；`repo_health` 增加
`user_files/root_tree_empty/history_not_empty/active_workspaces` 等
`next_action` 依据；修复 default line land/merge/reset/revert/sync 的
`refs/mainline` 写入，使 `/ready` 的 `refs_match` 成为可靠不变量。
详见 [`mcp-contract.md`](mcp-contract.md) 与 [`../PROGRESS.md`](../PROGRESS.md)。

**v0.4.3 重点（不改 `format_ver`）**：新增 `repo_init`，在应用卷上原子创建
`.p37`（p37.db + v0 + 默认 line + refs），支持 empty/import/repair 三种模式；
`p37 init --if-missing` 改为“校验 + 修复”；`repo_register` 只注册完整仓库；
`repo_health` 增加 `initialized/default_line_ready/next_action` 只读快路径；
`repo_import` 改为“先构建对象树，再事务切换”，避免 head 指向已删除版本。
详见 [`mcp-contract.md`](mcp-contract.md) 与 [`../PROGRESS.md`](../PROGRESS.md)。

**v0.4.2 重点（不改 `format_ver`）**：修复 `ws_land_into` 的有效树缓存一致性
（`workspace.ops_epoch` + `ReplaceOps` 原子替换），落盘改为「只写子改动路径 +
写前逐路径校验」，并新增 `base_tree/parent_tree/applied_paths/*_changed_paths`
等结构化字段、`expected_parent_tree|child_tree|base_tree` 乐观锁、
`allowed_paths` 越界拦截与 `merge_policy=strict`。详见
[`../PROGRESS.md`](../PROGRESS.md) 的 v0.4.2 条目与
[`mcp-contract.md`](mcp-contract.md)。`ops_epoch` 是幂等补列，旧库打开时自动添加。

## 里程碑

| 里程碑 | 状态 | 说明 |
|---|---|---|
| **M1 存储底座** | ✅ 完成 | BLAKE3、确定性编码、CDC、CAS、pack、对象索引、格式版本校验 |
| **M2 树与 op** | ✅ 完成 | histogram diff、块移动检测、rename 归并、ignore、EOL/二进制、重放 |
| **P2 归因** | ✅ 完成 | `blame` 行/hunk 级归因 + 缓存 |
| **M3 工作区与整合** | ✅ 完成 | overlay/copy 物化、重启重挂、未挂载防护、land 三段提交与恢复 |
| **M4 冲突** | ✅ 完成 | place、结构化冲突、resolution、候选解、不重叠行自动合并 |
| **M5 栈与回退** | ✅ 完成 | 折叠集合、`ws_land_into`、`version_revert`、journal |
| **多仓库 / 命名线** | ✅ 完成 | `--root`、registry、line、DAG、多 line 同步与恢复 |
| **应用卷生命周期** | ✅ 完成 | `repo_init` / `repo_register` / `repo_unregister` / `repo_scan`、原子初始化、半成品修复、挂载点保护、tombstone |

## 当前可用的端到端路径

```text
p37 init → p37 import → ws_open → 直接改文件 → ws_sync → land_preview → ws_land
                                                        ↓ 有冲突
                                        conflict_show → conflict_resolve → ws_land
```

以及：

- 会话线：`line_create` → `ws_open(parent=line)` → `ws_land(target=line)`；
- 子任务：`ws_open(parent=workspace)` → `ws_land_into`；
- 应用线合并：`line_merge(preview=true)` → `conflict_resolve*` → `line_merge`；
- 历史读取：`version_tree/file/archive/log/show/diff/compare/graph`、`blame`；
- 回滚：`version_revert`、`line_reset`；
- 同步：`sync_prepare/push/fetch/pull/restore/prune`；
- 部署导出：`repo_export`（同步或 `async=true` + `repo_export_status`）；
- 应用卷热插拔：`repo_register` / `repo_unregister` / `repo_scan`。

## 已知缺口与限制

| 项 | 现状 | 影响 / 绕过方式 |
|---|---|---|
| 在线 GC | 未实现 | 维护窗口停 daemon 后执行 `p37 gc --prune --pack`；daemon 持锁时 `p37 gc` 会拒绝 |
| 写时打包 / repack 压实 | 未实现 | 只有离线 `p37 gc --pack`；pack 内对象全部不可达才整包删除 |
| 多 line push | 使用全量 checkpoint | 单 line 可增量；大仓库建议按会话保留 VM |
| `sync_restore` | 不恢复 workspace / lease / tombstone / 幂等记录 | 恢复后由 P34 重新 `ws_ensure`；旧 ws_id 跨 VM 不保证可重放 |
| `remote_prefix` 单写者 | 同一前缀只允许一个写者 | P34 需串行化同一 app 的 push |
| `line_reset` 旧版本 grace | 由 GC 的 reset 日志保留 7 天 | 尚无独立后台清理任务 |
| `p37d --check --root` | 只做 registry / DB / line 链检查 | 完整 ext4/写探测/OverlayFS/inotify/磁盘检查用 `--check --repo <root>` |
| P36 payload 换盘 | 需要重启实例 | 服务级 `systemctl restart` 不保证换盘 |
| `p37d --stdio` | 启动时快照工具清单 | daemon 升级后客户端应重连 |
| `repo_unregister` | 不等待已经进入执行中的 MCP 调用 | detach 前由 P36 在上层确认应用空闲 |
| `repo_import.idempotency_key` | 保留但未接 root `init_idempotency` 表 | 需要跨重启幂等时用 `repo_init(mode=import)` |
| root 幂等索引丢失 | 同 path 仍靠 `.p37/init.json`；跨 path 冲突降级 | 接受 warning；重启后同 path key 仍有效 |
| 非 root OverlayFS | Ubuntu 26.04 可能 `EPERM` | 默认 `materialize=auto` 回退 copy；按 L2 容量收紧 `max_active` / `warn_disk_bytes` |
| 自更新 | 不支持 | 升级靠替换 payload 盘，不在实例内改 `/run/p37-payload` |
| 发布平台 | 只发布 linux/amd64 | linux/arm64 只做编译校验 |

## 下一步优先级

1. 在线 GC 与 daemon/CLI 共享写锁。
2. 写时打包与 repack 压实。
3. 多 line 增量同步。
4. P36 应用卷 detach 的 in-flight 调用排空机制。

文档与工具面的自动校验已加入 `internal/mcpsrv/docs_test.go` 和
`internal/buildinfo/buildinfo_test.go`。
