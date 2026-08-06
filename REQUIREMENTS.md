# 脚本执行网关 — 需求文档

## 1. 项目概述

本项目提供一个轻量级 RESTful API 服务（Go 实现），核心能力是：

> 将 **HTTP 接口** 与 **Shell 脚本** 建立预先定义的映射关系，外部系统通过调用指定接口并携带参数，触发工作目录下对应脚本的执行，并将执行结果（退出码、标准输出、耗时等）返回给调用方。

典型场景：运维平台、CI/CD 流水线、后台管理工具等通过 HTTP 统一调用服务器上的脚本（备份、部署、巡检、报表等），避免直接暴露 SSH 或 shell 访问。

## 2. 目标与范围

### 2.1 目标

- 提供统一、安全的脚本调用入口，屏蔽底层 shell 细节。
- 通过配置（非代码）即可增删改"接口 → 脚本"映射，无需重新编译。
- 参数经由 HTTP 传入，并安全地传递给脚本（不拼接 shell 命令，杜绝注入）。

### 2.2 范围

- **包含**：RESTful 接口、映射配置、参数校验、脚本执行、超时控制、结果返回、可选鉴权、健康检查、任务列表查询。
- **不包含**（本期）：任务调度/队列、脚本日志持久化、多机分布式执行、前端页面。可作为后续扩展方向（见 §9）。

## 3. 术语

| 术语                    | 说明                                                    |
| ----------------------- | ------------------------------------------------------- |
| 任务（Task）            | 配置中定义的一个"接口-脚本"映射条目，用 `name` 唯一标识 |
| 映射（Mapping）         | `name + method + script + params` 的集合                |
| 工作目录（work_dir）    | 脚本进程的当前工作目录（`cmd.Dir`）                     |
| 脚本目录（scripts_dir） | 存放脚本的根目录，脚本路径不得越出该目录                |

## 4. 功能需求

| 编号  | 需求                                                                                                                                      | 优先级 |
| ----- | ----------------------------------------------------------------------------------------------------------------------------------------- | ------ |
| FR-1  | 服务启动时从 YAML 配置文件加载接口-脚本映射关系                                                                                           | P0     |
| FR-2  | 提供 `POST /api/v1/tasks/{name}` 执行指定任务（支持按映射配置的 method 调用）                                                             | P0     |
| FR-3  | 执行映射对应的脚本：`bash <script> [args...]`，工作目录为配置的 work_dir                                                                  | P0     |
| FR-4  | 请求参数支持 **JSON Body** 与 **Query String** 两种传入方式，合并后传递给脚本                                                             | P0     |
| FR-5  | 参数传递方式：① 按映射定义的顺序作为脚本位置参数 `$1 $2 ...`；② 注入为环境变量 `PARAM_<NAME>`（如 `target_dir` → `PARAM_TARGET_DIR`）  | P0     |
| FR-6  | 对映射中声明为 `required: true` 的参数做必填校验，缺失时返回 400                                                                          | P0     |
| FR-7  | 映射中声明了 `default` 的参数，未提供时自动取默认值                                                                                       | P1     |
| FR-8  | 每个映射可配置超时时间（`timeout_seconds`），超时强制终止脚本进程并返回 500                                                               | P0     |
| FR-9  | 返回脚本退出码、stdout、stderr、执行耗时；stdout/stderr 超过 1MB 截断返回                                                                 | P1     |
| FR-10 | 提供 `GET /healthz` 健康检查                                                                                                              | P1     |
| FR-11 | 提供 `GET /api/v1/tasks` 列出所有可用任务及参数说明                                                                                       | P1     |
| FR-12 | 配置可选的访问令牌（`auth_token`），开启后除 `/healthz` 外所有接口要求 `Authorization: Bearer <token>` 或 `X-Auth-Token`（健康检查免鉴权）    | P1     |
| FR-13 | 服务支持优雅关闭（SIGINT/SIGTERM）                                                                                                        | P1     |
| FR-14 | 结构化的访问日志（method、path、状态码、耗时）                                                                                            | P2     |
| FR-15 | 提供配置项 `allow_outside_scripts`，可选允许执行工作目录之外的脚本（默认关闭）；开启后映射中的 `script` 可为绝对路径或含 `../` 的相对路径 | P1     |
| FR-16 | 提供 `history` 配置段，启用后以 JSONL 追加写持久化每次脚本执行（含参数、退出码、耗时、stdout/stderr、调用方 IP 等）；默认关闭 | P1     |
| FR-17 | 记录策略：成功执行始终记录；单个任务连续失败 3 次以内记录，第 4 次起跳过，直到该任务下一次成功执行后重置（计数仅存内存，重启清零） | P1     |
| FR-18 | 历史文件按大小滚动、按文件数/天数保留清理（默认 50MB/25 文件/60 天），崩溃残留半行启动时自动截断 | P1     |
| FR-19 | 提供 `GET /api/v1/history`（分页+过滤，默认不含输出）、`GET /api/v1/history/{id}`（详情含完整输出）、`DELETE /api/v1/history`（清空），均受 `auth_token` 鉴权保护 | P1     |
| FR-20 | 提供 `server.max_concurrent` 并发执行上限（0=不限）；达到上限时新请求立即返回 503，防止脚本执行被打满 | P2     |

