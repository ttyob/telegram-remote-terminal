package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/net/proxy"

	"terminal-bridge/internal/events"
	"terminal-bridge/internal/terminal"
)

type Config struct {
	Token           string
	APIEndpoint     string
	ProxyURL        string
	RequestTimeout  time.Duration
	LongPollTimeout int
	OpenWebTermURL  func(chatID int64) (string, error)
	AllowedUsers    map[int64]struct{}
	AllowedChats    map[int64]struct{}
	MaxChunkSize    int
	Bus             *events.Bus
	Manager         *terminal.Manager
}

type Bot struct {
	api             *tgbotapi.BotAPI
	longPollTimeout int
	allowedUsers    map[int64]struct{}
	allowedChats    map[int64]struct{}
	maxChunkSize    int
	bus             *events.Bus
	manager         *terminal.Manager
	openWebTermURL  func(chatID int64) (string, error)
	chatLocks       sync.Map
	streamRelays    sync.Map
}

type streamRelay struct {
	cancel context.CancelFunc
	ready  chan struct{}
}

func NewBot(cfg Config) (*Bot, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("telegram token is required")
	}
	if cfg.MaxChunkSize <= 0 {
		cfg.MaxChunkSize = 3000
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.LongPollTimeout <= 0 {
		cfg.LongPollTimeout = 60
	}
	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		cfg.APIEndpoint = tgbotapi.APIEndpoint
	}
	if strings.Count(cfg.APIEndpoint, "%s") != 2 {
		return nil, errors.New("TELEGRAM_API_ENDPOINT must contain two %s placeholders, e.g. https://api.telegram.org/bot%s/%s")
	}
	minTimeout := time.Duration(cfg.LongPollTimeout+15) * time.Second
	if cfg.RequestTimeout < minTimeout {
		cfg.RequestTimeout = minTimeout
	}
	if cfg.Bus == nil || cfg.Manager == nil {
		return nil, errors.New("bus and manager are required")
	}

	httpClient, err := newTelegramHTTPClient(cfg.ProxyURL, cfg.RequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid telegram proxy config: %w", err)
	}

	api, err := tgbotapi.NewBotAPIWithClient(cfg.Token, cfg.APIEndpoint, httpClient)
	if err != nil {
		return nil, err
	}

	return &Bot{
		api:             api,
		longPollTimeout: cfg.LongPollTimeout,
		allowedUsers:    cfg.AllowedUsers,
		allowedChats:    cfg.AllowedChats,
		maxChunkSize:    cfg.MaxChunkSize,
		bus:             cfg.Bus,
		manager:         cfg.Manager,
		openWebTermURL:  cfg.OpenWebTermURL,
	}, nil
}

func newTelegramHTTPClient(proxyRaw string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw != "" {
		u, err := url.Parse(proxyRaw)
		if err != nil {
			return nil, err
		}

		scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
		switch scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			var auth *proxy.Auth
			if u.User != nil {
				password, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: password}
			}
			dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
			if err != nil {
				return nil, err
			}
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
			transport.Proxy = nil
		default:
			return nil, fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
		}
	}

	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	b.syncBotCommands()

	updatesCfg := tgbotapi.NewUpdate(0)
	updatesCfg.Timeout = b.longPollTimeout

	updates := b.api.GetUpdatesChan(updatesCfg)
	defer b.api.StopReceivingUpdates()

	for {
		select {
		case <-ctx.Done():
			return nil
		case upd := <-updates:
			if upd.Message == nil || upd.Message.Text == "" {
				continue
			}
			if err := b.handleMessage(ctx, upd.Message); err != nil {
				slog.Error("handle telegram message failed", "error", err)
			}
		}
	}
}

