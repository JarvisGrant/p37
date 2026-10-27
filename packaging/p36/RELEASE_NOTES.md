# p37 @VERSION@ — 半初始化导入与幂等收紧

## 发布物

- 平台：`linux/amd64`，静态链接（`CGO_ENABLED=0`）
- 资产：
  - `p37-linux-amd64-@VERSION@.tar.gz`
  - `p37-linux-amd64-@VERSION@.tar.gz.sha256`
- tar 根目录：`p37`、`p37d`、`manifest.json`

## manifest

```json
{
  "version": "@VERSION@",
  "commit": "@COMMIT@",
  "buildTime": "@BUILD_TIME@",
  "goVersion": "@GO_VERSION@"
}
```

## 本版重点（v0.4.4）

- **半初始化 + 用户文件不再静默 repair**：`mode=empty/repair` 检测到用户文件
  时返回 `repo_not_empty` + `next_action=repo_init_import`，不改 DB、不动文件。
  `repo_register(init_if_missing)` 同样规则。
- **`mode=import` 的 v0 状态机**：`currentTree == v0Tree` 只 repair；v0 为空
  直接导入；v0 非空且不同需要 `force=true`；worktree 空 + v0 非空只能显式
  force 清空；历史版本/命名 line/活跃 workspace/lease 即使 force 也拒绝。
- **`repo_init` / `repo_import` 增加 `force`**；`repo_init` 支持
  `mode=empty|import|repair` + force 的请求幂等比较。
- **幂等键真正校验**：root `registry.db` 新增 `init_idempotency` 表
  （in_progress/done/failed + lease + request_hash + result_json）；每个 repo
  的 `.p37/init.json` 保留最近 32 个 key 的 requests map 作为 root 索引丢失
  时的同 path 兜底。同 key 不同请求返回 `idempotency_conflict`，并发返回
  `idempotency_in_progress`。`p37 init` 默认不再生成 `cli-init-*`，新增
  `--idempotency-key`。
- **`repo_register(init_if_missing)` 的 key 只在确实触发 InitRepo 时生效**；
  strict register 忽略 key，不会因冲突失败。
- **`repo_health` 扩充初始化判定**：新增
  `user_files/root_tree_empty/history_not_empty/active_workspaces/leases`；
  `next_action` 覆盖 import/repair/stop_workspaces；数据完整只缺 refs/root 时
  允许有用户文件的元数据 repair；健康缓存只缓存未初始化结果。
- **`refs/mainline` 对齐**：default line 的 `ws_land` / `line_merge` /
  `line_reset` / `version_revert` / `sync fetch` 都会写 refs；
  `refs_match` 可作为 P36 `/ready` 的可靠门槛。
- **import 事务故障注入**：`swap_after_delete` / `swap_after_insert` /
  `swap_before_commit` 回滚旧 v0；`after_commit_before_refs` 暴露
  DB 已提交、refs 未写的窗口，由 `repo_init(mode=repair)` 对齐。

## 上一版（v0.4.3）

- **新增 `repo_init`**：在已挂载的持久应用卷上原子创建完整仓库
  （`.p37/p37.db` + 空 v0 + 默认 line + `refs/mainline` + `repo.json` +
  `config.toml`），支持 `mode=empty|import|repair`。新目录先在 `.p37.tmp`
  完整构建，fsync 后 rename；失败不再留下半套 `.p37`。
- **半初始化仓库可修复**：`repair` 认领已有的唯一 root v0，补
  `mainline_head`/line/refs，不新建第二个根；多个根/对象缺失返回
  `repo_db_corrupt` + `backup_path`。
- **`p37 init --if-missing` 改为“校验 + 修复”**：不再以 `p37.db` 是否存在
  作为跳过条件；完整跳过、半成品修复、不存在完整初始化。新增 `--strict`
  （只校验）与 `--no-repair`（诊断）。
- **`repo_register` 只注册完整仓库**：缺 DB/head/line/refs 时返回
  `repo_not_initialized` 或 `repo_db_corrupt`，`detail.next_action` 指向
  `repo_init`/`repo_init_repair`；`init_if_missing=true` 保留但 deprecated。
- **`repo_health` 成为 P36 `/ready` 的廉价入口**：新增
  `initialized/default_line_ready/repo_db_ok/next_action`；支持
  `path`（未注册时按挂载点探测）、`with_quota=false`（不打开 repo、不跑
  EnsureSchema/migrate）与 `deep=true`（doctor 用）。
- **`repo_import` 事务切换**：先构建对象树并 flush，再在一个 SQLite 事务里
  退休旧 v0/插入新 v0/更新 head/line，最后写 refs；失败不会出现 head 指向
  已删除版本。未初始化的已注册仓库等价于 `repo_init(mode=import)`。
- **`repo_scan` / `p37 doctor` 复用完整性校验**：scan 默认只报告
  `uninitialized[]`，`repair=true` 才修复；doctor 输出 repo/line/version/refs
  一致性。