## 5. 接口设计

Base URL：`http://<host>:<port>`（默认 `:8080`）

### 5.1 执行任务

```
POST /api/v1/tasks/{name}
GET  /api/v1/tasks/{name}    （若映射配置的 method 为 GET）
```

- `{name}`：映射配置中的任务名。
- 参数来源：
  - JSON Body：`{"参数名": "值", ...}`，`Content-Type: application/json`
  - Query：`?参数名=值&...`
  - 两者可同时使用，JSON Body 优先。

**请求示例**

```bash
curl -X POST http://localhost:8080/api/v1/tasks/backup \
  -H "Content-Type: application/json" \
  -d '{"target": "./data", "destination": "./backups"}'
```

**响应示例（成功）**

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "exit_code": 0,
    "stdout": "备份完成: ./backups/backup_20240101120000.tar.gz\n...",
    "stderr": "",
    "duration_ms": 156,
    "timed_out": false
  }
}
```

### 5.2 任务列表

```
GET /api/v1/tasks
```

返回所有任务名、HTTP 方法、脚本路径、超时与参数说明（不含敏感配置）。

### 5.3 健康检查

```
GET /healthz
```

返回 `{"code": 0, "message": "ok"}`，服务存活即返回 200。

### 5.4 执行历史

```
GET    /api/v1/history
       ?task=<任务名>&exit_code=<退出码>&from=<RFC3339>&to=<RFC3339>&limit=<默认20，上限500>&offset=<默认0>
GET    /api/v1/history/{id}
DELETE /api/v1/history
```

- 列表按时间倒序，`data.total` 为过滤后总数；默认不含 stdout/stderr（详情接口返回完整输出）。
- `exit_code=-1` 表示系统级失败（超时/启动失败等）。
- 未启用 `history.enabled` 时三个接口均返回 404。
- 存储格式：JSONL（每行一条记录），文件名 `run-<时间戳>-<随机>.jsonl`；单文件超过 `max_file_size_mb` 滚动封存；启动时 + 每 6 小时清理超过 `max_files` 或 `retention_days` 的文件。

### 5.5 状态码与业务码

| HTTP | 业务 code | 含义                                              |
| ---- | --------- | ------------------------------------------------- |
| 200  | 0         | 脚本执行成功（exit_code=0）                       |
| 200  | 1001      | 脚本执行完成但退出码非 0，详情见 `data.exit_code` |
| 400  | 400       | 参数缺失/非法、脚本路径越界                       |
| 401  | 401       | 未授权                                            |
| 404  | 404       | 任务不存在                                        |
| 405  | 405       | HTTP 方法与映射配置的 method 不符                 |
| 500  | 500       | 脚本超时或系统错误                                |

## 6. 配置设计（config.yaml）

```yaml
server:
  addr: ":8080" # 监听地址
  auth_token: "" # 访问令牌，为空则关闭鉴权
  stdout_format: text # 任务结果 stdout 格式：text（默认）/ lines
  max_concurrent: 0 # 并发执行上限（0 = 不限；达到上限返回 503）

