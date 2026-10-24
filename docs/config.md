# 配置参考

配置文件位于 `<repo>/.p37/config.toml`；多仓库模式下 daemon 读取
`<root>/.p37/config.toml` 作为新 repo 的模板和 daemon 默认值。

- 文件不存在时使用 `internal/config.Default()` 的默认值。
- 配置是可选的；未知字段会被 TOML 解码忽略。
- 格式资产（CDC、编码、对象类型号）**不能在配置里改**。
- 凭证（S3 access key/secret）只从环境变量读，不写配置文件。

```toml
[repo]
format_ver = 2
# ignore_extra = ["*.log", "dist/"]   # 追加到内置忽略规则之后
# export_root = "/srv/repos/.p37/tmp/p34-deploy"

[files]
max_file_size = 2147483648
max_text_size = 4194304
binary_probe_bytes = 8000
max_path_bytes = 4096
max_segment_bytes = 255

[workspace]
materialize = "auto"
base_cache_bytes = 4294967296
scan_interval = "30s"
debounce = "500ms"
stable_retries = 5
stable_delay = "200ms"
max_active = 8
max_depth = 32
max_fanout = 16
# max_hidden = 32
# inotify_fallback = false
warn_disk_bytes = 12884901888

[merge]
context_lines = 3
move_similarity = 0.6
max_matches = 64
max_chain_length = 64

[retention]
keep_versions = 200
keep_epochs = 20
epoch_size = 50

[daemon]
# socket = "/srv/repos/.p37/p37d.sock"
listen_http = false
http_addr = "127.0.0.1"
http_port = 7757
# auth_token = "..."                 # HTTP 模式必填
metrics_http = false
log_level = "info"
land_queue_max = 256

[journal]
redact_task_desc = false

[gc]
scrub_sample = 0.01
verify_on_read = "sample"            # off | sample | always

[remote]
enabled = false
backend = "s3"                       # s3 | dir
# endpoint = "https://s3.example.com"
# region = "us-east-1"
# bucket = "p37"
# prefix = "repos/demo"
# remote_key = "rk_..."
# path_style = true
# auto_push = false
# dir = "/tmp/p37-remote"            # backend=dir 时使用
outbox = ".p37/sync/outbox"
min_versions = 10
min_bytes = 4194304
# checkpoint_every = 50
compress_level = 9
max_package_bytes = 536870912
# auto_prepare_every = "1h"
```

## 默认值

