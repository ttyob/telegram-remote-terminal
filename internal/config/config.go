package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	WebTermTokenTTL         time.Duration
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
	DashboardDBPath         string
	DashboardEncryptionKey  string
	ConsoleAuthPasswordHash string
	ConsoleAuthSessionTTL   time.Duration
}

func Load() (Config, error) {
	_ = godotenv.Load(".env")

	cfg := Config{
		WebTermTokenTTL:         mustDuration("WEB_TERM_TOKEN_TTL", 5*time.Minute),
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
		DashboardDBPath:         getWithDefault("DASHBOARD_DB_PATH", "data/dashboard.sqlite"),
		DashboardEncryptionKey:  strings.TrimSpace(os.Getenv("DASHBOARD_ENCRYPTION_KEY")),
		ConsoleAuthPasswordHash: strings.TrimSpace(os.Getenv("CONSOLE_AUTH_PASSWORD_HASH")),
		ConsoleAuthSessionTTL:   mustDuration("CONSOLE_AUTH_SESSION_TTL", 12*time.Hour),
	}

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
