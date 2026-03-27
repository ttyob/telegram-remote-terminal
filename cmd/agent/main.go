package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"terminal-bridge/internal/terminal"
)

type cfg struct {
	ServerURL string
	AgentID   string
	Token     string
	Shell     string
	WorkDir   string
}

type message struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chat_id,omitempty"`
	Data   string `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
}

type sessionItem struct {
	session *terminal.Session
}

func main() {
	cfg, err := load()
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		if err := runOnce(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("agent disconnected", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func runOnce(ctx context.Context, c cfg) error {
	endpoint, err := url.Parse(c.ServerURL)
	if err != nil {
		return err
	}
	q := endpoint.Query()
	q.Set("agent_id", c.AgentID)
	q.Set("token", c.Token)
	endpoint.RawQuery = q.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	slog.Info("agent connected", "server", endpoint.Host, "agent_id", c.AgentID)

	var writeMu sync.Mutex
	sessions := map[int64]*sessionItem{}
	var sessMu sync.Mutex

	send := func(msg message) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(msg)
	}

	closeAll := func() {
		sessMu.Lock()
		defer sessMu.Unlock()
		for id, item := range sessions {
			if item != nil && item.session != nil {
				item.session.Stop()
			}
			delete(sessions, id)
		}
	}
	defer closeAll()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var msg message
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}

		switch strings.ToLower(strings.TrimSpace(msg.Type)) {
		case "open":
			if msg.ChatID == 0 {
				continue
			}
			sessMu.Lock()
			if _, ok := sessions[msg.ChatID]; ok {
				sessMu.Unlock()
				continue
			}
			chatID := msg.ChatID
			s, err := terminal.NewSession(c.Shell, c.WorkDir,
				func(out []byte) {
					_ = send(message{Type: "output", ChatID: chatID, Data: string(out)})
				},
				func(exitErr error) {
					status := "\n[session closed]\n"
					if exitErr != nil {
						status = "\n[session closed: " + exitErr.Error() + "]\n"
					}
					_ = send(message{Type: "status", ChatID: chatID, Data: status})
					sessMu.Lock()
					delete(sessions, chatID)
					sessMu.Unlock()
				},
			)
			if err != nil {
				sessMu.Unlock()
				_ = send(message{Type: "status", ChatID: chatID, Data: "\n[open session failed: " + err.Error() + "]\n"})
				continue
			}
			sessions[chatID] = &sessionItem{session: s}
			sessMu.Unlock()
			_ = send(message{Type: "status", ChatID: chatID, Data: "[new terminal session created]\n"})
		case "close":
			if msg.ChatID == 0 {
				continue
			}
			sessMu.Lock()
			item := sessions[msg.ChatID]
			delete(sessions, msg.ChatID)
			sessMu.Unlock()
			if item != nil && item.session != nil {
				item.session.Stop()
			}
		case "input":
			if msg.ChatID == 0 {
				continue
			}
			sessMu.Lock()
			item := sessions[msg.ChatID]
			sessMu.Unlock()
			if item == nil || item.session == nil {
				continue
			}
			_ = item.session.WriteRaw(msg.Data)
		case "resize":
			if msg.ChatID == 0 || msg.Cols <= 0 || msg.Rows <= 0 {
				continue
			}
			sessMu.Lock()
			item := sessions[msg.ChatID]
			sessMu.Unlock()
			if item == nil || item.session == nil {
				continue
			}
			_ = item.session.Resize(msg.Cols, msg.Rows)
		case "uninstall":
			_ = send(message{Type: "status", ChatID: 0, Data: "[agent uninstall requested]\n"})
			go uninstallSelf()
			return errors.New("agent uninstall requested")
		}
	}
}

func uninstallSelf() {
	cmd := exec.Command("/bin/sh", "-c", "systemctl disable --now terminal-bridge-agent >/dev/null 2>&1 || true; rm -f /etc/terminal-bridge-agent.env /etc/systemd/system/terminal-bridge-agent.service; systemctl daemon-reload >/dev/null 2>&1 || true; (sleep 1; rm -f /usr/local/bin/terminal-bridge-agent) >/dev/null 2>&1 &")
	_ = cmd.Run()
	os.Exit(0)
}

func load() (cfg, error) {
	port := strings.TrimSpace(getenv("AGENT_PORT", ""))
	host := strings.TrimSpace(getenv("AGENT_SERVER_HOST", ""))
	serverURL := strings.TrimSpace(os.Getenv("AGENT_SERVER_URL"))
	if serverURL == "" && host != "" {
		scheme := "wss"
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "ws://") || strings.Contains(host, "127.0.0.1") || strings.Contains(host, "localhost") {
			scheme = "ws"
		}
		h := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "wss://"), "ws://")
		if port != "" {
			h = h + ":" + port
		}
		serverURL = fmt.Sprintf("%s://%s/agent/ws", scheme, h)
	}
	if serverURL == "" {
		return cfg{}, errors.New("AGENT_SERVER_URL is required")
	}
	if _, err := url.Parse(serverURL); err != nil {
		return cfg{}, fmt.Errorf("invalid AGENT_SERVER_URL: %w", err)
	}

	out := cfg{
		ServerURL: serverURL,
		AgentID:   strings.TrimSpace(os.Getenv("AGENT_ID")),
		Token:     strings.TrimSpace(os.Getenv("AGENT_TOKEN")),
		Shell:     getenv("AGENT_SHELL", "/bin/bash"),
		WorkDir:   getenv("AGENT_WORKDIR", "."),
	}
	if out.AgentID == "" || out.Token == "" {
		return cfg{}, errors.New("AGENT_ID and AGENT_TOKEN are required")
	}
	if out.Shell == "" {
		out.Shell = "/bin/bash"
	}
	if out.WorkDir == "" {
		out.WorkDir = "."
	}
	return out, nil
}

func getenv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
