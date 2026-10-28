# Go 项目目录与精简 TOML 配置

- 日期：2026-10-28（Asia/Shanghai）
- 状态：设计与样例，程序和启动脚本尚未实现。
- 主方案：[技术方案与实施计划](implementation-plan.md)

配置只保留部署差异与业务参数。固定路径、程序内部期限、浏览器控制细节采用代码约定，不增加 advanced 配置、隐藏环境变量或另一套配置文件。

## 1. 两个 Go 进程与 Nginx

| 进程 | 部署位置 | 职责 | 配置 |
| --- | --- | --- | --- |
| mcp-relay | 公网服务器 | MCP 入口、插件认证、设备路由、WSS 与事件转发 | config/mcp-relay.toml |
| mcp-gateway | 本地电脑 | API、任务管理、Ego 调度、本地工具、SQLite 持久化 | config/mcp-gateway.toml |

两个 Go 服务前面统一使用 Nginx。Nginx 负责 HTTPS、证书与反向代理；Go 服务只监听本机 HTTP，不提供原生 TLS 配置。公网连接到 Nginx 使用 HTTPS/WSS，Nginx 转给本机服务。

Go 只监听回环地址，不用客户端可伪造的转发头做认证、设备路由或权限判断；客户端访问记录由 Nginx 处理，不提供 trusted_proxies 参数。relay 的公网资源地址取配置中的 public_base_url，不从请求 Host 或转发头推断。

网关继续校验 API key，relay 继续校验插件 OAuth 和设备 token。使用 Nginx 不替代这些业务认证。部署时让 Nginx 支持 MCP/SSE 与 WebSocket 长连接，并与任务总期限匹配；具体 Nginx 文件在部署阶段提供。

## 2. 目录约定

| 路径 | 用途 |
| --- | --- |
| go.mod、go.sum | Go 模块元数据 |
| src/cmd/mcp-relay/、src/cmd/mcp-gateway/ | 两个入口 |
| src/internal/ | Go 实现 |
| ego_scripts/ | 独立 JS 脚本，修改无需重编译 Go |
| bin/ | 两个编译后的二进制 |
| config/ | 两份 TOML 与样例 |
| runtime/logs/ | 分进程文本日志 |
| runtime/mcp-relay.pid、runtime/mcp-gateway.pid | 分进程 PID |
| runtime/data/mcp-relay.sqlite、runtime/data/mcp-gateway.sqlite | 各自本机 SQLite |
| runtime/smoke/ | 专用冒烟测试区 |
| sbin/mcp-relay.sh、sbin/mcp-gateway.sh | start / stop / restart / status |
| build.sh | 根目录构建入口，输出到 bin |
| agents/ | 设计文档 |

相对路径固定相对于项目根目录，不受当前工作目录影响。启动脚本定位根目录，检查配置、进程身份及就绪状态，避免根据陈旧 PID 杀错进程。日志、PID、数据库路径不进入配置。

## 3. 保留的配置

### 公网代理

完整样例：[mcp-relay.example.toml](../config/mcp-relay.example.toml)。

| 配置节 | 保留字段 | 原因 |
| --- | --- | --- |
| server | listen、public_base_url | 部署端口及公网域名 |
| auth | oauth_issuer | ChatGPT 插件认证使用的授权服务 |
| devices | id、token、allowed_subjects | 连接本地设备及授权访问者 |
| log | max_size_mb、max_backups | 控制日志磁盘占用 |

每个设备的 MCP 地址固定为 public_base_url + /mcp/{device_id}，网关通道固定为 /agent。OAuth 元数据与资源地址按此推导，授权服务需要满足实际 ChatGPT 接入要求，单填 issuer 不代表完成认证接入。

事件协议是代理内置能力，是否为某个设备开放由该网关报告的 dots 状态决定，不再添加第二个 events 开关。未开启或未准备好的设备不能订阅执行 Dot 任务。

### 本地网关

完整样例：[mcp-gateway.example.toml](../config/mcp-gateway.example.toml)。

| 配置节 | 保留字段 | 原因 |
| --- | --- | --- |
| server | listen、api_key | Nginx 上游地址及调用认证 |
| relay | url、device_id、token | 公网中继连接 |
| tasks | max_concurrency、queue_capacity、timeout | 并发资源、积压和任务总期限 |
| dots | enabled、dot_id | 是否开放 dots 及使用哪个 Dot |
| workspaces | id、root、chatgpt_project_id、read_only、allow_commands | 本地授权目录、网页项目和操作权限 |
| log | max_size_mb、max_backups | 日志大小与数量 |

