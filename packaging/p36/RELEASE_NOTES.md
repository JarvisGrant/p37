# p37 @VERSION@ — 应用卷生命周期（热插拔）

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

## 本版重点（v0.3.2）

P36 把「每应用一个持久卷」做成 qcow2 挂到 `/srv/repos/<app>`。本版让 p37d
支持运行中 attach/detach，不需要重启 daemon。

- **`repo_register`**：把已挂载的目录注册为 repo。校验 root 内路径、拒绝
  `..`/root 自身/root `.p37`/symlink 逃逸；同名或同 path 幂等；
  `init_if_missing=true` 时用 root 配置模板初始化；有 DB 但缺
  `.p37/repo.json` 时生成 `repo_<32hex>` 身份。
- **`repo_unregister`**：卸载 overlay workspace、关闭 SQLite、释放 per-repo
  flock，然后从 registry 隐藏（tombstone）。不删除 `.p37`、workspaces 或
  repo 数据。有活跃 workspace/lease 返回 `repo_busy`；`force=true` 可强制
  注销。失败时不写 tombstone，可直接重试。
- **`repo_scan`**：扫描 `/srv/repos` 直接子目录，registry 空或非空都能
  reconcile。未注册但 repo.json 有效 → 加入；已注册但卷不在 → degraded
  （`prune_missing=true` 才删行）；repo.json 缺失 → degraded；`dry_run` 不写。
- **挂载点保护**：`repo_delete` 在 rename 到 trash 前检查
  `/proc/self/mountinfo` + `st_dev`。挂载点返回 `repo_is_mountpoint`，root
  自身/root 外返回 `invalid_path`。应用卷删除改走
  `repo_unregister → guest umount → 宿主删卷`。
- **可观测字段**：`repo_list` / `repo_info` 增加
  `path` / `mounted` / `available` / `degraded` / `degraded_msg`，且不会为了
  列表去 Open repo。
- **启动 reconcile**：`p37d --root` 启动时做一次非破坏性 scan；不可用的 repo
  只标记 degraded，不阻止 daemon 启动。
- **CLI**：新增 `repo register/unregister/scan`。daemon socket 可连接时必须
  通过 daemon 执行；daemon 不在时才直连 registry。
- **身份与恢复**：`.p37/repo.json` 是 repo 身份锚。注销后 daemon 重启或
  `repo_scan` 不会自动复活；卷重新挂载后显式 `repo_register` 恢复原 id/name。

## 接口变化

```text
repo_register(repo, path, default_line?, init_if_missing?, force_rename?)
→ { id, name, path, default_line, mounted, available, degraded, degraded_msg? }

repo_unregister(repo, force?)
→ { unregistered, already_unregistered, id?, name?, path? }

repo_scan(reconcile?, prune_missing?, dry_run?)
→ { added[], removed[], degraded[], skipped[], reconciled, dry_run }
```

CLI：

```bash
p37 --root /srv/repos repo register app-a /srv/repos/app-a --init-if-missing --json
p37 --root /srv/repos repo unregister app-a --json
p37 --root /srv/repos repo scan --dry-run --json
```

新增错误码：`repo_name_mismatch`、`repo_path_exists`、`repo_is_mountpoint`、
`already_unregistered`、`registry_busy`、`daemon_running`。复用
`repo_not_initialized`、`repo_busy`、`invalid_path`。

## 兼容性

- 仓库格式仍为 `format_ver=2`，无需迁移。
- SQLite / OverlayFS 存储引擎未改；没有 NFS 专用 DB 模式。
- 单仓库模式 `p37d --repo <root>` 行为不变。
- 现有 `repo_create/info/delete/list` 仍可用；`repo_delete` 现在额外拒绝
  挂载点、root 自身和 root 外路径。
- v0.3.1 的 `repo_export` 流式化、`export_root` 护栏与异步任务保留。

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
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部 `p37d --stdio --root /srv/repos`）。

部署细节与验收步骤见仓库 `packaging/p36/README.md` 与 `VERIFY.md`。

## 已知限制

- `repo_unregister` 不等待已经进入执行中的 MCP 调用结束；detach 前应由 P36
  确认应用空闲。活跃 workspace/lease 会被 `repo_busy` 挡住。
- 注销使用 tombstone；`repo_scan` 不会自动复活已注销 repo，必须显式
  `repo_register`。
- `repo_register` 不自动创建 path 目录（避免卷未挂载时写根盘）。
- 在线 GC 未实现；`p37 gc` 在 daemon 运行时拒绝，需维护窗口。
- copy 模式下 `p37 doctor` 仍会把 OverlayFS 标成 `FAIL`；正式门禁用
  `p37d --check --strict`。
- payload 换盘需要重启实例；服务级 restart 不保证换盘。
- `p37d --check` 有少量写探测，适合启动前检查，不建议作为在线高频探针。
