# P37 实现进度

> 设计规格为内部资料，不随本仓库发布
> 最近更新：2026-10-19

## 当前状态

| 里程碑 | 状态 | 说明 |
|---|---|---|
| **M1 存储底座** | ✅ 完成 | 附录 L.1 的全部单元/属性测试通过 |
| **M2 树与 op** | 🟡 主体完成 | 缺：行级移动检测；fs watcher 已实现（`internal/watch`） |
| **M3 工作区与整合** | ✅ 完成 | OverlayFS 已在真机（Ubuntu 26.04 / ext4）验证；重启重挂与未挂载防护已补齐 |
| **M4 冲突** | ✅ 核心完成 | place + 冲突对象 + resolution + MCP 工具层已打通；候选解 3 条 + 重叠自动合并 |
| **M5 栈与回退** | ✅ 完成 | 折叠集合、revert、journal；三层栈 + 分叉端到端用例齐备（并借此修掉 S20） |

测试：`go test ./...` 全绿（**124 个测试**）。约 17307 行 Go（含测试）。
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

- 行级移动检测（§5.3 第 1、2 趟）；目前只有树级重命名检测。
- `ws_rebase` 的「换 lower 只写 delta」（现为整树写进 upper，正确但费磁盘）。
- `blame`（§12.2，P2）。
- P2 的包（pack）与 GC：当前一对象一文件，10 万文件约 20 万对象；`syncfs` 之后
  元数据成本已不是瓶颈，但打包仍能把小文件场景再压一个量级。
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
| `internal/mcpsrv` | 22 个 MCP 工具 + 统一信封 + 错误码映射（附录 K、附录 E） |
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

### 到验证前的剩余工作

1. **fs watcher**（§5.2 触发源 a/c）：`fsnotify` + debounce + 轮转扫描调度器（§4.7）。
2. **行级移动检测**（§5.3 第 1、2 趟）：精确块移动 + 相似度移动。
3. **`blame`**（§12.2，P2）：算法已在规格中定稿，未编码。
4. **P2 的包与 GC**：`package` / mark-sweep / scrub / 保留策略。
5. `ws_rebase` 的「换 lower 只写 delta」优化。

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
