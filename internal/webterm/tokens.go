package webterm

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type tokenEntry struct {
	chatID    int64
	agentID   string
	expiresAt time.Time
	used      bool
	wsToken   string
}

type TokenStore struct {
	mu         sync.RWMutex
	pageTokens map[string]tokenEntry
	wsTokens   map[string]tokenEntry
	ttl        time.Duration
}

func NewTokenStore(ttl time.Duration) *TokenStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &TokenStore{
		pageTokens: make(map[string]tokenEntry),
		wsTokens:   make(map[string]tokenEntry),
		ttl:        ttl,
	}
}

func (s *TokenStore) Issue(chatID int64) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.pageTokens[token] = tokenEntry{chatID: chatID, expiresAt: time.Now().Add(s.ttl)}
	s.mu.Unlock()

	return token, nil
}

func (s *TokenStore) IssueWSToken(chatID int64, agentID string) (string, time.Time, error) {
	wsToken, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(s.ttl)

	s.mu.Lock()
	s.wsTokens[wsToken] = tokenEntry{chatID: chatID, agentID: agentID, expiresAt: expiresAt}
	s.mu.Unlock()

	return wsToken, expiresAt, nil
}

func (s *TokenStore) UsePageToken(token string) (string, int64, bool) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.pageTokens[token]
	if !ok {
		return "", 0, false
	}
	if now.After(entry.expiresAt) {
		delete(s.pageTokens, token)
		return "", 0, false
	}
	if entry.used {
		if entry.wsToken == "" {
			return "", 0, false
		}
		return entry.wsToken, entry.chatID, true
	}

	wsToken, err := newToken()
	if err != nil {
		return "", 0, false
	}
	entry.used = true
	entry.wsToken = wsToken
	s.pageTokens[token] = entry
	s.wsTokens[wsToken] = tokenEntry{chatID: entry.chatID, agentID: entry.agentID, expiresAt: entry.expiresAt}

	return wsToken, entry.chatID, true
}

func (s *TokenStore) ResolveWSToken(token string) (int64, string, bool) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.wsTokens[token]
	if !ok {
		return 0, "", false
	}
	if now.After(entry.expiresAt) {
		delete(s.wsTokens, token)
		return 0, "", false
	}
	return entry.chatID, entry.agentID, true
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *TokenStore) CleanupExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, entry := range s.pageTokens {
		if now.After(entry.expiresAt) {
			delete(s.pageTokens, token)
		}
	}
	for token, entry := range s.wsTokens {
		if now.After(entry.expiresAt) {
			delete(s.wsTokens, token)
		}
	}
}
