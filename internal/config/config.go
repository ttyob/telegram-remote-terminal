package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	TelegramEnabled         bool
	TelegramBotToken        string
	TelegramAPIEndpoint     string
	TelegramProxyURL        string
	TelegramRequestTimeout  time.Duration
	TelegramLongPollTimeout int
	WebTermDebugEnabled     bool
	WebPublicBaseURL        string
	WebTermTokenTTL         time.Duration
	AllowedTelegramUsers    map[int64]struct{}
	AllowedTelegramChats    map[int64]struct{}
	Shell                   string
	WorkDir                 string
	ListenAddr              string
	SessionIdleTimeout      time.Duration
	EventHistoryTTL         time.Duration
	ShutdownTimeout         time.Duration
	MaxOutputChunk          int
	CommandAllowPrefixes    []string
	CommandDenySubstrings   []string
	AuditLogPath            string
	WSToken                 string
	DefaultWSChatID         int64
}

func Load() (Config, error) {
	_ = godotenv.Load(".env")

	cfg := Config{
		TelegramEnabled:         mustBool("TELEGRAM_ENABLED", true),
		TelegramBotToken:        strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramAPIEndpoint:     getWithDefault("TELEGRAM_API_ENDPOINT", "https://api.telegram.org/bot%s/%s"),
		TelegramProxyURL:        strings.TrimSpace(os.Getenv("TELEGRAM_PROXY_URL")),
		TelegramRequestTimeout:  mustDuration("TELEGRAM_REQUEST_TIMEOUT", 30*time.Second),
		TelegramLongPollTimeout: mustInt("TELEGRAM_LONG_POLL_TIMEOUT", 60),
		WebTermDebugEnabled:     mustBool("WEB_TERM_DEBUG_ENABLED", false),
		WebPublicBaseURL:        strings.TrimRight(getWithDefault("WEB_PUBLIC_BASE_URL", "http://127.0.0.1:8080"), "/"),
		WebTermTokenTTL:         mustDuration("WEB_TERM_TOKEN_TTL", 5*time.Minute),
		AllowedTelegramUsers:    map[int64]struct{}{},
		AllowedTelegramChats:    map[int64]struct{}{},
		Shell:                   getWithDefault("SHELL_PATH", "/bin/bash"),
		WorkDir:                 getWithDefault("TERMINAL_WORKDIR", "."),
		ListenAddr:              getWithDefault("LISTEN_ADDR", ":8080"),
		SessionIdleTimeout:      mustDuration("SESSION_IDLE_TIMEOUT", 30*time.Minute),
		EventHistoryTTL:         mustDuration("EVENT_HISTORY_TTL", 24*time.Hour),
		ShutdownTimeout:         mustDuration("SHUTDOWN_TIMEOUT", 8*time.Second),
		MaxOutputChunk:          mustInt("MAX_OUTPUT_CHUNK", 3000),
		CommandAllowPrefixes:    parseStringList(os.Getenv("COMMAND_ALLOW_PREFIXES")),
		CommandDenySubstrings:   parseStringList(os.Getenv("COMMAND_DENY_SUBSTRINGS")),
		AuditLogPath:            getWithDefault("AUDIT_LOG_PATH", "logs/audit.log"),
		WSToken:                 strings.TrimSpace(os.Getenv("WS_TOKEN")),
		DefaultWSChatID:         mustInt64("WS_DEFAULT_CHAT_ID", 0),
	}

	if cfg.TelegramEnabled && cfg.TelegramBotToken == "" {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	ids, err := parseInt64Set(os.Getenv("ALLOWED_TELEGRAM_USERS"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid ALLOWED_TELEGRAM_USERS: %w", err)
	}
	cfg.AllowedTelegramUsers = ids

	chats, err := parseInt64Set(os.Getenv("ALLOWED_TELEGRAM_CHATS"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid ALLOWED_TELEGRAM_CHATS: %w", err)
	}
	cfg.AllowedTelegramChats = chats

	return cfg, nil
}

func parseStringList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

func parseInt64Set(raw string) (map[int64]struct{}, error) {
	result := make(map[int64]struct{})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return result, nil
	}

	for _, part := range strings.Split(raw, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not int64", v)
		}
		result[n] = struct{}{}
	}

	return result, nil
}

func getWithDefault(key, fallback string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	return val
}

func mustDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func mustInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func mustInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func mustBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}

	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
