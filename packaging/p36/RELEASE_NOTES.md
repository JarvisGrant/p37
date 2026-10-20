# p37 @VERSION@ — P36 microVM 集成

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

## P36 集成要点

- 平台胶水放在 **L1**，不改 L0；只有 `p37`/`p37d` 在 payload。
- payload：ext4、label `P37PAYLOAD`、**固定 96 MiB**、只读挂载到
  `/run/p37-payload`。
- P36 设备顺序：`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；按 label 挂载，
  不依赖顺序。
- v0.2 起默认多仓库：根目录 `/srv/repos`（`p36:p36`，`0750`），
  `p37d --root /srv/repos`；旧 `/srv/repo` 原地注册为 `default`，不移动目录。
- 单仓库 `p37d --repo /srv/repo` 仍保留。
- 默认 `workspace.materialize = "auto"`：Ubuntu 26.04 非 root 下 OverlayFS
  可能 `EPERM`，自动回退 copy；copy 模式默认 `max_active=4`、
  `warn_disk_bytes=8 GiB`。
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部 `p37d --stdio --root /srv/repos`；
  `/srv/repos/.p37` 不存在时回退 `--repo /srv/repo`）。
- 新增 `/usr/local/bin/p37` CLI 稳定入口。
- `p37d.service` 以 `User=p36` 运行，授予 `CAP_SYS_ADMIN`；**不要**加会创建
  私有 mount namespace 的 `PrivateTmp`/`Protect*` 选项，否则 agent 看不到
  payload 和 workspace overlay。
- `/run/p37-payload` 由 `/etc/tmpfiles.d/p37.conf` 创建，不用 `RuntimeDirectory=`
  （service 重启时 systemd 会在只读 payload 挂载点上重建目录而失败）。停服后
  payload 仍挂载，维护窗口可直接用 `/run/p37-payload/p37 gc`。换 payload 盘需
  重启实例。

部署细节与验收步骤见仓库 `packaging/p36/README.md` 与 `VERIFY.md`。

## 已知限制

- 在线 GC 未实现；`p37 gc` 在 daemon 运行时拒绝，需维护窗口。
- copy 模式下 `p37 doctor` 仍会把 OverlayFS 标成 `FAIL`；正式门禁用
  `p37d --check --strict`（auto 下 overlay 只 WARN）。
- payload 换盘需要重启实例；服务级 restart 不保证换盘。
- `p37d --check` 有少量写探测，适合启动前检查，不建议作为在线高频探针。
- Ubuntu 26.04 非 root 下 OverlayFS 可能 `EPERM`；默认 auto 回退 copy。
  copy 模式磁盘 ≈ 仓库树 × 活跃工作区数，需按 L2 容量调整 `max_active` /
  `warn_disk_bytes`。默认 acceptance 不再自动 root fallback；只有显式
  `P37_ALLOW_ROOT_FALLBACK=1` 才退化 root 做对照。root+overlay 用
  `config-overlay.toml` + `p37d-root-overlay.service`。
- `p37d --stdio` 在启动时快照 daemon 工具清单；daemon 升级后客户端应重连。
- HTTP 模式仍强制 `auth_token`；stdio 模式依赖 unix socket `0600`，客户端需与
  p37d 同 uid。
- 仓库格式 `format_ver=2`：多父 merge 版本、命名线、幂等、workspace 扩展；
  `p37 migrate` 会先备份 `p37.db` 再升级，旧仓库自动迁移。
- 新增多仓库、命名线、会话工作区、`ws_land_into`、`line_merge`、历史读取、
  多 line 同步/恢复；见 `docs/mcp-contract.md` 与 `docs/p34-integration.md`。
- 从 v0.1.12 升级：已有 `.p37/config.toml` 若仍是 `materialize="overlay"`，
  请改成 `auto`（或 `copy`）；`init --if-missing` 不会覆盖旧配置。
