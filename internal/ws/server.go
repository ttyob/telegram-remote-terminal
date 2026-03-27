package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"terminal-bridge/internal/events"
	"terminal-bridge/internal/terminal"
)

type Config struct {
	Bus           *events.Bus
	Manager       *terminal.Manager
	ReadToken     string
	DefaultChatID int64
}

type Server struct {
	bus           *events.Bus
	manager       *terminal.Manager
	readToken     string
	defaultChatID int64
	upgrader      websocket.Upgrader
}

type inboundMessage struct {
	ChatID  int64  `json:"chat_id"`
	Command string `json:"command"`
}

func NewServer(cfg Config) *Server {
	return &Server{
		bus:           cfg.Bus,
		manager:       cfg.Manager,
		readToken:     strings.TrimSpace(cfg.ReadToken),
		defaultChatID: cfg.DefaultChatID,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(_ *http.Request) bool { return true },
		},
	}
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	if s.readToken != "" && strings.TrimSpace(r.URL.Query().Get("token")) != s.readToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	chatID := s.defaultChatID
	if raw := strings.TrimSpace(r.URL.Query().Get("chat_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "invalid chat_id", http.StatusBadRequest)
			return
		}
		chatID = id
	}
	if chatID == 0 {
		http.Error(w, "chat_id is required", http.StatusBadRequest)
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

	go func() {
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
			var in inboundMessage
			if err := json.Unmarshal(payload, &in); err != nil {
				continue
			}
			if in.ChatID == 0 {
				in.ChatID = chatID
			}
			if strings.TrimSpace(in.Command) == "" {
				continue
			}
			_ = s.manager.ExecuteWithMeta(in.ChatID, in.Command, terminal.CommandMeta{Source: "ws"})
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
			if err := conn.WriteJSON(evt); err != nil {
				return
			}
		}
	}
}
