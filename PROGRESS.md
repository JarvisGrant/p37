# P37 实现进度

> 对应规格：`docs/DESIGN.md`（v0.4）
> 最近更新：2026-10-18

## 当前状态

| 里程碑 | 状态 | 说明 |
|---|---|---|
| **M1 存储底座** | ✅ 完成 | 附录 L.1 的全部单元/属性测试通过 |
| **M2 树与 op** | 🟡 主体完成 | 缺：行级移动检测、fs watcher；stat_cache 已落 SQLite |
| **M3 工作区与整合** | ✅ 主体完成 | 缺：OverlayFS 真机验证、fs watcher、真机上跑门禁 spike |
| **M4 冲突** | ✅ 核心完成 | place + 冲突对象 + resolution + MCP 工具层已打通；候选解只实现了 3 条规则 |
| **M5 栈与回退** | 🟡 部分完成 | 折叠集合、revert、journal 已实现；栈的端到端用例待补 |

测试：`go test ./...` 全绿（76 个测试）。约 13,800 行 Go（含测试）。

**已可用的端到端路径**（本机用 `copy` 模式验证通过）：

```
p37 init → p37 import → ws_open → 直接改文件 → ws_sync → land_preview → ws_land
                                                        ↓ 有冲突
                                        conflict_show → conflict_resolve → ws_land
```

以及 `version_revert`、`version_log/show/diff`、`ws_drop`、`fsck`、`doctor`、`gc --workspaces`。
MCP 层有进程内端到端测试（`internal/mcpsrv`），覆盖工具清单、闭环、冲突、revert、错误码映射。

**真机验证清单见 [`VERIFY.md`](VERIFY.md)。**

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

### 已知但**未做**（不影响发布，列出来以免误以为已覆盖）

- 行级移动检测、fs watcher（第三重采集触发）、`blame`、P2 的包与 GC、
  栈的三层端到端集成测试：见下文「下一步」。
- 符号链接**内容变化**目前走 delete + create（而非 modify），语义正确但 op 数偏多。

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
| `internal/mcpsrv` | 21 个 MCP 工具 + 统一信封 + 错误码映射（附录 K、附录 E） |
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

这些是写代码时才暴露出来的，需要回填到 `docs/DESIGN.md`。

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

### 到验证前的剩余工作

1. **fs watcher**（§5.2 触发源 a/c）：`fsnotify` + debounce + 轮转扫描调度器（§4.7）。
   当前采集只靠显式 `ws_sync` 与「以该 ws 为目标的 MCP 调用」两个触发源。
2. **行级移动检测**（§5.3 第 1、2 趟）：精确块移动 + 相似度移动。目前只做了
   树级的重命名检测。
3. **候选解补齐**（§6.3）：目前实现 3 条（take_lower / take_upper / both），
   其余（同锚点插入按 seq 拼接、delete 覆盖 edit 等）待补。
4. **`blame`**（§12.2，P2）：算法已在规格中定稿，未编码。
5. **栈的端到端用例**：折叠集合与重挂已实现，但缺少三层栈 + 分叉的集成测试。
6. **P2 的包与 GC**：`package` / mark-sweep / scrub / 保留策略。

### 需要真机

见 [`VERIFY.md`](VERIFY.md)。最关键的四处：OverlayFS 能否挂载、换 lower 重挂的
行为、stat 的 inode 是否可靠、fsnotify 是否可靠。这四点决定 §4.7 与 §4.9.2 的
实现是否需要调整，**必须在继续做 M4/M5 的栈特性之前拿到结论**。

## 开发命令

```bash
go test ./...                      # 全部测试
go test ./internal/codec -update   # 重新冻结编码黄金向量（仅格式变更时）
go test ./internal/cas -update     # 重新冻结 gear 表（仅格式变更时）
```
