# P36 microVM 部署包

p37 在 P36 上的定位是「每个实例启动即有的版本控制工具」。p37/p37d **不烙进
L0/L1**，而是由宿主投递一块只读 ext4 小盘（label `P37PAYLOAD`），实例内一段
稳定的平台胶水把它挂到 `/run/p37-payload`，再执行盘上的 `p37d`。

本目录放的是 **平台胶水**（L0 或 L1 都可以）；**P36 使用 L1**，不改 L0。
只有 `p37` 和 `p37d` 来自 payload。P36 在 L1 缺失时应让创建/启动直接报错。

## 1. 分层与文件落点

```
L0 只读 Ubuntu（P36 不改 L0）

L1 应用层（P36 把平台胶水放这里）
├── /usr/local/bin/p37-run          # 等盘、挂载、init、exec p37d
├── /usr/local/bin/p37-mcp          # MCP 客户端入口（exec p37d --stdio）
├── /usr/local/bin/p37-mount        # Go 挂载助手（按 label 只读挂载）
├── /usr/local/share/p37/config.toml
├── /etc/systemd/system/p37d.service
├── /etc/sysctl.d/90-p37.conf
├── /etc/tmpfiles.d/p37.conf
│   （可选 root+overlay：config-overlay.toml + p37d-root-overlay.service）

P37PAYLOAD（宿主投递，只读 ext4，额外一块 virtio-blk）
P36 中设备顺序：
  vda = root
  vdb = P36AGENT
  vdc = P37PAYLOAD
├── p37
├── p37d
└── manifest.json

L2 /srv/repos（每实例可写 ext4；旧 /srv/repo 原地注册为 default）
├── .p37/                           # 元数据库、CAS、overlay upper、tmp
└── workspaces/                     # overlay 挂载点，或 auto 回退后的 copy 目录
```

L2 必须预先把 `/srv/repos` 建成 `p36:p36` 可写（建议 `0750`）；p37d 以 `p36`
运行，不做 root 提权。`/srv/repos` 与 `workspaces/`、`.p37/` 必须在同一个
ext4 上。

`p37d.service` 有 `ConditionPathIsDirectory=/srv/repos`，所以 **`/srv/repos`
必须在服务启动前存在**。P36 的 L1 `install.sh` 负责创建它；不存在时服务不
启动是故意的 fail-fast。

默认 `materialize = "auto"`：先尝试 OverlayFS，失败自动回退 copy。
Ubuntu 26.04 非 root 下 overlay 可能 `EPERM`（即使有 `CAP_SYS_ADMIN`），
这时工作区是 `/srv/repos/<repo_id>/workspaces` 下的普通目录，不是 overlay。
copy 模式每个工作区是一份完整树；默认 `max_active=8`、`max_hidden=32`、
`warn_disk_bytes=8 GiB`，
按实际仓库体积和 L2 容量调整。

## 1.1 应用卷生命周期（热插拔）

P36 可以把「每应用一个持久卷」做成 qcow2，作为额外 virtio-blk 热加给运行中的
VM，guest 内格式化为 ext4 并挂到 `/srv/repos/<app>`。卷内就是 p37 的 repo 根
（`.p37/`、`workspaces/` 等）。VM 可随时丢弃，应用卷是持久真相源；p37 看到的
就是本地 ext4，不是 NFS。

标准流程：

```text
attach:
  宿主把 qcow2 作为 virtio-blk 热加给运行中的 VM
  guest mount /dev/vdX /srv/repos/app-a
  p37 init /srv/repos/app-a --if-missing   # 旧 P36 入口；内部原子初始化/修复
  # 或（新 P36）：MCP repo_init(mode=empty) 后再 repo_register
  # 或：p37 --root /srv/repos repo init app-a /srv/repos/app-a

detach:
  p37 --root /srv/repos repo unregister app-a
  guest umount /srv/repos/app-a
  宿主 remove-device（之后才可删除/回收 qcow2）

删除应用卷:
  repo unregister → guest umount → 宿主删卷
  不要对挂载点调用 repo_delete
```

