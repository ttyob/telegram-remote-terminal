package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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
		content, err := os.ReadFile("scripts/install-agent.sh")
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
