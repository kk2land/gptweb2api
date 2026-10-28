# 单进程 gateway 与精简 TOML 配置

- 日期：2026-10-28（Asia/Shanghai）
- 状态：配置加载、网关与启动脚本已实现并进行离线验证；Tunnel 注册、Ego 页面与真实账号验收尚未完成。见 [实施与验证记录](implementation-status.md)。
- 主方案：[技术方案与实施计划](implementation-plan.md)

## 1. 部署方式

只构建并部署一个 mcp-gateway Go 服务，包含 API、任务管理、Ego 调度、MCP 和官方 Tunnel Go SDK。MCP server 与 SDK 使用内存传输，不启动独立 tunnel-client 或额外 stdio 服务进程。Ego 浏览器、其 CLI 和任务命令仍是依赖进程。

Nginx 继续提供兼容 API 的 HTTPS 入口，转发给本机 HTTP。ChatGPT 的 MCP 调用由 OpenAI 托管 Tunnel 接入，本地 SDK 主动出站获取调用并回传结果；这条链路不经 Nginx，也不开放 MCP HTTP 端口。

取消自建公网中继及其配置、二进制、PID、数据库和启停脚本。官方 Tunnel 不是兼容 API 的公网反向代理，API 调用方仍需能访问 Nginx 入口。本方案面向自用的私有 MCP 插件，公开插件市场分发不在范围内。

## 2. 固定目录

| 路径 | 用途 |
| --- | --- |
| go.mod、go.sum | Go 模块与锁定依赖 |
| src/cmd/mcp-gateway/ | 单个服务入口 |
| src/internal/ | API、任务、MCP、Tunnel 适配与浏览器调度 |
| ego_scripts/ | 独立 JS，修改无需重编译 Go |
| bin/mcp-gateway | 编译后的网关 |
| config/mcp-gateway.toml | 实际部署配置 |
| config/mcp-gateway.example.toml | 不含有效密钥的样例 |
| runtime/logs/mcp-gateway.log | 文本滚动日志，包括适配后的 SDK 日志 |
| runtime/mcp-gateway.pid | 网关 PID |
| runtime/data/mcp-gateway.sqlite | 任务、结果、会话绑定、订阅和事件 outbox |
| runtime/smoke/ | 专用冒烟测试区 |
| sbin/mcp-gateway.sh | start / stop / restart / status |
| build.sh | 根目录构建入口，输出一个二进制 |

路径以项目根目录为基准，不依赖当前工作目录。启动/停止脚本核对进程身份，不能根据陈旧 PID 盲目杀死同号进程。构建时固定并验证 Tunnel SDK、MCP SDK 和 Go 工具链；不嵌入 JS，不在安装时追随未经验证的上游版本。

## 3. 唯一配置

完整样例：[mcp-gateway.example.toml](../config/mcp-gateway.example.toml)。

| 配置节 | 字段 | 用途 |
| --- | --- | --- |
| server | listen、api_key | Nginx 上游地址、调用方认证 |
| tunnel | id、api_key | OpenAI Tunnel ID、runtime API key |
| tasks | max_concurrency、queue_capacity、timeout | Space 并发、等待队列和任务总期限 |
| ego（可选） | app_name | 指定 Ego 浏览器实例/服务名；空值使用默认实例 |
| dots | enabled | 是否开放已完成事件验收的 dots 模型 |
| log | max_size_mb、max_backups | 日志大小与历史份数 |

两处 api_key 是不同凭据：server.api_key 给 API 调用方使用，tunnel.api_key 仅供内嵌 SDK 访问 OpenAI 控制面，不能互换。实际值由部署配置填写，不提交仓库、不写提示词或日志。缺失或仍为占位符时拒绝启动。

只读取这一份 TOML。未知字段、错误类型、非正限制和无效时长明确报错；不增加隐藏环境变量、TLS、PID、路径、设备路由、OAuth issuer 或高级覆盖层配置，变更经排空与重启生效。Ego 仅提供用户要求的可选 app name，不开放 CLI 路径、脚本路径、Space 名称或 Profile 覆盖配置。

```toml
[ego]
app_name = "app-a"
```

app name 是 Ego 原生服务的实例名，去除首尾空白后最多 128 个字符，不允许控制字符。非空时，每次 Ego 调用均为 `ego-browser --ego-server-name=<app_name> nodejs`；通过独立进程参数传值，不拼接 shell，也不使用 `EGO_BROWSER_NAME` 环境变量代替实例路由。普通任务、轻量探测、完整冒烟、取消清理和 Dot Space 预留共用同一实例设置。