`repo_unregister` 会先卸载该 repo 的 overlay workspace、关闭 SQLite、释放
per-repo flock，再写 registry tombstone；`.p37`、workspaces 和 repo 数据全部
保留。卷重新挂载后执行 `repo_register` 即可恢复，id/name 不变。

0.4.4 起 `repo_register` **只注册完整仓库**，不再创建半初始化 DB。path 下没有
`.p37/p37.db` 或 DB 缺 head/line/refs 时：

- 默认返回 `repo_not_initialized`/`repo_db_corrupt`，`detail.next_action` 指向
  `repo_init`/`repo_init_repair`；
- `--init-if-missing` 仍保留（deprecated），内部等价于
  `repo_init(mode=empty)`；完整失败会回滚；
- 新代码建议先 `repo_init` 或 `p37 init`，成功后再 register；半成品有用户文件时
  必须显式 `repo_init(mode=import)`，v0 非空且内容不同再显式 `force=true`。

**初始化失败时 P36 必须保持卷挂载、不删 `.p37`**，返回 409 +
`mounted=true` + `initialized=false` + `next_action`；用户可重试 repair。只有
卷本身无法挂载/损坏才 umount。P36 `/ready` 用
`repo_health {path, with_quota:false}` 消费 `initialized/refs_ok/refs_match/
next_action`；default line 的 land/merge/reset/revert/sync 已同步 refs，
`refs_match=true` 可作为就绪门槛。

`.p37/repo.json` 是仓库身份锚（`id` / `name` / `default_line`）：

- 有 `.p37/p37.db` 但没有 repo.json：`repo_register` 会生成并写入；
- repo.json 已存在：以其中的 id/name 为准，调用方 name 冲突返回
  `repo_name_mismatch`；
- 不要手工删除 repo.json。它缺失时 `repo_scan` 会把 registry 行标记为
  degraded，但不会删数据。

`repo_scan` 用于 daemon 启动后或卷变更后 reconcile：

```bash
p37 --root /srv/repos repo scan [--dry-run] [--prune-missing] [--json]
```

- 扫描 `/srv/repos` 的直接子目录，跳过隐藏目录、非目录和 symlink；
- 未注册但 repo.json 有效的目录加入 registry；
- 已注册但卷不在：默认标记 degraded，`--prune-missing` 才删除 registry 行；
- 已注册但 repo.json 缺失：标记 degraded，不删数据；
- 已注销（tombstone）的 repo 不会自动复活，必须显式 `repo register`。

启动时 `p37d --root` 会自动做一次 `repo_scan`（不 prune、不删除），所以
预先挂载好的应用卷不需要重启 daemon 就能出现在 `repo_list`。

`repo_list` / `repo_info` 返回：

- `path`：repo 根；
- `available`：path 是否存在；
- `mounted`：path 是否为挂载点；
- `degraded` / `degraded_msg`：卷缺失或 repo.json 缺失等。

这些字段只做轻量 stat/mount 探测，不会为了列表去打开 repo。

CLI 在 `/srv/repos/.p37/p37d.sock` 可连接时必须通过 daemon 执行
register/unregister/scan；socket 不存在或已失效时才直连 registry。socket 权限
`0600`，CLI 必须以运行 p37d 的同一 uid（默认 `p36`）执行。

## 2. 构建与发布物

p37 仓库：

```bash
make release VERSION=0.1.0
# dist/p37-linux-amd64-0.1.0.tar.gz
# dist/p37-linux-amd64-0.1.0.tar.gz.sha256

make p36-bootstrap
# dist/p37-mount-linux-amd64
# dist/p36-bootstrap/{p37-run,p37-mcp,p37-mount,config.toml,p37d.service,90-p37.conf,tmpfiles-p37.conf}
# 可选 root+overlay：config-overlay.toml、p37d-root-overlay.service
```