history:
  enabled: false          # 执行历史总开关，默认关闭
  dir: "./history"        # 存储目录（自动创建，权限 0750）
  max_file_size_mb: 50    # 单个文件滚动阈值（MB）
  max_files: 25           # 保留文件数
  retention_days: 60      # 保留天数（0 = 不按时间清理）
  max_output_bytes: 65536 # 每条记录 stdout/stderr 持久化上限（字节）

scripts_dir: "./scripts" # 脚本根目录
work_dir: "./" # 脚本工作目录

mappings:
  - name: deploy # 任务名（接口路径 {name}）
    method: POST # 允许的 HTTP 方法
    script: deploy.sh # 脚本名（相对 scripts_dir）
    timeout_seconds: 60 # 超时（默认 30）
    params: # 参数定义（顺序即 $1,$2... 顺序）
      - name: env #   参数名
        required: true #   是否必填
        default: "" #   默认值
        description: 部署环境 #   说明（用于任务列表展示）
      - name: version
        required: true
```

映射校验规则：`name` 唯一且非空、`script` 非空、`method` 属于 GET/POST/PUT/DELETE、参数名不重复。

## 7. 安全需求

| 编号 | 需求                   | 说明                                                                                                                                                        |
| ---- | ---------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| SR-1 | 脚本路径白名单（默认） | 脚本名不得包含 `..`，解析后的绝对路径必须位于 `scripts_dir` 内，防止路径穿越。该限制可通过配置项 `allow_outside_scripts: true` 显式放开，需确保映射配置可信 |
| SR-2 | 禁止命令拼接           | 参数一律通过 `exec.CommandContext` 以独立 argv 传入，**不得**拼接进 shell 命令字符串                                                                        |
| SR-3 | 超时强杀               | 超时后通过进程组方式终止脚本及其子进程                                                                                                                      |
| SR-4 | 输出限制               | stdout/stderr 上限 1MB，防止内存膨胀与日志洪泛                                                                                                              |
| SR-5 | 可选鉴权               | 配置 `auth_token` 后，非授权请求返回 401                                                                                                                    |
| SR-6 | 请求体限制             | JSON Body 上限 1MB                                                                                                                                          |
| SR-7 | 历史数据访问控制       | 执行历史目录/文件权限 0750/0640；查询/清空接口受 `auth_token` 保护；历史含参数与输出可能含敏感信息（未脱敏），需做好目录访问控制 |

## 8. 非功能需求

| 类别     | 要求                                                                                       |
| -------- | ------------------------------------------------------------------------------------------ |
| 性能     | 每个请求独立 goroutine 执行；单脚本默认超时 30s，可配置                                    |
| 可靠性   | 脚本崩溃不影响服务进程；启动时校验配置，非法配置拒绝启动                                   |
| 可维护性 | 分层：config（配置）/ executor（执行）/ server（HTTP），无第三方框架依赖（仅 YAML 解析库） |
| 可观测性 | 结构化日志记录每个请求与执行结果                                                           |
| 部署     | 单二进制，`go build` 即可，无运行时依赖                                                    |

## 9. 后续扩展（Roadmap）

- 异步任务：`202 Accepted` + 任务 ID，轮询/回调获取结果
- ~~任务并发上限与排队（信号量/worker 池）~~ **（已实现：`server.max_concurrent`，0=不限，达到上限返回 503）**
- 任务排队：达到并发上限时先排队而非直接 503
- ~~执行历史与日志持久化~~ **（已实现：见 README「执行历史与日志持久化」）**
- ~~接口-脚本映射的运行时热加载~~ **（已实现：见 README「配置热更新」）**
- gRPC / OpenAPI 文档生成
