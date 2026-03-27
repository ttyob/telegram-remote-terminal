package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

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
	webTermServer := webterm.NewServer(webterm.Config{
		Bus:     bus,
		Manager: mgr,
		Tokens:  webTermTokens,
		Store:   dashboardStore,
		Auth:    consoleAuth,
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
	mux.HandleFunc("/term/api/auth/session", webTermServer.HandleAuthSession)
	mux.HandleFunc("/term/api/auth/login", webTermServer.HandleAuthLogin)
	mux.HandleFunc("/term/api/auth/logout", webTermServer.HandleAuthLogout)

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
