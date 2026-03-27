package webterm

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type DashboardStateStore struct {
	db    *sql.DB
	codec *stateCodec
}

func NewDashboardStateStore(path, encryptionKey string) (*DashboardStateStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("db path is required")
	}
	codec, err := newStateCodec(encryptionKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS dashboard_state (
  profile_id TEXT PRIMARY KEY,
  payload TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &DashboardStateStore{db: db, codec: codec}, nil
}

func (s *DashboardStateStore) Get(profileID string) (string, bool, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload FROM dashboard_state WHERE profile_id = ?`, profileID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	payload, err = s.codec.Decrypt(payload)
	if err != nil {
		return "", false, err
	}
	return payload, true, nil
}

func (s *DashboardStateStore) Save(profileID, payload string) error {
	encrypted, err := s.codec.Encrypt(payload)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO dashboard_state(profile_id, payload, updated_at)
VALUES (?, ?, datetime('now'))
ON CONFLICT(profile_id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at;
`, profileID, encrypted)
	return err
}

func (s *DashboardStateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
