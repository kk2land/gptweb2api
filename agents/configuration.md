# Go 项目目录与 TOML 配置设计

- 日期：2026-10-28（Asia/Shanghai）
- 状态：配置契约设计；样例通过 TOML 语法检查，不代表程序、启动脚本或配置加载器已经实现。
- 主方案：[技术方案与实施计划](implementation-plan.md)

## 1. 两个 Go 进程

| 进程 | 部署位置 | 职责 | 生效配置 |
| --- | --- | --- | --- |
| `mcp-relay` | 公网服务器 | HTTPS MCP 入口、插件认证、设备路由、与本地网关的 WSS 通道、可选事件投递 | `config/mcp-relay.toml` |
| `mcp-gateway` | 本地电脑 | OpenAI API、任务队列、Ego Space 调度、本地 MCP 工具执行、结果持久化与回传 | `config/mcp-gateway.toml` |

网关中的 API、任务管理和 MCP 执行器共享一个进程与一份本地 SQLite。浏览器及其 CLI 是依赖进程，不再拆出第三个项目服务。

公网与本地可以分别部署，只启动本机所需进程。使用同一份源码构建两个二进制，并不要求在两台机器共享 runtime 或数据库。

## 2. 目录约定

| 路径 | 用途 |
| --- | --- |
| `go.mod`、`go.sum` | Go 模块与依赖元数据，放项目根目录 |
| `src/cmd/mcp-relay/` | 公网代理入口 |
| `src/cmd/mcp-gateway/` | 本地网关入口 |
| `src/internal/` | 两个入口使用的 Go 实现 |
| `src/internal/browser/scripts/` | 预编写 Ego JS 脚本；默认通过 Go embed 编入网关 |
| `bin/mcp-relay`、`bin/mcp-gateway` | 编译后的二进制 |
| `config/` | TOML 配置与不含真实密钥的样例 |
| `runtime/logs/` | 按大小滚动的可读文本日志 |
| `runtime/mcp-relay.pid`、`runtime/mcp-gateway.pid` | 两个进程各自的 PID 文件 |
| `runtime/data/` | 两个进程各自的 SQLite 及其 WAL / SHM 文件 |
| `runtime/smoke/` | 冒烟测试专用文件区 |
| `sbin/mcp-relay.sh`、`sbin/mcp-gateway.sh` | 各自支持 start / stop / restart / status |
| `build.sh` | 根目录构建入口，输出到 bin |
| `agents/` | 设计、配置契约与实施说明 |

当前提交只增加配置样例和说明；表中的源码、二进制、运行目录与脚本是后续实现约定。运行时生成 PID、日志、数据库和测试文件，不以占位文件充当实现。

所有相对路径统一相对于项目根目录，不能取决于执行命令时的工作目录。默认从 `bin/` 中真实可执行文件的位置确定根目录，允许显式 root 参数覆盖。配置文件路径也按此规则处理，解析后规范化为绝对路径。

`sbin` 脚本负责定位根目录、传入配置、检查启动状态和管理 PID；TOML 必须交给 Go 配置解析器读取，不使用 grep/sed 猜测配置字段。进程首次就绪后才能报告启动成功。停止时先请求优雅退出，超过配置期限后明确报告或处理残留进程，不根据陈旧 PID 文件盲目杀死同号进程。

`build.sh` 负责构建两个 Go 入口，JS 默认编入网关；部署不要求携带整个 src。SQLite 驱动和交叉编译限制在选型后明确，不提前承诺所有平台静态编译可用。

## 3. 配置通用规则

1. 使用 `config_version = 1`，版本不支持时启动失败。
2. 样例为 [mcp-relay.example.toml](../config/mcp-relay.example.toml) 与 [mcp-gateway.example.toml](../config/mcp-gateway.example.toml)。部署时复制为不含 `.example` 的同名配置。
3. 时长统一使用 Go 风格字符串，如 `"10s"`、`"5m"`、`"6h"`；不接受 `"1d"`。保留天数使用显式 `_days` 整数。
4. `*_bytes` 按字节，`*_mb` 按 MiB（1024 × 1024 字节）；正整数有明确上限。`max_backups` 必须大于 0，不用 0 隐含“无限保留”。
5. `*_env` 的值是环境变量名，程序读取该变量的值作为密钥。不是 `${...}` 插值，也不执行 shell。对应功能禁用时不要求其专用密钥；启用后缺失则拒绝启动或注册能力。
6. 配置不保存真实 token 样例；加载错误、日志和诊断输出均不得打印密钥内容。
7. 未知字段、重复 ID、无效模型、负数限制、相互矛盾的开关均报错，防止拼写错误被静默忽略。
8. 首版配置通过检查后启动，变更经排空与重启生效；不承诺热重载。不在运行中静默改变已有任务的权限、后端或目录。
9. 活动任务、会话归属、Ego 实际 Space ID、订阅和执行租约属于运行状态，写 SQLite，不回写 TOML。

