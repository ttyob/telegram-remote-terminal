package webterm

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"terminal-bridge/internal/agent"
	"terminal-bridge/internal/events"
	"terminal-bridge/internal/terminal"
)

//go:embed static/*
var staticFS embed.FS

type Config struct {
	Bus      *events.Bus
	Manager  *terminal.Manager
	Tokens   *TokenStore
	Store    *DashboardStateStore
	Auth     *ConsoleAuth
	AgentHub *agent.Hub
}

type Server struct {
	bus      *events.Bus
	manager  *terminal.Manager
	tokens   *TokenStore
	store    *DashboardStateStore
	auth     *ConsoleAuth
	agentHub *agent.Hub
	upgrader websocket.Upgrader
}

type clientMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func NewServer(cfg Config) *Server {
	return &Server{
		bus:      cfg.Bus,
		manager:  cfg.Manager,
		tokens:   cfg.Tokens,
		store:    cfg.Store,
		auth:     cfg.Auth,
		agentHub: cfg.AgentHub,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(_ *http.Request) bool { return true },
		},
	}
}

func (s *Server) requireConsoleAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.auth == nil {
		return true
	}
	return s.auth.RequireAuth(w, r)
}

func (s *Server) HandleAuthSession(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "authenticated": true})
		return
	}
	s.auth.HandleSession(w, r)
}

func (s *Server) HandleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": false})
		return
	}
	s.auth.HandleLogin(w, r)
}

func (s *Server) HandleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	s.auth.HandleLogout(w, r)
}