首版按个人自用场景使用一个 API key，它有权访问本地配置登记的项目及已启用模型。更细的多调用方权限不提前引入。真实 key 和 token 由部署配置填写，样例仅为占位符，实际配置不提交到仓库；缺失或仍是占位值时拒绝启动。

max_concurrency 决定活动执行槽及受管 Space 数量，每个活动槽独占一个 Space，同一会话串行。dots 同样占用全局额度；同一个 Dot 固定最多一个活动任务，不再单独配置 Dot 并发。

timeout 是唯一的业务任务期限，从接收请求开始计时，包含排队、执行、审批与补交结果。连接断开后任务固定继续执行到完成或期限结束，可凭任务 ID 查询；取消必须显式请求，不提供断开策略开关。排队满则返回限流错误，不再另配排队超时和同步等待期限。

workspace.root 必须是已经存在的已授权目录，启动时规范化并检查；read_only 时不得同时允许命令。内部冒烟区由程序按固定路径创建，不额外登记为可供 API 使用的 workspace。chatgpt_project_id 为空使用普通聊天，非空则发送前校验项目归属。

## 4. 固定约定

| 项目 | 固定行为 |
| --- | --- |
| TLS 与证书 | 由 Nginx 管理 |
| 日志 | info 级别，可读文本，不使用 JSON Lines，不压缩历史文件 |
| 日志位置 | runtime/logs/ 下按进程分文件，不另写无限增长的 stdout.log |
| PID 与退出 | 固定 PID 路径；收到停止请求先排空，relay 最多 30 秒、gateway 最多 60 秒 |
| SQLite | 固定各自路径、WAL、5 秒 busy timeout |
| 数据清理 | 每小时清理超过 7 天的终态数据，跳过活动任务和有效状态 |
| MCP 单调用 | 本地最多 80 秒，公网外层最多 90 秒 |
| 长命令 | 异步启动并查询，命令本身默认最多 2 分钟 |
| 连接维护 | 内置心跳、有限退避重连、调用大小上限及错误处理 |
| Ego 调用 | PATH 中的 ego-browser，使用已确认的浏览器身份，缺失时停止相关能力 |
| JS 来源 | 根目录 ego_scripts/，每个执行尝试固定脚本版本，新任务读取更新 |
| Space 命名与插件 | 程序生成唯一 Space 标识；插件名固定 gptweb2api |
| 网页模型 | 固定 web/gpt-6 与 web/gpt-5.6-sol，仍需核对实际账号可用性 |
| Dot 模型 | dots.enabled 开启并准备完成后才展示 dots，不回退到网页模型 |
| 页面检查 | 提交前必须校验；每 30 分钟在空闲槽轻量检查 |
| 冒烟测试 | 每 6 小时在空闲槽运行小型端到端测试，使用固定专用目录，不额外扩容 |
| 结果补交 | 最多提醒一次，不重发原任务；完成后 5 分钟内允许相同结果重试确认 |

冒烟间隔是实现内的初始约定，会消耗消息额度；首次真实验证若证明不合适，修改代码中的默认策略，而不是增加一组配置项。普通提交检查失败时暂停执行，不为保持可用而跳过检查。

日志写入前按大小轮转，限制单条记录大小；max_backups=10 表示当前文件之外最多保留 10 份历史。密钥、提示词和文件全文不进入普通日志。两个进程分别计算占用。

## 5. 加载与校验

只读取两份 TOML，不增加配置版本字段、通用环境变量插值、覆盖层或热重载。缺省使用少量内置默认值；未知字段、重复 ID、无效时长、非正的并发/日志限制和错误引用明确报错，变更经排空与重启生效。

两端的设备 ID 与 token 必须相同。gateway 的 relay URL 指向 WSS，relay 的 public_base_url 使用 HTTPS；Go 监听地址必须为回环地址。已启用的 dots 要求有效 Dot ID 和完整后端准备，未开启时无需填写 dot_id。

两个 TOML 的语法及设备引用已检查。实际配置加载、OAuth、Nginx、Ego、进程控制和运行验证仍待实现。
