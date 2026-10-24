# 运维手册

本文覆盖升级、迁移、备份、GC 维护窗口、监控和常见故障。P36 的 payload /
systemd / L1 胶水细节见 [`../packaging/p36/README.md`](../packaging/p36/README.md)。

## 1. 升级

### 1.1 发布物

```bash
make release VERSION=0.3.3
# dist/p37-linux-amd64-0.3.3.tar.gz
# dist/p37-linux-amd64-0.3.3.tar.gz.sha256
make p36-bootstrap
# dist/p36-bootstrap/...
```

发布 tar 根目录严格只有 `p37`、`p37d`、`manifest.json`。P36 日常升级只换
payload 盘；改 L1 胶水（`p37-run`/`p37-mcp`/`p37-mount`/unit/config/sysctl）才
需要重建 L1。

### 1.2 步骤

1. 从 GitHub Release 下载 tar 与 `.sha256`，校验 sha256。
2. 生成新的 `P37PAYLOAD` ext4 盘（同 label，固定 96 MiB）。
3. 停止实例（或按 P36 方式替换第三块盘 `vdc`）。
4. 启动实例；`p37-run` 会校验 `manifest.version` 与 `p37d --version`。
5. 打开仓库时自动处理格式迁移（见下节）。
6. 跑一次 `p37d --check` 和 `p37 fsck`。

只 `systemctl restart p37d` 时旧 payload 可能仍挂在 `/run/p37-payload`；
**换盘请重启实例**。

### 1.3 v1 → v2 格式迁移

- `repo.Open` 发现 `PRAGMA user_version=1` 时自动迁移。
- 迁移前用 `VACUUM INTO` 备份到 `<repo>/.p37/backup/p37-v1-<ts>.db`。
- 回填 `version_parent`，用 `mainline_head` 建立 protected 的 `main` line。
- 历史 CAS 对象不重写；回滚只需恢复 `p37.db` 备份。
- 一般不需要手工执行 `p37 migrate`；该命令只是显式打开并走同一路径。
- 旧二进制遇到 v2 会明确报错；升级后不要回退到不认识 v2 的版本。

详情见 [`format-v2.md`](format-v2.md)。

### 1.4 配置迁移

从 v0.1.12 升级时，已有 `.p37/config.toml` 若仍是
`materialize = "overlay"`，需要手工改成 `auto`（或 `copy`）。
`p37 init --if-missing` 不会覆盖旧配置。

## 2. 维护窗口与 GC

`p37 gc` 会删除对象；daemon 的 `landMu` 只在本进程内有效，跨进程靠 per-repo
flock 和 `p37 gc` 的持锁检查。

```bash
# 单仓库
systemctl stop p37d
p37 gc /srv/repo --prune --pack
systemctl start p37d

# 多仓库（逐个 repo；--root + --repo）
systemctl stop p37d
p37 --root /srv/repos --repo app-a gc --prune --pack
p37 --root /srv/repos --repo app-b gc --prune --pack
systemctl start p37d
```

- 默认只预演；`--prune` 才真的删版本行/对象。
- `--pack` 需要配合 `--prune`；否则只提示。
- `--scrub` 抽样校验对象；`--report-roots` 打印 GC 根；`--workspaces` 只清孤儿
  工作区目录。
- daemon 持锁时 `p37 gc` 拒绝；`--force` 跳过检查并打印警告，不推荐在线使用。
- 在线 GC 尚未实现。
- `fsck`、`migrate`、`import` 等写命令也应在维护窗口执行。

## 3. 备份与恢复

### 3.1 备份

- 对象和 pack 是不可变的；最需要一致备份的是 `.p37/p37.db` 和
  `.p37/config.toml`、`.p37/repo.json`。
- 停 daemon 后可直接快照整个 repo 目录。
- 不停 daemon 时，至少用 SQLite 的一致备份（例如 `sqlite3 p37.db
  ".backup /path/p37.db"` 或 `VACUUM INTO`）；不要在 WAL 活跃时直接 `cp`
  DB 文件。
- 迁移会自动生成 `.p37/backup/p37-v1-<ts>.db`。

### 3.2 恢复

- 同机回滚：停 daemon，恢复 `p37.db`（以及必要时的 config/repo.json），
  再启动并跑 `fsck`。
- 新 VM / 新 daemon：用 `sync_restore(repo, remote_prefix, remote?, force?)`
  从远端恢复；它恢复所有 line/protected/default_line，不恢复 workspace/lease，
  之后由 P34 重新 `ws_open`。