发布 tar 的根目录严格只有：

```text
p37
p37d
manifest.json
```

`manifest.json` 含 `version`、`commit`、`buildTime`、`goVersion`。
发布物由 GitHub Actions 从干净提交构建：tag `v*` 触发
`.github/workflows/release.yml`（build → P36 验收 → Release）；
`.github/workflows/p36-acceptance.yml` 可对已发布 Release 再跑一遍验收。

## 3. 宿主生成 payload

payload 固定 **96 MiB**。32 MiB 的 ext4 扣掉 journal/元数据后放不下约 30 MiB
的两个二进制，会 ENOSPC。

```bash
VERSION=0.1.0
rm -rf /tmp/p37-payload-seed
mkdir -p /tmp/p37-payload-seed
tar -xzf p37-linux-amd64-$VERSION.tar.gz -C /tmp/p37-payload-seed

truncate -s 96M payload.ext4
mkfs.ext4 -q -F -L P37PAYLOAD -m 0 -d /tmp/p37-payload-seed payload.ext4
# mkfs.ext4 -d 可能创建 lost+found；若要求 payload 根目录严格只有
# p37/p37d/manifest.json，用 debugfs 删掉它（不需要挂载）：
debugfs -w -R "rmdir /lost+found" payload.ext4 >/dev/null 2>&1 || true
chmod 0444 payload.ext4
```

payload 作为额外一块只读 virtio-blk 投递；P36 中它是第三块（`vdc`），
第二块是 `P36AGENT`（`vdb`）。`p37-mount` 按 label 挂载，不依赖设备顺序。

建议放在该实例 `image_store` 池下，例如 `<pool>/p37/<version>/payload.ext4`，
不要与 `P36AGENT` 混用同一目录。

## 4. L1 安装

以下示例把文件安装到目标 L1 的根目录（`/mnt/l1` 是挂载点，按 P36 的实际
构建方式替换）：

```bash
install -m 0755 dist/p36-bootstrap/p37         /mnt/l1/usr/local/bin/p37
install -m 0755 dist/p36-bootstrap/p37-run     /mnt/l1/usr/local/bin/p37-run
install -m 0755 dist/p36-bootstrap/p37-mcp     /mnt/l1/usr/local/bin/p37-mcp
install -m 0755 dist/p36-bootstrap/p37-mount   /mnt/l1/usr/local/bin/p37-mount
install -m 0644 dist/p36-bootstrap/config.toml /mnt/l1/usr/local/share/p37/config.toml
install -m 0644 dist/p36-bootstrap/p37d.service /mnt/l1/etc/systemd/system/p37d.service
install -m 0644 dist/p36-bootstrap/90-p37.conf /mnt/l1/etc/sysctl.d/90-p37.conf
install -m 0644 dist/p36-bootstrap/tmpfiles-p37.conf /mnt/l1/etc/tmpfiles.d/p37.conf

# 可选：root + overlay 变体（默认不装）
# install -m 0644 dist/p36-bootstrap/config-overlay.toml /mnt/l1/usr/local/share/p37/config-overlay.toml
# install -m 0644 dist/p36-bootstrap/p37d-root-overlay.service /mnt/l1/etc/systemd/system/p37d-root-overlay.service
```

P36 的 L1 `install.sh` 还应：

- 创建 `/srv/repos`（`p36:p36`，`0750`）；旧 `/srv/repo` 可选保留用于迁移；
- 确保 `p36` 能读 `/dev/disk/by-label/P37PAYLOAD` 对应的块设备
  （`SupplementaryGroups=disk` 或 udev 规则）；
- 执行 `sysctl --system`（或确保实例启动时应用 `/etc/sysctl.d/90-p37.conf`）；
- 执行 `systemd-tmpfiles --create /etc/tmpfiles.d/p37.conf` 创建
  `/run/p37-payload`（属主 `p36:p36`）；