func (b *Bot) syncBotCommands() {
	commands := []tgbotapi.BotCommand{
		{Command: "help", Description: "查看帮助信息"},
		{Command: "open", Description: "打开网页终端"},
		{Command: "attach", Description: "开启流式回传"},
		{Command: "detach", Description: "关闭流式回传"},
		{Command: "new", Description: "重置终端会话"},
		{Command: "send", Description: "发送原始输入"},
		{Command: "enter", Description: "发送回车"},
		{Command: "ctrlc", Description: "发送 Ctrl+C"},
		{Command: "ctrld", Description: "发送 Ctrl+D"},
		{Command: "tab", Description: "发送 Tab"},
		{Command: "up", Description: "上一条历史命令"},
		{Command: "down", Description: "下一条历史命令"},
	}

	cfg := tgbotapi.NewSetMyCommands(commands...)
	if _, err := b.api.Request(cfg); err != nil {
		slog.Warn("set telegram commands failed", "error", err)
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) error {
	chatID := msg.Chat.ID
	if msg.Chat.Type != "private" {
		b.sendText(chatID, "for security reasons, please use private chat with this bot")
		return nil
	}
	if msg.From == nil {
		b.sendText(chatID, "missing sender")
		return nil
	}
	userID := msg.From.ID

	if !b.isAllowed(userID) {
		b.sendText(chatID, "access denied")
		return nil
	}
	if !b.isChatAllowed(chatID) {
		b.sendText(chatID, "chat denied")
		return nil
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return nil
	}

	if strings.HasPrefix(text, "/") {
		return b.handleCommand(ctx, chatID, userID, text)
	}
	text = normalizeInteractiveCLICommand(text)

	if b.isAttached(chatID) {
		if err := b.ensureAttached(chatID); err != nil {
			b.sendText(chatID, fmt.Sprintf("attach state error: %v", err))
			return nil
		}
		if err := b.manager.ExecuteWithMeta(chatID, text, terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
			b.sendText(chatID, fmt.Sprintf("command failed: %v", err))
		}
		return nil
	}

	return b.executeAndReply(chatID, userID, text)
}

func (b *Bot) handleCommand(ctx context.Context, chatID int64, userID int64, text string) error {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil
	}
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/start", "/help":
		b.sendText(chatID, "send shell commands directly.\n/open 打开网页终端(推荐交互工具)\n/attach 开启流式回传\n/detach 关闭流式回传\n/new 重置会话\n/send <text> 发送原始输入(不自动回车)\n/enter 发送回车\n/ctrlc 发送 Ctrl+C\n/ctrld 发送 Ctrl+D\n/tab 发送 Tab\n/up 上一条历史命令\n/down 下一条历史命令")
	case "/open":
		if b.openWebTermURL == nil {
			b.sendText(chatID, "web terminal is not configured")
			return nil
		}
		link, err := b.openWebTermURL(chatID)
		if err != nil {
			b.sendText(chatID, fmt.Sprintf("create link failed: %v", err))
			return nil
		}
		b.sendText(chatID, "open web terminal:\n"+link)
	case "/attach":
		if err := b.attachStream(chatID); err != nil {
			b.sendText(chatID, fmt.Sprintf("attach failed: %v", err))
			return nil
		}
		b.sendText(chatID, "stream mode enabled")
	case "/detach":
		b.detachStream(chatID)
		b.sendText(chatID, "stream mode disabled")
	case "/new", "/reset":
		b.manager.Reset(chatID)
		b.sendText(chatID, "terminal session reset")
	case "/send":
		raw := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
		if raw == "" {
			b.sendText(chatID, "usage: /send <text>")
			return nil
		}
		if b.isAttached(chatID) {
			if err := b.manager.SendInteractiveInput(chatID, raw, false, terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("send failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendInteractiveInput(chatID, raw, false, terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	case "/enter":
		b.sendAction(chatID, "[KEY] ENTER")
		if b.isAttached(chatID) {
			if err := b.manager.SendInteractiveInput(chatID, "", true, terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("enter failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendInteractiveInput(chatID, "", true, terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	case "/ctrlc":
		b.sendAction(chatID, "[KEY] CTRL+C")
		if b.isAttached(chatID) {
			if err := b.manager.SendControl(chatID, "ctrlc", terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("ctrlc failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendControl(chatID, "ctrlc", terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	case "/ctrld":
		b.sendAction(chatID, "[KEY] CTRL+D")
		if b.isAttached(chatID) {
			if err := b.manager.SendControl(chatID, "ctrld", terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("ctrld failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendControl(chatID, "ctrld", terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	case "/tab":
		b.sendAction(chatID, "[KEY] TAB")
		if b.isAttached(chatID) {
			if err := b.manager.SendControl(chatID, "tab", terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("tab failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendControl(chatID, "tab", terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	case "/up":
		b.sendAction(chatID, "[KEY] UP")
		if b.isAttached(chatID) {
			if err := b.manager.SendControl(chatID, "up", terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("up failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendControl(chatID, "up", terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	case "/down":
		b.sendAction(chatID, "[KEY] DOWN")
		if b.isAttached(chatID) {
			if err := b.manager.SendControl(chatID, "down", terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
				b.sendText(chatID, fmt.Sprintf("down failed: %v", err))
			}
			return nil
		}
		return b.sendInteractiveAndReply(chatID, func() error {
			return b.manager.SendControl(chatID, "down", terminal.CommandMeta{UserID: userID, Source: "telegram"})
		})
	default:
		b.sendText(chatID, "unknown command")
	}
	return nil
}

func (b *Bot) executeAndReply(chatID, userID int64, command string) error {
	lock := b.getChatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	eventCh, unsubscribe := b.bus.Subscribe(chatID)
	defer unsubscribe()

	if err := b.manager.ExecuteWithMeta(chatID, command, terminal.CommandMeta{UserID: userID, Source: "telegram"}); err != nil {
		b.sendText(chatID, fmt.Sprintf("command failed: %v", err))
		return nil
	}

	output := b.collectCommandOutput(eventCh, 8*time.Second, 1200*time.Millisecond, 90*time.Second)
	if strings.TrimSpace(output) == "" {
		b.sendText(chatID, "(running or no output)")
		return nil
	}
	b.sendChunkedCode(chatID, output)
	return nil
}

func (b *Bot) sendInteractiveAndReply(chatID int64, fn func() error) error {
	lock := b.getChatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	eventCh, unsubscribe := b.bus.Subscribe(chatID)
	defer unsubscribe()

	if err := fn(); err != nil {
		b.sendText(chatID, fmt.Sprintf("interactive action failed: %v", err))
		return nil
	}

	output := b.collectCommandOutput(eventCh, 3*time.Second, 900*time.Millisecond, 45*time.Second)
	if strings.TrimSpace(output) == "" {
		return nil
	}
	b.sendChunkedCode(chatID, output)
	return nil
}

func (b *Bot) sendChunkedCode(chatID int64, text string) {
	chunks := splitForTelegramHTML(text, b.maxChunkSize)
	for i, c := range chunks {
		title := "[OUT]"
		if looksLikeErrorChunk(c) {
			title = "[ERR]"
		}
		body := "<pre>" + html.EscapeString(c) + "</pre>"
		body = fmt.Sprintf("<b>%s</b>\n%s", title, body)
		if len(chunks) > 1 {
			body = fmt.Sprintf("<b>%s %d/%d</b>\n%s", title, i+1, len(chunks), "<pre>"+html.EscapeString(c)+"</pre>")
		}
		msg := tgbotapi.NewMessage(chatID, body)
		msg.ParseMode = tgbotapi.ModeHTML
		if _, err := b.api.Send(msg); err != nil {
			slog.Warn("send telegram output failed", "chat_id", chatID, "error", err)
		}
	}
}

func (b *Bot) sendStatus(chatID int64, text string) {
	clean := strings.TrimSpace(sanitizeTerminalOutput(text))
	if clean == "" {
		return
	}
	msg := tgbotapi.NewMessage(chatID, "<b>[SESSION]</b> <code>"+html.EscapeString(clean)+"</code>")
	msg.ParseMode = tgbotapi.ModeHTML
	if _, err := b.api.Send(msg); err != nil {
		slog.Warn("send telegram status failed", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) sendAction(chatID int64, action string) {
	msg := tgbotapi.NewMessage(chatID, "<b>"+html.EscapeString(action)+"</b>")
	msg.ParseMode = tgbotapi.ModeHTML
	if _, err := b.api.Send(msg); err != nil {
		slog.Warn("send action message failed", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) sendText(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		slog.Warn("send telegram message failed", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) isAllowed(userID int64) bool {
	if len(b.allowedUsers) == 0 {
		return true
	}
	_, ok := b.allowedUsers[userID]
	return ok
}

func (b *Bot) isChatAllowed(chatID int64) bool {
	if len(b.allowedChats) == 0 {
		return true
	}
	_, ok := b.allowedChats[chatID]
	return ok
}

func (b *Bot) isAttached(chatID int64) bool {
	_, ok := b.streamRelays.Load(chatID)
	return ok
}

func (b *Bot) attachStream(chatID int64) error {
	if err := b.ensureAttached(chatID); err != nil {
		return err
	}
	return nil
}

func (b *Bot) ensureAttached(chatID int64) error {
	if v, ok := b.streamRelays.Load(chatID); ok {
		r, _ := v.(*streamRelay)
		if r != nil {
			select {
			case <-r.ready:
			default:
			}
			return nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	relay := &streamRelay{cancel: cancel, ready: make(chan struct{})}
	actual, loaded := b.streamRelays.LoadOrStore(chatID, relay)
	if loaded {
		cancel()
		if existing, ok := actual.(*streamRelay); ok {
			select {
			case <-existing.ready:
			default:
			}
		}
		return nil
	}

	eventCh, unsubscribe := b.bus.Subscribe(chatID)
	go func() {
		close(relay.ready)
		defer unsubscribe()
		defer b.streamRelays.Delete(chatID)

		ticker := time.NewTicker(700 * time.Millisecond)
		defer ticker.Stop()

		var out strings.Builder
		flush := func() {
			if out.Len() == 0 {
				return
			}
			b.sendChunkedCode(chatID, out.String())
			out.Reset()
		}

		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case evt, ok := <-eventCh:
				if !ok {
					flush()
					return
				}
				if evt.Type == "status" {
					flush()
					b.sendStatus(chatID, evt.Data)
					continue
				}
				clean := sanitizeTerminalOutput(evt.Data)
				if clean == "" {
					continue
				}
				out.WriteString(clean)
				if out.Len() >= b.maxChunkSize {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	return nil
}

func (b *Bot) detachStream(chatID int64) {
	v, ok := b.streamRelays.Load(chatID)
	if !ok {
		return
	}
	b.streamRelays.Delete(chatID)
	if relay, ok := v.(*streamRelay); ok {
		relay.cancel()
	}
}

func (b *Bot) getChatLock(chatID int64) *sync.Mutex {
	actual, _ := b.chatLocks.LoadOrStore(chatID, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

func (b *Bot) collectCommandOutput(eventCh <-chan events.OutputEvent, firstOutputWait, idleWindow, maxWait time.Duration) string {
	if firstOutputWait <= 0 {
		firstOutputWait = 5 * time.Second
	}
	if idleWindow <= 0 {
		idleWindow = 900 * time.Millisecond
	}
	if maxWait <= 0 {
		maxWait = 45 * time.Second
	}

	idleTimer := time.NewTimer(firstOutputWait)
	defer idleTimer.Stop()
	hardTimer := time.NewTimer(maxWait)
	defer hardTimer.Stop()

	var out strings.Builder
	received := false

	for {
		select {
		case <-hardTimer.C:
			if out.Len() > 0 {
				out.WriteString("\n[truncated: wait timeout]\n")
			}
			return out.String()
		case <-idleTimer.C:
			if received {
				return out.String()
			}
			return ""
		case evt, ok := <-eventCh:
			if !ok {
				return out.String()
			}

			chunk := sanitizeTerminalOutput(evt.Data)
			if chunk == "" {
				continue
			}
			if evt.Type == "status" {
				out.WriteString("[SESSION] ")
			}
			out.WriteString(chunk)
			out.WriteByte('\n')
			received = true

			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idleWindow)
		}
	}
}

func splitForTelegramHTML(text string, max int) []string {
	if max <= 0 {
		max = 3000
	}
	const wrapperLen = len("<pre></pre>")
	contentMax := max - wrapperLen
	if contentMax < 64 {
		contentMax = 64
	}

	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}

	out := make([]string, 0, len(runes)/contentMax+1)
	start := 0
	for start < len(runes) {
		used := 0
		end := start
		for end < len(runes) {
			escapedLen := len(html.EscapeString(string(runes[end])))
			if used+escapedLen > contentMax {
				break
			}
			used += escapedLen
			end++
		}
		if end == start {
			end++
		}
		out = append(out, string(runes[start:end]))
		start = end
	}

	return out
}

func looksLikeErrorChunk(s string) bool {
	lower := strings.ToLower(s)
	keywords := []string{"error", "failed", "exception", "traceback", "panic", "permission denied", "not found"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func normalizeInteractiveCLICommand(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return command
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return command
	}
	if strings.EqualFold(parts[0], "codex") || strings.EqualFold(parts[0], "opencode") {
		return "TERM=dumb NO_COLOR=1 CLICOLOR=0 FORCE_COLOR=0 " + command
	}
	return command
}
