package terminal

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"terminal-bridge/internal/audit"
	"terminal-bridge/internal/events"
)

type ManagerConfig struct {
	Shell          string
	WorkDir        string
	IdleTimeout    time.Duration
	Bus            *events.Bus
	AllowPrefixes  []string
	DenySubstrings []string
	AuditLogger    *audit.Logger
}

type CommandMeta struct {
	UserID int64
	Source string
}

type Manager struct {
	cfg ManagerConfig

	mu       sync.Mutex
	sessions map[int64]*managedSession
	closed   bool
	stopCh   chan struct{}
}

type managedSession struct {
	session    *Session
	lastActive time.Time
}

func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Shell == "" {
		return nil, errors.New("shell is required")
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("workdir is required")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.Bus == nil {
		return nil, errors.New("events bus is required")
	}

	m := &Manager{
		cfg:      cfg,
		sessions: make(map[int64]*managedSession),
		stopCh:   make(chan struct{}),
	}

	go m.gcLoop()
	return m, nil
}

func (m *Manager) Execute(chatID int64, command string) error {
	return m.ExecuteWithMeta(chatID, command, CommandMeta{Source: "internal"})
}

func (m *Manager) ExecuteWithMeta(chatID int64, command string, meta CommandMeta) error {
	command = strings.TrimSpace(command)
	if command == "" {
		err := errors.New("command is empty")
		m.audit(chatID, command, meta, false, err)
		return err
	}
	if meta.Source == "" {
		meta.Source = "unknown"
	}

	if err := m.validateCommand(command); err != nil {
		m.audit(chatID, command, meta, false, err)
		return err
	}

	s, err := m.getOrCreate(chatID)
	if err != nil {
		m.audit(chatID, command, meta, false, err)
		return err
	}

	m.touch(chatID)
	err = s.WriteLine(command)
	m.audit(chatID, command, meta, err == nil, err)
	return err
}

func (m *Manager) SendInteractiveInput(chatID int64, input string, appendNewline bool, meta CommandMeta) error {
	if meta.Source == "" {
		meta.Source = "unknown"
	}

	s, err := m.getOrCreate(chatID)
	if err != nil {
		m.audit(chatID, interactiveAuditCommand(input, appendNewline), meta, false, err)
		return err
	}

	m.touch(chatID)
	if appendNewline {
		err = s.WriteLine(input)
	} else {
		err = s.WriteRaw(input)
	}
	m.audit(chatID, interactiveAuditCommand(input, appendNewline), meta, err == nil, err)
	return err
}

func (m *Manager) SendControl(chatID int64, control string, meta CommandMeta) error {
	if meta.Source == "" {
		meta.Source = "unknown"
	}

	s, err := m.getOrCreate(chatID)
	if err != nil {
		m.audit(chatID, "[control:"+control+"]", meta, false, err)
		return err
	}

	var payload []byte
	switch strings.ToLower(strings.TrimSpace(control)) {
	case "ctrlc":
		payload = []byte{0x03}
	case "ctrld":
		payload = []byte{0x04}
	case "ctrlz":
		payload = []byte{0x1a}
	case "tab":
		payload = []byte{0x09}
	case "up":
		payload = []byte("\x1b[A")
	case "down":
		payload = []byte("\x1b[B")
	default:
		err = fmt.Errorf("unsupported control: %s", control)
		m.audit(chatID, "[control:"+control+"]", meta, false, err)
		return err
	}

	m.touch(chatID)
	err = s.WriteBytes(payload)
	m.audit(chatID, "[control:"+control+"]", meta, err == nil, err)
	return err
}

func (m *Manager) Resize(chatID int64, cols, rows int, meta CommandMeta) error {
	if meta.Source == "" {
		meta.Source = "unknown"
	}

	s, err := m.getOrCreate(chatID)
	if err != nil {
		m.audit(chatID, fmt.Sprintf("[resize cols=%d rows=%d]", cols, rows), meta, false, err)
		return err
	}

	m.touch(chatID)
	err = s.Resize(cols, rows)
	m.audit(chatID, fmt.Sprintf("[resize cols=%d rows=%d]", cols, rows), meta, err == nil, err)
	return err
}

func (m *Manager) Reset(chatID int64) {
	m.mu.Lock()
	entry := m.sessions[chatID]
	delete(m.sessions, chatID)
	m.mu.Unlock()

	if entry != nil {
		entry.session.Stop()
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stopCh)
	entries := make([]*managedSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		entries = append(entries, s)
	}
	m.sessions = map[int64]*managedSession{}
	m.mu.Unlock()

	for _, entry := range entries {
		entry.session.Stop()
	}

	if m.cfg.AuditLogger != nil {
		_ = m.cfg.AuditLogger.Close()
	}
}

