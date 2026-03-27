# terminal-bridge

使用 Go 启动并管理 Shell 会话，通过 Telegram 消息进行远程控制。
终端输出会近实时回传到 Telegram，同时提供 WebSocket 接口，便于后续接入飞书或自定义前端。

## 功能特性

- 按 Telegram `chat_id` 隔离会话（每个会话独立 Shell）
- 仅允许 Telegram 私聊（群聊/频道消息默认拒绝）
- 普通模式下一条命令对应一次聚合回复（自动分片）
- 会话控制：`/new`、`/reset`
- 交互输入：`/send`、`/enter`、`/ctrlc`、`/ctrld`
- 历史/补全辅助：`/tab`、`/up`、`/down`
- 流式模式：`/attach`、`/detach`
- Web 终端短链：`/open`（适合 `codex` / `opencode`）
- 空闲会话自动回收
- 命令安全策略：前缀白名单 + 子串黑名单
- JSONL 审计日志（命令、来源、用户、结果）
- Telegram 输出采用 HTML `<pre>`，避免 Markdown 转义问题
- 输出标签：`[OUT]`、`[ERR]`、`[SESSION]`
- 内部 WebSocket 通道：`/ws`

## 快速开始

1. 复制环境变量模板：

```bash
cp .env.example .env
```

2. 至少配置以下项：

- `TELEGRAM_ENABLED`（默认 `true`，本地纯调试可设为 `false`）
- `TELEGRAM_BOT_TOKEN`（当 `TELEGRAM_ENABLED=true` 时必填）
- `ALLOWED_TELEGRAM_USERS` / `ALLOWED_TELEGRAM_CHATS`（强烈建议配置）
- `COMMAND_ALLOW_PREFIXES` / `COMMAND_DENY_SUBSTRINGS`（按需收敛权限）

3. 启动：

```bash
go run ./cmd/bridge
```

## Docker 部署

1. 准备配置：

```bash
cp .env.example .env
```

2. 按需修改 `.env`（本地调试可先 `TELEGRAM_ENABLED=false`）。

3. 构建并启动：

```bash
docker compose up -d --build
```

若构建阶段出现 `go mod download` 超时（例如无法访问 `proxy.golang.org`），可在 `.env` 中设置：

```text
DOCKER_GOPROXY=https://goproxy.cn,direct
DOCKER_GOSUMDB=sum.golang.google.cn
```

然后重新构建：

```bash
docker compose build --no-cache
docker compose up -d
```

4. 健康检查：

```bash
curl --noproxy '*' http://127.0.0.1:8083/healthz
```

说明：

- 命令在容器内执行
- `./logs` 挂载到 `/app/logs`，用于持久化审计日志
- 项目目录挂载到 `/workspace`，默认工作目录也是 `/workspace`

## 本地仅调试模式（不接 Telegram）

如果你想先调 `/ws` 和 `/term`：

```text
TELEGRAM_ENABLED=false
```

正常启动后即可本地联调。

若要在不使用 Telegram `/open` 的情况下测试网页终端，可开启调试发链接口：

```text
WEB_TERM_DEBUG_ENABLED=true
```

然后获取临时链接：

```bash
curl --noproxy '*' "http://127.0.0.1:8083/term/debug/open?chat_id=1"
```

返回 JSON 中会包含可访问的 `url`。

## Telegram 使用说明

- 先发 `/start` 查看帮助
- 必须私聊机器人（群聊会被拒绝）
- 直接发送命令即可，例如：

```text
pwd
ls -la
top
```

- ` /new` 重置当前会话
- `/open` 获取网页终端链接
- 交互操作命令：

```text
/send y
/enter
/ctrlc
/ctrld
/tab
/up
/down
/attach
/detach
/open
```

## Web 终端

- 在 Telegram 发送 `/open` 获取临时访问链接
- 页面地址：`/term?token=<token>`
- 终端 WS：`/term/ws?token=<token>`
- `WEB_PUBLIC_BASE_URL` 控制外部访问基地址
- `WEB_TERM_TOKEN_TTL` 控制 token 有效期
- 同一个页面链接在 token 有效期内可重复打开并重连原会话
- 页面关闭后会话不会立刻销毁，直到 `SESSION_IDLE_TIMEOUT`
- 支持断线自动重连、连接状态显示、窗口 resize 同步
- 移动端已适配（含工具面板、输入框、常用快捷键）
- 网页输入框执行 `codex`/`opencode` 时会自动补 `TERM=dumb` + 低色环境变量
- 刷新回放历史窗口由 `EVENT_HISTORY_TTL` 控制（默认 `24h`）

## WebSocket API

连接地址示例：

```text
ws://127.0.0.1:8080/ws?chat_id=<chat_id>&token=<WS_TOKEN>
```

客户端发送：

```json
{"chat_id": 123456789, "command": "ls -la"}
```

服务端返回：

```json
{"chat_id": 123456789, "data": "...", "type": "output", "timestamp": "2026-03-27T12:00:00Z"}
```

## 安全建议

- 用 `ALLOWED_TELEGRAM_USERS` 限制用户
- 用 `ALLOWED_TELEGRAM_CHATS` 限制会话
- 用 `COMMAND_ALLOW_PREFIXES` + `COMMAND_DENY_SUBSTRINGS` 收敛风险命令
- 定期检查 `AUDIT_LOG_PATH` 的 JSONL 审计记录
- 建议在沙箱/容器中运行，并限制文件系统和网络权限
- 避免使用 root 运行
- 对外开放 `/ws` 时务必设置 `WS_TOKEN`

## Telegram 代理说明

- 支持 `http://`、`https://`、`socks5://`
- `TELEGRAM_API_ENDPOINT` 必须保留 `bot%s/%s` 两个占位符
- `TELEGRAM_REQUEST_TIMEOUT` 应大于 `TELEGRAM_LONG_POLL_TIMEOUT`
- 示例：

```text
TELEGRAM_PROXY_URL=http://127.0.0.1:7890
```

## 后续计划

- 接入飞书适配器，统一多渠道接口
- 增强审计存储（更完整的操作者、时间、退出码）
- 继续完善命令策略能力（更细粒度 allow/deny）
