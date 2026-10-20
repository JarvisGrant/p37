# P37 实现进度

> 设计规格为内部资料，不随本仓库发布
> 最近更新：2026-10-19

## 当前状态

| 里程碑 | 状态 | 说明 |
|---|---|---|
| **M1 存储底座** | ✅ 完成 | 附录 L.1 的全部单元/属性测试通过 |
| **M2 树与 op** | 🟡 主体完成 | 缺：行级移动检测；fs watcher 已实现（`internal/watch`） |
| **P2 归因** | ✅ 完成 | `blame`（§12.2）行级归因 + 缓存 |
| **M3 工作区与整合** | ✅ 完成 | OverlayFS 已在真机（Ubuntu 26.04 / ext4）验证；重启重挂与未挂载防护已补齐 |
| **M4 冲突** | ✅ 核心完成 | place + 冲突对象 + resolution + MCP 工具层已打通；候选解 3 条 + 重叠自动合并 |
| **M5 栈与回退** | ✅ 完成 | 折叠集合、revert、journal；三层栈 + 分叉端到端用例齐备（并借此修掉 S20） |

测试：`go test ./...` 全绿（**166 个测试**）。约 21,400 行 Go（含测试）。
交叉编译：`darwin/amd64`、`linux/amd64`、`linux/arm64` 全部通过。

**已可用的端到端路径**（本机用 `copy` 模式验证通过）：

```
p37 init → p37 import → ws_open → 直接改文件 → ws_sync → land_preview → ws_land
                                                        ↓ 有冲突
                                        conflict_show → conflict_resolve → ws_land
```

以及 `version_revert`、`version_log/show/diff`、`ws_drop`、`fsck`、`doctor`、`gc --workspaces`。
MCP 层有进程内端到端测试（`internal/mcpsrv`），覆盖工具清单、闭环、冲突、revert、错误码映射。

**真机验证清单见 [`VERIFY.md`](VERIFY.md)。**

## P36 可部署性适配（本轮）

目标：把 p37 做成 P36 microVM 里“实例启动即有”的版本控制工具，同时**不改
仓库格式与核心版本控制语义**。`format_ver` 仍为 1，`p37 migrate` 没有新增
迁移路径；改动集中在外围。

| 改动 | 落点 |
|---|---|
| 构建信息可注入 | `internal/buildinfo`；`p37`/`p37d --version` 输出 `version/commit/buildTime/goVersion`；修掉此前 `-X` 打在 `const` 上静默失效的问题 |
| 静态发布物 | `make release` 产出 `dist/p37-linux-amd64-<version>.tar.gz`（根目录只有 `p37`/`p37d`/`manifest.json`）与 `.sha256` |
| stdio MCP 代理 | `internal/mcpstdio` + `p37d --stdio`：连接同仓库 daemon 的 unix socket，不取仓库锁、不启动 daemon；多客户端各自独立 session |
| 幂等初始化 | `p37 init --if-missing`、`p37 init --config <file>`；已有仓库绝不覆盖 config |
| 部署健康检查 | `internal/health` + `p37d --check [--json] [--strict]`；可写/ext4/同 FS/OverlayFS/inotify/磁盘/daemon 状态 |
| GC 维护保护 | `internal/repolock`；daemon 持锁时 `p37 gc` 拒绝，`--force` 才显式跳过 |
| 默认值适配 20GB L2 | `base_cache_bytes=4 GiB`、`warn_disk_bytes=8 GiB`、`max_active=4`（v0.1.13 copy 回退；硬上限仍 64） |
| P36 部署包 | `packaging/p36/`：`p37d.service`、`p37-run`、`p37-mcp`、`config.toml`、`p37-mount` Go 挂载助手 |

测试新增：stdio 双客户端并发闭环、daemon 不在时的错误、init 幂等/模板、
`--check` 退出码与 JSON、GC 拒绝、只读目录执行、工具名一致性。`go test ./...`
全绿；linux/amd64 发布物由 `make release` 生成。仓库格式兼容性由既有
`meta.FormatVer=1` 与黄金向量测试继续保证。

### P36 返工（L1 胶水 / 共享 mount namespace / 96 MiB payload）

P36 侧确认后，部署形态做了以下修正；仓库格式与版本控制语义仍未变：

- **胶水放 L1**：P36 不改 L0；`p37-run`/`p37-mcp`/`p37-mount`/config/unit/
  sysctl 都放 L1。只有 `p37`/`p37d` 在 payload。
- **不创建私有 mount namespace**：`p37d.service` 删除 `PrivateTmp`、
  `ProtectSystem`、`ProtectHome`、`ReadWritePaths`、`ProtectKernel*`、
  `ProtectControlGroups`，否则 agent 的 shell 与 `p37-mcp` 看不到 payload
  和 workspace overlay。保留 `NoNewPrivileges` + `CAP_SYS_ADMIN`。
- **停服保留 payload**：`/run/p37-payload` 改由 `/etc/tmpfiles.d/p37.conf`
  创建，不用 `RuntimeDirectory=`（否则 service 重启时 systemd 会在只读
  payload 挂载点上重建目录，报 `status=233/RUNTIME_DIRECTORY`），也不用
  `ExecStopPost=umount`；否则停服后维护窗口没有 `p37` 可执行。
- **payload 固定 96 MiB**：32 MiB 的 ext4 扣掉 journal/元数据后放不下约
  30 MiB 的两个二进制；生成示例使用 `mkfs.ext4 -m 0`。
