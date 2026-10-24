# p37 文档索引

先看哪里取决于你的角色：

## 我是第一次接触 p37

1. [`../README.md`](../README.md) —— 项目定位、快速开始、与 git 的差异。
2. [`architecture.md`](architecture.md) —— repo / line / version / tree / op /
   workspace / land 的概念和数据模型。
3. [`mcp-contract.md`](mcp-contract.md) —— agent 实际调用的 MCP 工具。

## 我要接入 p37

- [`p34-integration.md`](p34-integration.md) —— P34 会话、子任务、导出、回滚、
  新 VM 恢复的完整示例。
- [`mcp-contract.md`](mcp-contract.md) —— 参数、返回字段、错误码。
- [`sync-contract.md`](sync-contract.md) —— 远端包格式、push/fetch/pull/restore。

## 我要部署 / 运维

- [`../packaging/p36/README.md`](../packaging/p36/README.md) —— P36 payload、
  L1 胶水、systemd、应用卷热插拔。
- [`operations.md`](operations.md) —— 升级、迁移、备份、GC 维护窗口、监控、
  常见故障。
- [`cli.md`](cli.md) —— `p37` / `p37d` 命令与退出码。
- [`config.md`](config.md) —— 全部配置字段和默认值。
- [`../VERIFY.md`](../VERIFY.md) —— 真机验证清单。
- [`acceptance.md`](acceptance.md) —— 自动化与 P36 验收矩阵。

## 我要改代码

- [`../CONTRIBUTING.md`](../CONTRIBUTING.md) —— 构建、测试、交叉编译、发布、
  文档维护约定。
- [`architecture.md`](architecture.md) —— 模块边界与关键不变量。
- [`concurrency.md`](concurrency.md) —— 代码里实际使用的锁和串行化方式。
- [`format-v2.md`](format-v2.md) —— 仓库格式与迁移。
- [`../PROGRESS.md`](../PROGRESS.md) —— 实现历史和真机缺陷复盘（不是当前状态
  的唯一来源）。

## 当前状态与历史

- [`status.md`](status.md) —— 当前里程碑、已知缺口、支持版本。
- [`../PROGRESS.md`](../PROGRESS.md) —— 按时间倒序的实现记录和 S 系列缺陷复盘。
- [`../packaging/p36/RELEASE_NOTES.md`](../packaging/p36/RELEASE_NOTES.md) —— 最近一次发布说明模板
  （`make release` 会生成 `dist/RELEASE_NOTES.md`）。

## 文档维护约定

- 工具名、参数、返回字段、错误码以 `internal/mcpsrv` 的注册为准；
  `mcp-contract.md` 必须覆盖全部已注册工具。
- 配置默认值以 `internal/config/config.go` 的 `Default()` 为准；
  P36 模板的差异在 `packaging/p36/config.toml` 中单独标注。
- 版本号以 git tag 为准；`README.md`、`Makefile`、`internal/buildinfo` 必须与
  最新 tag 一致。
- 历史记录写在 `PROGRESS.md`；当前状态写在 `status.md`。不要把“本轮”“目前”
  这类相对时间词写进长期文档。
