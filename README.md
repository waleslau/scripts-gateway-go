# 脚本执行网关 (Scripts Gateway)

对外暴露 RESTful 接口，将 **HTTP 接口 ↔ Shell 脚本** 通过 `config.yaml` 建立映射。
调用方按参数请求接口后，服务在工作目录中执行对应的脚本，并返回退出码、输出与耗时。

- 需求文档：[REQUIREMENTS.md](./REQUIREMENTS.md)
- 实现语言：Go（标准库 net/http + yaml.v3），单二进制部署，无其他运行时依赖。

## 快速开始

```bash
# 1. 构建
go build -o scripts-gateway-go .

# 2. 运行（默认加载 ./config.yaml）
./scripts-gateway-go -config config.yaml

# 3. 调用
curl -X POST http://localhost:8080/api/v1/tasks/deploy \
  -H "Content-Type: application/json" \
  -d '{"env": "prod", "version": "1.2.3"}' | jq

curl -X POST http://localhost:8080/api/v1/tasks/backup \
  -d '{"target": "/path/to/target"}' \
  -H "Authorization: Bearer abc123" \
  -H "Content-Type: application/json" | jq

curl -H "Authorization: Bearer abc123" \
     -X GET http://localhost:8080/api/v1/tasks | jq

curl -H "Authorization: Bearer abc123" \
     -X POST http://localhost:8080/api/v1/tasks/backup-scripts | jq
```

