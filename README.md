# qry-partition-compactor

把数据表的分区键、分区清单、文件数量与压缩策略记录成可查询的服务，支持按表评估小文件比例、给出压缩建议并跟踪压缩任务进度。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `qry-partition-compactor.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /api/v1/tables/{id}/partitions`

上报一个分区的快照。路径 `id` 是表编号，首次上报时自动登记表。请求体：

```json
{"partition_key":"dt","partition_value":"2024-02","file_count":3,"target_file_size":1048576}
```

成功时 HTTP 200，返回保存后的快照（`table_id`、`partition_key`、`partition_value`、`file_count`、`target_file_size`）。同一表编号与分区值再次上报会更新原记录而不是新增。

字段缺失、`file_count` 小于 0、`target_file_size` 不大于 0 或字段类型错误时 HTTP 400，`code` 为 `invalid_input`，`message` 指明具体缺失或无效项。

### `GET /api/v1/tables/{id}/advice`

按分区值升序返回该表每个分区的压缩建议，只读、不改变任何任务状态。表不存在时 HTTP 404，`code` 为 `table_not_found`。成功时 HTTP 200：

```json
{"table_id":1,"advice":[{"partition_key":"dt","partition_value":"2024-02","file_count":3,"target_file_size":1048576,"action":"compact","reason":"multiple_files"}]}
```

`action` 与 `reason` 的取值由 `file_count` 决定，建议以 `target_file_size` 为目标：

| `file_count` | `action` | `reason` |
|---|---|---|
| 0 | `no_compaction` | `empty_partition` |
| 1 | `no_compaction` | `already_compact` |
| 大于 1 | `compact` | `multiple_files` |

### `POST /api/v1/compactions`

基于已上报的分区快照创建压缩任务。请求体：

```json
{"table_id":1,"partition_value":"2024-02"}
```

成功时 HTTP 201，返回任务 `id`、`status`（初始为 `running`）与快照字段。输入缺失或类型错误时 HTTP 400（`invalid_input`）；表不存在时 HTTP 404（`table_not_found`）；分区不存在时 HTTP 404（`partition_not_found`）；同一表与分区已有 `running` 任务时 HTTP 409（`compaction_in_progress`），不覆盖也不新建。

### `GET /api/v1/compactions/{id}`

查询一个压缩任务，返回 `id`、`status` 与快照字段，查询不改变状态。未知 `id` 返回 HTTP 404，`code` 为 `compaction_not_found`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