func (m *Manager) validateCommand(command string) error {
	lower := strings.ToLower(command)
	for _, deny := range m.cfg.DenySubstrings {
		d := strings.TrimSpace(strings.ToLower(deny))
		if d == "" {
			continue
		}
		if strings.Contains(lower, d) {
			return fmt.Errorf("command blocked by deny rule: %q", deny)
		}
	}

	if len(m.cfg.AllowPrefixes) == 0 {
		return nil
	}

	for _, prefix := range m.cfg.AllowPrefixes {
		p := strings.TrimSpace(prefix)
		if p == "" {
			continue
		}
		if strings.HasPrefix(command, p) {
			return nil
		}
	}

	return errors.New("command blocked: not matched by allow prefixes")
}

func (m *Manager) audit(chatID int64, command string, meta CommandMeta, allowed bool, err error) {
	if m.cfg.AuditLogger == nil {
		return
	}
	rec := audit.Record{
		Timestamp: time.Now(),
		ChatID:    chatID,
		UserID:    meta.UserID,
		Source:    meta.Source,
		Command:   command,
		Allowed:   allowed,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if logErr := m.cfg.AuditLogger.Log(rec); logErr != nil {
		slog.Warn("write audit log failed", "error", logErr)
	}
}

func interactiveAuditCommand(input string, appendNewline bool) string {
	trimmed := strings.TrimSpace(input)
	preview := trimmed
	if len(preview) > 48 {
		preview = preview[:48] + "..."
	}
	if preview == "" {
		preview = "<empty>"
	}
	return fmt.Sprintf("[interactive newline=%t preview=%q len=%d]", appendNewline, preview, len(input))
}

func (m *Manager) getOrCreate(chatID int64) (*Session, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("manager closed")
	}
	if existing, ok := m.sessions[chatID]; ok {
		existing.lastActive = time.Now()
		s := existing.session
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	var created *Session
	newSession, err := NewSession(m.cfg.Shell, m.cfg.WorkDir,
		func(out []byte) {
			m.touch(chatID)
			m.cfg.Bus.Publish(events.OutputEvent{
				ChatID:    chatID,
				Data:      string(out),
				Type:      "output",
				Timestamp: time.Now(),
			})
		},
		func(err error) {
			m.cfg.Bus.Publish(events.OutputEvent{
				ChatID:    chatID,
				Data:      fmt.Sprintf("\n[session closed: %v]\n", err),
				Type:      "status",
				Timestamp: time.Now(),
			})

			m.mu.Lock()
			entry, ok := m.sessions[chatID]
			if ok && entry.session == created {
				delete(m.sessions, chatID)
			}
			m.mu.Unlock()
		},
	)
	if err != nil {
		return nil, err
	}
	created = newSession

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		newSession.Stop()
		return nil, errors.New("manager closed")
	}
	if existing, ok := m.sessions[chatID]; ok {
		m.mu.Unlock()
		newSession.Stop()
		return existing.session, nil
	}
	m.sessions[chatID] = &managedSession{session: newSession, lastActive: time.Now()}
	m.mu.Unlock()

	m.cfg.Bus.Publish(events.OutputEvent{
		ChatID:    chatID,
		Data:      "[new terminal session created]\n",
		Type:      "status",
		Timestamp: time.Now(),
	})

	return newSession, nil
}

func (m *Manager) touch(chatID int64) {
	m.mu.Lock()
	if entry, ok := m.sessions[chatID]; ok {
		entry.lastActive = time.Now()
	}
	m.mu.Unlock()
}

func (m *Manager) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cutoff := time.Now().Add(-m.cfg.IdleTimeout)
			var toClose []*managedSession

			m.mu.Lock()
			for chatID, entry := range m.sessions {
				if entry.lastActive.Before(cutoff) {
					toClose = append(toClose, entry)
					delete(m.sessions, chatID)
					m.cfg.Bus.Publish(events.OutputEvent{
						ChatID:    chatID,
						Data:      "\n[session closed due to inactivity]\n",
						Type:      "status",
						Timestamp: time.Now(),
					})
				}
			}
			m.mu.Unlock()

			for _, s := range toClose {
				s.session.Stop()
			}
		case <-m.stopCh:
			return
		}
	}
}