响应示例：

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "exit_code": 0,
    "stdout": "开始部署 [环境=prod, 版本=1.2.3]\n部署完成: prod/1.2.3\n",
    "stderr": "",
    "duration_ms": 2013,
    "timed_out": false
  }
}
```

## 接口一览

| 接口                        | 说明                                                         |
| --------------------------- | ------------------------------------------------------------ |
| `POST /api/v1/tasks/{name}` | 执行指定任务（方法由映射配置决定，支持 GET/POST/PUT/DELETE） |
| `GET  /api/v1/tasks`        | 列出所有可用任务及参数说明                                   |
| `GET  /api/v1/history`      | 查询执行历史（需 `history.enabled: true`）                   |
| `GET  /api/v1/history/{id}` | 查询单条执行记录详情（含完整 stdout/stderr）                 |
| `DELETE /api/v1/history`    | 清空全部执行历史                                             |
| `GET  /healthz`             | 健康检查                                                     |

参数支持 **JSON Body**、**表单（form-urlencoded）** 与 **Query String** 三种方式（请求体优先于 Query String）。

JSON 请求体**无需强制携带** `Content-Type: application/json` 头：服务端会按请求体内容自动识别，以 `{` 开头的请求体即按 JSON 解析。因此以下写法等价：

```bash
# 写法一：带 Content-Type 头
curl -X POST http://localhost:8080/api/v1/tasks/deploy \
  -H "Content-Type: application/json" \
  -d '{"env": "prod", "version": "1.2.3"}'

# 写法二：不带 Content-Type 头（curl -d 默认按 form 发送，服务端自动识别为 JSON）
curl -X POST http://localhost:8080/api/v1/tasks/deploy \
  -d '{"env": "prod", "version": "1.2.3"}'

# 写法三：表单
curl -X POST http://localhost:8080/api/v1/tasks/deploy \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d 'env=prod&version=1.2.3'

# 写法四：Query 方式
curl -X POST "http://localhost:8080/api/v1/tasks/deploy?env=prod&version=1.2.3"

# GET 方式（report 映射配置为 GET）
curl "http://localhost:8080/api/v1/tasks/report?days=30"

# 任务列表
curl http://localhost:8080/api/v1/tasks
```

> 注：写法二依赖请求体以 `{` 开头且为合法 JSON 对象；其他情况请明确携带 `Content-Type` 头。

## stdout 返回格式

`server.stdout_format` 控制**任务执行响应**中 `data.stdout` 的形态：
执行历史的落盘 JSONL 与查询接口由独立的 `history.stdout_format` 控制
（见「执行历史与日志持久化」；未配置时默认跟随 `server.stdout_format`）。

- `text`（默认）：原样字符串，如 `"备份完成: ...\n-rw-r--r-- ...\n"`
- `lines`：按行拆分为数组，每行一个元素，如 `["备份完成: ...", "-rw-r--r-- ..."]`
  （结尾换行不产生多余空元素，空输出为 `[]`）

```yaml
server:
  stdout_format: "lines" # 或 "text"
```

启用后响应示例：

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "exit_code": 0,
    "stdout": [
      "备份完成: ./backups/backup_20260806225446.tar.gz",
      "-rw-r--r-- 1 idea idea 13K 8月 6日 22:54 ./backups/backup_20260806225446.tar.gz"
    ],
    "stderr": "",
    "duration_ms": 8,
    "timed_out": false
  }
}
```

## 参数如何传递给脚本

以映射 `{name: backup, params: [target, destination]}` 为例，请求
`POST /api/v1/tasks/backup` + body `{"target": "./data", "destination": "./backups"}` 会执行：

```bash
bash ./scripts/backup.sh ./data ./backups
```

- 位置参数 `$1 $2 ...`：按配置中 `params` 的声明顺序。
- 环境变量 `PARAM_<NAME>`：`target` → `PARAM_TARGET`，`destination` → `PARAM_DESTINATION`（便于脚本内具名引用）。
- 必填参数缺失返回 400；带 `default` 的参数未提供时自动取默认值。

## 配置

见 [`config.yaml`](./config.yaml)。要点：

```yaml
server:
  addr: ":8080"
  auth_token: "" # 非空则启用鉴权（Authorization: Bearer <token> 或 X-Auth-Token）
  stdout_format: "lines" # 任务结果 stdout 格式：text=原样字符串（默认）；lines=按行拆分为数组
  max_concurrent: 0 # 并发执行上限（0=不限；达到上限时新请求返回 503）

scripts_dir: "./scripts" # 脚本根目录（禁止越出，防路径穿越）
work_dir: "./" # 脚本进程工作目录
allow_outside_scripts: false # true 则允许 script 指向脚本目录之外（绝对路径或含 ../ 的相对路径）

mappings:
  - name: deploy
    method: POST
    script: deploy.sh
    timeout_seconds: 60
    params:
      - name: env
        required: true
        description: 部署环境
      - name: version
        required: true
```

## 运行参数

| 参数          | 默认          | 说明                                                  |
| ------------- | ------------- | ----------------------------------------------------- |
| `-config`     | `config.yaml` | 配置文件路径                                          |
| `-h`, `-help` | -             | 显示帮助信息（启动参数、配置项、REST 接口与调用示例） |

## 安全设计

- 默认脚本名含 `..` 或解析后超出 `scripts_dir` 一律拒绝（防路径穿越）；仅当配置 `allow_outside_scripts: true` 时才放开该限制。
- 参数通过 `exec.CommandContext` 以独立 argv 传入，**不拼接 shell 命令**（防注入）。
- 每个任务独立超时，超时**按进程组强制终止脚本及其所有子进程**，不会残留后台进程。
- stdout/stderr 上限 1MB，JSON Body 上限 1MB。
- 可选令牌鉴权（`auth_token`，除 `/healthz` 外所有接口生效，含执行历史查询；
  健康检查免鉴权，便于负载均衡探活）。
- 可选并发执行上限（`server.max_concurrent`，0=不限；达到上限时新请求立即返回 503，
  防止脚本执行被外部请求打满）。
- 执行历史目录/文件权限 0750/0640，仅服务账号可读；历史含请求参数与输出，可能含敏感信息（未脱敏），需做好目录访问控制（见「执行历史与日志持久化」安全提示）。

## 配置热更新

服务运行期间**每秒轮询监测配置文件**，修改保存后自动生效，**无需重启进程**：

| 配置项                                               | 生效方式                                     |
| ---------------------------------------------------- | -------------------------------------------- |
| `mappings`（增删改任务）                             | 立即生效（下次请求即按新映射路由）           |
| `server.auth_token`                                  | 立即生效（每次请求读取当前令牌）             |
| `server.stdout_format`                               | 立即生效                                     |
| `history.stdout_format`                              | 立即生效（server 层读取当前配置，无需重建存储） |
| `server.max_concurrent`                              | 立即生效（并发执行上限调整无需重启）         |
| `scripts_dir` / `work_dir` / `allow_outside_scripts` | 自动重建执行器并原子切换                     |
| `server.addr`                                        | 自动切换监听：先绑定新地址，再优雅关闭旧监听 |

- 通过内容哈希判断是否真正变化，仅注释/空白修改不会触发重载；
- 配置文件解析/校验失败、执行器重建失败、新地址无法绑定时，**保持原配置生效**并记录 WARN 日志（修正后再次保存即可生效）；
- 变更按"全部准备就绪再统一生效"执行，保证配置与实际运行状态一致。

### 配置回滚（config.yaml.bak）

每次**成功应用**的配置都会自动原子备份到与配置文件同级的 `config.yaml.bak`（启动时也会备份初始配置）。新配置出差错时：

- **运行中的服务仍使用内存中的旧配置**，不会中断；
- 磁盘上的旧配置内容可在 `config.yaml.bak` 中找到，直接回滚即可：

```bash
cp config.yaml.bak config.yaml
```

> 说明：备份始终等于"当前正在生效的配置"。坏配置写入时不会覆盖备份，失败日志中也会同时打印备份文件路径（`backup=...`）。

## 执行历史与日志持久化

`history.enabled: true` 后，每次脚本执行都会以 **JSONL**（每行一条 JSON 记录）追加写入历史目录，并提供查询接口，服务重启后仍可追溯。

```yaml
history:
  enabled: false           # 总开关
  dir: "./history"         # 历史存储目录（自动创建，权限 0750，文件 0640）
  max_file_size_mb: 50     # 单个文件滚动阈值（MB），超出后封存并开新文件
  max_files: 25            # 保留文件数兜底（超出删除最旧的）
  retention_days: 60       # 记录保留天数（0 = 不按时间清理）
  max_output_bytes: 65536  # 每条记录持久化的 stdout/stderr 上限（字节）
  stdout_format: "lines"   # 历史落盘与查询接口 stdout 格式：text=原样字符串（默认）；lines=按行拆分为数组；未配置时跟随 server.stdout_format
```

> `history.stdout_format` 与 `server.stdout_format` 相互独立：前者控制历史落盘 JSONL 与查询接口中 stdout 的形态，后者只控制任务执行响应；历史未配置 `stdout_format` 时默认跟随 `server.stdout_format`。

**记录策略**：

- 成功执行（退出码 0 且无系统错误）**始终记录**；
- 失败执行（退出码非 0 或超时/启动失败等系统错误）：**单个任务连续失败 3 次以内记录**，第 4 次起跳过，直到该任务下一次成功执行后重置计数（计数仅存内存，服务重启后清零）。
- 未实际执行脚本的请求（路由 404、参数校验 400 等）不进入执行历史，由访问日志覆盖。
- 每条记录的 stdout/stderr 按 `max_output_bytes` 截断落盘（不影响实时返回给调用方的内容）。

每条记录包含：`id`（时间戳+随机）、`time`、`task`、`method`、`script`、`params`、`remote_addr`、`exit_code`（`-1` 表示系统级失败）、`duration_ms`、`timed_out`、`http_status`、`stdout`、`stderr`、`error`。`stdout` 的落盘形态遵循 `history.stdout_format`（未配置时跟随 `server.stdout_format`）：`lines` 时为行数组，`text` 时为原始字符串。

**查询接口**：

```bash
# 列表（按时间倒序，分页 + 过滤；默认不含 stdout/stderr，减小响应）
curl -H "Authorization: Bearer abc123" "http://localhost:8080/api/v1/history?task=deploy&exit_code=1&limit=20&offset=0"
curl -H "Authorization: Bearer abc123" "http://localhost:8080/api/v1/history?from=2026-08-01T00:00:00%2B08:00&to=2026-08-02T00:00:00%2B08:00"

# 详情（含完整 stdout/stderr 与参数；stdout 形态遵循 history.stdout_format，lines 时为行数组）
curl -H "Authorization: Bearer abc123" "http://localhost:8080/api/v1/history/20260807T011639-0deb1076"

# 清空
curl -X DELETE -H "Authorization: Bearer abc123" "http://localhost:8080/api/v1/history"
```

列表响应：`data.total` 为过滤后总数，`data.items` 为当前页（默认 20 条，上限 500）。`exit_code=-1` 表示系统级失败（超时等）。`from`/`to` 为 RFC3339 时间。

**存储格式与生命周期**：

- 目录下形如 `run-20260807T011638-26cf36d6.jsonl` 的文件，可直接 `jq`/`grep` 排查；当前追加的文件即最新的一个。
- 单文件超过 `max_file_size_mb` 时滚动封存并开新文件；文件数超过 `max_files` 或按修改时间超过 `retention_days` 时自动删除最旧文件（启动时 + 每 6 小时清理一次）。
- 崩溃安全：重启时自动截断末尾不完整的半行，保证文件始终是合法 JSONL。

**安全提示**：执行历史包含请求参数与脚本输出，可能含有敏感信息（令牌、密码等），当前**未做脱敏**。请确保：

- 历史目录与文件权限保持默认的 0750/0640，仅允许服务运行账号读取；
- 查询/清空接口受 `auth_token` 鉴权保护（除 `/healthz` 外全局生效）；
- 避免在参数中传递敏感凭据；如确需传递，建议对历史目录做额外访问控制。

> 配置热更新同样适用于 `history` 段：修改保存后自动重建存储并原子切换；连续失败计数会随重建清零。

```bash
# 示例：新增一个任务映射，保存 config.yaml 后约 1 秒内即可调用
curl -X POST http://localhost:8080/api/v1/tasks/backup-scripts \
  -H "Authorization: Bearer abc123"
```

## 项目结构

```
.
├── REQUIREMENTS.md          # 需求文档
├── config.yaml              # 接口-脚本映射配置
├── main.go                  # 入口（配置加载、优雅关闭）
├── internal/
│   ├── config/config.go     # 配置解析与校验
│   ├── executor/executor.go # 脚本路径解析与执行
│   ├── history/             # 执行历史与日志持久化（JSONL 存储/滚动/清理/查询）
│   │   ├── record.go
│   │   └── store.go
│   └── server/server.go     # HTTP 路由、参数收集、鉴权、日志、历史接口
└── scripts/                 # 示例脚本
    ├── deploy.sh
    ├── backup.sh
    └── report.sh
```
