package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"terminal-bridge/internal/events"
)

type TokenValidator func(agentID, token string) (bool, error)

type OnConnect func(agentID string)

type Hub struct {
	bus          *events.Bus
	validate     TokenValidator
	onConnect    OnConnect
	onDisconnect OnConnect

	mu     sync.RWMutex
	agents map[string]*agentConn
}

type agentConn struct {
	id   string
	conn *websocket.Conn
	mu   sync.Mutex
	refs map[int64]int
}

type message struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chat_id,omitempty"`
	Data   string `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
}

func NewHub(bus *events.Bus, validate TokenValidator, onConnect OnConnect, onDisconnect OnConnect) *Hub {
	return &Hub{
		bus:          bus,
		validate:     validate,
		onConnect:    onConnect,
		onDisconnect: onDisconnect,
		agents:       map[string]*agentConn{},
	}
}

func (h *Hub) Enabled() bool {
	return h != nil && h.validate != nil
}

func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		http.Error(w, "agent gateway disabled", http.StatusNotFound)
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if agentID == "" || token == "" {
		http.Error(w, "agent_id and token required", http.StatusBadRequest)
		return
	}

	ok, err := h.validate(agentID, token)
	if err != nil || !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ac := &agentConn{id: agentID, conn: conn, refs: map[int64]int{}}
	h.mu.Lock()
	if old, ok := h.agents[agentID]; ok {
		_ = old.conn.Close()
	}
	h.agents[agentID] = ac
	h.mu.Unlock()
	if h.onConnect != nil {
		h.onConnect(agentID)
	}

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var msg message
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(msg.Type)) {
		case "output":
			if msg.ChatID == 0 || msg.Data == "" {
				continue
			}
			h.bus.Publish(events.OutputEvent{ChatID: msg.ChatID, Data: msg.Data, Type: "output", Timestamp: time.Now()})
		case "status":
			if msg.ChatID == 0 || msg.Data == "" {
				continue
			}
			h.bus.Publish(events.OutputEvent{ChatID: msg.ChatID, Data: msg.Data, Type: "status", Timestamp: time.Now()})
		}
	}

	h.mu.Lock()
	if current, ok := h.agents[agentID]; ok && current == ac {
		delete(h.agents, agentID)
	}
	h.mu.Unlock()
	if h.onDisconnect != nil {
		h.onDisconnect(agentID)
	}
}

func (h *Hub) HasAgent(agentID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.agents[agentID]
	return ok
}

func (h *Hub) OnlineSet() map[string]struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]struct{}, len(h.agents))
	for id := range h.agents {
		out[id] = struct{}{}
	}
	return out
}

func (h *Hub) Open(agentID string, chatID int64) error {
	ac, err := h.get(agentID)
	if err != nil {
		return err
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.refs[chatID]++
	if ac.refs[chatID] > 1 {
		return nil
	}
	return ac.send(message{Type: "open", ChatID: chatID})
}

func (h *Hub) Close(agentID string, chatID int64) {
	ac, err := h.get(agentID)
	if err != nil {
		return
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	current := ac.refs[chatID]
	if current <= 1 {
		delete(ac.refs, chatID)
		_ = ac.send(message{Type: "close", ChatID: chatID})
		return
	}
	ac.refs[chatID] = current - 1
}

func (h *Hub) Input(agentID string, chatID int64, data string) error {
	ac, err := h.get(agentID)
	if err != nil {
		return err
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if _, ok := ac.refs[chatID]; !ok {
		ac.refs[chatID] = 1
		if err := ac.send(message{Type: "open", ChatID: chatID}); err != nil {
			return err
		}
	}
	return ac.send(message{Type: "input", ChatID: chatID, Data: data})
}

func (h *Hub) Resize(agentID string, chatID int64, cols, rows int) error {
	ac, err := h.get(agentID)
	if err != nil {
		return err
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if _, ok := ac.refs[chatID]; !ok {
		ac.refs[chatID] = 1
		if err := ac.send(message{Type: "open", ChatID: chatID}); err != nil {
			return err
		}
	}
	return ac.send(message{Type: "resize", ChatID: chatID, Cols: cols, Rows: rows})
}

func (h *Hub) Uninstall(agentID string) error {
	ac, err := h.get(agentID)
	if err != nil {
		return err
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if err := ac.send(message{Type: "uninstall"}); err != nil {
		return err
	}
	_ = ac.conn.Close()
	return nil
}

func (h *Hub) get(agentID string) (*agentConn, error) {
	h.mu.RLock()
	ac, ok := h.agents[agentID]
	h.mu.RUnlock()
	if !ok || ac == nil {
		return nil, errors.New("agent offline")
	}
	return ac, nil
}

func (a *agentConn) send(msg message) error {
	a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return a.conn.WriteJSON(msg)
}
