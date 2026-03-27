package webterm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const consoleSessionCookieName = "webterm_console_session"

type ConsoleAuth struct {
	enabled      bool
	passwordHash []byte
	sessionTTL   time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewConsoleAuth(passwordHash string, sessionTTL time.Duration) *ConsoleAuth {
	passwordHash = strings.TrimSpace(passwordHash)
	if passwordHash == "" {
		return &ConsoleAuth{enabled: false}
	}
	if sessionTTL <= 0 {
		sessionTTL = 12 * time.Hour
	}
	return &ConsoleAuth{
		enabled:      true,
		passwordHash: []byte(passwordHash),
		sessionTTL:   sessionTTL,
		sessions:     make(map[string]time.Time),
	}
}

func (a *ConsoleAuth) Enabled() bool {
	return a != nil && a.enabled
}

func (a *ConsoleAuth) IsAuthenticated(r *http.Request) bool {
	if !a.Enabled() {
		return true
	}
	cookie, err := r.Cookie(consoleSessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return false
	}
	sessionID := strings.TrimSpace(cookie.Value)

	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for sid, expiresAt := range a.sessions {
		if now.After(expiresAt) {
			delete(a.sessions, sid)
		}
	}
	expiresAt, ok := a.sessions[sessionID]
	return ok && now.Before(expiresAt)
}

func (a *ConsoleAuth) VerifyPassword(password string) bool {
	if !a.Enabled() {
		return true
	}
	return bcrypt.CompareHashAndPassword(a.passwordHash, []byte(password)) == nil
}

func (a *ConsoleAuth) StartSession(w http.ResponseWriter, r *http.Request) bool {
	if !a.Enabled() {
		return true
	}
	token, err := newAuthToken()
	if err != nil {
		return false
	}
	expiresAt := time.Now().Add(a.sessionTTL)

	a.mu.Lock()
	a.sessions[token] = expiresAt
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		Expires:  expiresAt,
	})
	return true
}

func (a *ConsoleAuth) EndSession(w http.ResponseWriter, r *http.Request) {
	if a.Enabled() {
		if cookie, err := r.Cookie(consoleSessionCookieName); err == nil {
			a.mu.Lock()
			delete(a.sessions, strings.TrimSpace(cookie.Value))
			a.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func (a *ConsoleAuth) RequireAuth(w http.ResponseWriter, r *http.Request) bool {
	if a.IsAuthenticated(r) {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (a *ConsoleAuth) HandleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled":       a.Enabled(),
		"authenticated": a.IsAuthenticated(r),
	})
}

func (a *ConsoleAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.Enabled() {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": false})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if !a.VerifyPassword(req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if ok := a.StartSession(w, r); !ok {
		http.Error(w, "create session failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": true})
}

func (a *ConsoleAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.EndSession(w, r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func newAuthToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := strings.TrimSpace(strings.ToLower(r.Header.Get("X-Forwarded-Proto")))
	return proto == "https"
}