- **`repo_create` 增加 `path`**：传 path 等价于 `repo_init(mode=empty)`；
  不传 path 仍是 L2 scratch（`mounted=false`），文档明确不能用于应用卷。

## 上一版（v0.4.2）

- **修复子会话 land 回退父改动**：`ws_effective_tree` 缓存原来用 `len(ops)`
  当版本号且 `Sync` 不失效缓存；同一批文件再改一次时命中旧树，`ApplyTree`
  再把旧树整棵写回父目录。现在 op-log 替换与 `ops_epoch+1`、缓存删除在同一
  事务完成（`meta.ReplaceOps`），并按工作区加读写锁。
- **land 只写子改动路径**：`ws_land_into` 从整树重写改为 `ApplyTreeDelta`，
  父在子运行期间改过、子没改的路径一个字节都不会被碰。
- **写前逐路径校验**：落盘前重新哈希 `applied_paths`，父内容已变则返回
  `parent_moved` + 路径，不写盘；结果树触碰子未改路径直接
  `land_invariant_violation`。
- **结构化三方结果**：`base_tree/child_tree/parent_tree_before/parent_tree_after`、
  `applied_paths`、`child_only/parent_only/both_changed/auto_merged/conflict_paths`、
  `paths[]`（含三方 blob 坐标）。
- **乐观锁**：`expected_parent_tree/expected_child_tree/expected_base_tree`；
  `ws_info` 暴露 `effective_tree/ops_epoch/parent_tree`，P34 可直接回传。
- **工作卡范围**：`allowed_paths/allowed_prefixes`，越界 `path_not_allowed`。
- **严格合并**：`merge_policy=strict` 时间一路径双方都改即冲突。
- **采集可见性**：`ws_land_into` 使用 `SyncStrict`（禁用快路径）；不稳定文件重试后返回
  `ws_unstable`，不再把「正在被写」误判为未变。
- **幂等补强**：拿到 `landMu` 后二次检查 idem/landed，并发同 key 可重放。

## 上一版（v0.4.1）

- **repo 路径不一致保护**：同名 repo 已注册在旧路径时，`repo_register` 返回
  `repo_path_mismatch`，不再静默继续用旧路径；`rebind=true` 才改 registry 指针。
- **repo.root 自愈**：repo 目录迁移/应用卷重挂后，打开 repo 时自动把
  `p37.db` 里的 `repo.root` 修到实际路径。
- **registry 重建更安全**：同名多候选优先选择目录名 == repo name 的路径，并把
  冲突写入启动日志；`repo_scan` 的 skipped 也会打印。
- **quota_exceeded 按 scope 区分**：`workspace_active/hidden` 可重试并带
  `retry_after_ms/next_action`；depth/fanout 不可重试。
- **`ws_diff` 对已回收 ws**：返回 `ok=true,state=already_landed,items=[]`。
- **`ws_reap name_prefix`**：按会话前缀过滤 eligible 候选。
- **新增 `repo_health`**：一次返回 repo/quota/sync/workspace/line/persistence。
- 文档明确 `name` 会话级稳定、`idempotency_key` turn 级。

## 上一版（v0.4.0）

- **工作区协议补齐幂等与消失语义**：`ws_open` 支持 `idempotency_key`；新增
  `ws_ensure`（按 name+parent 原子 open-or-get）；并发/重试不再产生同名工作区。
- **`ws_gone` / `already_landed`**：已落地并回收的 ws 对 `ws_info/ws_sync/
  land_preview/ws_land` 幂等返回 `already_landed`；显式 drop/过期返回结构化
  `ws_gone`，并区分 `ws_broken/ws_not_mounted`（存储/挂载异常）。
- **回收记录**：`ws_list include_dropped/include_recently_dropped` 可查
  tombstone（默认 7 天），带 reason/line/line_head。
- **错误上下文**：ws/repo/line 错误统一带 `repo`、标识符、`reason`。
- **可观测**：`ws_quota.workspaces[]`、`ws_reap.candidates[]/skipped[]`。
- **持久性声明**：`ws_land` 返回 `durable/durability`；`repo_info` 增加
  `p37_version` 与 `persistence`。仅 `sync_push(durable=true)` 可宣称跨 VM 持久。
- 仓库格式仍为 `format_ver=2`，无需迁移；老二进制可打开新仓库。

## 上一版（v0.3.3）

本版是 **v0.3.2 的文档/契约收口 + 一个乐观并发修复**，不改变仓库格式、存储
布局和 MCP 工具面。

- **文档对齐当前实现**：README、MCP 契约、状态、并发、同步、验收、VERIFY 和
  P36 部署说明全部按 v0.3.x 的实际行为重写；修正了错误的版本号、
  `format_ver`、`/srv/repo` 路径、错误码和错误信封字段名。
- **MCP 契约补全**：覆盖全部 61 个工具；错误字段统一为 `detail`（单数）；
  错误码表与代码实际返回的 45 个码对齐。