- `sync_restore` 对非空仓库需要 `force=true`；旧数据移到 `.p37/trash/`。
- 恢复后跑 `p37d --check --strict --repo <root>` 和 `p37 fsck <root>`。

## 4. 监控

### 4.1 健康检查

```bash
# 单仓库：完整检查（SQLite/写探测/ext4/OverlayFS/inotify/磁盘）
p37d --check --strict --repo /srv/repo --json

# 多仓库：registry 级检查（每个 repo 的 DB；strict 下检查 line 链）
p37d --check --strict --root /srv/repos --json
```

退出码：`0=通过`、`1=必需项失败`、`2=用法`。`--check` 有写探测，只适合
daemon 启动前检查，不建议高频在线探针。

### 4.2 Prometheus

开启：

```toml
[daemon]
listen_http = true
metrics_http = true
auth_token = "<token>"
```

`/metrics` 与 MCP 共用 HTTP 监听器，同样要求
`Authorization: Bearer <token>`。指标是只读规模信息，不暴露路径或任务描述：

- 单仓库：`p37_build_info`、`p37_versions_total`、`p37_epoch`、
  `p37_mainline_head_present`、`p37_last_land_timestamp_seconds`、
  `p37_workspaces{state=...}`、`p37_workspaces_total`
- 多仓库：当前只输出 `p37_repos_total`；per-repo 指标尚未实现

### 4.3 日志

- `p37d` 日志写 stderr，P36 由 systemd journal 收集。
- `log_level` 默认 `info`。
- `daemon.metrics_http=true` 但 `listen_http=false` 时启动会告警。

## 5. 常见故障

| 现象 / 错误码 | 常见原因 | 处理 |
|---|---|---|
| `overlay_unavailable` / `p37 doctor` 的 `[FAIL] OverlayFS` | 非 root + Ubuntu 26.04 的 `EPERM`，或缺 `CAP_SYS_ADMIN` | 接受 `materialize=auto` 的 copy 回退；或启用 root+overlay 变体 |
| `ws_not_mounted` | overlay 工作区被 `umount` / 重启后未恢复 | 重启 daemon 触发 `RemountAll`；或 `ws_rebase`/重建工作区 |
| `ws_broken` | 工作区目录缺失或未挂载 | `ws_reap` 回收，或 `ws_drop` 后重开 |
| `repo_busy` | 活跃工作区/lease，或 per-repo flock 被占 | 先 `ws_list`/`lease_query` 确认；必要时 `force`，但会丢未落地数据 |
| `repo_is_mountpoint` | 对应用卷挂载点调用 `repo_delete` | 走 `repo_unregister → guest umount → 宿主删卷` |
| `repo_not_initialized` | `repo_register` 时 path 下没有 p37 DB | 用 `init_if_missing=true`，或先 `p37 init` |
| `quota_exceeded` | `max_active`/`max_hidden`/`max_depth`/`max_fanout` 达到上限 | 读 `ws_quota`，先 `ws_drop`/`ws_reap`；不要把它当成模型空转 |
| `land_queue_full` | 并发 land 超过 `land_queue_max` | 退避重试；检查是否有工作区长时间卡住 |
| `sync_in_progress` | 同 repo 已有同步在跑 | 等待或检查是否有残留进程 |
| `sync_not_configured` | `[remote]` 未启用或缺少 endpoint/bucket | 检查 `repo_set_remote` / config |
| `sync_remote_moved` | `expected_remote_generation` 不匹配 | 重新 `sync_status/pull` 后再推 |
| `p37 gc` 被拒绝 | daemon 持有 per-repo flock | 停 daemon；或确认风险后 `--force` |
| stdio 连不上 | socket 不存在、uid 不同、daemon 未启动 | 确认 `/srv/repos/.p37/p37d.sock` 属主 `0600`，客户端与 p37d 同 uid |
| HTTP 401 | 未带/带错 Bearer token | 检查 `daemon.auth_token` 和客户端 header |

## 6. 容量规划

- copy 模式下每个工作区是一份完整树；磁盘 ≈ 仓库树 × 活跃工作区数。
- overlay 模式共享只读基底，upper 只存改动；`base_cache_bytes` 控制基底缓存。
- 20GB L2 的 P36 模板：`max_active=8`、`max_hidden=32`、
  `warn_disk_bytes=8 GiB`。按 `repo_disk_usage` / `ws_disk_usage` 复核。
- `land_queue_max` 是排队上限，不是并发执行数；land 本身由 `landMu` 串行。
- 多 line 仓库 push 当前是全量 checkpoint；大仓库建议按会话保留 VM，减少
  restore 次数。
