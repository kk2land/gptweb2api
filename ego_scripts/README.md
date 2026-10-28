# Ego JS 脚本目录

本目录固定存放 ChatGPT 网页操作脚本。网关从项目根目录的 `ego_scripts/` 加载 JS，脚本可直接修改并单独部署，调整后无需重新编译 Go。

目录中已包含脚本实现。Native API 契约和脚本预检规则已离线验证，真实 ChatGPT DOM 选择器、模型标签、插件稳定 ID 和项目路由尚待目标账号实测；证据不足时脚本明确失败。

## 加载位置

目录与 Ego 调用方式采用固定约定，不提供 TOML 配置。路径相对于项目根目录；所需脚本缺失时停止相关能力，不能静默切换实现。

Space 名称由可选 `[ego].space_name` 提供，通过结构化参数输入脚本；不改变脚本目录或 CLI。非空时首槽使用指定名称，后续槽追加 `-1`、`-2`；唯一同名 Space 可复用，普通用户 Space 首次选用时按 ID 取得代理控制且不改名。空值沿用 `gptweb2api-slot-0`、`-1` 的默认命名。绑定后以数字 ID 为准，重名、Profile 不匹配或用户接管时停止。

## 适用范围

会话、项目和页面操作脚本用于 `web/gpt-6` 与 `web/gpt-5.6-sol`。`dots` 采用 MCP 事件领取独立任务，不通过这些脚本选择项目或切换会话。全局执行槽仍按约定独占 Ego Space，但这种资源绑定不代表 Dot 原生会话隔离。

## 脚本

| 文件 | 职责 |
| --- | --- |
| `common.js` | Native EgoBindings、Space 名称解析/ID 绑定、CDP、结构化参数、页面观察与严格预检 |
| `dot-space.js` | 只预留/验证 Space，不操作 Dot 的页面、项目或原生会话 |
| `prepare-page.js` | 在已绑定 Space 中创建/验证标签页，先持久化身份再导航 |
| `probe.js` | 检查浏览器、登录态、输入框与必要控件 |
| `create-conversation.js` | 创建并记录网页会话 |
| `select-project.js` | 选择并校验 ChatGPT 项目归属 |
| `select-model.js` | 选择并验证请求对应的网页模型 |
| `attach-plugin.js` | 挂载并验证 MCP 插件 |
| `submit-task.js` | 提交提示词并确认接收 |
| `inspect-turn.js` | 检查生成、工具活动、审批、完成与异常状态 |
| `recover-completion.js` | 在原会话中请求补交结果 |
| `cancel-turn.js` | 停止网页生成并确认状态 |

## 加载与调整规则

- 每个执行尝试开始时加载并检查完整脚本集合，固定本轮使用的版本；后续新任务读取更新后的脚本。
- 活动任务不在中途切换脚本，避免一次任务使用混合版本。
- 提交任务前始终校验页面状态，定期轻量检查与完整冒烟按程序内置策略执行。
- 用户提示词及参数以结构化数据输入，不拼接成可执行 JS。
- 内容指纹和必要的脱敏诊断写入运行记录，便于定位页面变化与脚本版本。

完整配置设计见 [agents/configuration.md](../agents/configuration.md)。