- **设备顺序**：P36 中 `vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；
  `p37-mount` 仍按 label 挂载。
- **新增 `packaging/p36/90-p37.conf`**：inotify 额度由 P36 L1 install.sh 安装。
- **发布流程**：`make release` 增加 dirty 检查、manifest version/commit 自检、
  release notes 生成；GitHub Actions 负责 tag → build → P36 验收 → Release，
  另保留对已发布 Release 的 `workflow_dispatch` 验收。

### v0.1.13（P36 非 root overlay EPERM 的默认回退）

真实 P36 实测：`User=p36 + NoNewPrivileges=yes + CAP_SYS_ADMIN` 下 payload
挂载、MCP/CLI/land/GC 正常，但 overlay mount 返回 `permission denied`；root
下 overlay 正常。因此 v0.1.13 只改 packaging/docs：

- `config.toml` 默认 `materialize="auto"`，`max_active=4`、
  `warn_disk_bytes=8 GiB`；copy 模式每个工作区是一份完整树。
- 验收读取 `ws_open`/`ws_list` 的实际 `materialize`，输出
  `MATERIALIZE=copy|overlay`；copy 下只要求工作区在 `/srv/repo` ext4 上。
- 默认 acceptance 不再自动 root fallback；只有
  `P37_ALLOW_ROOT_FALLBACK=1` 才做 root 对照。`p37 doctor` 在 copy 下仍会
  报 `[FAIL] OverlayFS`，正式门禁改用 `p37d --check --strict`。
- 新增可选 `config-overlay.toml` + `p37d-root-overlay.service`（root+overlay）。
- 产品 `cmd/`、`internal/` 未改；仓库格式仍为 `format_ver=1`。

## 真机第二遍（S30–S34 + 采集提速）

在 P36 实例（Ubuntu 26.04 / ext4 / overlay，2 vCPU / 1 GiB）上按 `VERIFY.md` 又走了一遍
完整功能与性能清单，抓到并修掉以下问题；两项核心功能（权限位、rename 跟随）在真机上复验通过。

| 编号 | 问题 | 处理 | 回归测试 |
|---|---|---|---|
| **S30** | **可执行位（权限位）丢失**：op 不承载权限位，`replay` 一律按 `ModeNormal` 落条目。改过内容的可执行脚本、新建的可执行文件，落地后都变成 0644；只 `chmod` 会被误记成 `blob_put` | `op.Op` 增加 `mode_before` / `mode_after`（可空），capture 在 create/modify/attr/blob_put/rename 上填好，replay 按 `resolveMode` 落位；纯权限变化改判为 `attr`；`invert` 交换前后权限，revert 可还原；`op` 表补同名列 | `repo.TestExecutableBitSurvivesOps`、`TestExecutableBitRevert` |
| **S31** | **对侧已落地的重命名不被跟随**：S21 的 rename 跟随只在「rename 与 modify 在同一次重放内」成立；对侧重命名并落地后，本侧基于旧路径的改动报 `missing_base` | 新增 `op.RenameFollows`（只按内容 id 唯一匹配，歧义不猜），`plan` / `Rebase` 在重放前构建「旧路径→新路径」映射，`replay` 在目标路径缺失时改道 | `repo.TestRenameFollowAcrossLand` |
| **S32** | **`.p37ignore` 从未被读取**：文档与 `tree.ignore` 注释都承诺读仓库根的 `.p37ignore`，但全仓只有注释，没有读取代码 | 新增 `tree.LoadIgnoreFile`，`repo.Open` 与 `ignore-check` 把文件规则并到 `ignore_extra` 之后（后匹配覆盖先匹配） | `tree.TestLoadIgnoreFile*`（3 个）、`repo.TestP37IgnoreFileIsApplied` |
| **S33** | **CLI 版本 id 自相矛盾**：`p37 log` 打印 16 位短 id，`show/revert/blame --version/ws-open --parent` 却只认 64 位完整 id | `meta.FindVersionByPrefix`（唯一前缀，歧义报错）+ CLI/`resolveParent` 统一解析；短 id 可以直接回喂 | `repo.TestShortVersionPrefixResolves` |
| **S34** | **老仓库不会自动补列**：补列逻辑只在 `repo.Init` 里，已存在的仓库打开时不会执行，新列缺失导致插 op 报 `table op has no column named mode_before` | 抽出 `meta.EnsureSchema`（幂等补列），`repo.Open` 在 `CheckFormat` 之后调用 | `meta.TestEnsureSchemaAddsLaterColumns` |

**采集提速（真机实测，10k 文件 / 2 vCPU）**：

- **单次扫描**：稳定点判定（`collectCandidates`）与建树（`dir`）原先各走一遍全树
  （readdir + lstat ×2）。现改为一次 `scan` 产出「目录 → 子项 + FileInfo + 仓库相对路径 +
  忽略判定」快照，两者共用；`dir` 不再重复 `rel`/`Ignore.Match`。
- **跳过等价写入**：`putStatWith` 与缓存快照逐字段比较，相同就不落库。此前每轮采集都把
  全部条目重写一遍（10k 次写），这是无变更采集的主要开销。
- 结果：`ws_sync` 无变更 264ms → **~140ms**；改 10 文件 ~250–530ms → **~190ms**；
  `land` 0.69–1.54s → **~0.5s**。`import` 1 万文件 15–21s（目标 <30s）。
  无变更 1 万文件仍高于附录 D.2 的 100ms 目标：在本机（2 vCPU）`readdir+lstat` 单遍
  约 60ms、SQLite 载入 1 万行 stat_cache 约 30–50ms，已接近该硬件的下限。

**仍未做（与同步有关，需要独立设计）**：P3 的远端同步（`sync_status` 目前是占位）。
小文件的对象放大已由 P2 的**离线打包**解决（见下节 P2）。

### 补齐两处「声明未接线」的配置

排查附录 B 的配置项时发现两项声明了但从未被读取，已接线：

| 配置 | 处理 | 回归测试 |
|---|---|---|
| `journal.redact_task_desc` | 为真时血缘（journal）不落任务描述原文（版本行仍保留）。`repo.Options.RedactTaskDesc` 从配置映射；blame 的任务列随之显示为空 | `repo.TestRedactTaskDesc` |
| `daemon.metrics_http` | 为真时在 HTTP 监听器上挂 `/metrics`（Prometheus 文本），与 MCP 共用同一监听器、同样要求 Bearer token；未开 `listen_http` 时启动即告警 | `mcpsrv.TestMetricsEndpoint` |

真机复验：`/metrics` 无 token 返回 401、带 token 返回指标；`redact_task_desc=true` 时
journal 的 `task_desc` 为空、blame 对应行任务列为 `-`。

> 顺带核对：`DeleteBlameCacheForVersion` 虽无独立调用点，但 `meta.PruneVersion` 已在
> 删除版本行时一并 `DELETE FROM blame_cache WHERE version_id = ?`，GC 剪枝不会留下
> 悬空的 blame 缓存。PROGRESS 早前那条「等 GC 接」的说法已过时。

### P2 · 对象打包（离线，已实现）

「一对象一文件」在两个目标场景下都吃亏：小文件多时，每个 20 字节的对象也要占一个
4 KiB 块；1 万个小文件实测对象库 148 MB（逻辑内容仅 240 KB）。本实现把大量小对象
合并进少数 **pack 文件**：

| 决策 | 说明 |
|---|---|
| **离线打包** | 写路径不变（仍写松散对象），打包是维护操作 `p37 gc --prune --pack`。这样「对象落盘 → 登记索引」的既有纪律不变，不会引入「索引里有、字节还没落盘」的窗口 |
| **pack 不可变** | 内容是对象字节 + 尾部索引，key = 内容哈希（`packs/<hash>`）。只创建不改写，符合 BlobStore 的 S3 子集 |
| **自描述、可重建** | 尾部索引给出 `id → (offset, length, type)`，`object_index` 仍是纯缓存，扫描 `packs/` 即可重建（`fsck` 走的就是这条路） |
| **崩溃安全** | 先写 pack 并 `Flush`，再删松散副本。任何时刻崩溃，对象要么在松散文件、要么在 pack，读者总取得到；两处都有时按 id 去重 |
| **整包回收** | GC 只在 pack 内**所有**对象都不可达时整包删除；不做 repack 压实（空间浪费有界） |
| **透明读** | `PackStore` 是 BlobStore 的装饰层，`objects/<id>` 的读取自动解析到 pack 区间；`ListObjects`/`RebuildIndex`/`Head` 无需改动 |
| **常驻进程一致** | 打包由另一个进程执行，故读端在位置表未命中时会增量扫描 `packs/` 发现新 pack（先写 pack 再删松散，顺序保证安全） |

真机（1 万个小文件）：`.p37` **151 MB → 5.2 MB**（对象库 148 MB → 2.0 MB pack，
约 **74×**），打包后工作区内容与源逐字节一致、`fsck` 通过；打包后再采集/落地
（pack 与松散混用）也正常。

新格式（冻结）：`P37PACK1` 头 + 对象字节 + 索引 + `P37PKEND` 尾。**注意**：这是
在缺少 §9 原文时的**保守实现**，待内部规格回填后需逐字段对齐。

### P3 · 远端同步（S3 兼容对象存储，已实现）

同步单位是**增量包**，包先落本地 `outbox`、审查后再上传；远端只有少量包对象 +
一个 ref，直接压掉对象存储的 PUT/GET/LIST 次数。

| 项 | 设计 |
|---|---|
| **包内容** | `manifest.json`（可审查）+ `meta.json`（version/journal 行）+ `payload.pack`（整段 zstd 的对象原始字节 + 索引）。版本是元数据而非 CAS 对象，随包一起迁移，空仓库也能还原完整历史与 blame |
| **增量** | 首次（base 全零）生成 checkpoint（全量闭包）；之后 `closure(result) − closure(base)` 生成 delta。delta 不含 base 已覆盖的对象，不重复占存储 |
| **低频** | 默认把「上次已上传包之后的全部版本」合成一个包；`min_versions`/`min_bytes` 以下不生成（`--force` 覆盖）；首次 checkpoint 不受阈值限制。可选 `auto_prepare_every` 由 daemon 定时暂存（不上传） |
| **压缩** | 与本地 P2 pack 不同，同步 payload 拼接**原始字节**后整体 zstd（默认 level 9）。小文件不再被每对象 zstd 帧放大：真机 200 文件仓库 payload 39.9 KiB → **18.2 KiB**；1 万小文件按同一比例估算约 0.4 MB（本地 P2 pack 约 2 MB） |
| **S3 口** | `internal/remotesync` 的 `Remote` 接口 + `S3Remote`（aws-sdk-go-v2：SigV4、S3 兼容 endpoint、path-style、multipart 上传/下载、条件写）；另带 `DirRemote` 仅用于本地端到端。布局 `<prefix>/packages/<id>/{manifest,meta,payload}` + `<prefix>/refs/mainline.json` |
| **凭证** | 只从环境变量 `P37_S3_ACCESS_KEY` / `P37_S3_SECRET_KEY`（可选 `P37_S3_SESSION_TOKEN`），不落仓库 |
| **一致性** | 上传顺序：包对象 → 条件更新 ref → 标记 uploaded；ref 用 `If-Match`/`If-None-Match` CAS，冲突返回 `REMOTE_MOVED`。拉取先校验 manifest/meta/payload 哈希与逐对象内容哈希，全部通过才写库 |
| **git 语义** | `fetch` 只导入对象并更新远端跟踪 ref；`pull` 仅在「本地是远端祖先」时快进；分叉（双方各有新提交）保持本地 head 不动并报告 ahead/behind，不自动合并。空仓库的 `p37-init` 视为空历史，可快进并在之后清理 |
| **回收** | `sync prune --local` 清理已上传的本地包；`--remote` 保留「最新 checkpoint 及其之后的 delta」，删除更早的包（checkpoint 含全量闭包，删旧 delta 安全） |

落地位置：

```
internal/remotesync/   manifest/meta/payload/prepare/stage/remote/dir/s3/push/fetch/prune/status
internal/cas/store.go  GetStored / PutStored（松散与 P2 pack 内对象统一）
internal/meta/         package+sync_state CRUD、InsertVersion 幂等、InsertJournalOnce
cmd/p37, cmd/p37d     p37 sync ...、定时打包
internal/mcpsrv        sync_status / sync_prepare / sync_list / sync_show
```

真机（P36 实例，Ubuntu 26.04 / ext4）复验：200 文件仓库 checkpoint 18.2 KiB
上传 → 空仓库 pull 后 200 文件与源 `diff -r` 一致、`fsck` 通过、blame/版本链正常；
随后 A 追加一版生成 delta（1.3 KiB）并推送，B pull 快进。daemon 运行时的
`sync_prepare/list/show/status` 也通过 MCP 验证。

## 发布前的安全加固（已完成）

开源发布前做了一轮审计，修掉 2 个真漏洞 + 2 项加固，并补了对应的回归测试。

| 编号 | 问题 | 处理 | 回归测试 |
|---|---|---|---|
| A1 | HTTP 模式的 `auth_token` **从未被校验**（只做了启动检查），任何本机进程都能获得完整写能力 | `mcpsrv.BearerAuth` + 定长比较 + fail-closed | `mcpsrv.TestBearerAuth` / `TestBearerAuthEmptyTokenFailsClosed` |
| A2 | 物化/重放会**穿过符号链接**写到工作区之外（`os.WriteFile` 与 `MkdirAll` 都跟随） | 新增 `internal/safefs`，所有写入先 `Lstat` 并在类型不符时先删除 | `safefs` 7 个测试 + `workspace.TestWriteTreeIntoDoesNotFollowSymlinks` + `repo.TestRebaseDoesNotEscapeViaSymlink` |
| A3 | 仓库目录 `0755`、数据库 `0644` → 同机其它用户可读全部源码与元数据 | `.p37/`、`workspaces/` 收紧为 `0700`，DB 与 `-wal`/`-shm` 为 `0600`，socket `0600` | `repo.TestRepoPermissionsHardened` |
| A4 | `WorkspaceDir` 直接拼 `wsID`，存在潜在路径穿越（一处死代码调用点未先查库） | 加 `ValidWSID` 格式校验 + 哨兵路径；`WorkspaceTree` 补上库校验 | `repo.TestWSIDValidation` |
| B1 | `collectPaths` 不递归 → `ws_rebase` 不删除**嵌套**旧文件 | 改为递归读取子树 | `repo.TestRebaseDeletesNestedPaths` |
| B2 | `land_queue_max` 声明了但没实现 | 加 `landSem`（满则 `LAND_QUEUE_FULL`）+ `landMu` 串行化整条流水线（§7.4） | 由 `repo` 现有 land 测试覆盖 |

发布卫生：新增 `LICENSE`（MIT）与 `.gitignore`。

### 最后一轮修正（S13 / S14）

| 编号 | 问题 | 处理 | 回归测试 |
|---|---|---|---|
| S13 | op 没有承载条目类型 → 「文件 ↔ 符号链接」重放后退化成常规文件；且符号链接的条目 id 指向 Blob，而物化走了 `ReadFileData`（要求 FileContent），**物化含符号链接的工作区会直接报错** | `op.Op` 增加 `EntryKind`；`meta.op` 增加 `entry_kind` 列；新增 `cas.Store.ReadBlob`，物化符号链接改走它 | `repo.TestSymlinkRoundTrip` / `TestSymlinkToFileTransition`、`workspace.TestWriteTreeIntoReplacesSymlinkEntry` |
| S14 | 采集产出的 op 按 kind 字典序排序，而 `"create" < "delete"` → 同一路径的类型变化变成「先建后删」，重放第一步误报重叠冲突 | 引入 `kindOrder`：同一路径固定 `delete → rename → create → modify → attr → blob_put` | 同上（S13 的测试正是靠它才通过） |

顺带清理了全部死代码（未使用的导出函数、`var _ =` 占位、魔法数字），
`go vet` 与 gofmt 干净。

### 真机发现并修正的问题（S15 / S16）

在 P36 平台的 Ubuntu 26.04 / ext4 上做端到端验证时，发现两个**会造成数据丢失**
的缺口。两者都已修复并在真机上复验。

| 编号 | 问题 | 处理 | 回归测试 |
|---|---|---|---|
| S15 | **daemon 重启后不恢复活跃工作区的挂载**（§7.7 步骤 3 未实现）。挂载消失后工作区目录变成空目录，`ws_sync` 会采集出「全部文件被删除」 | 新增 `workspace.RemountAll()`，在 `repo.Open` 的启动流程里、land 修复之后调用；只重建挂载，upper 层原样保留。由 `Options.RemountWorkspaces` 控制，**仅常驻 daemon 打开**（CLI 不重挂，避免与 daemon 打架） | 真机：重启前 6 个工作区 → 停止后 0 挂载 → 重启后 6 个挂载全部恢复、每个 1000 文件 |
| S16 | **没有「应挂载却未挂载」的防护**。人为 `umount`、或清理工具误删挂载后，`ws_sync` 会安静地采集出 N 条 `delete`，落地即清空仓库 | `workspace.Sync` 在采集前校验：`materialize=overlay` 且不是挂载点 → 置 `state=broken` 并返回 `ErrNotMounted`（MCP 错误码 `WS_NOT_MOUNTED`）。挂载点判定改用 `/proc/self/mountinfo`，不再依赖不可靠的 `st_dev` 比较 | `mcpsrv.TestErrorCodesCoverNewStates`；真机：`umount` 后 `ws_sync` 返回 `WS_NOT_MOUNTED`，重启重挂后恢复正常 |

### 栈折叠顺序（S20）

补三层栈端到端用例时抓到的**正确性**缺陷。

| 编号 | 问题 | 处理 | 回归测试 |
|---|---|---|---|
| S20 | `FoldSet` 里有一句 `sort.SliceStable(out, func(i, j int) bool { return i > j })` —— 它比较的是**下标**而不是元素，效果是把切片反转；但紧接着又有一句显式反转，两者相互抵消，折叠集合最终以 `[叶子…根]` 返回。后果有二：① `CollectOps` 把叶子的 op 拼在父的前面，而叶子的 hunk 锚点是**建立在父的改动之上**的，重放顺序颠倒会误报冲突或落错位置；② `leaf := fold[len(fold)-1]` 取到根，版本归属（agent / task_desc）记到错误的工作区 | 删掉那句排序，只保留显式反转 | `repo.TestStackFoldSetRootFirst`、`TestStackLeafOwnsVersion`、`TestStackParentAndChildEditSameFile`（**已验证**：把错误顺序放回去，三者分别以「顺序反了」「task_desc 记到根」「land 冲突」失败） |

栈的端到端用例见 `internal/repo/stack_test.go`：三层折叠成一个版本、子可见父的未落地改动、
冻结快照（父后续改动对子不可见，D23）、兄弟分叉各自落地、落地后幸存子的血缘重挂、
`ws_drop` 的 `cascade` 语义（子重挂到祖父而非被删）。

### 自动合并的锚点定位（S23）

真机验证时发现一个**假冲突**：本侧在文件末尾追加一行，而对侧刚刚改过紧邻的
那一行。本侧 hunk 的 `Before` 上下文里含有被改的那行，`place` 找不到完整模式；
而当时 `autoMerge` 只肯用完整的 `Before`/`After` 做锚点，于是报冲突。

这是最常见的场景（父工作区改了某行、子工作区在文件尾追加），必须消解。

| 改动 | 说明 |
|---|---|
| `After` 为空 ⟹ 末尾追加 | 直接追加到文件末尾。追加位置**不依赖**紧邻的上文，因此上一行被改过也不影响。 |
| `Before`/`After` 逐级放宽 | 从完整上下文开始，找不到就试更短的**后缀/前缀**，直到唯一匹配。每一级都要求唯一，因此放宽不会把改动落到错误位置。 |
| `appendIndex` | 行切分把「以换行结尾」表示成尾随空元素，追加点要取 `len-1`，否则会多出一个空行。 |

结果（**真机复验**，全新仓库）：

```
B. 对侧改末行 + 本侧末尾追加 -> can_land=true, conflicts=0, auto_merged=1
C. 两侧改同一行            -> can_land=false, conflicts=1, 候选 [take_lower, take_upper]
D. rename 跟随（对侧改内容）-> can_land=true, conflicts=0
```

回归测试：`TestAutoMergeAppendAfterChangedNeighbor`、
`TestAutoMergeAppendWithTrailingEmptyAfter`、`TestAutoMergeInsertBetweenChangedLines`。

### 真机上抓到的两个严重缺陷（S28 / S29）

这一轮所有功能都在真机（Ubuntu 26.04 / ext4 / overlay）上复验过，抓到两个
**本地测试完全测不出来**的缺陷 —— 两条都出在「只有真机才会走的代码路径」上。

| 编号 | 症状 | 根因 | 回归测试 |
|---|---|---|---|
| **S28** | **land 时 daemon 直接 panic 退出**。栈：`plan → mountmgr.Close → releaseBase` | `mountWithLower` 往 `baseRefs` 里存的是**基底目录路径字符串**，而 `releaseBase` 按 `hash.ID` 做类型断言。这是上一轮「修 R1」时引入的 —— 修之前那个 map 根本没人写，所以断言从来没被执行到 | `mountmgr.TestReleaseBaseNeverPanics`、`TestBaseRefsStoresTreeIDNotPath` |
| **S29** | **rebase 之后 agent 的改动从 op-log 里消失**：`captured_ops` 变成 0，`ws_land` 报 `NOT_DIRTY` | 惰性 stat 判定的不变量是「物化完成时磁盘内容 == base_tree」。rebase 把磁盘写成了重放结果 `res.Tree`（≠ 新基准），不变量被打破；那些「mtime 不比物化时刻新」的路径被直接判成「未变、等于基准」 | `repo.TestRebaseThenLandKeepsChanges` |

S29 是**早于本轮就存在**的老缺陷，只是此前没有任何用例走过「rebase 之后再
采集 / 再落地」这条路径。修法：rebase 之后把物化时刻置 0（下一次采集走全量读）
并清掉该工作区的 stat 缓存（`meta.StatCache.Clear`）—— 那次采集之后缓存重新填好，
后续采集照常走快路径。两个测试都验证过：**还原修复即失败**。

教训记在这里：**这两个缺陷的共同点是没有单元测试能覆盖的路径**（一个需要真实
overlay 挂载并在 land 时触发，一个需要「rebase 之后再采集」的两步序列）。单元
测试全绿并不能替代真机端到端走一遍。

### ws_rebase 增量重挂（S27，§4.9.2）+ 两处附带修复

**问题**：`ws_rebase` 原来把**整棵目标树**写进工作区。overlay 模式下写一个文件
就会 copy-up 到 upper，所以一次 rebase 之后 upper 里躺着的是整棵树 —— 64 并发
× 10 万文件时是数量级的磁盘浪费。

**改法**（§4.9.2 的「换 lower 重挂」）：

1. 卸载 → 物化新基底 → **保留 upper、换 lower 重挂**（`mountmgr.Relower`）。
2. 只把「新基准 → 目标树」的**差异**写进去（`applyDelta`）：
   - 两边一致的文件**一个字节都不碰**（直接由新 lower 提供）
   - 只在一侧存在的路径：写入或删除（删除由 overlayfs 自动落白洞）
   - 未变的**子树整棵跳过**（靠树 id 相等判断）
3. copy 模式没有 lower 可换，退回整树覆写；换 lower 失败也退回整树覆写
   （宁可慢，不能错）。

关键性质都有用例钉住：**未变文件的 mtime 不被改动**（证明真的没重写）、
嵌套目录的删除生效、未跟踪文件保持不动。

**附带修复的两处**（都是写这个功能时撞出来的）：

| | 问题 |
|---|---|
| R1 | `mountmgr.releaseBase` 读的是一个**从未被写入**的 map（`wsBaseRef`），于是 `inUse` 只增不减 —— `Evict` 会认为所有基底都被引用，**基底缓存永远腾不出空间**。改为使用真正被写入的 `baseRefs`。 |
| R2 | 符号链接**目标变化**原先走「删 + 建」，一次改链接记两个 op。改为 `blob_put` —— 它的重放规则恰好就是「目标 id == blob_before → 换成 blob_after」，表达更贴切、op 数减半。**类型**变化（文件↔链接）仍然走删+建，那不能糊。 |

回归测试：`internal/workspace/delta_test.go`（3 个）、
`internal/capture/symlink_test.go`（2 个）。

### 块移动检测（S26，§5.3 第 1、2 趟）

此前 `merge.move_similarity` 零引用，而且**根本不做移动检测**：把一段代码整体
搬走会被拆成互不相干的「删一处 + 插一处」，插入侧的锚点常常只剩一两行上下文。

实现方式：**不重写 diff 内核**，而是在 histogram 产出的区间列表上做后处理
（`internal/tree/move.go`）。这样做的好处是正确性仍然由已被大量测试覆盖的
histogram 保证，移动检测只换**等价**的切分方式。

1. **第 1 趟 精确块移动**：纯删除区间与纯插入区间内容逐字节相同。
2. **第 2 趟 相似度移动**：按行的**多重集合**相似度 `2·|共有行|/(|x|+|y|)`
   ≥ `move_similarity`（默认 0.6）。
3. 其余区间由 histogram 产出，本模块不动。
4. 重命名检测在 capture 层（`mergeRenames`），不在这里。

检测到一对之后把「删块 + 插块」合并成**一个**区间 —— 于是 hunk 的
`before/remove/after` 连成一条完整、精确的模式。

**实测效果**（`TestDiffDetectsBlockMove`）：把 5 行代码整块搬到文件开头，
产出的 op 现在是 **1 个 hunk**（remove 5 行 / insert 5 行），而不是 2 个碎片。

两个实现时踩到的点，都留了注释：

- 只有**纯**删除 / **纯**插入区间参与配对。一个 replace 区间里既有删又有插，
  那是「改动了这几行」，不是移动。
- `Move` 必须携带**两个完整区间坐标**，不能只存「内容在另一侧的位置」：
  删除区间的 `BStart` 与插入区间的 `BStart` 是两回事，用内容位置反查区间会
  永远找不到（这个 bug 是靠测试里「期望 1 个 hunk 得到 2 个」暴露的）。

回归测试 `internal/tree/move_test.go`（7 个）+ `internal/capture/move_test.go`（2 个）：
精确移动的六种形态（含单行重排、整段替换、纯追加这些**不该**判为移动的）、
相似度匹配与阈值生效、合并后模式唯一命中、**开关移动检测都不改变重建结果**、
确定性、区间不重叠且按 AStart 升序。

### 保留与 GC（S25，§10）+ 读时校验（§8.3）

这是「仓库只增不减」的正解：此前 `retention.*` 与 `gc.*` 整组配置零引用，
`p37 gc` 只清孤儿工作区目录，**没有任何对象回收**。

新实现分两件事：

| | 做什么 |
|---|---|
| **mark-sweep** | 扫掉不可达的 CAS 对象（磁盘空间真正的大头） |
| **retention** | 剪掉最老的一批版本行（§10.3） |

根集合（§10.1）实现了 4 条，其中**第 4 条规格里没写但必须有**：

1. `mainline_head`
2. 保留策略命中的 version
3. 所有活跃工作区的 `base_tree`
4. **活跃工作区 op 引用的内容对象** —— 采集产出的 FileContent 只被 op 表引用，
   不属于任何版本的树。漏了它，下一次 land 会去读一个已被扫掉的对象。

实现要点：

- **标记阶段只解压 Tree 与 FileContent**（都小），Blob 是叶子，只看头部字节
  不解压 —— 所以 10 GB 仓库的标记不需要读那 10 GB。新增 `Store.Head()` 与
  `Store.ListObjects()`（对应 `RebuildIndex` 的「全量解压+校验」是 fsck 的路径）。
- **GC 与 land 互斥**（共用 `landMu`）：land 正在拼树时把对象扫掉就完了。
- **顺序：先剪版本行、再删对象。** 反过来会出现「版本还在但对象没了」的窗口。
- **GC 后清空 `stat_cache`。** 采集过程中产生的中间 Tree 对象只被 stat 缓存
  引用；扫掉它们之后缓存里就是悬空 id，下一次采集会直接返回不存在的对象 id，
  树立刻烂掉。缓存是纯的，清掉即可（有回归用例覆盖）。
- **版本 id 不能改**（Merkle 包含 parent），所以剪枝会让最老的保留版本留下一个
  指向已删版本的 parent。链的读取路径（`VersionChain` / `WalkHead` / `blame` /
  `version_show` / `version_diff`）全部改成**把「祖先不可读」当作链的终点**，
  而不是报错 —— 否则剪枝一次之后这些功能全废。
- **读时校验策略**（`gc.verify_on_read`）：`off` / `sample` / `always`。
  `sample` 对小对象全校验、大对象按 1/8 抽样（大对象解压+哈希的代价明显）。
- **scrub**（`gc.scrub_sample`）按对象 id 排序后按步长抽样，**确定性**可复现；
  `p37 gc --scrub` 与 `p37 doctor` 都会跑。

CLI：`p37 gc [--prune] [--scrub] [--report-roots] [--workspaces]`，
默认只预演，`--prune` 才真删。

回归测试 `internal/repo/gc_test.go`（7 个）：剪枝+回收、预演无副作用、
**活跃工作区的 op 引用对象不被扫**（并能在 GC 后正常落地）、被工作区引用的版本
不被剪、剪枝后链仍可走、幂等、**stat 缓存清空后重新采集仍正确**、
**scrub 能发现被篡改的对象**。

### blame（S24，§12.2）

`internal/repo/blame.go` 实现行级归因，MCP 工具 `blame` 与 CLI `p37 blame` 都已接通。

**算法：从文件诞生的那一版正向推进**，而不是从目标版本反向回溯。

反向回溯看起来更自然（「未变更的行归 parent」），但会**丢归属**。举个真实的例子：
V1 建 `a.txt`（3 行）→ V2 改第 3 行 → V3 改第 1 行。反向回溯从 V3 走到 V2 时把
未变更的第 1 行交给 V2，再走到 V1 时又交给 V1 —— 第 1 行明明被 V3 改过，却
被抹成了 V1。正向推进则没有歧义：每一跳把新文件里**被改动的行**判给当前版本，
其余行沿用上一版的归属。

配套细节：

- 行号剥离行切分产生的尾随空元素 —— 「以换行结尾」是编码产物，不是用户眼里的一行。
- `import` 现在也写一条 journal。否则 blame 会把「导入」这一版报成
  「journal 已清理」，而它恰恰是最需要解释来源的一版。
- 每条记录带 `agent` / `task` —— 这是 git 给不了的能力：**行级 blame 直接返回
  当时的任务描述**。
- 结果缓存进 `blame_cache`（附录 J 已有该表），并配了
  `DeleteBlameCacheForVersion` 供 GC 使用。
- 新错误码 `PATH_NOT_FOUND`。

回归测试 `internal/repo/blame_test.go`（5 个）：上面那个卡反向回溯的三版例子、
文件在中间版本诞生、某版本完全没碰该文件、区间裁剪与错误路径、
**缓存命中与不计算的结果必须逐字段一致**。

### fs watcher（S22，§5.2 触发源 a/c + §4.7）

新增 `internal/watch`：把「什么时候该采集」从采集本身里拆出来。

| 触发源 | 实现 |
|---|---|
| a. fs 事件 | `fsnotify` 递归监听工作区目录；新建子目录时补挂；每个工作区独立 debounce（默认 500ms） |
| c. 周期扫描 | 轮转调度：`scan_interval / 工作区数` 依次扫一个，把 64 并发下的 IO 峰值摊平（§4.7） |
| b. MCP 调用 | 仍由 `mcpsrv` 负责；另暴露 `Watcher.Touch(wsID)` 供外部提前入队 |

设计要点：

- 本包**不做采集**，只通过 `Syncer` 回调交给 `workspace.Manager.Sync`。既不懂树也不碰磁盘。
- 全部调度在**单个 goroutine** 里推进，状态无锁；只有「在途采集标记」跨 goroutine。
- 采集失败一律只记日志：watcher 是只读触发源，它的判断必然**落后于**状态变化
  （`rebasing` / `integrating` / `broken` / 未挂载都会被 Sync 自己拒绝），下次对齐即可。
  **绝不因为一次失败就停掉监听。**
- 同一工作区同时只允许一个在途采集，避免与触发源 b 叠加。
- 监听集合每次对齐都由 `repo.LiveWorkspaces()` 重新给出，只含 active/conflicted
  且确实可读的工作区 —— 未挂载的 overlay 工作区是空目录，监听它没有意义。
- 附录 G.3：`fs.inotify.max_user_watches` 低于建议值时自动降级为只监听工作区根目录。

回归测试 `internal/watch/watch_test.go`（9 个）：fs 事件触发、debounce 合并
（5 次写入只触发 1 次）、**新建子目录内的改动也能看到**、Touch、轮转扫描覆盖全部
工作区、工作区消失后不再采集、降级模式、Close 幂等、路径前缀判定。
全部通过 `-race`。

### 候选解与自动合并（S21，§6.3）

补齐 §6.3 的确定性候选解。实现后明确了一条边界：**能自动消解的绝不留成冲突**。

| 情形 | 处理 | 位置 |
|---|---|---|
| 两侧改**不重叠行** | **自动都保留**（不再报冲突） | `op.autoMerge`，在 `applyModify` 里 place 失败后先试 |
| 两侧在**同锚点插入** | **自动按 seq 拼接**（本侧内容拼在其后） | 同上 |
| 匹配有歧义（同一段原文出现多次） | **绝不自动合并**，退回冲突 | 同上（要求唯一匹配） |
| `delete_vs_edit` | 候选：取删除（首选）/ 保留被删文件 | `op.candidatesFor` |
| `rename_vs_edit` 内容未分叉 | **跟随重命名**，在新路径内容上应用本侧 hunks；放不下才冲突 | `applyRename` |
| `binary` 任一侧未变 | 快路径已消化（id 相等即幂等）；两侧都变则只给二选一 | `applyBlobPut` + `candidatesFor` |
| `attr_conflict` | 取 upper（首选） | `candidatesFor` |
| 其余 | 无候选，交 AI 裁决 | —— |

`ReplayResult.AutoMerged` 与 `land_preview`/`ws_land` 返回里的 `auto_merged` 字段
暴露自动合并的 hunk 数，便于观察规则覆盖了多少冲突。

回归测试 `internal/op/candidates_test.go`：不重叠行自动合并、同锚点插入的确定性拼接
（含两次重放逐字节相同）、**同一行的两侧改动仍是硬冲突**、歧义不合并、
rename 跟随、各类候选解。

### 性能修正（真机实测）

真机上 `import` 1000 个文件要 6.7s，`ws_sync` 无变更要 0.12–0.18s。逐项定位后修正：

| 编号 | 根因 | 处理 | 效果 |
|---|---|---|---|
| S17 | 每写一个对象都 `CreateTemp` → `link` → `remove`，5 次元数据操作 | 改为直接 `O_CREAT\|O_EXCL` 写最终路径；key 是内容寻址的，**替换同内容文件无害**。残留残片用「大小校验」兜底（head 未推进前不会被引用） | 单对象 2.31ms → **0.51ms** |
| S18 | `Flush` 对 2063 个对象逐个 `fsync`。实测在 2 vCPU 虚拟机上一次对象创建只要 63ms，而 import 要数秒 —— **瓶颈全在 fsync** | 改用 `syncfs`（一次系统调用刷完整个文件系统），小批量时回退到并行 fsync | import 6.7s → **1.23s**（三次稳定，5.4x） |
| S19 | `stat_cache` 逐条 `SELECT`，1000 个文件就是 1000 次查询 | 新增 `StatCache.Load(wsID)` 一次性载入内存快照，`Build` 期间只查 1 次 | `ws_sync` 无变更 0.15s → **0.03s** |

真机最终数据（1000 文件 / 2063 对象，2 vCPU / 956MB / ext4）：

```
import          1.23s
ws_open         0.03s   （O(1)，与文件数无关）
ws_sync 无变更  0.03s
ws_sync 改10文件 0.08s
ws_land         0.23s
```

### API 一致性

`ws_info` 曾直接返回 `workspace.Status`，该结构体没有 json tag，字段名是
PascalCase（`ID`、`ParentKind`、`Dir`…），与附录 K 的 snake_case 约定不符，
按 schema 实现的客户端会解析失败。已补全 tag 并加回归测试
`mcpsrv.TestWSInfoUsesSnakeCase`。

### 已知但**未做**（不影响发布，列出来以免误以为已覆盖）

> 本节写作时间较早，多条已实现；权威清单见文件末尾「仍然未做」。

- ~~行级移动检测（§5.3 第 1、2 趟）~~：已实现（S26）。
- ~~`ws_rebase` 的「换 lower 只写 delta」~~：已实现（S27）。
- ~~P2 的包（pack）~~：已实现**离线打包**（`p37 gc --prune --pack`）。写时打包与
  repack 压实仍未做。
- ~~符号链接**内容变化**走 delete + create~~：已改为 `blob_put`（R2）。

## 已实现

### M1

| 包 | 内容 |
|---|---|
| `internal/hash` | BLAKE3-256 对象标识（`ID`、JSON/Text 编解码） |
| `internal/codec` | 附录 A 的确定性编码原语（`Encoder`/`Decoder`）、路径校验、对象类型号 |
| `internal/cas` | **CDC 分块器**（附录 H，gear 表按 BLAKE3 派生并冻结）、`BlobStore` 接口 + `LocalFS`（S3 语义子集、对象目录分片）、类型化 `Store`、`ObjectIndex` 接口 + `MemIndex` |
| `internal/tree` | `FileContent` / `Tree` 编解码与查找、行切分与 EOL 判定 |
| `internal/version` | `Version` 编解码（Merkle 链） |
| `internal/meta` | 附录 J 的完整 DDL、格式版本校验、`ObjectIndex` 适配器、repo/version 查询 |

冻结的格式资产：

- `internal/codec/testdata/vectors/encoding.json`（附录 A.5 黄金向量）
- `internal/cas/testdata/cdc-gear/gear.json`（附录 H.3 gear 表快照）

两者都由 `-update` 生成、默认只校验；任何变化即视为格式破坏。

### M2 / M3 / M4 / M5

| 包 | 内容 |
|---|---|
| `internal/tree` | **histogram diff**（附录 I）、带上下文 hunk 生成、`ignore` 匹配器（§3.4）、EOL/二进制判定（§3.5） |
| `internal/op` | op 模型（JSON + base64 行编码）、**place 引擎**（§6.1 六条规则）、**ConflictKey**（§6.4）、**六种 kind 的重放语义**（§6.5）、resolution 查表与应用、嵌套树 flatten/unflatten |
| `internal/capture` | **Build**（目录 → 树，含 stat 快路径与子树复用）、**Diff**（两树 → op，含重命名归并）、`StatCache` 接口 + `MemStat` |

新增（M3 及之后）：

| 包 | 内容 |
|---|---|
| `internal/workspace` | 生命周期、采集（整体重建 op-log）、有效树（§4.9.1）、`ws_rebase`（§4.9.2）、`ws_drop` 级联重挂 |
| `internal/mountmgr` | `overlay`（Linux，含 `ProbeOverlay` 自检）与 `copy`（跨平台）两种物化、基底 LRU、大小写冲突检测、标记文件在被跟踪树之外 |
| `internal/repo` | init / import / **land 流水线**（可恢复的三段提交协议）/ revert / 状态 / 冲突视图 / 租约违规检测 |
| `internal/meta` | stat_cache / workspace / op / conflict / resolution / journal 的读写；`CommitLand` / `FinishLand` / `RollbackLand` / `ReconcileLand` |
| `internal/mcpsrv` | 22 个 MCP 工具（20 个可用 + 2 个 P2 占位） + 统一信封 + 错误码映射（附录 K、附录 E） |
| `internal/config` | 附录 B 的配置与默认值 |
| `cmd/p37` | 运维 CLI（init/import/status/doctor/fsck/gc/migrate/ignore-check + 调试用版本操作） |
| `cmd/p37d` | 常驻 daemon：flock、unix socket、可选 HTTP（强制 token）、优雅退出 |

关键验证（`internal/capture/capture_test.go:TestDiffRoundTrip`）——

```
baseline 目录 --Build--> T0
修改目录      --Build--> T1
Diff(T0,T1)   --Replay--> T2
断言 T2 == T1
```

同一测试覆盖了 modify / create / delete / rename / attr / blob_put 六种 op。

其它已验证的强性质：

- `apply(A, hunks) == B`：10,000 组随机对（锚点唯一）
- 相邻 hunk 的锚点区间**不重叠**（保证顺序重放可用）
- `ConflictKey` 在 `op_id`/`hunk_index` 变化后**不变**（G7 回归）
- 重放**确定性**：同一 (base, ops) 两次重放逐字节相同
- 单点修改后 CDC 只新增 **1 个分块**（4 MiB 样本）

## 实现过程中发现并修正的规格问题

这些是写代码时才暴露出来的，已回填到内部设计规格。

| 编号 | 位置 | 问题 | 处理 |
|---|---|---|---|
| S1 | §5.3 | 原文「间距 < 2·ctx 的改动区块合并为一个 hunk」。字面实现会把中间未变更行塞进 `remove/insert`，**且会让相邻 hunk 的锚点区间重叠**，导致顺序重放时后一个 hunk 匹配失败 | 改为：**只在 A 侧完全相邻时合并**，并把上下文裁剪到不越过邻近区间。面向展示的合并另设 `HunkMerged` |
| S2 | 附录 I.4 | 「`histogram(A,B)` 与 `histogram(B,A)` 互为镜像」**不成立**：锚点选择规则本质方向相关（JGit 亦然） | 该不变量已移除，改为断言**确定性**。管线中无任何环节依赖镜像性（revert 用显式取逆） |
| S3 | §5.1 | op 缺少「期望的当前 FileContent id」，导致 delete/attr/rename/blob_put 无法精确校验目标是否被第三方改动 | 新增 `BaseFile`（≡ `blob_before`，全零表示不应存在）与 `NewFile`（≡ `blob_after`）；rename 新增 `DestPath` |
| S4 | §3.5 | `attr` op 只说「只更新 eol 属性」。但 D16 规定**存储保留原样**，所以纯 EOL 变化必然改变了字节，属性无法独立于内容 | `attr` op 必须同时携带新的 FileContent id（复用了 `NewFile`）；它与 `blob_put` 的区别仅在于**报告原因**，从而影响冲突分类 |
| S5 | §6.4 | resolution 只记录 kind。当锚点已被破坏时，`take_upper` 无法重新定位 hunk，解决会失败 | 明确语义：`take_upper` 先尝试常规 place，失败则**退化为在 hint 位置用本侧 insert 替换等长当前行**；`take_lower` 为恒等；`both` 在冲突位置之前插入。语义保真由 `ConflictKey` 承担（内容变则冲突自动重浮现） |
| S6 | §5.5 / §17 U2 | 「空目录不跟踪」与「重放后重建嵌套树」必须一致，否则 `Diff`→`Replay` 往返不等（空目录无法用扁平路径表达） | `Build` 显式跳过产出零条目的目录；`unflatten` 因此只需处理有内容的目录 |
| S7 | §8.3 | `object_index` 是「本地存在性索引」，但没有说明对象类型从何而来。若仅靠索引，崩溃后无法重建 | 本地存储形态改为**自描述**：`0x01 || type || zstd(raw)`。因此索引降级为**纯缓存**，可由扫描 `objects/` 前缀完全重建（与「ops 表可重建」同一哲学） |

## 下一步

### 仍然未做

> 本清单是早期版本，部分已过时；以顶部「当前状态」与各轮修复小节为准。

1. ~~**行级移动检测**（§5.3 第 1、2 趟）~~：已实现（S26）。
2. **P2 的对象打包**：已实现**离线打包**（`p37 gc --prune --pack`，见顶部 P2 节）。
   仍未做：`package` 表的 checkpoint/delta 语义、`retention.keep_epochs` 的对应物、
   以及写时打包 / repack 压实。`blame_cache` 的回收已由 `meta.PruneVersion` 覆盖。
3. ~~**`ws_rebase` 的「换 lower 只写 delta」**~~：已实现（S27）。
4. ~~符号链接内容变化走 delete + create~~：已改为 `blob_put`（R2）。
5. ~~**P3 远端同步**~~：已实现（S3 兼容对象存储 + 增量包 + 本地暂存审查，见顶部 P3 节）。

### 下一步建议顺序

`行级移动检测` → `P2 包与 GC` → `ws_rebase 增量重挂`。

### 真机结论（已完成）

`VERIFY.md` 门禁全部通过。四点关键结论：

- OverlayFS 在 ext4 上可挂载，换 lowerdir 正常。
- 跨 copy-up 与重挂，`stat` 的 mtime/size/inode 精确映射到 lower，inode 不变 ——
  惰性 stat 判定（D23 相关）成立。
- inotify 在 overlay 上行为与裸 ext4 一致（含 copy-up、50 次快速写 → 101 事件）。
  故 fs watcher 可按普通目录实现。
- 一次 `umount`/重启会让所有挂载消失 —— 这正是 S15/S16 的来源。

## 开发命令

```bash
go test ./...                      # 全部测试
go test ./internal/codec -update   # 重新冻结编码黄金向量（仅格式变更时）
go test ./internal/cas -update     # 重新冻结 gear 表（仅格式变更时）
```

## v0.2 · 目标形态改造（format_ver=2）

本轮把 p37 从「单仓库 + 单父 mainline」扩到 P34 需要的目标形态。

### 多仓库

- 新增 `internal/hub`：`/srv/repos/.p37/registry.db`，单 daemon 懒加载多 repo。
- `p37d --root /srv/repos`；保留 `p37d --repo /srv/repo` 单仓库兼容。
- 旧 `/srv/repo` 原地注册为 `default`，不移动目录。
- 所有 MCP 工具自动获得可选 `repo` 参数；多仓库模式省略返回 `repo_required`。
- `repo_create/list/info/delete/disk_usage/import/export/set_remote`。

### 命名线与 DAG

- `line` 表 + `version_parent` 表；`mainline_head` 保留为 default line 的兼容镜像。
- 线性版本继续使用 v1 编码/ID；merge 版本使用 `TypeVersionMerge`，ID 覆盖全部 parents。
- `line_create/list/info/delete/protect/reset/log/compare/graph/merge`。
- `line_merge` 默认 fast-forward，`no_ff=true` 产生双亲 merge；冲突使用隐藏 merge workspace + `merge_id`。
- `line_reset` 默认只允许祖先，`force+reason` 才越过并写审计。
- GC 保留集合按每条 line 的祖先深度分别计算，并保留 reset 7 天 grace 与未过期幂等引用。

### 工作区

- `ws_open(parent={kind:line|version|workspace})`；line parent 记录 `base_line`。
- `ws_land(target=line|workspace)`、`land_preview(target=...)`、`ws_land_into`。
- `ws_land` 支持 `expected_head`、`expected_ws_tree`、`expected_ws_ops_epoch`、`idempotency_key`。
- `ws_land_into` 用 `preview_id` + `expected_parent_tree` 做乐观并发；父工作区保留，子默认销毁。
- `ws_rebase(to=...)`、`ws_diff`、`ws_reap`、`ws_disk_usage`、隐藏 merge/preview 工作区。
- `landed` 通过 `landed_ts` 表达，支持 `drop_workspace=false`。

### 历史读取

- `version_tree/file/archive/log/graph/compare`；路径严格校验，symlink/权限位/二进制/truncated 显式返回。
- `version_archive` 支持 tar.gz/zip，VM 内临时文件 1h 过期。

### 同步

- `refs/lines.json`（旧 `mainline.json` 可读）+ 稳定 `remote_key`，本地 repo id 可变。
- 多 line 仓库使用全量 checkpoint；`sync_push/fetch/pull/restore/prune` 可在线执行。
- 显式 `sync_push` 同步 durable；`auto_push` 仍是异步便利功能。
- 版本元数据包含 `parents[]`，restore 后 DAG 与 merge 关系保持。

### 测试

新增：多仓库生命周期/重启、line 会话循环、line_merge 冲突、land_into 冲突、line reset/protect、历史读取、rebase、幂等、目录同步 push/restore（含 merge parents）。`go test ./...` 全绿。

契约文档：`docs/mcp-contract.md`、`docs/format-v2.md`、`docs/sync-contract.md`、
`docs/concurrency.md`、`docs/p34-integration.md`。