- `systemctl enable p37d.service`。

`p37d.service` 使用 `User=p36`、`AmbientCapabilities=CAP_SYS_ADMIN`、
`NoNewPrivileges=yes`、`materialize=auto`。`p37-mount` 用 `mount(2)`，不依赖
setuid `mount`。

如果 P36 明确允许 root，且必须使用 OverlayFS，可启用可选的
`p37d-root-overlay.service`（`User=root` + `config-overlay.toml`）。它与默认
unit 互斥；默认部署仍保持 `User=p36` + auto。

> **不要**给 unit 加 `PrivateTmp`、`ProtectSystem=strict`、`ProtectHome`、
> `ReadWritePaths`、`ProtectKernel*`、`ProtectControlGroups` 等会创建私有
> mount namespace 的选项。p37d 在里面挂载 payload 和 workspace overlay，
> agent 的 shell 与 `p37-mcp` 在宿主 namespace，看不到私有 namespace 的挂载。
>
> unit **不使用** `RuntimeDirectory=`，而用 `/etc/tmpfiles.d/p37.conf` 创建
> `/run/p37-payload`。原因是：service 重启时 systemd 会尝试在 RuntimeDirectory
> 路径上重新创建/chmod 目录，而它已经是只读 payload 的挂载点，会报
> `status=233/RUNTIME_DIRECTORY`（EROFS）。tmpfiles 不参与 unit 生命周期，
> 停服后 payload 仍挂载；维护窗口可以直接使用 `/run/p37-payload/p37 gc`。
> 换 payload 盘仍需重启实例。需要彻底卸载时手工执行
> `systemctl stop p37d && sudo umount /run/p37-payload`。

MCP 客户端配置：

```json
{ "mcpServers": { "p37": { "command": "/usr/local/bin/p37-mcp" } } }
```

`p37-mcp` 会执行 `/run/p37-payload/p37d --stdio --root /srv/repos`。它不取
仓库锁、不自动启动 daemon；socket 权限 `0600`，所以 MCP 客户端必须与 p37d
同 uid（默认都是 `p36`）。

## 5. 版本升级

1. 从 GitHub Release 下载新的 tar 与 sha256，校验 sha256。
2. 生成新的 `P37PAYLOAD` 盘（同 label，新版本目录）。
3. 停止实例（或按 P36 的方式替换第三块磁盘 `vdc`）。
4. 启动实例；`p37-run` 挂载新盘并校验 `manifest.version` 与 `p37d --version`。
5. 打开仓库时自动处理格式迁移（v1 → v2 会先备份 `p37.db`）；一般不需要
   手工执行 `p37 migrate`。迁移与回滚见 `docs/format-v2.md`。

> **从 v0.1.12 升级到 v0.1.13**：已有仓库的 `.p37/config.toml` 可能仍写着
> `materialize = "overlay"`。`p37 init --if-missing` 不会覆盖现有配置；
> 请先把它改成 `auto`（或 `copy`），否则 p37d 仍会按 overlay 启动并失败。
> 新实例使用 v0.1.13 的模板会自动是 auto。

> 只 `systemctl restart p37d` 时，旧 payload 可能仍挂在 `/run/p37-payload`；
> **换盘请重启实例**。这是当前已知限制。

仅修改平台胶水（`p37-run`/`p37-mcp`/`p37-mount`/unit/config/sysctl）时，
需要重建 **L1**；日常升级 p37 只换 payload 盘。

## 6. 维护窗口

`p37 gc --prune` 会删对象，而 daemon 的 `landMu` 只在进程内有效。daemon 运行
时 `p37 gc` 会直接拒绝（除非显式 `--force`）。维护窗口内先：

```bash
systemctl stop p37d
p37 --root /srv/repos --repo <id|name> gc --prune --pack
systemctl start p37d
```

停服后 payload 仍挂载，所以 `/run/p37-payload/p37` 仍然可用。`fsck`、`migrate`、
`import` 等写命令也建议在 daemon 停止时执行。