func (s *Server) HandleGateways(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "dashboard state store not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.requireConsoleAuth(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		online := map[string]struct{}{}
		if s.agentHub != nil {
			online = s.agentHub.OnlineSet()
		}
		items, err := s.store.ListGateways(online)
		if err != nil {
			http.Error(w, "list gateways failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case http.MethodPost:
		var req struct {
			AgentID string `json:"agent_id"`
			Name    string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		agentID, token, err := s.store.CreateGateway(req.AgentID, req.Name)
		if err != nil {
			http.Error(w, "create gateway failed", http.StatusBadRequest)
			return
		}
		command := s.buildInstallCommand(r, agentID, token)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": agentID, "token": token, "install_command": command})
	case http.MethodPut:
		var req struct {
			AgentID string `json:"agent_id"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if err := s.store.RenameGateway(req.AgentID, req.Name); err != nil {
			http.Error(w, "rename gateway failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case http.MethodDelete:
		agentID := normalizeAgentID(r.URL.Query().Get("agent_id"))
		if agentID == "" {
			http.Error(w, "agent_id required", http.StatusBadRequest)
			return
		}
		uninstalled := false
		if s.agentHub != nil && s.agentHub.HasAgent(agentID) {
			if err := s.agentHub.Uninstall(agentID); err == nil {
				uninstalled = true
			}
		}
		if err := s.store.DeleteGateway(agentID); err != nil {
			http.Error(w, "delete gateway failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "agent_id": agentID, "uninstalled": uninstalled})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) HandleGatewayInstallCommand(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "dashboard state store not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireConsoleAuth(w, r) {
		return
	}
	agentID := normalizeAgentID(r.URL.Query().Get("agent_id"))
	if agentID == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	token, ok, err := s.store.GetGatewayToken(agentID)
	if err != nil {
		http.Error(w, "read gateway failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "gateway not found", http.StatusNotFound)
		return
	}
	cmd := s.buildInstallCommand(r, agentID, token)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": agentID, "install_command": cmd})
}

func (s *Server) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	content, err := staticFS.ReadFile("static/dashboard.html")
	if err != nil {
		http.Error(w, "page not available", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (s *Server) HandleIssueWSToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireConsoleAuth(w, r) {
		return
	}

	chatID, err := parseChatID(r)
	if err != nil {
		http.Error(w, "invalid chat_id", http.StatusBadRequest)
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID != "" {
		if s.agentHub == nil || !s.agentHub.Enabled() {
			http.Error(w, "agent gateway disabled", http.StatusBadRequest)
			return
		}
		if _, ok, err := s.store.GetGatewayToken(agentID); err != nil || !ok {
			http.Error(w, "gateway not found", http.StatusBadRequest)
			return
		}
		if !s.agentHub.HasAgent(agentID) {
			http.Error(w, "agent offline", http.StatusServiceUnavailable)
			return
		}
	}

	wsToken, expiresAt, err := s.tokens.IssueWSToken(chatID, agentID)
	if err != nil {
		http.Error(w, "issue token failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"chat_id":    chatID,
		"agent_id":   agentID,
		"ws_token":   wsToken,
		"ws_url":     "/term/ws?token=" + wsToken,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) buildInstallCommand(r *http.Request, agentID, token string) string {
	host := strings.TrimSpace(r.Host)
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xf != "" {
		host = xf
	}
	https := r.TLS != nil
	if xfProto := strings.TrimSpace(strings.ToLower(r.Header.Get("X-Forwarded-Proto"))); xfProto != "" {
		https = xfProto == "https"
	}
	httpScheme := "http"
	wsScheme := "ws"
	if https {
		httpScheme = "https"
		wsScheme = "wss"
	}
	installURL := (&url.URL{Scheme: httpScheme, Host: host, Path: "/install-agent.sh"}).String()
	wsURL := (&url.URL{Scheme: wsScheme, Host: host, Path: "/agent/ws"}).String()
	httpBase := (&url.URL{Scheme: httpScheme, Host: host}).String()
	return fmt.Sprintf("curl --noproxy '*' -fsSL '%s' | AGENT_ID=%s AGENT_TOKEN=%s AGENT_SERVER_URL='%s' AGENT_HTTP_BASE='%s' AGENT_CURL_NOPROXY='*' bash", installURL, agentID, token, wsURL, httpBase)
}

func (s *Server) HandleDashboardState(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "dashboard state store not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.requireConsoleAuth(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetDashboardState(w, r)
	case http.MethodPost:
		s.handleSaveDashboardState(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetDashboardState(w http.ResponseWriter, r *http.Request) {
	profileID := strings.TrimSpace(r.URL.Query().Get("profile_id"))
	if !validProfileID(profileID) {
		http.Error(w, "invalid profile_id", http.StatusBadRequest)
		return
	}

	payload, found, err := s.store.Get(profileID)
	if err != nil {
		http.Error(w, "read dashboard state failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !found {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"profile_id": profileID,
			"found":      false,
			"payload": map[string]any{
				"services": []any{},
				"windows":  []any{},
			},
		})
		return
	}

	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"profile_id": profileID,
			"found":      false,
			"payload": map[string]any{
				"services": []any{},
				"windows":  []any{},
			},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"profile_id": profileID,
		"found":      true,
		"payload":    parsed,
	})
}

func (s *Server) handleSaveDashboardState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProfileID string          `json:"profile_id"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.ProfileID = strings.TrimSpace(req.ProfileID)
	if !validProfileID(req.ProfileID) {
		http.Error(w, "invalid profile_id", http.StatusBadRequest)
		return
	}
	payload := strings.TrimSpace(string(req.Payload))
	if payload == "" || payload == "null" {
		http.Error(w, "payload is required", http.StatusBadRequest)
		return
	}
	if len(payload) > 1_000_000 {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var tmp any
	if err := json.Unmarshal([]byte(payload), &tmp); err != nil {
		http.Error(w, "payload must be valid json", http.StatusBadRequest)
		return
	}

	if err := s.store.Save(req.ProfileID, payload); err != nil {
		http.Error(w, "save dashboard state failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func validProfileID(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 8 || len(v) > 128 {
		return false
	}
	for _, ch := range v {
		if ch >= 'a' && ch <= 'z' {
			continue
		}
		if ch >= 'A' && ch <= 'Z' {
			continue
		}
		if ch >= '0' && ch <= '9' {
			continue
		}
		switch ch {
		case '-', '_', '.':
			continue
		default:
			return false
		}
	}
	return true
}

func parseChatID(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("chat_id"))
	if raw != "" {
		chatID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || chatID == 0 {
			return 0, strconv.ErrSyntax
		}
		return chatID, nil
	}

	if r.Method == http.MethodPost {
		var req struct {
			ChatID int64 `json:"chat_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.ChatID != 0 {
			return req.ChatID, nil
		}
	}

	return 0, errors.New("chat_id required")
}

func (s *Server) HandlePage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	wsToken, _, ok := s.tokens.UsePageToken(token)
	if !ok {
		http.Error(w, "invalid or expired token", http.StatusForbidden)
		return
	}

	content, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "page not available", http.StatusInternalServerError)
		return
	}

	html := strings.ReplaceAll(string(content), "__TERM_WS_URL__", "/term/ws?token="+wsToken)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	chatID, agentID, ok := s.tokens.ResolveWSToken(token)
	if !ok {
		http.Error(w, "invalid or expired token", http.StatusForbidden)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	eventCh, unsubscribe := s.bus.Subscribe(chatID)
	defer unsubscribe()
	if agentID != "" {
		if s.agentHub == nil || !s.agentHub.Enabled() {
			http.Error(w, "agent gateway disabled", http.StatusBadRequest)
			return
		}
		if err := s.agentHub.Open(agentID, chatID); err != nil {
			http.Error(w, fmt.Sprintf("agent open failed: %v", err), http.StatusServiceUnavailable)
			return
		}
		defer s.agentHub.Close(agentID, chatID)
	}
	history := s.bus.Snapshot(chatID)
	for _, evt := range history {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte(evt.Data)); err != nil {
			return
		}
	}

	go func() {
		for {
			msgType, payload, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
			if msgType != websocket.TextMessage {
				continue
			}
			if len(payload) == 0 {
				continue
			}
			trimmed := strings.TrimSpace(string(payload))

			if strings.HasPrefix(trimmed, "{") {
				var msg clientMessage
				if err := json.Unmarshal([]byte(trimmed), &msg); err == nil {
					switch strings.ToLower(strings.TrimSpace(msg.Type)) {
					case "ping":
						_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
						_ = conn.WriteJSON(map[string]any{"type": "pong", "ts": time.Now().UnixMilli(), "echo": msg.Data})
						continue
					case "resize":
						if msg.Cols > 0 && msg.Rows > 0 {
							if agentID != "" {
								_ = s.agentHub.Resize(agentID, chatID, msg.Cols, msg.Rows)
							} else {
								_ = s.manager.Resize(chatID, msg.Cols, msg.Rows, terminal.CommandMeta{Source: "webterm"})
							}
						}
						continue
					case "input":
						if msg.Data != "" {
							if agentID != "" {
								_ = s.agentHub.Input(agentID, chatID, msg.Data)
							} else {
								_ = s.manager.SendInteractiveInput(chatID, msg.Data, false, terminal.CommandMeta{Source: "webterm"})
							}
						}
						continue
					}
				}
			}

			if agentID != "" {
				_ = s.agentHub.Input(agentID, chatID, string(payload))
			} else {
				_ = s.manager.SendInteractiveInput(chatID, string(payload), false, terminal.CommandMeta{Source: "webterm"})
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-eventCh:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, []byte(evt.Data)); err != nil {
				return
			}
		}
	}
}
