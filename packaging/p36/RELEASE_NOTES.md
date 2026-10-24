# p37 @VERSION@ — 文档对齐与 land 乐观并发修复

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

## 本版重点（v0.3.3）

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

MCP 工具名、参数 Schema、返回字段没有变化。只有一个行为变化：

```text
ws_land(expected_ws_ops_epoch = <旧值>)  → target_moved
```

`expected_ws_ops_epoch` 省略或为 0 时不校验；传当前值正常 land。这样调用方可以
真正用 `land_preview` 返回的 `ws_ops_epoch` 做乐观并发保护。

`expected_ws_tree`、`expected_head`、`expected_parent_tree` 的语义不变。

## 兼容性

- 仓库格式仍为 `format_ver=2`，无需迁移。
- SQLite / OverlayFS / CAS / pack 格式未改。
- 已有仓库、工作区、同步包可直接使用。
- 普通 MCP 客户端（不传 `expected_ws_ops_epoch`）行为不变。
- 传了过期 `expected_ws_ops_epoch` 的客户端会开始得到 `target_moved`；这是
  有意的并发保护，不是回归。
- 单仓库模式 `p37d --repo <root>` 与多仓库模式 `p37d --root <root>` 均不变。

## P36 集成要点

- 平台胶水放在 **L1**，不改 L0；只有 `p37`/`p37d` 在 payload。
- payload：ext4、label `P37PAYLOAD`、固定 96 MiB、只读挂载到
  `/run/p37-payload`。
- P36 设备顺序：`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；按 label 挂载。
- 默认多仓库：根目录 `/srv/repos`（`p36:p36`，`0750`），
  `p37d --root /srv/repos`；旧 `/srv/repo` 原地注册为 `default`。
- 应用卷：guest 挂到 `/srv/repos/<app>`，卷内就是 p37 repo 根。
  attach → `repo register`；detach → `repo unregister` → guest umount →
  宿主 remove-device。
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部
  `p37d --stdio --root /srv/repos`）。
- `p37d --check --repo <root>` 是完整健康检查；`p37d --check --root <dir>` 是
  registry/DB/line 链检查。`p37-run` 启动前调用非 strict 的 `--check --root`。

部署细节与验收步骤见仓库 `packaging/p36/README.md` 与 `VERIFY.md`。

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
