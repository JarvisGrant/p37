# P36 microVM 部署包

p37 在 P36 上的定位是「每个实例启动即有的版本控制工具」。p37/p37d **不烙进
L0/L1**，而是由宿主投递一块只读 ext4 小盘（label `P37PAYLOAD`），实例内一段
稳定的引导脚本把它挂到 `/run/p37-payload`，再执行盘上的 `p37d`。

本目录只放 **L0 侧稳定胶水**；版本控制二进制来自 payload。

## 1. 分层与文件落点

```
L0 只读 Ubuntu
├── /usr/local/bin/p37-run          # 等盘、挂载、init、exec p37d
├── /usr/local/bin/p37-mcp          # MCP 客户端入口（exec p37d --stdio）
├── /usr/local/bin/p37-mount        # Go 挂载助手（按 label 只读挂载）
├── /usr/local/share/p37/config.toml
└── /etc/systemd/system/p37d.service

P37PAYLOAD（宿主投递，只读 ext4，第二块 virtio-blk）
├── p37
├── p37d
└── manifest.json

L2 /srv/repo（每实例可写 ext4）
├── .p37/                           # 元数据库、CAS、overlay upper、tmp
└── workspaces/                     # OverlayFS 工作区挂载点
```

L2 必须预先把 `/srv/repo` 建成 `p36:p36` 可写；p37d 以 `p36` 运行，不做
root 提权。`/srv/repo` 与 `workspaces/`、`.p37/` 必须在同一个 ext4 上。

## 2. 构建

p37 仓库：

```bash
make release VERSION=0.1.0
# dist/p37-linux-amd64-0.1.0.tar.gz
# dist/p37-linux-amd64-0.1.0.tar.gz.sha256

make p36-bootstrap
# dist/p37-mount-linux-amd64
# dist/p36-bootstrap/{p37-run,p37-mcp,p37-mount,config.toml,p37d.service}
```

宿主生成 payload（不需要 root，`mkfs.ext4 -d` 直接从目录填充）：

```bash
VERSION=0.1.0
tar -xzf dist/p37-linux-amd64-$VERSION.tar.gz -C /tmp/p37-payload-seed
truncate -s 32M /path/to/payload.ext4
mkfs.ext4 -q -F -L P37PAYLOAD -d /tmp/p37-payload-seed /path/to/payload.ext4
chmod 0444 /path/to/payload.ext4
```

投递盘只读挂到实例，建议放在该实例 L0 所在存储池下，例如
`<pool>/p37/<version>/payload.ext4`，不要与 `P36AGENT` 混用同一目录。

## 3. systemd

```bash
install -m 0755 dist/p36-bootstrap/p37-run   /mnt/l0/usr/local/bin/p37-run
install -m 0755 dist/p36-bootstrap/p37-mcp   /mnt/l0/usr/local/bin/p37-mcp
install -m 0755 dist/p36-bootstrap/p37-mount /mnt/l0/usr/local/bin/p37-mount
install -m 0644 dist/p36-bootstrap/config.toml /mnt/l0/usr/local/share/p37/config.toml
install -m 0644 dist/p36-bootstrap/p37d.service /mnt/l0/etc/systemd/system/p37d.service
systemctl enable p37d.service
```

`p37d.service` 使用 `User=p36`、`AmbientCapabilities=CAP_SYS_ADMIN`、
`NoNewPrivileges=yes`。`p37-mount` 用 `mount(2)`，不依赖 setuid `mount`。

MCP 客户端配置：

```json
{ "mcpServers": { "p37": { "command": "/usr/local/bin/p37-mcp" } } }
```

## 4. 版本升级

1. 构建并校验新的发布 tar（sha256 必须匹配）。
2. 生成新的 `P37PAYLOAD` 盘（同一 label，新版本目录）。
3. 停止实例（或按 P36 的方式替换第二块磁盘）。
4. 启动实例；`p37-run` 挂载新盘并校验 `manifest.version` 与 `p37d --version`。
5. 仓库格式不变，不需要 `p37 migrate`。

> 只 `systemctl restart p37d` 时，旧 payload 可能仍挂在 `/run/p37-payload`；
> **换盘请重启实例**。这是当前已知限制。

## 5. 维护窗口

`p37 gc --prune` 会删对象，而 daemon 的 `landMu` 只在进程内有效。daemon 运行
时 `p37 gc` 会直接拒绝（除非显式 `--force`）。维护窗口内先：

```bash
systemctl stop p37d
p37 gc /srv/repo --prune --pack
systemctl start p37d
```

`fsck`、`migrate`、`import` 等写命令也建议在 daemon 停止时执行。

## 6. 已知限制

- 只支持 linux/amd64 发布物；linux/arm64 只做编译校验。
- 在线 GC 未实现；没有 daemon 与 CLI 共享的 land 锁。
- payload 换盘需要重启实例，服务级 restart 不保证换盘。
- `p37d --stdio` 在启动时快照 daemon 工具清单；daemon 升级后客户端应重连。
- HTTP 模式仍强制 `auth_token`；stdio 模式不引入 token，依赖 socket `0600`。
  因此 MCP 客户端进程必须与 p37d 同 uid（默认都是 `p36`）；换用户会连不上。
- 没有自更新；不要试图在实例内替换 `/run/p37-payload` 下的文件。

## 7. P36 集成需要的字段

| 字段 | 值 |
|---|---|
| 发布物 | `p37-linux-amd64-<version>.tar.gz` |
| 校验 | 同名 `.sha256` |
| manifest | `version` / `commit` / `buildTime` / `goVersion` |
| payload 文件 | `payload.ext4`，ext4，label `P37PAYLOAD`，建议 32 MiB |
| payload 根 | `p37`、`p37d`、`manifest.json` |
| VM 创建参数（建议名） | `p37PayloadPath`、`p37PayloadVersion`、`p37PayloadSHA256` |
| 实例内仓库 | `/srv/repo`（L2 ext4，`p36:p36` 可写） |
| 挂载点 | `/run/p37-payload` |
| 运行时环境 | `P37_REPO=/srv/repo`、`P37_PAYLOAD_LABEL=P37PAYLOAD`、`TMPDIR=/srv/repo/.p37/tmp` |
