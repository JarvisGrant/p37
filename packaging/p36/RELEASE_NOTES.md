# p37 @VERSION@ — 部署导出（repo_export）加固

## 发布物

- 平台：`linux/amd64`，静态链接（`CGO_ENABLED=0`）
- 资产：
  - `p37-linux-amd64-@VERSION@.tar.gz`
  - `p37-linux-amd64-@VERSION@.tar.gz.sha256`
- tar 根目录：`p37`、`p37d`、`manifest.json`

## manifest

```json
{
  "version": "@VERSION@",
  "commit": "@COMMIT@",
  "buildTime": "@BUILD_TIME@",
  "goVersion": "@GO_VERSION@"
}
```

## 本版重点（v0.3.1）

面向 P34「不创建 workspace、直接从版本线导出并构建部署」的场景：

- **导出不再整文件进内存**：`repo_export` 按 CAS 分块流式写文件，
  `version_archive` 也改为流式写入；1 GiB 实例导出大文件不会再因为单个文件
  触发 OOM。
- **`version_file` 只读前缀**：不再先 `ReadFileData` 再截断；25 MiB/更大文件
  也只读取 `max_bytes` 对应的分块，返回 `size` + `truncated`。
- **导出目录护栏**：`dest_dir` 必须是绝对路径、位于仓库目录之外；配置
  `[repo] export_root` 后还必须位于该目录之下。拒绝目标本身是 symlink。
  P36 模板默认 `export_root = "/srv/repos/.p37/tmp/p34-deploy"`。
- **结构化错误**：新增 `destination_not_empty`；非法导出目录返回
  `invalid_argument`；`line:` 引用不存在返回 `line_not_found`（不再误报
  `ws_not_found`）。
- **导出结果可观测**：`repo_export` 返回 `version_id` / `files` / `bytes` /
  `duration_ms`，调用方可以确认实际导出的版本和内容量。
- **异步导出**：`repo_export(async=true)` 返回 `job_id`，通过新增的
  `repo_export_status` 轮询，避免大仓库导出撞上 Cloudflare 100 秒限制。
  任务只在当前 daemon 进程内有效，daemon 重启后失效。
- **保留语义不变**：导出仍然不创建 workspace、不占 `ws_quota`、不出现在
  `ws_list`、不被 fs watcher 采集；构建产物不会进入 CAS/op/版本。
- **可执行位/chmod 加固**：导出文件显式 chmod，避免 umask 抹掉可执行位。

## 接口变化（P34 对接）

```text
repo_export(repo, ref, dest_dir, async=false)
→ { exported, ref, dest_dir, version_id, files, bytes, duration_ms }

repo_export_status(repo, job_id)
→ { job_id, repo, ref, dest_dir, state, started_ts, finished_ts?,
    version_id?, files?, bytes?, error? }
```

- `dest_dir` 必须不存在或为空；非空返回 `destination_not_empty`。
- 每次 job 使用唯一目录；P37 不自动清理导出目录，调用方负责 `rm -rf`。
- 异步任务不持久化；daemon 重启后请重新发起导出并清理残留目录。

## 配置变化

```toml
[repo]
format_ver = 2
export_root = "/srv/repos/.p37/tmp/p34-deploy"
```

- `export_root` 为空时只要求目标在仓库目录之外（兼容旧行为）。
- P36 模板已默认设置为 `/srv/repos/.p37/tmp/p34-deploy`。
- 已有仓库的 `.p37/config.toml` 不会自动改写；需要限制导出路径时手工添加。

## 兼容性

- 仓库格式仍为 `format_ver=2`，无需迁移。
- 现有 `ws_*`、`line_*`、`version_*`、`sync_*` 行为不变。
- `repo_export` 从“返回 `exported/ref/dest_dir`”扩展为额外返回
  `version_id/files/bytes/duration_ms`；旧字段保留。
- `dest_dir` 现在必须是绝对路径且位于仓库外；原先写到相对路径或仓库内的调用
  需要调整。

## P36 集成要点

- 平台胶水放在 **L1**，不改 L0；只有 `p37`/`p37d` 在 payload。
- payload：ext4、label `P37PAYLOAD`、**固定 96 MiB**、只读挂载到
  `/run/p37-payload`。
- P36 设备顺序：`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；按 label 挂载，
  不依赖顺序。
- v0.2 起默认多仓库：根目录 `/srv/repos`（`p36:p36`，`0750`），
  `p37d --root /srv/repos`；旧 `/srv/repo` 原地注册为 `default`，不移动目录。
- 默认 `workspace.materialize = "auto"`：Ubuntu 26.04 非 root 下 OverlayFS
  可能 `EPERM`，自动回退 copy；copy 模式默认 `max_active=8`、
  `max_hidden=32`、`warn_disk_bytes=8 GiB`。
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部 `p37d --stdio --root /srv/repos`）。
- P36 模板默认 `[repo] export_root = "/srv/repos/.p37/tmp/p34-deploy"`。

部署细节与验收步骤见仓库 `packaging/p36/README.md` 与 `VERIFY.md`。

## 已知限制

- 异步导出任务不持久化；daemon 重启后 `job_id` 失效。
- 导出目录无 TTL，需调用方清理；`version_archive` 的 1 小时清理只作用于
  `<repo>/.p37/tmp/archive`。
- 在线 GC 未实现；`p37 gc` 在 daemon 运行时拒绝，需维护窗口。
- 历史版本仍按 `retention.keep_versions`（默认 200/线）保留，超出窗口且被 GC
  的版本导出时返回 `version_not_found`。
- copy 模式下 `p37 doctor` 仍会把 OverlayFS 标成 `FAIL`；正式门禁用
  `p37d --check --strict`（auto 下 overlay 只 WARN）。
- payload 换盘需要重启实例；服务级 restart 不保证换盘。
- `p37d --check` 有少量写探测，适合启动前检查，不建议作为在线高频探针。
- `ws_land_into` 的幂等只保证同一 VM 生命周期（restore 不恢复 workspace）。
