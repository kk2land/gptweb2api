# gptweb2api

单进程 Go 网关：把文本 Chat Completions 请求交给 ChatGPT 网页或 Dot，经官方 OpenAI Secure MCP Tunnel 执行本地文件工具并回传完整答案。

**当前状态：核心代码和离线测试已落地，真实账号端到端验收尚未完成。** 本地测试覆盖了官方 SDK 的模拟控制面、MCP 内存传输、文件修改和完成回传；不代表已经连上用户的 Tunnel、Ego、ChatGPT 项目或 Dot。网页脚本中的选择器需要在目标 Ego/ChatGPT 版本验证，证据不足时会停止执行。[实施状态](agents/implementation-status.md)记录具体边界。

## 构建和启动

目标平台为 Linux 与 macOS。构建使用固定 Go 1.27.3，SQLite 不依赖 CGO。运行时需要 PATH 中的 `ego-browser`、已登录的 Ego Profile、OpenAI Tunnel 与其 runtime key。MCP 插件名固定为 `gptweb2api`。

```sh
sh build.sh
cp config/mcp-gateway.example.toml config/mcp-gateway.toml
chmod 600 config/mcp-gateway.toml
# 编辑实际配置，填写不同的调用方 API key、Tunnel ID 和 runtime API key。
bin/mcp-gateway --check-config
sh sbin/mcp-gateway.sh start
sh sbin/mcp-gateway.sh status
sh sbin/mcp-gateway.sh stop
sh sbin/mcp-gateway.sh restart
```

脚本从自身位置确定项目根目录；二进制固定放在 `bin/`。配置只读取 `config/mcp-gateway.toml`，不提供环境变量覆盖或第二套高级配置。未知字段、占位符、无效期限和非回环监听均拒绝启动。修改配置后排空并重启。直接运行二进制时应使用绝对路径，以便启停脚本通过 `ps` 校验进程命令与启动时间。

Nginx 配置参考 [nginx.example.conf](config/nginx.example.conf)，放入已有的 HTTPS server 块。Go 只监听回环 HTTP；MCP 不经 Nginx，没有另开的 MCP 端口、relay 或 stdio MCP 子进程。

## 连接 ChatGPT

1. 在 OpenAI Platform 创建/选择 Tunnel，关联目标 ChatGPT 工作区，取得同组织的 Tunnels Read + Use runtime key。
2. 启动网关，在 ChatGPT 添加名为 `gptweb2api` 的私有自定义 MCP 插件，Connection 选择 Tunnel，填入相同 Tunnel ID。
3. 从 ChatGPT 实际调用 `health_check`。这是工具连通验证；SDK 首次轮询成功本身不代表业务闭环成功。
4. Ego 探测目标 Profile、Space、实际模型选项和插件稳定 ID。脚本使用精确模型匹配，不替换模型；界面缺少可核实的插件 ID 时不会仅凭同名按钮发送任务。
5. 查看经过 API key 认证的 `/health/ready` 和 `runtime/logs/mcp-gateway.log`。首次未就绪时每 30 秒在空闲状态重试，成功后每 30 分钟检查。
6. 先完成单任务文件读写和 MCP 回传实测，再验收项目、续聊、并发和故障恢复。页面不兼容时调整 `ego_scripts/`，无需重编译 Go。

```sh
curl -H 'Authorization: Bearer YOUR_API_KEY' http://127.0.0.1:8788/health/ready
curl -H 'Authorization: Bearer YOUR_API_KEY' http://127.0.0.1:8788/v1/models
```

`/health/live` 只报告进程存活。`/health/ready` 在没有可用模型时返回 503；`/v1/models` 返回已通过相应准备检查的模型。Dot 事件订阅与验证失败不会回退到网页路径。

## API

```sh
curl http://127.0.0.1:8788/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "web/gpt-6",
    "messages": [{"role":"user","content":"读取 README.md，说明项目用途"}],
    "x_gptweb2api": {"workdir":"/absolute/path/to/project"}
  }'
```

| 接口 | 行为 |
| --- | --- |
| `GET /v1/models` | 仅列出已就绪模型 |
| `POST /v1/chat/completions` | 等待 `complete_task` 的完整答案 |
| `POST /v1/tasks` | 异步提交，返回 202 与任务 ID |
| `GET /v1/tasks/{id}` | 查询活动任务、终态、结果或错误 |
| `POST /v1/tasks/{id}/cancel` | 显式取消，已发生的修改不会撤销 |

`stream=true` 返回标准 SSE：角色 chunk、保活、最终完整文本、结束 chunk 和 `[DONE]`，不承诺逐 token。流中失败返回 `data: {"error":...}`，随后 `[DONE]`，不会伪造成功结束。非流式等待最多 5 分钟，等待超时返回可查询 ID，任务仍按总期限继续运行。

