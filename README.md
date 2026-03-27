# terminal-bridge

一个基于 Go 的远程终端桥接服务，提供：

- Web 控制台（首页 `/`）
- WebTerm 临时 token 机制（`/term/api/ws-token` -> `/term/ws`）
- 多会话隔离（按 `chat_id`）
- 审计日志（JSONL）

## 功能概览

- 多 SSH 服务管理与多窗口切换
- 移动端适配：服务面板/窗口面板抽屉化，主终端区域优先
- 会话空闲自动回收（默认 30m）
- 输出历史回放窗口（默认 24h）
- 控制台密码登录（Cookie 会话）
- 仪表盘状态持久化（SQLite，可选加密）
- 命令白名单前缀 + 黑名单子串策略（可用于命令执行路径约束）

## 快速开始

1) 复制配置

```bash
cp .env.example .env
```

2) 最少建议配置

- `LISTEN_ADDR`
- `CONSOLE_AUTH_PASSWORD_HASH`
- `COMMAND_ALLOW_PREFIXES` / `COMMAND_DENY_SUBSTRINGS`

3) 启动

```bash
go run ./cmd/bridge
```

4) 访问

- 控制台：`http://127.0.0.1:8083/`
- 健康检查：`http://127.0.0.1:8083/healthz`

## Docker

```bash
cp .env.example .env
docker compose up -d --build
```

构建网络受限时可配置：

```text
DOCKER_GOPROXY=https://goproxy.cn,direct
DOCKER_GOSUMDB=sum.golang.google.cn
```

## 主要接口

- `GET /`：Web 控制台页面
- `GET /healthz`：健康检查
- `GET|POST /term/api/ws-token?chat_id=<id>`：申请 WebTerm WS token（需已登录控制台）
- `GET /term/ws?token=<ws_token>`：WebTerm 终端连接
- `GET|POST /term/api/state`：仪表盘状态读写（需已登录控制台）
- `GET|POST /term/api/auth/*`：控制台登录会话相关接口

## 配置说明

见 `.env.example`，重点如下：

- 终端行为：`SHELL_PATH`、`TERMINAL_WORKDIR`
- 服务监听：`LISTEN_ADDR`
- 会话与历史：`SESSION_IDLE_TIMEOUT`、`EVENT_HISTORY_TTL`
- 安全策略：`COMMAND_ALLOW_PREFIXES`、`COMMAND_DENY_SUBSTRINGS`
- 审计日志：`AUDIT_LOG_PATH`
- 控制台登录：`CONSOLE_AUTH_PASSWORD_HASH`、`CONSOLE_AUTH_SESSION_TTL`
- 状态存储：`DASHBOARD_DB_PATH`、`DASHBOARD_ENCRYPTION_KEY`

## 安全建议（重要）

- 强烈建议设置 `CONSOLE_AUTH_PASSWORD_HASH`，不要关闭控制台登录
- 强烈建议通过 Nginx/防火墙限制来源 IP，不要直接公网裸露
- 生产环境务必启用 HTTPS 反代
- 若保存 SSH 密码，建议同时设置 `DASHBOARD_ENCRYPTION_KEY`
- 配置 `COMMAND_ALLOW_PREFIXES` 时尽量最小化允许范围
- 定期检查 `AUDIT_LOG_PATH` 日志

## 安全自检结果（当前版本）

以下是已识别的风险点（按优先级）：

1. WebSocket `CheckOrigin` 为全放行，跨站环境需结合反向代理策略
2. 交互输入路径（WebTerm 输入）不走前缀白名单校验
3. 未配置 `DASHBOARD_ENCRYPTION_KEY` 时，状态数据明文存储在 SQLite

建议：

- 在反代层限制 Origin、IP、BasicAuth 或统一 SSO
- 对外网部署务必启用 TLS + 访问控制

## 审计日志

默认路径：`logs/audit.log`

格式为 JSONL，每行一条，包含：

- 时间戳
- chat_id
- 来源（webterm/internal）
- 命令（交互输入为预览摘要）
- 是否允许
- 错误信息（如有）
