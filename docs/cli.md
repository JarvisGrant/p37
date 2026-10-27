# 命令行参考

业务主路径是 MCP；`p37` 用于初始化、运维、调试，`p37d` 是 daemon / stdio 代理。

## 退出码

| 码 | 含义 |
|---|---|
| `0` | 成功 |
| `1` | 运行时错误（仓库不存在、冲突、检查失败等） |
| `2` | 用法错误（缺参数、未知参数、互斥参数） |

`p37d --check` 的 `1` 表示有必需项失败；用法错误同样是 `2`。

## p37 全局参数

```bash
p37 <命令> [参数]                                  # 单仓库：参数里带 <root>
p37 --root <root> repo <子命令> ...                # 多仓库维护
p37 --root <root> --repo <id|name> <命令> [参数]   # 对指定 repo 执行
```

- `--root` 与 `--repo` 可以写成 `--root=/srv/repos` 形式。
- 多仓库模式除 `repo` 子命令外必须提供 `--repo`；CLI 会先把 id/name 解析成
  路径，再执行命令。
- `p37 --root ... init` 被拒绝；多仓库请用 `repo init <name> <path>`（应用卷）或 `repo create`（L2 scratch）。
- CLI 不会自动启动 daemon。daemon 持锁时，`gc` 会拒绝；其他写命令应在维护
  窗口执行（见 [`operations.md`](operations.md)）。

## 初始化与运维

| 命令 | 说明 |
|---|---|
| `p37 init <root> [--if-missing] [--mode empty\|import\|repair] [--default-line main] [--name <name>] [--config <file>] [--allow-nonpersistent]` | 原子初始化/修复仓库；`--if-missing` 完整则跳过、半成品则修复、不存在则完整初始化；绝不覆盖已有 config |
| `p37 init <root> --strict` | 只校验完整性，不写库、不修复 |
| `p37 init <root> --no-repair` | 诊断：看到半成品直接报错，不修复 |
| `p37 import <root> <srcdir>` | 把目录树作为 v0 导入（仅空仓库） |
| `p37 status <root>` | 仓库摘要 |
| `p37 doctor <root>` | 部署自检（比 `--check --repo` 更严格地判定 OverlayFS） |
| `p37 fsck <root>` | 校验对象完整性与封闭性 |
| `p37 gc <root> [--prune] [--scrub] [--pack] [--report-roots] [--workspaces] [--force]` | 保留策略 + 对象回收 + 抽样校验；默认预演 |
| `p37 migrate <root>` | 打开仓库并走自动迁移；已是最新则提示无需迁移 |
| `p37 ignore-check <root> <path>` | 检查路径是否被忽略 |

GC：

- 默认只预演；`--prune` 才真的删版本行/对象。
- `--pack` 需要配合 `--prune`；否则只提示。
- `--workspaces` 只清理孤儿工作区目录。
- daemon 持锁时拒绝；`--force` 跳过锁检查并打印警告。
- 速度/空间细节见 [`operations.md`](operations.md)。

## 多仓库维护

```bash
p37 --root /srv/repos repo list [--json]
p37 --root /srv/repos repo init <name> <path> [--mode empty|import|repair] [--default-line main] [--allow-nonpersistent] [--json]
p37 --root /srv/repos repo create <name> [--default-line main] [--remote-key rk_...] [--remote-prefix <p>] [--json]  # 不传 path 是 L2 scratch
p37 --root /srv/repos repo info <id|name> [--json]
p37 --root /srv/repos repo delete <id|name> [--force]
p37 --root /srv/repos repo register <name> <path> [--init-if-missing] [--default-line main] [--json]
p37 --root /srv/repos repo unregister <id|name> [--force] [--json]
p37 --root /srv/repos repo scan [--no-reconcile] [--prune-missing] [--dry-run] [--repair] [--json]
```

`register` / `unregister` / `scan` 在 socket 可连接时通过 daemon 执行；socket
不存在或已失效时才直连 registry。socket 权限 `0600`，CLI 必须与 p37d 同 uid。

## 工作区调试命令

这些命令直接打开仓库目录，主要给开发和真机排障用；产品路径用 MCP。

```bash
p37 ws-open <root> <task> [--materialize auto|overlay|copy] [--parent <id>]
p37 ws-list <root>
p37 ws-info <root> <ws_id>
p37 ws-sync <root> <ws_id>
p37 ws-diff <root> <ws_id>
p37 ws-land <root> <ws_id>
p37 ws-drop <root> <ws_id> [--cascade=false]
p37 log <root> [limit]
p37 show <root> <version_id|前缀|head>
p37 revert <root> <version_id|前缀|head>
p37 blame <root> <path> [--range a-b] [--version <id>] [--hunk]
```

`show` / `revert` / `blame --version` 接受完整 id 或唯一前缀；`log` 打印 16 位
短 id，可直接回喂。

## 同步

```bash
p37 sync <root> prepare [--full] [--force]
p37 sync <root> list
p37 sync <root> show <id|last> [--verify]
p37 sync <root> status [--remote]
p37 sync <root> push [--all|<id>]
p37 sync <root> fetch
p37 sync <root> pull
p37 sync <root> prune [--local] [--remote]
```

多仓库模式：

```bash
p37 --root /srv/repos --repo <name> sync <子命令> ...
```

语义见 [`sync-contract.md`](sync-contract.md)。

## 版本

```bash
p37 version          # p37 version=... commit=... buildTime=... goVersion=... formatVer=2
p37d --version
```

## p37d

```bash
p37d --repo <root> [--http]                          # 单仓库 daemon
p37d --root <dir> [--http]                           # 多仓库 daemon
p37d --stdio (--repo <root> | --root <dir>) [--connect-timeout 15s]
p37d --check (--repo <root> | --root <dir>) [--json] [--strict]
p37d --version
```

| 参数 | 说明 |
|---|---|
| `--repo` | 单仓库根目录；与 `--root` 互斥 |
| `--root` | 多仓库根目录（通常是 `/srv/repos`） |
| `--http` | 额外监听 HTTP；要求配置 `daemon.auth_token` |
| `--stdio` | 连接已有 daemon 的 unix socket，作为 stdio MCP 代理；不取仓库锁、不启动 daemon |
| `--check` | 启动前检查；`--repo` 是完整健康检查，`--root` 是 registry 级检查 |
| `--json` | `--check` 输出 JSON |
| `--strict` | 把 ext4 / line 链等视为必需项 |
| `--connect-timeout` | `--stdio` 等待 socket 的时长 |

`p37d --stdio` 在启动时快照 daemon 工具清单；daemon 升级后客户端应重连。
`p37d --check` 有写探测，不适合作为在线高频探针。
