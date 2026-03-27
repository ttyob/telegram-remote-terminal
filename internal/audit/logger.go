package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record struct {
	Timestamp time.Time `json:"timestamp"`
	ChatID    int64     `json:"chat_id"`
	UserID    int64     `json:"user_id,omitempty"`
	Source    string    `json:"source"`
	Command   string    `json:"command"`
	Allowed   bool      `json:"allowed"`
	Error     string    `json:"error,omitempty"`
}

type Logger struct {
	mu   sync.Mutex
	file *os.File
}

func New(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}

	return &Logger{file: f}, nil
}

func (l *Logger) Log(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
