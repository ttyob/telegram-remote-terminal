package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"terminal-bridge/internal/agent"
	"terminal-bridge/internal/audit"
	"terminal-bridge/internal/config"
	"terminal-bridge/internal/events"
	"terminal-bridge/internal/terminal"
	"terminal-bridge/internal/webterm"
)

const fallbackInstallAgentScript = `#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "Please run as root" >&2
  exit 1
fi

AGENT_ID="${AGENT_ID:-}"
AGENT_TOKEN="${AGENT_TOKEN:-}"
AGENT_SERVER_URL="${AGENT_SERVER_URL:-}"
AGENT_HTTP_BASE="${AGENT_HTTP_BASE:-}"
AGENT_SHELL="${AGENT_SHELL:-/bin/bash}"
AGENT_WORKDIR="${AGENT_WORKDIR:-.}"
AGENT_CURL_NOPROXY="${AGENT_CURL_NOPROXY:-*}"

BIN_PATH="/usr/local/bin/terminal-bridge-agent"
SERVICE_PATH="/etc/systemd/system/terminal-bridge-agent.service"
ENV_PATH="/etc/terminal-bridge-agent.env"

if [[ -z "${AGENT_ID}" || -z "${AGENT_TOKEN}" || -z "${AGENT_SERVER_URL}" ]]; then
  echo "Missing required envs: AGENT_ID, AGENT_TOKEN, AGENT_SERVER_URL" >&2
  echo "Example:" >&2
  echo "  AGENT_ID=home-gw AGENT_TOKEN=xxx AGENT_SERVER_URL=wss://your-domain/agent/ws AGENT_HTTP_BASE=https://your-domain bash install-agent.sh" >&2
  exit 1
fi

if [[ -z "${AGENT_HTTP_BASE}" ]]; then
  AGENT_HTTP_BASE="${AGENT_SERVER_URL}"
  AGENT_HTTP_BASE="${AGENT_HTTP_BASE%%/agent/ws*}"
  AGENT_HTTP_BASE="${AGENT_HTTP_BASE/ws:\/\//http:\/\/}"
  AGENT_HTTP_BASE="${AGENT_HTTP_BASE/wss:\/\//https:\/\/}"
fi

ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
  x86_64|amd64)
    AGENT_ARCH="amd64"
    ;;
  aarch64|arm64)
    AGENT_ARCH="arm64"
    ;;
  *)
    echo "Unsupported architecture: ${ARCH_RAW}" >&2
    exit 1
    ;;
esac

DOWNLOAD_URL="${AGENT_HTTP_BASE%/}/agent/download?os=linux&arch=${AGENT_ARCH}"

echo "Downloading agent binary from ${DOWNLOAD_URL} ..."
curl --noproxy "${AGENT_CURL_NOPROXY}" -fsSL "${DOWNLOAD_URL}" -o "${BIN_PATH}"
chmod +x "${BIN_PATH}"

cat >"${ENV_PATH}" <<EOF
AGENT_SERVER_URL=${AGENT_SERVER_URL}
AGENT_ID=${AGENT_ID}
AGENT_TOKEN=${AGENT_TOKEN}
AGENT_SHELL=${AGENT_SHELL}
AGENT_WORKDIR=${AGENT_WORKDIR}
EOF

chmod 600 "${ENV_PATH}"

cat >"${SERVICE_PATH}" <<EOF
[Unit]
Description=terminal-bridge gateway agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_PATH}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3
User=root
WorkingDirectory=/

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now terminal-bridge-agent

echo "Installed. Service status:"
systemctl --no-pager --full status terminal-bridge-agent || true
`

