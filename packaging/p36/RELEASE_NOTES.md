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
- 仓库固定在 L2 的 `/srv/repo`（`p36:p36`，建议 `0750`，ext4）。
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部 `p37d --stdio --repo /srv/repo`）。
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
- payload 换盘需要重启实例；服务级 restart 不保证换盘。
- `p37d --check` 有少量写探测，适合启动前检查，不建议作为在线高频探针。
- GitHub-hosted runner 可能禁止非 root 挂载 OverlayFS；Actions 验收会退化
  `User=root` 并标记 `ROOT_FALLBACK=1`。真实 P36 的 `User=p36` 验收需用
  P36 实例或 self-hosted runner（`vars.P37_RUNNER`）。
- `p37d --stdio` 在启动时快照 daemon 工具清单；daemon 升级后客户端应重连。
- HTTP 模式仍强制 `auth_token`；stdio 模式依赖 unix socket `0600`，客户端需与
  p37d 同 uid。
- 仓库格式 `format_ver=1` 不变，不需要 `p37 migrate`。