省略本节或 `app_name = ""` 时使用默认的 `ego-browser nodejs`。指定服务不可用时返回未就绪或明确失败，不尝试默认实例或其他 app。目标 Ego 必须预先运行并注册同名服务；上游浏览器启动端使用 `--startup-ego-browser-service=<app_name>`，CLI 端使用 `--ego-server-name=<app_name>`，见 [上游实例连接说明](https://github.com/citrolabs/ego-lite/issues/150)。

app name 与数字 Space/Page/Profile/插件标识一起持久化。切换实例时，在同一 SQLite 事务中记录新实例并使原网页会话失效，丢弃旧实例的所有浏览器标识；历史记录保留，调用方使用新会话。相同实例重启仍核对旧页面，不自动重发原任务。

Space 名称恢复为固定的 `gptweb2api-slot-0`、`gptweb2api-slot-1`，每个活动槽独占不同的数字 Space ID。上一版误加的 `[ego].space_name` 已移除；存在该配置时明确提示改用 `ego.app_name`，不会将它当作 app 名称静默使用。

## 4. Tunnel 初次接入

1. 在 [OpenAI Platform Tunnels](https://platform.openai.com/settings/organization/tunnels) 创建或选择 Tunnel；创建/编辑需 Tunnels Read + Manage。
2. 将 Tunnel 关联到目标 ChatGPT 工作区及相关 Platform 组织。
3. 创建同组织且具有 Tunnels Read + Use 的 runtime API key，填写 tunnel.id 和 tunnel.api_key。
4. 启动 gateway 内嵌 SDK，确认进程存活、连接就绪与本地 MCP 初始化完成。
5. 在 ChatGPT 添加私有自定义 MCP 插件，Connection 选择 Tunnel，使用同一 ID，安装并扫描工具。
6. 使用账号允许的认证选项；参考项目的私有连接采用无额外 OAuth，本项目不自建授权服务器。平台或工作区限制必须在真实账号验证。
7. 从 ChatGPT 实际调用测试工具，再验证带 request_id 的工具操作与 complete_task 回传。仅显示 ready、保存插件或列出工具不等于闭环成功。

账号中的平台权限和工作区设置不转化成一组本地高级配置。Tunnel 由 OpenAI 托管，但本地 gateway 仍须持续运行并能出站访问 OpenAI。

## 5. 请求参数与模型边界

首版按个人自用场景使用一个 API key，持有者可以提交本地工作目录。目录和可选网页项目放在每次 API 请求中，不在 TOML 中登记项目清单：

```json
{
  "model": "web/gpt-6",
  "messages": [
    {"role": "user", "content": "读取 README，说明项目用途"}
  ],
  "x_gptweb2api": {
    "workdir": "/absolute/path/to/project",
    "chatgpt_project_id": ""
  }
}
```

workdir 必须是现有绝对目录，接收任务时规范化并绑定本轮凭据，不能由模型另换目录。未提供时只支持纯聊天，不使用进程当前目录操作文件。命令沿用固定策略，首版默认不开放任意命令；内部冒烟区按固定路径创建。

web/* 的 chatgpt_project_id 为空时使用普通聊天，非空时发送前校验项目归属；session_id 用于选择既有网页会话。续聊不能静默改变目录或网页项目，需要改变时新建会话。

dots 每次读取调用方提供的完整 messages 和本轮 workdir，不提供 ChatGPT 项目选择或 Dot 原生会话切换。session_id 和 chatgpt_project_id 不参与 Dot 路由，由客户端自行避免提出不支持的要求。网关不检测或推断客户端是否持久化会话，不新增探测字段或相应限制，也不清空 Dot 自身记忆。

## 6. 固定运行约定

| 项目 | 固定行为 |
| --- | --- |
| API 与 TLS | Nginx 提供 API HTTPS；Go 只监听回环 HTTP |
| MCP 接入 | SDK 与 MCP 内存传输，主动出站连接 OpenAI Tunnel |
| SDK 生命周期 | 与网关统一启动、就绪、排空、停止，不开启独立管理监听 |
| 日志 | info、可读文本、按大小轮转，不使用 JSON Lines，不压缩 |
| 日志保留 | max_backups 不含当前文件；单条记录有上限 |
| SDK 日志 | 经适配器并入同一文本滚动日志，不另写无限增长日志 |
| SQLite | 固定路径、WAL、5 秒 busy timeout |
| 记录清理 | 每小时清理超过 7 天的终态数据，保留活动任务和有效订阅 |
| 停止 | 最多 60 秒排空，到期明确取消/记录未完成状态，随后停止 SDK |
| MCP 单调用 | 本地 90 秒；长命令快速返回执行标识后分次查询 |
| 命令期限 | 默认最多 2 分钟，不占住一次 Tunnel 调用等待到底 |
| Ego 与 JS | PATH 中 ego-browser，根目录 ego_scripts/，本轮固定脚本版本 |
| Space | 每个活动槽独占一个，队列等待不占用槽 |
| Dot 互斥 | 同一运行时 Dot 身份最多一个活动任务，是本项目首版策略 |
| 页面检查 | 提交前必须校验，空闲时每 30 分钟轻量检查 |
| 冒烟 | 每 6 小时在空闲槽做小型端到端测试，不额外扩容 |
| 补交与去重 | 最多提醒一次；成功结果 5 分钟内允许相同提交再次确认 |

tasks.timeout 从接收请求开始，包含排队、执行、审批和补交等待；连接断开后任务继续到完成或总期限，可凭任务 ID 查询。取消通过显式请求。达到并发上限排队，队列满返回限流错误。

研究时官方 Tunnel 单次请求预算为 120 秒且包含托管排队，进度通知不延长预算；完整序列化消息默认上限为 10 MiB。30 分钟业务任务必须由多次有界调用完成，不能用一条 MCP 请求覆盖全程。上线前重新核对平台限制。

## 7. dots 事件验收

事件发现/订阅和任务工具经 Tunnel 接入，gateway 主动出站向 OpenAI 提供的 callback URL 发送验证与签名事件。我们不托管公网 callback，也不需要额外事件进程；订阅、签名信息和 outbox 保存在本地 SQLite。

这条链路是待验证设计，不以普通工具可用代替事件验收。所选 SDK、本地协议层和目标账号必须共同通过 MCP 2.0 发现、订阅、callback 验证、签名投递、Dot 实际唤醒、原子领取、工具执行与完成回传。通过前保持 dots.enabled=false，不宣告该模型已就绪。

## 8. 实施验证状态

配置样例与文档可做语法和一致性检查；本文不代表已经注册 Tunnel、取得权限、固定 SDK 依赖、构建程序或跑通账号。真实接入与端到端验收按主计划分阶段执行，失败时报告具体状态，不自动切换模型或传输方式。