## 7. `p37d --check` 的边界

`p37d --check` 适合 **daemon 启动前** 做部署检查。它有两种模式，检查范围不同：

- `p37d --check --repo <root>`：**完整健康检查**。`meta.Open` 打开 SQLite
  （可能产生 WAL/SHM、权限收紧）；在 `.p37/tmp`、`workspaces` 下创建并删除
  探测文件；做一次 OverlayFS probe；读取 inotify/磁盘信息。
- `p37d --check --root <root>`：**registry 级检查**。只检查每个已注册 repo 的
  `.p37/p37.db` 是否存在；`--strict` 时打开 repo 并走一遍 line 链。它不做写
  探测、OverlayFS probe 或 inotify/磁盘检查。

`p37-run` 启动前调用的是非 strict 的 `--check --root`，只确认 registry 和
DB 可见；如果要在部署阶段做完整健康检查或 line 链检查，需另外执行
`p37d --check --strict --repo <root>`（完整）或
`p37d --check --strict --root <root>`（registry）。

退出码：`0=通过`、`1=必需项失败`、`2=用法`。`--strict` 把 ext4（`--repo` 模式）
或 line 链（`--root` 模式）视为必需项。inotify 不足只 WARN，不会自动修改内核
参数。

`--check --repo` 的 OverlayFS probe 需要 `CAP_SYS_ADMIN`。在 P36 里可由 systemd
unit / `p37-run` 提供；手工执行时要带同样的能力（例如 `setpriv --ambient-caps
+sys_admin` 或 `p37-run` 的 unit 环境），不要用 root 直接跑后留下 root 属主的
`.p37` 子目录。

auto 模式下 OverlayFS 不可用只产生 `WARN`，`ok` 仍为 true；完整部署门禁用
`p37d --check --strict --repo <root>`。`p37 doctor` 的 OverlayFS 判定更严格，
copy 模式下会显示 `[FAIL] OverlayFS`，这是预期，不应据此判定 P36 部署失败。

如果后续需要在线监控，应单独实现只读检查，而不是高频调用 `--check`。

## 8. 已知限制

- 只支持 linux/amd64 发布物；linux/arm64 只做编译校验。
- 在线 GC 未实现；没有 daemon 与 CLI 共享的 land 锁。
- payload 换盘需要重启实例，服务级 restart 不保证换盘。
- `p37d --stdio` 在启动时快照 daemon 工具清单；daemon 升级后客户端应重连。
- HTTP 模式仍强制 `auth_token`；stdio 模式不引入 token，依赖 socket `0600`。
  因此 MCP 客户端进程必须与 p37d 同 uid（默认都是 `p36`）；换用户会连不上。
- 没有自更新；不要试图在实例内替换 `/run/p37-payload` 下的文件。
- `repo_unregister` 不等待已经进入执行中的 MCP 调用结束；detach 前应由 P36
  在上层确认应用空闲。活跃 workspace/lease 会被 `repo_busy` 挡住。
- `repo_delete` 拒绝挂载点、root 自身和 root 外路径；应用卷删除只能走
  `repo_unregister → guest umount → 宿主删卷`。
- `p37d --check --repo` 有写探测，不适合当作在线探针；`--check --root` 只做
  registry/DB/line 检查，也不应高频轮询。
- Ubuntu 26.04 非 root 下 OverlayFS 可能 `EPERM`（即使有 ambient
  `CAP_SYS_ADMIN`）。默认 auto 会回退 copy；copy 模式磁盘占用 ≈ 仓库树 ×
  活跃工作区数，必须按 L2 容量收紧 `max_active` / `warn_disk_bytes`。
- 默认 acceptance **不再自动 root fallback**；只有显式
  `P37_ALLOW_ROOT_FALLBACK=1` 才退化 root 做对照。真实 P36 仍应验证
  `User=p36` + auto copy；root+overlay 用可选变体。