## 4. 两个进程共有的配置

| 配置节 | 字段 | 规则与建议值 |
| --- | --- | --- |
| `[process]` | `pid_file`、`startup_timeout`、`shutdown_timeout` | 分进程 PID；启动等待 20–30 秒，退出排空 30–60 秒 |
| `[log]` | `level`、`format`、`file`、`console`、`timezone` | info、text、独立日志；守护运行默认不输出到控制台 |
| `[log]` | `max_size_mb`、`max_backups`、`compress` | 20 MiB、10 个历史文件、压缩历史文件 |
| `[storage]` | `sqlite_file`、`busy_timeout` | 独立本地数据库，busy timeout 5 秒，程序固定启用 WAL |
| `[storage]` | `terminal_retention_days`、`cleanup_interval` | 终态数据默认保留 7 天，每小时清理；不删除活动任务、有效订阅和当前绑定 |

### 4.1 文本日志与大小滚动

日志采用可读文本，示意如下：

```text
2026-10-28T20:00:00+08:00 INFO task.started request_id=req_demo model=web/gpt-6 worker=worker-01
2026-10-28T20:00:18+08:00 INFO task.completed request_id=req_demo elapsed=18s
```

不使用一行一个 JSON 的日志。固定公共字段包括时间、级别、模块、事件及必要关联标识；异常可以附带可读堆栈。默认不写完整提示词、文件内容、最终答案、认证头或任务凭据，命令输出按上限截断。

写入会超过文件大小上限前进行轮转，并限制单条日志大小，避免一条大记录突破文件限制。历史日志按时间或序号命名，压缩后仍计入保留数量。`max_backups = 10` 表示当前日志之外最多保留 10 个历史文件，删除最旧历史，不存在隐含的无限保留模式。

默认单进程最多约 20 MiB × 11 份日志；压缩后的实际磁盘占用通常更小，轮转过程可能有短暂额外文件。两个进程分别计算，不共享日志文件。

`sbin` 不再额外无上限重定向出一份 stdout.log；正常日志由 Go 进程自行滚动管理。启动早期错误应传给启动者，进入后台后记录到进程日志。

### 4.2 SQLite 内容与边界

- relay 数据库：必要的 MCP 会话与路由记录、RPC 去重和终态记录，以及启用事件后的订阅、回调信息和事件 outbox。
- gateway 数据库：任务、执行尝试、结果、工具状态、会话和 Space 绑定、幂等记录。
- gateway 是任务及结果的权威来源；relay 的事件只携带路由和任务标识，不创建第二份可独立执行的任务队列。
- 数据库放在各进程本机磁盘，不能让两进程或多台机器共用同一个 SQLite 文件。
- PID、日志、配置、数据库不能互相覆盖；启动时检查冲突路径。
- 保留期清理只操作已过期的终态数据，不能用数据库文件轮转替代数据清理。

## 5. 公网 mcp-relay 配置

完整样例见 [config/mcp-relay.example.toml](../config/mcp-relay.example.toml)。

| 配置节 | 内容 |
| --- | --- |
| `[server]` | 监听地址、公网 HTTPS 根 URL、可信反向代理、HTTP 头超时与请求大小 |
| `[server.tls]` | 是否由 Go 直接终结 TLS、证书和私钥路径 |
| `[mcp]` | 设备 MCP 路径前缀、单次调用超时、空闲会话期限 |
| `[mcp.auth]` | OAuth issuer、scope 和 JWKS 缓存 |
| `[tunnel]` | 网关 WSS 路径、握手期限、心跳、设备未完成 RPC 数量上限 |
| `[[devices]]` | 设备 ID、设备 token 环境变量、可访问该设备的 OAuth subject |
| `[events]` | 事件功能开关、存储加密密钥、投递期限及退避重试 |

### 5.1 HTTPS 与设备入口

推荐公网 Nginx/Caddy 提供 HTTPS，relay 监听 `127.0.0.1:8787`。也可启用 Go 原生 TLS，但证书与私钥必须完整；公网外部地址始终使用 HTTPS。

示例设备 `local-01` 的插件地址为 `https://mcp.example.com/mcp/local-01`，本地网关连接 `wss://mcp.example.com/agent`。设备 ID 来自已配置列表，不允许通过工具参数任意切换设备。

只有来自 `trusted_proxies` 的转发头可被信任。MCP 流与 WSS 需要反向代理支持长连接；不设置覆盖所有请求的短全局 HTTP 写超时，而由具体 MCP 调用、握手和流空闲控制期限。

### 5.2 两条不同的认证链路