func readInstallScript() ([]byte, error) {
	candidates := []string{"scripts/install-agent.sh"}
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "scripts", "install-agent.sh"),
			filepath.Join(exeDir, "..", "scripts", "install-agent.sh"),
		)
	}
	for _, p := range candidates {
		content, err := os.ReadFile(filepath.Clean(p))
		if err == nil {
			return content, nil
		}
	}
	return []byte(fallbackInstallAgentScript), nil
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bus := events.NewBus(200, cfg.EventHistoryTTL)
	auditLogger, err := audit.New(cfg.AuditLogPath)
	if err != nil {
		slog.Error("create audit logger failed", "error", err)
		os.Exit(1)
	}

	mgr, err := terminal.NewManager(terminal.ManagerConfig{
		Shell:          cfg.Shell,
		WorkDir:        cfg.WorkDir,
		IdleTimeout:    cfg.SessionIdleTimeout,
		Bus:            bus,
		AllowPrefixes:  cfg.CommandAllowPrefixes,
		DenySubstrings: cfg.CommandDenySubstrings,
		AuditLogger:    auditLogger,
	})
	if err != nil {
		slog.Error("create terminal manager failed", "error", err)
		os.Exit(1)
	}
	defer mgr.Close()

	dashboardStore, err := webterm.NewDashboardStateStore(cfg.DashboardDBPath, cfg.DashboardEncryptionKey)
	if err != nil {
		slog.Error("create dashboard state store failed", "error", err)
		os.Exit(1)
	}
	defer dashboardStore.Close()
	consoleAuth := webterm.NewConsoleAuth(cfg.ConsoleAuthPasswordHash, cfg.ConsoleAuthSessionTTL)
	webTermTokens := webterm.NewTokenStore(cfg.WebTermTokenTTL)
	agentHub := agent.NewHub(bus,
		func(agentID, token string) (bool, error) {
			return dashboardStore.ValidateGatewayToken(agentID, token)
		},
		func(agentID string) {
			_ = dashboardStore.TouchGatewaySeen(agentID)
		},
		nil,
	)
	webTermServer := webterm.NewServer(webterm.Config{
		Bus:      bus,
		Manager:  mgr,
		Tokens:   webTermTokens,
		Store:    dashboardStore,
		Auth:     consoleAuth,
		AgentHub: agentHub,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", webTermServer.HandleDashboard)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/term", webTermServer.HandlePage)
	mux.HandleFunc("/term/ws", webTermServer.HandleWS)
	mux.HandleFunc("/term/api/ws-token", webTermServer.HandleIssueWSToken)
	mux.HandleFunc("/term/api/state", webTermServer.HandleDashboardState)
	mux.HandleFunc("/term/api/gateways", webTermServer.HandleGateways)
	mux.HandleFunc("/term/api/gateways/install", webTermServer.HandleGatewayInstallCommand)
	mux.HandleFunc("/term/api/auth/session", webTermServer.HandleAuthSession)
	mux.HandleFunc("/term/api/auth/login", webTermServer.HandleAuthLogin)
	mux.HandleFunc("/term/api/auth/logout", webTermServer.HandleAuthLogout)
	mux.HandleFunc("/install-agent.sh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		content, err := readInstallScript()
		if err != nil {
			http.Error(w, "script not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(content)
	})
	mux.HandleFunc("/agent/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		goos := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("os")))
		goarch := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("arch")))
		if goos == "" {
			goos = "linux"
		}
		if goarch == "" {
			goarch = "amd64"
		}
		if goos != "linux" || (goarch != "amd64" && goarch != "arm64") {
			http.Error(w, "unsupported target", http.StatusBadRequest)
			return
		}
		tmpFile, err := os.CreateTemp("", "terminal-bridge-agent-*")
		if err != nil {
			http.Error(w, "create temp failed", http.StatusInternalServerError)
			return
		}
		binPath := tmpFile.Name()
		_ = tmpFile.Close()
		defer os.Remove(binPath)

		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/agent")
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Error("build agent binary failed", "error", err, "output", string(out))
			http.Error(w, "build agent failed", http.StatusInternalServerError)
			return
		}
		content, err := os.ReadFile(binPath)
		if err != nil {
			http.Error(w, "read binary failed", http.StatusInternalServerError)
			return
		}
		fileName := "terminal-bridge-agent-" + goos + "-" + goarch
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+fileName)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
	})
	mux.HandleFunc("/agent/ws", agentHub.HandleWS)

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		slog.Info("http server started", "addr", cfg.ListenAddr)
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				webTermTokens.CleanupExpired()
			}
		}
	})

	if err := g.Wait(); err != nil {
		slog.Error("service exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("service stopped")
}
