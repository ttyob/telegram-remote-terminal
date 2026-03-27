# terminal-bridge

Use Go to spawn shell sessions and control them through Telegram messages.
Terminal output is streamed back to Telegram in near real-time, and a WebSocket endpoint is also exposed for multi-channel integration (Feishu, custom dashboards, etc.).

## Features

- One shell session per Telegram chat (`chat_id`)
- Only accepts Telegram private chat messages (group/channel disabled)
- Send one command, receive one consolidated reply (auto-split if too long)
- `/new` or `/reset` to restart current shell session
- Interactive helpers: `/send`, `/enter`, `/ctrlc`, `/ctrld`
- History/completion helpers: `/tab`, `/up`, `/down`
- Stream mode for interactive CLIs: `/attach` and `/detach`
- Web terminal short-link via `/open` (recommended for codex/opencode)
- Idle session auto-recycle
- Command security rules (allow prefixes + deny substrings)
- JSONL audit log for command attempts and execution results
- Telegram output rendered with safe HTML `<pre>` mode (no Markdown escaping issues)
- Readability tags in Telegram output: `[OUT]`, `[ERR]`, `[SESSION]`
- Internal WebSocket stream (`/ws`) for live output and command injection

## Quick start

1. Copy env template:

```bash
cp .env.example .env
```

2. Set required env vars:

- `TELEGRAM_ENABLED` (`true` by default; set `false` for local-only debugging)
- `TELEGRAM_BOT_TOKEN` (required when `TELEGRAM_ENABLED=true`)
- `TELEGRAM_PROXY_URL` (optional, for mainland network environments)
- `ALLOWED_TELEGRAM_USERS` / `ALLOWED_TELEGRAM_CHATS` (optional but strongly recommended)
- `COMMAND_ALLOW_PREFIXES` / `COMMAND_DENY_SUBSTRINGS` for policy control

3. Run:

```bash
go run ./cmd/bridge
```

## Docker deployment

1. Prepare env:

```bash
cp .env.example .env
```

2. Edit `.env` as needed (for local-only test you can set `TELEGRAM_ENABLED=false`).

3. Build and start:

```bash
docker compose up -d --build
```

4. Check service:

```bash
curl --noproxy '*' http://127.0.0.1:8083/healthz
```

Notes:

- Commands run inside the container shell environment.
- `./logs` is mounted to `/app/logs` for audit log persistence.
- Current project directory is mounted to `/workspace`; default `TERMINAL_WORKDIR` is `/workspace` in `docker-compose.yml`.

### Local-only debug mode (without Telegram)

If you want to debug `/ws` and `/term` locally before connecting Telegram:

```text
TELEGRAM_ENABLED=false
```

Then start the service normally:

```bash
go run ./cmd/bridge
```

To test the web terminal page without Telegram `/open`, enable debug issuer endpoint:

```text
WEB_TERM_DEBUG_ENABLED=true
```

Then request a temporary page link locally:

```bash
curl --noproxy '*' "http://127.0.0.1:8083/term/debug/open?chat_id=1"
```

The response returns JSON with a single-use `url`. Open that URL in browser to test `/term` page behavior.

## Telegram usage

- Send `/start` to see help.
- Use private chat with the bot (group messages are rejected).
- Send command directly, for example:

```text
pwd
ls -la
top
```

- Send `/new` to reset your shell session.
- Send `/open` to get a temporary browser terminal link.
- For interactive programs/prompts:

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

## Web terminal

- Open `/open` in Telegram to receive a temporary link
- The `/open` page link can be reopened within token TTL to reconnect the same terminal session
- Browser page: `/term?token=<token>`
- Terminal WebSocket: `/term/ws?token=<token>`
- Link base URL is configured by `WEB_PUBLIC_BASE_URL`
- Token lifetime is configured by `WEB_TERM_TOKEN_TTL`
- Browser terminal supports auto reconnect and shows connection status bar
- Browser terminal syncs terminal resize to PTY for better interactive CLI compatibility
- Browser terminal is optimized for mobile viewport and touch operation
- Web command input auto-prefixes `codex`/`opencode` with low-color `TERM=dumb` env
- Shell session remains alive after page close until `SESSION_IDLE_TIMEOUT`
- Refresh replay history window is configurable by `EVENT_HISTORY_TTL` (default `24h`)

## WebSocket API

Endpoint:

```text
ws://127.0.0.1:8080/ws?chat_id=<chat_id>&token=<WS_TOKEN>
```

Incoming message format:

```json
{"chat_id": 123456789, "command": "ls -la"}
```

Outgoing message format:

```json
{"chat_id": 123456789, "data": "...", "type": "output", "timestamp": "2026-03-27T12:00:00Z"}
```

## Security notes

- Restrict Telegram users via `ALLOWED_TELEGRAM_USERS`
- Restrict Telegram chats via `ALLOWED_TELEGRAM_CHATS`
- Use `COMMAND_ALLOW_PREFIXES` and `COMMAND_DENY_SUBSTRINGS` to reduce dangerous commands
- Review `AUDIT_LOG_PATH` JSONL records regularly
- Run this service in a sandbox/container with limited filesystem and network access
- Use non-root user
- Set `WS_TOKEN` when exposing `/ws`

## Telegram proxy

- Supports `http://`, `https://`, `socks5://` proxy URL
- `TELEGRAM_API_ENDPOINT` must keep `bot%s/%s` placeholders
- Set `TELEGRAM_REQUEST_TIMEOUT` greater than `TELEGRAM_LONG_POLL_TIMEOUT`
- Example:

```text
TELEGRAM_PROXY_URL=http://127.0.0.1:7890
```

## Next steps

- Add Feishu adapter under a unified channel interface
- Add audit log persistence (who, when, command, exit status)
- Add command allowlist/denylist policies
