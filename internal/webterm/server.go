package webterm

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"terminal-bridge/internal/events"
	"terminal-bridge/internal/terminal"
)

//go:embed static/*
var staticFS embed.FS

type Config struct {
	BaseURL string
	Bus     *events.Bus
	Manager *terminal.Manager
	Tokens  *TokenStore
}

type Server struct {
	baseURL  string
	bus      *events.Bus
	manager  *terminal.Manager
	tokens   *TokenStore
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
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		bus:     cfg.Bus,
		manager: cfg.Manager,
		tokens:  cfg.Tokens,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(_ *http.Request) bool { return true },
		},
	}
}

func (s *Server) IssueURL(chatID int64) (string, error) {
	token, err := s.tokens.Issue(chatID)
	if err != nil {
		return "", err
	}
	return s.baseURL + "/term?token=" + token, nil
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
	chatID, ok := s.tokens.ResolveWSToken(token)
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
	history := s.bus.Snapshot(chatID)
	for _, evt := range history {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(evt.Data)); err != nil {
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
					case "resize":
						if msg.Cols > 0 && msg.Rows > 0 {
							_ = s.manager.Resize(chatID, msg.Cols, msg.Rows, terminal.CommandMeta{Source: "webterm"})
						}
						continue
					case "input":
						if msg.Data != "" {
							_ = s.manager.SendInteractiveInput(chatID, msg.Data, false, terminal.CommandMeta{Source: "webterm"})
						}
						continue
					}
				}
			}

			_ = s.manager.SendInteractiveInput(chatID, string(payload), false, terminal.CommandMeta{Source: "webterm"})
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
			if err := conn.WriteMessage(websocket.TextMessage, []byte(evt.Data)); err != nil {
				return
			}
		}
	}
}