ChatGPT → relay：OAuth 校验主体和 scope，并检查该主体属于目标设备的 `allowed_subjects`。首版设计采用支持 issuer 发现与 JWT 验证的外部 OAuth 授权服务，relay 负责资源服务和所需元数据，不在这个配置设计中承诺已实现完整账号系统。

MCP protected resource URL 与 token audience 由 `public_base_url`、`path_prefix` 和设备 ID 推导，必须与授权服务约定匹配，不把客户端请求的 Host 当作任意可信资源地址。授权服务还需满足实际 ChatGPT 客户端的连接要求；只填一个 issuer URL 并不能证明完整接入成功。

gateway → relay：使用设备 token，双方的 `device_id` 和 token 值必须匹配。它不替代插件 OAuth，也不能作为调用方 API key。设备 token 不进入网页提示词。

### 5.3 事件可选能力

`events.enabled=false` 时不宣告事件能力，相关加密密钥不必存在。启用后使用 `encryption_key_env` 指定的独立随机密钥保护需要保存的回调签名信息；具体编码和加密格式在实现时固定。

投递采用有界指数退避与抖动，达到 `max_attempts` 后标记未送达。事件成功不等于任务完成，也不允许重试事件时重新执行原任务。订阅有效期与续期按实际 MCP Events 契约处理，不另行假设永久订阅。

## 6. 本地 mcp-gateway 配置

完整样例见 [config/mcp-gateway.example.toml](../config/mcp-gateway.example.toml)。

| 配置节 | 内容 |
| --- | --- |
| `[api]` | 回环监听、请求大小、API 等待期限和 SSE 保活 |
| `[[api.keys]]` | 调用方密钥环境变量、允许使用的模型和 workspace |
| `[relay]` | 公网 WSS、设备身份、连接与重连参数、可选出站代理 |
| `[mcp]` | 插件身份、单次工具调用期限、结果大小上限 |
| `[scheduler]` | 全局并发、等待队列长度、排队期限 |
| `[tasks]` | 实际执行期限、断开策略、完成重试确认窗口、补交提醒次数 |
| `[ego]` | CLI、Profile、Space 前缀、脚本来源、页面操作与清理期限 |
| `[ego.healthcheck]` | 空闲页面检查、可选端到端冒烟及测试模型/目录 |
| `[[models]]` | 网页模型列表与已验证目标映射 |
| `[dots]` | dots 总开关、Dot 身份、单 Dot 并发、领取期限和租约 |
| `[executor]` | 文件、命令输出大小及命令执行时间上限 |
| `[[workspaces]]` | 本地目录、可选 ChatGPT 项目、只读与命令权限 |

### 6.1 模型开关与权限

`[[models]]` 声明 `web/gpt-6` 和 `web/gpt-5.6-sol` 的网页映射。`target_model` 是本项目内部语义标识，由脚本适配并验证真实网页选项，不宣称是私有网络接口 slug。

`dots` 是保留的后端模型 ID，统一由 `[dots].enabled` 管理，不再重复放入 `[[models]]`，避免出现两个开关互相矛盾。启用还需 Dot ID、插件以及远端事件后端准备完成；只打开布尔开关不代表立刻可用。

`GET /v1/models` 只展示该调用方有权使用、已启用且完成能力准备的模型。禁用的 `dots` 即使在 API key 的允许列表中，也不能展示或调用。未知/关闭模型明确报错；模型临时不可用报可用性错误，不静默替换后端。

请求的 workspace 同样要经过 API key 的允许列表。模型、workspace、设备和本轮凭据的权限取交集，提示词不能扩大范围。

### 6.2 全局并发与 Space 一一对应

`scheduler.max_concurrency = 2` 表示两个活动执行槽，分别绑定两个不同的 Ego Space，例如逻辑 worker-01 和 worker-02。实际 Space ID 通过 Ego 创建/领取后写 SQLite，不将示例名称当成未经验证的原生 API。

队列容量不包含这两个活动槽。全局配额、Space 锁、会话锁和 Dot 身份锁必须统一调度，未获得全部必要资源的请求继续等待，不先占一个槽无限等待其他锁。

同一网页会话无论在哪个 Space 打开都串行执行。`complete_task` 已回传只结束业务任务；原网页确认、工具退出和页面清理完成后才能释放槽。清理失败的槽隔离重建，不能直接交给下一个任务。

Dots 同样计入全局配额并绑定独立 Space，但 `dots.max_concurrency` 是另一个后端上限。单个 Dot 首版固定为 1，因为不同 Space 不代表不同云端 Dot 上下文。即使全局并发为 2，也只能同时运行一个同身份 Dot 任务。

降低并发通过排空和重启生效，不驱逐正在执行的槽；保留会话的 Space 归属在恢复时重新检查。

### 6.3 超时与断开行为

