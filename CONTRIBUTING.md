# 贡献指南

## 环境

- Go 版本以 [`go.mod`](go.mod) 为准。
- Linux 真机验证需要 ext4 / OverlayFS；日常开发可以在 macOS 用 `copy` 模式。
- 发布物只构建 `linux/amd64`；`darwin/amd64`、`linux/amd64`、
  `linux/arm64` 做编译校验。

## 构建与测试

```bash
make build          # dist/p37、dist/p37d
make test           # go test ./... -count=1
make vet            # go vet ./...
make fmt            # gofmt -l -w
make cross          # 三个平台编译校验
```

发布前建议额外跑：

```bash
go test -race ./internal/hub ./internal/repo ./internal/mcpsrv
```

CI 在 GitHub runner 上会设置 `P37_FORCE_HASH=1` 关闭 stat 快路径，因为
runner 的 mtime 精度可能不足；本地测试通常不需要。

## 目录

```text
cmd/p37         运维 / 调试 CLI
cmd/p37d        daemon、stdio 代理、--check
cmd/p37-mount   P36 payload 挂载助手
internal/cas    内容寻址存储、CDC、pack
internal/capture 目录采集与树 diff
internal/codec  确定性编码与路径校验
internal/hub    多仓库 registry / 生命周期
internal/mcpsrv MCP 工具、信封、错误码、导出任务、metrics
internal/meta   SQLite 元数据、land 协议、DAG、GC 根
internal/op     op 模型、place、replay、冲突
internal/remotesync 同步包与 S3/dir 远端
internal/repo   init/import/land/revert/read/blame/gc
internal/workspace 工作区生命周期、物化、配额
internal/tree   tree/diff/ignore/lines
```

## 改动约定

- 不要改冻结的格式资产（CDC gear 表、黄金向量），除非明确做格式升级并更新
  `docs/format-v2.md`。
- version id 不可改写；GC 只能删对象/版本行。
- 所有物化/重放写入必须走 `internal/safefs`，不能穿过 symlink。
- 新增 MCP 工具时同步更新：
  - `docs/mcp-contract.md`（工具列表、参数、返回、错误码）；
  - `docs/status.md` 的工具数（如果变化）；
  - 相关测试（至少覆盖成功路径和主要错误码）。
- 新增/修改错误码时同步更新 `docs/mcp-contract.md` 的错误码表；错误码统一小写
  snake_case。
- 新增配置项时同步更新 `docs/config.md`，并确认 `config.Default()` 与新字段
  一致；P36 模板差异单独注明。
- 文档里的版本号必须与最新 git tag、`Makefile` 的 `VERSION`、
  `internal/buildinfo.Version` 一致。

## 文档维护

- 当前状态写 [`docs/status.md`](docs/status.md)；历史记录写
  [`PROGRESS.md`](PROGRESS.md)。不要把“本轮”“目前”这类相对时间词写进长期文档。
- 工具/错误码/配置以代码为准；文档漂移应视为 bug。
- 文档索引见 [`docs/README.md`](docs/README.md)。
- 文档校验由 `internal/mcpsrv/docs_test.go` 自动执行：工具覆盖、错误码、相对
  链接；`internal/buildinfo/buildinfo_test.go` 校验版本号与 Makefile/README
  一致。
- 公开文档避免依赖未随仓库发布的内部规格编号；必须引用时说明它仅供内部追溯。

## 发布

```bash
make release VERSION=0.3.2
make p36-bootstrap
```

- `make release` 会拒绝脏工作区，并校验 `manifest.json` 的 version/commit。
- 发布 tar 根目录严格只有 `p37`、`p37d`、`manifest.json`。
- P36 验收与 Release 由 `.github/workflows/release.yml` 完成；也可对已发布
  Release 手动触发 `p36-acceptance.yml`。
- 发布前更新：`Makefile` 默认 `VERSION`、`internal/buildinfo.Version`、
  `README.md` 的版本/示例、`docs/status.md`、`packaging/p36/RELEASE_NOTES.md`。

## 测试环境

- 测试会用临时目录；必要时设置 `TMPDIR` 到支持纳秒 mtime 的文件系统。
- `P37_FORCE_HASH=1` 强制走内容哈希，避免依赖 stat 快路径。
- P36 真机验收见 [`VERIFY.md`](VERIFY.md)；
  自动化矩阵见 [`docs/acceptance.md`](docs/acceptance.md)。