| 字段 | 默认值 | 说明 |
|---|---|---|
| `repo.format_ver` | `2` | 期望的仓库格式；`p37 init` 用它校验模板 |
| `repo.ignore_extra` | `[]` | 追加忽略规则；后匹配覆盖先匹配 |
| `repo.export_root` | `""` | 非空时 `repo_export` 的 `dest_dir` 必须位于其下 |
| `files.max_file_size` | 2 GiB | 单文件上限 |
| `files.max_text_size` | 4 MiB | 文本 diff/行视图上限 |
| `files.binary_probe_bytes` | 8000 | 二进制探测字节数 |
| `files.max_path_bytes` | 4096 | 路径总长度上限 |
| `files.max_segment_bytes` | 255 | 单段路径长度上限 |
| `workspace.materialize` | `auto` | `auto` / `overlay` / `copy` |
| `workspace.base_cache_bytes` | 4 GiB | overlay 基底缓存上限 |
| `workspace.scan_interval` | `30s` | 周期扫描间隔；多工作区轮转 |
| `workspace.debounce` | `500ms` | fs 事件合并窗口 |
| `workspace.stable_retries` | `5` | 文件稳定性重试次数 |
| `workspace.stable_delay` | `200ms` | 稳定性重试间隔 |
| `workspace.max_active` | `8` | 用户可见工作区上限；硬上限 64 |
| `workspace.max_depth` | `32` | 工作区栈最大深度 |
| `workspace.max_fanout` | `16` | 单父工作区直接子工作区上限 |
| `workspace.max_hidden` | `32` | 隐藏 merge/preview 上限；代码里 0 表示取 32 |
| `workspace.inotify_fallback` | `false` | inotify 额度不足时是否降级为只监听根目录 |
| `workspace.warn_disk_bytes` | 12 GiB | 剩余空间告警阈值；P36 模板改为 8 GiB |
| `merge.context_lines` | `3` | hunk 上下文行数 |
| `merge.move_similarity` | `0.6` | 块移动相似度阈值 |
| `merge.max_matches` | `64` | place 匹配上限 |
| `merge.max_chain_length` | `64` | 重命名/移动链长度上限 |
| `retention.keep_versions` | `200` | 每条 line 保留的祖先版本数 |
| `retention.keep_epochs` | `20` | 保留的 epoch 数 |
| `retention.epoch_size` | `50` | 每个 epoch 的版本数 |
| `daemon.socket` | `""` | 空则 `<root>/.p37/p37d.sock`；多仓库模式固定用该路径 |
| `daemon.listen_http` | `false` | 是否额外监听 HTTP |
| `daemon.http_addr` | `127.0.0.1` | HTTP 监听地址 |
| `daemon.http_port` | `7757` | HTTP 监听端口 |
| `daemon.auth_token` | `""` | HTTP 模式必填；未配置时 fail-closed |
| `daemon.metrics_http` | `false` | 在 HTTP 监听器上挂 `/metrics`（同样要求 Bearer token） |
| `daemon.log_level` | `info` | 日志级别 |
| `daemon.land_queue_max` | `256` | land 队列容量；满返回 `land_queue_full` |
| `journal.redact_task_desc` | `false` | 为真时 journal 不落任务描述原文 |
| `gc.scrub_sample` | `0.01` | scrub 抽样比例 |
| `gc.verify_on_read` | `sample` | 读时校验策略：off/sample/always |
| `remote.enabled` | `false` | 是否启用远端同步 |
| `remote.backend` | `s3` | `s3` 或 `dir` |
| `remote.endpoint` | `""` | S3 兼容 endpoint |
| `remote.region` | `""` | S3 region |
| `remote.bucket` | `""` | bucket |
| `remote.prefix` | `""` | 仓库前缀；实际远端根是 `<prefix>/<remote_key>` |
| `remote.remote_key` | `""` | 空时首次 push 生成 `rk_<32 hex>` |
| `remote.path_style` | `true` | S3 path-style |
| `remote.auto_push` | `false` | land 后异步便利推送；不承担 durable 语义 |
| `remote.dir` | `""` | `backend=dir` 的本地远端目录 |
| `remote.outbox` | `.p37/sync/outbox` | 本地暂存包目录（相对 repo 根） |
| `remote.min_versions` | `10` | delta 包最少版本数 |
| `remote.min_bytes` | 4 MiB | delta 包最少字节数 |
| `remote.checkpoint_every` | `0` | >0 时每 N 个版本强制 checkpoint |
| `remote.compress_level` | `9` | 同步 payload 的 zstd 级别 |
| `remote.max_package_bytes` | 512 MiB | 单个 payload 分片上限 |
| `remote.auto_prepare_every` | `""` | daemon 定时 prepare 间隔；只暂存不上传 |

## 环境变量

| 变量 | 用途 |
|---|---|
| `P37_ROOT` | P36 多仓库根目录（默认 `/srv/repos`） |
| `P37_REPO` | legacy 单仓库路径（默认 `/srv/repo`） |
| `P37_PAYLOAD_LABEL` | payload 盘 label（默认 `P37PAYLOAD`） |
| `P37_PAYLOAD_MOUNT` | payload 挂载点（默认 `/run/p37-payload` 或 L1 的 `/usr/local/lib/p37`） |
| `P37_CONFIG` | L1 配置模板路径 |
| `P37_S3_ACCESS_KEY` / `P37_S3_SECRET_KEY` | S3 凭证；只从环境变量读 |
| `P37_S3_SESSION_TOKEN` | 可选临时凭证 |
| `P37_FORCE_HASH` | 测试/CI 用：关闭 stat 快路径 |
| `P37_ALLOW_ROOT_FALLBACK` | P36 验收可选：允许 root + overlay 对照 |

## 注意

- `warn_disk_bytes` 的代码默认是 12 GiB；P36 模板为了 20GB L2 改成 8 GiB。
  文档里看到 8 GiB 时，指的是 P36 模板。
- `max_active` / `max_depth` / `max_fanout` 只能调小，不能超过硬上限。
- 多仓库 daemon 的 unix socket 固定为 `<root>/.p37/p37d.sock`；不要在多仓库
  配置里改 `daemon.socket`，否则 `p37d --stdio` 可能连接到不同路径。
- `auth_token` 不要写进随镜像分发的配置；由实例侧注入。
- 配置里的相对路径（`outbox` 等）相对 repo 根解析。