| 配置 | 起点与边界 |
| --- | --- |
| `api.wait_timeout` | API 提交被接受后开始计时，包括排队和运行；样例 10 分钟 |
| `scheduler.queue_wait_timeout` | 最长排队 5 分钟，尚未获得完整执行资源就到期则结束 |
| `tasks.run_timeout` | 获得执行槽后最长 30 分钟，包括必要审批、工具与补交等待 |
| `mcp.call_timeout` | 本地一次 MCP/RPC 处理，样例 80 秒 |
| relay `mcp.call_timeout` | 公网外层调用期限，样例 90 秒，留出返回错误的余量 |
| `executor.command_timeout` | 单个命令真正执行的上限；样例 2 分钟，启动/查询调用本身快速返回 |
| `process.shutdown_timeout` | 停止接收新任务后的最大排空期限；到期不能宣告所有任务成功完成 |

API 等待结束或调用方断开时，`disconnect_policy="continue"` 保留任务并提供查询 ID；`cancel` 则请求取消并拒绝新增操作。取消不回滚已经写入的文件。任务运行期限始终生效，SSE 保活不会延长它。

完成提交先持久化再确认，`completion_retry_window` 只允许相同结果再次确认，不重新授权文件修改；保留期必须覆盖这一窗口。补交提醒次数有界，不能把漏回调变成无限自动重发原任务。

### 6.4 Ego 与定期检查

`ego.executable` 可以是绝对路径或 PATH 中的 CLI。`profile_id` 必须替换为安装环境中实际选定的身份；不能因找不到 Profile 而悄悄换账号。

`scripts_dir=""` 使用编译进二进制的固定 JS，非空表示显式使用外部脚本覆盖。外部脚本必须通过版本与能力检查；用户任务作为结构化输入传入，不拼接成可执行 JS。

每个任务提交前的关键状态校验始终执行，不提供关闭开关。定期 `interval` 是不发送消息的轻量检查，只在空闲槽进行，不改变正在执行的会话。

端到端测试由 `smoke_enabled` 单独开启，按 `smoke_interval` 提交随机 nonce 测试，使用 `smoke_workspace_id` 指定的专用目录，并占用同一个全局 Space 池。没有空闲槽时延后，不创建超出并发限制的额外 Space。此检查会消耗账号消息额度，所以样例默认关闭；开启后仍不得自动修改失败脚本或绕过账号限制。

`session_idle_timeout` 只释放本地无活动任务的会话绑定及页面资源，不删除 ChatGPT 云端历史。回收后旧 session_id 不能静默绑定新上下文，应明确返回过期并由调用方新建会话。

### 6.5 Workspace 与执行权限

`root` 在解析后规范化为绝对路径；`create_if_missing=false` 要求目录已经存在，`true` 才允许按本地配置创建。自动创建不接受 API 或提示词临时指定路径。普通项目与 smoke 测试目录分别登记。

`chatgpt_project_id=""` 表示普通聊天；非空时必须确认会话位于该项目，失败则停止。请求扩展不能选择未获授权的其他 ChatGPT 项目。

`read_only=true` 禁止文件修改与命令执行；因此同时配置 `allow_commands=true` 应拒绝启动。样例允许补丁修改，但默认禁用命令，需要时在对应 workspace 显式开启。

路径访问检查必须覆盖规范化路径与符号链接，不能仅比较输入字符串前缀。文件权限边界由网关执行器实现，不因为 Space 独立即假设获得文件系统隔离。

## 7. 启动前校验清单

这些检查将由待实现的 Go 配置加载器完成；目前只验证了样例语法与设计中的可静态校验关系。

1. 检查 TOML、版本、未知字段、枚举、正数范围、时长和时区。
2. 解析根目录与全部路径，检查所需目录、权限、PID 与数据库冲突。
3. 启用功能的 `*_env` 必须能读取非空值；Dot/事件关闭时跳过其专用密钥要求。
4. 公网 URL 必须 HTTPS，网关通道必须 WSS；Go TLS 开启时要求有效证书文件；不能跳过 TLS 验证。
5. 检查设备 ID、API key、模型 ID 和 workspace ID 唯一，引用存在且组合有效。
6. 检查本地单调用期限小于外层 relay 期限；部署就绪检查通过实际连接核对两端，不能只看本地一份配置。
7. 检查 Ego Profile、CLI、脚本、目标模型和插件；配置正确但后端暂不可达应区分存活与就绪，不公布未验证能力。
8. `dots.enabled` 要求 Dot 身份有效、并发为 1 且 relay 事件能力就绪；smoke 开启要求其模型和 workspace 有效。
9. 终态保留期覆盖完成确认窗口，且清理跳过有效状态；所有日志使用 text 并有大小和数量上限。

实际部署配置、二进制和 runtime 数据应由实现阶段的 gitignore 排除；样例和设计文档进入版本控制。启动脚本、配置检查 CLI、配置解析单元测试及真实运行验证随实现一并提供。