- `p37 doctor` 在 copy 模式下会报 `[FAIL] OverlayFS`，这是检查器的既有严格
  判定；P36 完整部署门禁看 `p37d --check --strict --repo <root>`，多仓库
  registry 检查用 `--check --strict --root <root>`。

## 9. P36 集成需要的字段

| 字段 | 值 |
|---|---|
| 发布物 | `p37-linux-amd64-<version>.tar.gz` |
| 校验 | 同名 `.sha256` |
| manifest | `version` / `commit` / `buildTime` / `goVersion` |
| payload 文件 | `payload.ext4`，ext4，label `P37PAYLOAD`，**固定 96 MiB** |
| payload 根 | `p37`、`p37d`、`manifest.json` |
| 设备顺序 | 额外一块；P36 中 `vdc`（`vda=root`，`vdb=P36AGENT`） |
| VM 创建参数（建议名） | `p37PayloadPath`、`p37PayloadVersion`、`p37PayloadSHA256` |
| 实例内仓库根 | `/srv/repos`（L2 ext4，`p36:p36` 可写，建议 `0750`） |
| 挂载点 | `/run/p37-payload` |
| 工作区模式 | 默认 `workspace.materialize = "auto"`；非 root overlay EPERM 时回退 copy |
| 运行时环境 | `P37_ROOT=/srv/repos`、`P37_REPO=/srv/repo`、`P37_PAYLOAD_LABEL=P37PAYLOAD`、`TMPDIR=/srv/repos/.p37/tmp` |
| L1 必需文件 | `p37`、`p37-run`、`p37-mcp`、`p37-mount`、`config.toml`、`p37d.service`、`90-p37.conf`、`tmpfiles-p37.conf` |
| L1 可选文件 | `config-overlay.toml`、`p37d-root-overlay.service`（root+overlay 变体） |

## v0.2 多仓库变更（format_ver=2）

- 运行根目录改为 `/srv/repos`；`p37d --root /srv/repos`。
- 控制面：`/srv/repos/.p37/{registry.db,p37d.sock,tmp,trash}`。
- 每个 repo 目录：`/srv/repos/<repo_id>`；repo id 为 `repo_<32 hex>`。
- 旧 `/srv/repo` 不移动，daemon 启动时原地注册为 `default`；单仓库
  `p37d --repo /srv/repo` 仍可用。
- L1 需要预创建 `/srv/repos`（`p36:p36 0750`），并安装：
  - `/usr/local/bin/p37`（payload 内 CLI 的稳定入口）
  - `/usr/local/bin/p37-run`、`/usr/local/bin/p37-mcp`
- `p37-run` 默认从 `/usr/local/lib/p37` 找二进制；找不到时回退
  `/run/p37-payload`。
- `p37-mcp` 默认 `--stdio --root /srv/repos`；`/srv/repos/.p37` 不存在时
  回退单仓库 `--repo /srv/repo`。
- `p37d.service`：`ConditionPathIsDirectory=/srv/repos`，
  `Environment=P37_ROOT=/srv/repos`，`TMPDIR=/srv/repos/.p37/tmp`。
- 发布物 `dist/p36-bootstrap` 新增 `p37` wrapper。


## 多仓库 CLI（v0.2+）

```bash
p37 --root /srv/repos repo list --json
p37 --root /srv/repos repo create app
p37 --root /srv/repos repo info app --json
p37 --root /srv/repos --repo app status
p37 --root /srv/repos --repo app doctor
p37 --root /srv/repos --repo app ws-list
p37 --root /srv/repos --repo app gc --prune --pack
```

P36 acceptance 现在创建 `/srv/repos` + `accept` repo，并把 `/srv/repo`
初始化为 legacy repo 验证自动注册为 `default`。CI 使用：

```text
P37_ROOT=/srv/repos
P37_REPO=/srv/repo
P37_REPO_NAME=accept
```
