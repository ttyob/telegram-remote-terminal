package webterm

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DashboardStateStore struct {
	db    *sql.DB
	codec *stateCodec
}

type GatewayRecord struct {
	AgentID    string    `json:"agent_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Online     bool      `json:"online"`
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

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS gateway_agent (
  agent_id TEXT PRIMARY KEY,
  token_cipher TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL DEFAULT ''
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

func (s *DashboardStateStore) CreateGateway(agentID string) (string, string, error) {
	agentID = normalizeAgentID(agentID)
	if agentID == "" {
		agentID = "gw-" + randomHex(6)
	}
	token := randomHex(24)
	cipher, err := s.codec.Encrypt(token)
	if err != nil {
		return "", "", err
	}
	_, err = s.db.Exec(`
INSERT INTO gateway_agent(agent_id, token_cipher, created_at, updated_at, last_seen_at)
VALUES (?, ?, datetime('now'), datetime('now'), '')
`, agentID, cipher)
	if err != nil {
		return "", "", err
	}
	return agentID, token, nil
}

func (s *DashboardStateStore) ListGateways(online map[string]struct{}) ([]GatewayRecord, error) {
	rows, err := s.db.Query(`
SELECT agent_id, created_at, updated_at, last_seen_at
FROM gateway_agent
ORDER BY created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]GatewayRecord, 0)
	for rows.Next() {
		var id, createdRaw, updatedRaw, seenRaw string
		if err := rows.Scan(&id, &createdRaw, &updatedRaw, &seenRaw); err != nil {
			return nil, err
		}
		rec := GatewayRecord{AgentID: id, Online: hasOnline(online, id)}
		rec.CreatedAt = parseSQLiteTime(createdRaw)
		rec.UpdatedAt = parseSQLiteTime(updatedRaw)
		rec.LastSeenAt = parseSQLiteTime(seenRaw)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *DashboardStateStore) GetGatewayToken(agentID string) (string, bool, error) {
	agentID = normalizeAgentID(agentID)
	if agentID == "" {
		return "", false, nil
	}
	var cipher string
	err := s.db.QueryRow(`SELECT token_cipher FROM gateway_agent WHERE agent_id = ?`, agentID).Scan(&cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	plain, err := s.codec.Decrypt(cipher)
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}

func (s *DashboardStateStore) ValidateGatewayToken(agentID, token string) (bool, error) {
	stored, ok, err := s.GetGatewayToken(agentID)
	if err != nil || !ok {
		return false, err
	}
	return stored == token, nil
}

func (s *DashboardStateStore) TouchGatewaySeen(agentID string) error {
	agentID = normalizeAgentID(agentID)
	if agentID == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE gateway_agent SET last_seen_at=datetime('now'), updated_at=datetime('now') WHERE agent_id = ?`, agentID)
	return err
}

func normalizeAgentID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	out := make([]rune, 0, len(v))
	for _, ch := range v {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' {
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return ""
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return string(out)
}

func randomHex(bytes int) string {
	if bytes <= 0 {
		bytes = 16
	}
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

func parseSQLiteTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05", raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func hasOnline(m map[string]struct{}, id string) bool {
	if len(m) == 0 {
		return false
	}
	_, ok := m[id]
	return ok
}