所有提交都返回 `X-Request-ID` 与 `Location`。网页路径额外返回 `X-Gptweb2api-Session-ID`，成功响应的 `x_gptweb2api.session_id` 也含同一值。续聊时将其放回请求的 `x_gptweb2api.session_id`，并提交完整、未改写的历史及新消息；网关仅发送增量。目录、项目、模型或历史冲突时拒绝续聊。需要改变绑定时创建新会话。

`workdir` 可省略，此时仅支持聊天，文件工具拒绝执行。非空时必须是现有绝对目录；目录设备/inode 身份与本轮凭据绑定。`chatgpt_project_id` 只接受项目标识，不接受 URL；为空时使用普通聊天。

只接受文本 `user`、`assistant`、`system`、`developer` 消息。网页中的角色上下文不具备官方 API 的强制优先级。采样参数、`tools`、`tool_calls`、附件、结构化输出和 `/v1/responses` 均未支持，明确报错；不伪造 token usage。

## MCP 文件与结果协议

每轮生成随机 `turn_token`，数据库只保存摘要；API key 和 Tunnel runtime key 不写入提示词。模型先调用 `start_task`，再调用本轮工具，最后调用 `complete_task`。完整答案以 MCP 的 `final_answer` 为准。

可用工具为 `health_check`、`start_task`、`list_files`、`read_file`、`apply_patch`、`report_progress`、`complete_task`。`run_command` 与 `get_command_status` 明确返回 `commands_disabled`，首版不开放任意命令。

`apply_patch` 首版采用单文件精确文本替换：

```json
{
  "request_id": "本轮请求 ID",
  "turn_token": "本轮短期凭据",
  "operation_id": "本轮唯一的操作标识",
  "path": "README.md",
  "old_text": "唯一匹配的原文",
  "new_text": "替换后的文本"
}
```

创建新文件时使用 `create: true` 与空 `old_text`，父目录必须存在。读取和补丁目标限 UTF-8 普通文件及 1 MiB；路径由 Go `os.Root` 约束，阻止目录穿越和指向目录外的符号链接。替换写入临时文件并原子发布，先记录操作、再执行、再持久化结果；同一 `operation_id` 的相同请求返回原结果，不同内容返回冲突。执行状态未知时不重放写操作。

完成提交只允许 `succeeded` 或 `failed`；失败需提供 `error.code`、`error.message` 和可读答案。SQLite 提交成功后才确认；活动工具未结束时拒绝完成。相同完成结果可在 5 分钟内确认重试，不同结果不能覆盖终态。取消、过期和完成竞争只产生一个终态。

## Dot

默认 `dots.enabled=false`。即使设为 true，Dot 也不会立即出现在模型列表。先让目标 Dot 经同一 Tunnel 订阅 `task.available`，过滤参数为 `{"queue":"default"}`。网关验证 OpenAI 提供的 HTTPS callback 与 `whsec_` secret，出站发送签名校准事件；Dot 领取校准任务、调用 `start_task` 并原样提交 nonce 后，才开放 `dots`。

事件接口为 MCP 2.0 的 `server/discover`、`events/list`、`events/subscribe`、`events/unsubscribe`。采用 Standard Webhooks 签名，拒绝非公开网络地址、DNS 重绑定和重定向；事件内容不包含 messages、目录或任务凭据。订阅有效期 24 小时，按 `refreshBefore` 续期。outbox 保存稳定事件 ID 与原始字节，失败有界退避重试。

Dot 使用 `claim_task` 领取任务，取得完整 `messages`、`workdir`、短期凭据和 90 秒租约，执行期间调用 `renew_task`，总期限不延长。租约过期明确失败，不重新执行未知副作用。同一运行时 Dot 身份只有一个活动任务，并共享全局 Space 配额。Dot 不导航页面、不选择项目、不切换原生会话；请求里的 session/project 字段不参与路由。网关不探测客户端是否保存会话。

## 运行数据与验证

任务、结果、历史、文件操作日志、订阅、outbox 和 Ego 身份保存在 `runtime/data/mcp-gateway.sqlite`，启用 WAL 和 5 秒 busy timeout。重启保留排队任务；执行中的任务标记 `restart_uncertain`，会话失效，网页先核对并清理，不自动重发原任务。

每个活动槽独占一个 Space，同会话串行，业务完成后仍等待页面停止才释放槽；无法证明清理成功时隔离槽。空闲时每 6 小时在 `runtime/smoke/` 做小型文件读写与 nonce 回传冒烟，不额外扩容。终态数据每小时清理，保留 7 天。网关先停止接收新任务，最多排空 60 秒，到期取消剩余任务，再停止内嵌 SDK。

```sh
go test -race ./src/...
go vet ./src/...
node --test ego_scripts/scripts.test.cjs
```

Go 测试使用临时目录、本地模拟控制面和模拟 callback，不使用真实账号。Node 测试验证脚本语法及模型、插件、会话预检规则，不证明选择器适用于当前 ChatGPT 页面。

原始约束见 [技术方案](agents/implementation-plan.md) 与 [配置说明](agents/configuration.md)。
