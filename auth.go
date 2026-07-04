package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters per RFC 9106 low-memory recommendation.
const (
	argonTime    = 3
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
)

func hashPassword(password string) string {
	salt := randomBytes(16)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s", argonTime, argonMemory, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func verifyPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false
	}
	var t, m, p int
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &t, &m, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randomHex(n int) string { return hex.EncodeToString(randomBytes(n)) }

const sessionCookie = "emissio_session"
const sessionTTL = 30 * 24 * time.Hour

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (a *App) createSession(w http.ResponseWriter, userID int64) error {
	token := randomHex(32)
	_, err := a.db.Exec("INSERT INTO sessions (token_hash, user_id, expires_at, created_at) VALUES (?,?,?,?)",
		hashToken(token), userID, time.Now().Add(sessionTTL).Unix(), now())
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     a.cookiePath(),
		HttpOnly: true,
		Secure:   a.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

func (a *App) cookiePath() string {
	if a.cfg.BasePath == "" {
		return "/"
	}
	return a.cfg.BasePath
}

// currentUser resolves the session cookie. Returns (user, sessionToken, nil)
// or (nil, "", nil) when not signed in.
func (a *App) currentUser(r *http.Request) (*User, string, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, "", nil
	}
	var userID, expires int64
	err = a.db.QueryRow("SELECT user_id, expires_at FROM sessions WHERE token_hash = ?", hashToken(c.Value)).
		Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if expires < now() {
		a.db.Exec("DELETE FROM sessions WHERE token_hash = ?", hashToken(c.Value))
		return nil, "", nil
	}
	u, err := getUserByID(a.db, userID)
	if err != nil {
		return nil, "", err
	}
	return u, c.Value, nil
}

func (a *App) destroySession(w http.ResponseWriter, token string) {
	if token != "" {
		a.db.Exec("DELETE FROM sessions WHERE token_hash = ?", hashToken(token))
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: a.cookiePath(),
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// csrfToken derives a per-session CSRF token from the (secret) session token,
// so nothing extra needs storing. Anonymous pages get no token.
func csrfToken(sessionToken string) string {
	if sessionToken == "" {
		return ""
	}
	h := sha256.Sum256([]byte("emissio-csrf|" + sessionToken))
	return hex.EncodeToString(h[:16])
}

func checkCSRF(r *http.Request, sessionToken string) bool {
	want := csrfToken(sessionToken)
	got := r.FormValue("csrf")
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// rateLimiter is a small fixed-window per-IP limiter for auth endpoints.
type rateLimiter struct {
	mu     sync.Mutex
	window map[string]*rateWindow
	limit  int
	per    time.Duration
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, per time.Duration) *rateLimiter {
	return &rateLimiter{window: map[string]*rateWindow{}, limit: limit, per: per}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	nowT := time.Now()
	w := rl.window[key]
	if w == nil || nowT.Sub(w.start) > rl.per {
		if len(rl.window) > 100000 { // bound memory
			rl.window = map[string]*rateWindow{}
		}
		rl.window[key] = &rateWindow{start: nowT, count: 1}
		return true
	}
	w.count++
	return w.count <= rl.limit
}

func clientIP(r *http.Request) string {
	// Behind Caddy on the same host; trust the last X-Forwarded-For hop.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
