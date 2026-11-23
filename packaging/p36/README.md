# P36 L1 集成包

P36 当前把 **p37 / p37d 内嵌在 L1**：L0 只读，L1 只读，新建实例只挂
`vda=root`、`vdb=P36AGENT`，不再有 `P37PAYLOAD` / `vdc` 投递盘，也不再有
“每实例可写的 L2 系统盘 + 应用卷”这套 p37 专用模型。

- p37 二进制与 manifest 在 `/usr/local/lib/p37/`；
- 稳定入口是 `/usr/local/bin/p37-run`、`/usr/local/bin/p37-mcp`；
- `/srv/repos` 由 L1 预创建（`p36:p36`，`0750`），是 VM 根盘上的多仓库根；
- 每个应用卷挂到 `/srv/repos/<repo>`，本地在 P36 的 `staging/appdata`，
  唯一持久副本归档到对象存储；
- `/srv/repos` 根上的 repo 是临时的，VM close 后消失。

## 1. 文件落点

```text
L0 只读 Ubuntu（P36 不改 L0）

L1 应用层（只读）
├── /usr/local/lib/p37/p37
├── /usr/local/lib/p37/p37d
├── /usr/local/lib/p37/manifest.json
├── /usr/local/bin/p37-run          # 创建控制目录、校验版本、exec p37d
├── /usr/local/bin/p37-mcp          # MCP stdio wrapper
├── /usr/local/bin/p37-mount        # legacy payload 挂载助手（保留兼容）
├── /usr/local/bin/p37-mount-local  # L1 本地模式空操作
├── /usr/local/share/p37/config.toml
├── /etc/systemd/system/p37d.service
├── /etc/sysctl.d/90-p37.conf
└── /etc/tmpfiles.d/p37.conf

/srv/repos（L1 预创建，p36:p36 0750）
├── .p37/                           # registry、p37d.sock、tmp、trash
└── <repo>/                         # 应用卷挂载点；.p37/ + workspaces/
```

`p37d.service` 有 `ConditionPathIsDirectory=/srv/repos`，所以 `/srv/repos`
必须在服务启动前存在；P36 的 L1 install/注入负责创建它。

默认 `materialize = "auto"`：先尝试 OverlayFS，失败自动回退 copy。
Ubuntu 26.04 非 root 下 overlay 可能 `EPERM`（即使有 `CAP_SYS_ADMIN`），
这时工作区是 `/srv/repos/<repo>/workspaces` 下的普通目录。
copy 模式每个工作区是一份完整树，按应用卷实际容量调整
`max_active` / `warn_disk_bytes`。

## 2. P36 如何使用本目录

P36 的 one-click L1 构建会从 **p37 tag 源码包**的 `packaging/p36/` 取这些文件：

- `p37-run`
- `p37-mcp`
- `config.toml`
- `p37d.service`
- `90-p37.conf`
- `tmpfiles-p37.conf`

P36 自己再生成 `p37-mcp-wrapper`、`p37-mount-local`，并把 p37 Release 里的
`p37`、`p37d`、`manifest.json` 一起装进 L1。因此：

- 改本目录会被 P36 下一次 L1 构建使用，但**必须发新的 p37 tag**；
- 不要在 p37 里改文件名；P36 的 `p37build` 按固定清单提取；
- P36 的 `p37repo.MinVersion` 应随 p37 最低版本同步提升。

## 3. 应用卷生命周期（热插拔）

P36 把每个应用卷做成 qcow2，热加给运行中的 VM，guest 内挂到
`/srv/repos/<app>`。卷内就是 p37 repo 根（`.p37/`、`workspaces/` 等）。

```text
attach:
  宿主把 qcow2 作为 virtio-blk 热加给 VM
  guest mount /dev/vdX /srv/repos/app-a
  repo_health(path) → repo_init / repo_init_repair / repo_register

detach:
  repo_quiesce
  guest umount /srv/repos/app-a
  repo_unregister
  宿主 remove-device（之后才可删除/回收 qcow2）
```

`repo_register` 只注册完整仓库；半成品用 `repo_init(mode=repair)` 或
`repo_init(mode=import)`。初始化失败时 P36 保持卷挂载、不删 `.p37`。

## 4. 发布与升级

```bash
make release VERSION=<version>
# dist/p37-linux-amd64-<version>.tar.gz
# dist/p37-linux-amd64-<version>.tar.gz.sha256

make p36-bootstrap
# dist/p36-bootstrap/...（手工排查用；正式构建由 P36 控制面完成）
```

发布 tar 根目录严格只有：

```text
p37
p37d
manifest.json
```

P36 升级 p37 的方式是**重建/重新选择 L1**：

1. 发布新的 p37 Release；
2. 在 P36 控制面重建默认 L1（或选择包含新 p37 的 L1）；
3. 对需要升级的实例重建/重新选择应用 L1；
4. 旧实例不会自动切换；不要回退到不认识新仓库格式的旧 L1。

## 5. systemd 注意事项

- `User=p36`、`AmbientCapabilities=CAP_SYS_ADMIN`、`NoNewPrivileges=yes`；
- **不要**给 unit 加 `PrivateTmp`、`ProtectSystem=strict`、`ProtectHome`、
  `ReadWritePaths`、`ProtectKernel*`、`ProtectControlGroups` 等会创建私有
  mount namespace 的选项，否则 agent shell / `p37-mcp` 看不到工作区挂载；
- 不要用 `RuntimeDirectory=` 重建 p37 二进制目录；L1 内路径是只读的；
- HTTP 模式必须配置 `daemon.auth_token`，否则 daemon 启动直接失败。

MCP 客户端配置：

```json
{ "mcpServers": { "p37": { "command": "/usr/local/bin/p37-mcp" } } }
```

`p37-mcp` 只连接已有 daemon，不取仓库锁、不自动启动第二个 daemon。
socket 权限 `0600`，MCP 客户端必须与 p37d 同 uid（默认都是 `p36`）。

## 6. `p37d --check`

- `p37d --check --repo <root>`：完整健康检查（ext4/写探测/OverlayFS/inotify/磁盘）；
- `p37d --check --root <root>`：registry 级检查（每个 repo 的 `.p37/p37.db`，
  `--strict` 时打开 repo 走 line 链）。

退出码：`0=通过`、`1=必需项失败`、`2=用法`。auto 模式下 OverlayFS 不可用
只产生 `WARN`；`p37 doctor` 在 copy 模式下会显示 `[FAIL] OverlayFS`，这是
检查器的既有严格判定，不要据此判定 P36 部署失败。

## 7. 已知限制

- 只发布 `linux/amd64`；`linux/arm64` 只做编译校验。
- 在线 GC 未实现；GC/fsck/migrate 等写命令应在维护窗口停 daemon 后执行。
- 旧 L1 回退可能读不了新格式仓库；P36 必须把最低 p37 版本作为硬门槛。
- `repo_unregister` 不等待已进入执行中的 MCP 调用；detach 前应由 P36 在上层
  确认应用空闲。
- 应用卷删除只能走 `repo_unregister → guest umount → 宿主删卷`；不要对挂载点
  调用 `repo_delete`。
