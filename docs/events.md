# P37 语义事件（v0.4.8+）

P37 在仓库状态发生变化时，向本机 unix socket 写一行 JSON。它只负责“通知变化”，
不携带数据；消费方收到后再调用 MCP `repo_usage` 取准确值。

## 配置

```toml
[events]
# 为空表示关闭；P36 模板默认监听 /srv/repos/.p36/p37-events.sock
unix_socket = "/srv/repos/.p36/p37-events.sock"
```

P37 以 best-effort 写入：socket 不存在、对端断开或超时都不会阻塞 repo 操作。

## 事件格式

```json
{
  "v": 1,
  "event": "version_landed",
  "repo": "p34-1",
  "root": "/srv/repos/p34-1",
  "head": "b6b8...",
  "version_id": "b6b8...",
  "ws_id": "ws_...",
  "ts": 1794096693616
}
```

字段：

| 字段 | 说明 |
|---|---|
| `v` | 事件格式版本，当前 `1` |
| `event` | 事件名 |
| `repo` | repo 名（通常是目录名） |
| `root` | repo 根路径 |
| `head` | 事件发生后的 mainline head（可能为空） |
| `version_id` | 新版本 id（land/revert/import 时） |
| `ws_id` | 工作区 id（workspace_changed 时） |
| `ts` | Unix 毫秒 |

## 事件名

| 事件 | 触发点 | 消费方动作 |
|---|---|---|
| `workspace_changed` | 工作区文件采集完成（debounce 后） | 只刷新 `workspace_bytes` 或做一次轻量 `repo_usage` |
| `version_landed` | `ws_land` / `ws_land_into` 成功 | 全量 `repo_usage` |
| `version_reverted` | `version_revert` / line reset 成功 | 全量 `repo_usage` |
| `sync_pulled` | `sync_pull` 快进 head | 全量 `repo_usage` |
| `repo_initialized` / `repo_imported` | 初始化/导入完成 | 全量 `repo_usage` |
| `gc_completed` | `gc --prune` / pack 完成 | 全量 `repo_usage` |

## P36 集成

P36 guest agent 监听该 socket，收到事件后 debounce 并调用 `repo_usage`，再通过
vsock 把结果推给宿主 Agent。宿主 Agent 用 `vm_app_volume_usage` 消息更新
`app_volumes`。没有事件时不做周期轮询；open/close/manual 作为兜底。