- **`ws_land.expected_ws_ops_epoch` 真正生效**：此前参数被解析但未校验。
  现在传过期 epoch 会返回 `target_moved`；不传或传当前 epoch 不受影响。
  这是本版唯一有意的行为收紧，符合 `docs/p34-integration.md` 的原有示例。
- **文档防漂移**：新增测试校验 MCP 工具覆盖、错误码、错误字段、相对链接，
  以及 `Makefile` / `buildinfo` / README 的版本一致性。
- **文档结构**：新增文档索引、当前状态、架构、CLI、配置、运维手册和贡献指南；
  `PROGRESS.md` 明确为历史记录，当前状态改由 `docs/status.md` 维护。

## 接口变化

- 0.4.4 新增：`repo_init.force`、`repo_import.force`（已有参数语义收紧）、
  `repo_init` 幂等键 root 表、`p37 init --idempotency-key`、
  `repo_health` 的 `user_files/root_tree_empty/history_not_empty/
  active_workspaces/leases`；新增错误码 `repo_history_not_empty`、
  `idempotency_in_progress`。
- 新增 MCP `repo_init`（推荐的应用卷入口）。
- `repo_register` 对半初始化仓库从“静默注册”改为返回
  `repo_not_initialized`/`repo_db_corrupt`；`init_if_missing=true` 仍可用，
  但内部等价于 `repo_init(mode=empty)`，并标记 deprecated。
- `repo_health` 新增字段 `initialized/reason/next_action/default_line_ready/
  repo_db_ok/repo_json_ok/root_ok/refs_ok/refs_match/format_ok`，并支持
  `path/deep/with_quota`。
- `repo_import` 新增 `force`；`repo_scan` 新增 `repair`；`repo_create` 新增
  `path`。
- `p37 init` 新增 `--mode/--strict/--no-repair/--name/--default-line/
  --force/--idempotency-key/--allow-nonpersistent`；`--if-missing` 语义为
  “校验 + 修复”，有用户文件时必须显式 `--mode=import`。

## 兼容性

- 仓库格式仍为 `format_ver=2`，无需迁移；SQLite / CAS / pack / OverlayFS
  布局未改。
- 0.4.4 可以打开 0.4.3/0.4.2 及更早的完整仓库；`repo_init(mode=repair)` 可修复
  历史半成品。
- **最低兼容矩阵**：
  - P36：需要能返回 409 + `mounted=true` + `initialized=false`，并且
    初始化失败不 umount；`/ready` 消费 `refs_ok/refs_match`；P37 < 0.4.3 时
    返回 `p37_version_too_old`。旧 P36 仍可依赖 `p37 init --if-missing`，
    但“半成品 + 用户文件”需要显式 `--mode=import`。
  - P34：需要改用 `repo_init`/`repo_health`，删除 `EnsureRepoDefaultLine`；
    有用户文件时用 `mode=import`，v0 非空且内容不同时显式 `force=true`；
    旧 P34 的 `line_create` 不带 `from` 无法创建根版本，不能修复半成品。
  - P37：应用卷统一用 `repo_init` 或 `p37 init`；`repo_create` 不带 path
    仅用于 L2 scratch。

## P36 集成要点

- 应用卷 attach：guest mount 后调用 `p37 init <mountPath> --if-missing`
  （旧镜像）或 MCP `repo_init(mode=empty)`（新镜像）；成功后再
  `repo_register`。
- 初始化失败：**保持卷挂载、不删除 `.p37`**，返回 409 + `mounted=true` +
  `initialized=false` + `next_action`；用户可点“重试初始化”。
- P36 `/ready`：调用 `repo_health {path, with_quota:false}`，消费
  `initialized/default_line_ready/refs_ok/refs_match/next_action`；
  `initialized=false` 不是 p37d down。0.4.4 起 default line 的 land/merge/reset/
  revert/sync 已同步 refs。
- detach 仍为 `repo_unregister` → guest umount → 宿主 remove-device。
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部
  `p37d --stdio --root /srv/repos`）。

## 已知限制

- 在线 GC 未实现；`p37 gc` 在 daemon 运行时拒绝，需维护窗口。
- 写时打包与 repack 压实未实现；只有离线 `p37 gc --prune --pack`。
- 多 line push 当前每次生成全量 checkpoint；单 line 可增量。
- `sync_restore` 不恢复 workspace/lease；P34 重新 `ws_open`。
- 同一 `remote_prefix` 只支持单写者；P34 需串行化同一 app 的 push。
- `repo_unregister` 不等待已经进入执行中的 MCP 调用结束；detach 前由上层确认
  应用空闲。
- `p37d --check --repo` 有写探测，不适合在线高频探针；`--check --root` 只做
  registry/DB/line 检查，也不应高频轮询。
- copy 模式下 `p37 doctor` 仍会把 OverlayFS 标成 `FAIL`；完整部署门禁用
  `p37d --check --strict --repo <root>`。
- payload 换盘需要重启实例；服务级 restart 不保证换盘。
