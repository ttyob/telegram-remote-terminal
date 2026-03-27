package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"terminal-bridge/internal/audit"
	"terminal-bridge/internal/config"
	"terminal-bridge/internal/events"
	"terminal-bridge/internal/telegram"
	"terminal-bridge/internal/terminal"
	"terminal-bridge/internal/webterm"
	"terminal-bridge/internal/ws"
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

	wsServer := ws.NewServer(ws.Config{
		Bus:           bus,
		Manager:       mgr,
		ReadToken:     cfg.WSToken,
		DefaultChatID: cfg.DefaultWSChatID,
	})
	webTermTokens := webterm.NewTokenStore(cfg.WebTermTokenTTL)
	webTermServer := webterm.NewServer(webterm.Config{
		BaseURL: cfg.WebPublicBaseURL,
		Bus:     bus,
		Manager: mgr,
		Tokens:  webTermTokens,
	})

	var tg *telegram.Bot
	if cfg.TelegramEnabled {
		tg, err = telegram.NewBot(telegram.Config{
			Token:           cfg.TelegramBotToken,
			APIEndpoint:     cfg.TelegramAPIEndpoint,
			ProxyURL:        cfg.TelegramProxyURL,
			RequestTimeout:  cfg.TelegramRequestTimeout,
			LongPollTimeout: cfg.TelegramLongPollTimeout,
			OpenWebTermURL:  webTermServer.IssueURL,
			AllowedUsers:    cfg.AllowedTelegramUsers,
			AllowedChats:    cfg.AllowedTelegramChats,
			MaxChunkSize:    cfg.MaxOutputChunk,
			Bus:             bus,
			Manager:         mgr,
		})
		if err != nil {
			slog.Error("create telegram bot failed", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("telegram bot disabled by config")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", wsServer.HandleWS)
	mux.HandleFunc("/term", webTermServer.HandlePage)
	mux.HandleFunc("/term/ws", webTermServer.HandleWS)
	if cfg.WebTermDebugEnabled {
		slog.Warn("webterm debug endpoint enabled", "path", "/term/debug/open")
		mux.HandleFunc("/term/debug/open", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			chatID := int64(1)
			raw := r.URL.Query().Get("chat_id")
			if raw != "" {
				parsed, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || parsed == 0 {
					http.Error(w, "invalid chat_id", http.StatusBadRequest)
					return
				}
				chatID = parsed
			}

			link, err := webTermServer.IssueURL(chatID)
			if err != nil {
				http.Error(w, "issue token failed", http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"chat_id": chatID,
				"url":     link,
			})
		})
	}

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	g, gctx := errgroup.WithContext(ctx)
	if tg != nil {
		g.Go(func() error {
			slog.Info("telegram bot started")
			return tg.Run(gctx)
		})
	}
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
