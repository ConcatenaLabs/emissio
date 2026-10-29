package main

// Email: verifying an account's address, and resetting a forgotten
// password. Mail goes out through any SMTP submission service named in the
// configuration (EMISSIO_SMTP_*). Without one, both features are off: the
// verification gate is lifted and a forgotten password is reset through an
// admin, who makes a one-time reset link from the account's admin page.
//
// A link carries a random token; only its hash is stored, it works once,
// and it expires (a day for verification, an hour for a reset).

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tokenSchema = `
CREATE TABLE IF NOT EXISTS email_tokens (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	purpose TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS email_tokens_user ON email_tokens(user_id, purpose);
`

const (
	verifyTTL = 24 * time.Hour
	resetTTL  = time.Hour
)

// Mailer sends one plain-text message.
type Mailer interface {
	Send(to, subject, body string) error
}

type smtpMailer struct {
	host, port, user, pass, from string
	roots                        *x509.CertPool // nil: the system's
}

func (m *smtpMailer) Send(to, subject, body string) error {
	addr := net.JoinHostPort(m.host, m.port)
	var conn net.Conn
	var err error
	d := &net.Dialer{Timeout: 15 * time.Second}
	if m.port == "465" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: m.host, RootCAs: m.roots})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if m.port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the mail server offers no STARTTLS; refusing to send credentials in the clear")
		}
		if err := c.StartTLS(&tls.Config{ServerName: m.host, RootCAs: m.roots}); err != nil {
			return err
		}
	}
	if m.user != "" {
		if err := c.Auth(smtp.PlainAuth("", m.user, m.pass, m.host)); err != nil {
			return err
		}
	}
	from := m.from
	if a, err := parseAddr(from); err == nil {
		from = a
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + m.from + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) +
		"\r\nMessage-ID: <" + randomHex(16) + "@" + domainOf(from) + ">" +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func parseAddr(s string) (string, error) {
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.LastIndex(s, ">"); j > i {
			return s[i+1 : j], nil
		}
	}
	if strings.Contains(s, "@") {
		return strings.TrimSpace(s), nil
	}
	return "", errors.New("no address")
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}

// memMailer keeps messages in memory: the tests read links from it.
type memMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct{ To, Subject, Body string }

func (m *memMailer) Send(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, body})
	return nil
}

func (m *memMailer) last() sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return sentMail{}
	}
	return m.sent[len(m.sent)-1]
}

func newMailer(cfg Config) Mailer {
	if cfg.SMTPHost == "" || cfg.MailFrom == "" {
		return nil
	}
	return &smtpMailer{host: cfg.SMTPHost, port: cfg.SMTPPort, user: cfg.SMTPUser, pass: cfg.SMTPPass, from: cfg.MailFrom}
}

// mailOn says whether this instance sends mail, and so whether accounts
// must confirm their address before they earn.
func (a *App) mailOn() bool { return a.mailer != nil }

func (a *App) newEmailToken(userID int64, purpose string, ttl time.Duration) (string, error) {
	tok := randomHex(24)
	// Only the newest link of a purpose works.
	if _, err := a.db.Exec("UPDATE email_tokens SET used_at = ? WHERE user_id = ? AND purpose = ? AND used_at = 0", now(), userID, purpose); err != nil {
		return "", err
	}
	_, err := a.db.Exec("INSERT INTO email_tokens (token_hash, user_id, purpose, expires_at, created_at) VALUES (?,?,?,?,?)",
		hashToken(tok), userID, purpose, time.Now().Add(ttl).Unix(), now())
	return tok, err
}

// useEmailToken consumes a token of the purpose, returning its account.
func (a *App) useEmailToken(tok, purpose string, consume bool) (int64, error) {
	var userID, expires, used int64
	err := a.db.QueryRow("SELECT user_id, expires_at, used_at FROM email_tokens WHERE token_hash = ? AND purpose = ?",
		hashToken(tok), purpose).Scan(&userID, &expires, &used)
	if err != nil || used != 0 || expires < now() {
		return 0, errors.New("This link has expired or has already been used.")
	}
	if consume {
		res, err := a.db.Exec("UPDATE email_tokens SET used_at = ? WHERE token_hash = ? AND used_at = 0", now(), hashToken(tok))
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return 0, errors.New("This link has expired or has already been used.")
		}
	}
	return userID, nil
}

// publicURL is the address links in mail point at.
func (a *App) publicURL(r *http.Request) string {
	if a.cfg.PublicURL != "" {
		return strings.TrimRight(a.cfg.PublicURL, "/")
	}
	scheme := "http"
	if a.cfg.SecureCookies || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + a.cfg.BasePath
}

// mailAllowed bounds the mail one account can be sent (three an hour) and
// that one network address can cause (ten an hour).
func (a *App) mailAllowed(key string) bool {
	l := a.mailLimit
	if strings.HasPrefix(key, "ip|") {
		l = a.mailIPLimit
	}
	ok, _ := l.allow(key, time.Now())
	return ok
}

func (a *App) sendVerification(r *http.Request, u *User) error {
	tok, err := a.newEmailToken(u.ID, "verify", verifyTTL)
	if err != nil {
		return err
	}
	link := a.publicURL(r) + "/verify-email?t=" + url.QueryEscape(tok)
	body := "Confirm the email address of your Sequentia Emissio account by opening this link:\n\n" + link +
		"\n\nThe link works once, for 24 hours. If you did not create an account, ignore this message.\n"
	return a.mailer.Send(u.Email, "Confirm your Emissio account", body)
}

func (a *App) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	userID, err := a.useEmailToken(r.URL.Query().Get("t"), "verify", true)
	if err != nil {
		a.redirect(w, r, "/account", "", err.Error()+" Send a new one from your account page.")
		return
	}
	if _, err := a.db.Exec("UPDATE users SET email_verified_at = ? WHERE id = ? AND email_verified_at = 0", now(), userID); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/account", "Email confirmed. You can submit evidence and reports.", "")
}

func (a *App) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	if !a.mailOn() || user.EmailVerifiedAt != 0 {
		a.redirect(w, r, "/account", "", "")
		return
	}
	if !a.mailAllowed("verify|"+strconv.FormatInt(user.ID, 10)) || !a.mailAllowed("ip|"+clientIP(r)) {
		a.redirect(w, r, "/account", "", "A confirmation email was sent a moment ago. Check your inbox and spam folder, or try again in an hour.")
		return
	}
	if err := a.sendVerification(r, user); err != nil {
		log.Printf("mail: verification to user %d: %v", user.ID, err)
		a.redirect(w, r, "/account", "", "The email could not be sent just now. Try again in a few minutes.")
		return
	}
	a.redirect(w, r, "/account", "Sent. Open the link in the email to confirm your address.", "")
}

// gate holds back an account that has not confirmed its email from
// earning, when this instance sends mail. It answers the request itself
// when it refuses.
func (a *App) gate(w http.ResponseWriter, r *http.Request, user *User, back string) bool {
	if !a.mailOn() || user.EmailVerifiedAt != 0 || user.IsAdmin {
		return true
	}
	a.redirect(w, r, back, "", "Confirm your email address first: open the link we sent to "+user.Email+". You can send a new one from your account page.")
	return false
}

// ---------- password reset ----------

func (a *App) handleResetForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "reset", "Reset your password", map[string]any{"MailOn": a.mailOn()})
}

const resetSent = "If an account uses that address, a reset link is on its way. It works once, for an hour."

func (a *App) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	if !a.authLimit.allow("reset|" + clientIP(r)) {
		a.redirect(w, r, "/reset", "", "Too many attempts. Wait a minute and try again.")
		return
	}
	r.ParseForm()
	if !a.mailOn() {
		a.redirect(w, r, "/reset", "", "")
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	// The answer is the same whether or not the account exists.
	u, err := getUserByEmail(a.db, email)
	if err == nil && a.mailAllowed("reset|"+strconv.FormatInt(u.ID, 10)) && a.mailAllowed("ip|"+clientIP(r)) {
		tok, err := a.newEmailToken(u.ID, "reset", resetTTL)
		if err == nil {
			link := a.publicURL(r) + "/reset/confirm?t=" + url.QueryEscape(tok)
			body := "Someone asked to reset the password of your Sequentia Emissio account. To choose a new password, open:\n\n" + link +
				"\n\nThe link works once, for one hour. If it was not you, ignore this message; your password stays as it is.\n"
			if err := a.mailer.Send(u.Email, "Reset your Emissio password", body); err != nil {
				log.Printf("mail: reset to user %d: %v", u.ID, err)
			}
		}
	}
	a.redirect(w, r, "/reset", resetSent, "")
}

func (a *App) handleResetConfirmForm(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("t")
	if _, err := a.useEmailToken(tok, "reset", false); err != nil {
		a.redirect(w, r, "/reset", "", err.Error()+" Ask for a new one.")
		return
	}
	a.render(w, r, "reset_confirm", "Choose a new password", map[string]any{"Token": tok})
}

func (a *App) handleResetConfirm(w http.ResponseWriter, r *http.Request) {
	if !a.authLimit.allow("resetc|" + clientIP(r)) {
		a.redirect(w, r, "/reset", "", "Too many attempts. Wait a minute and try again.")
		return
	}
	r.ParseForm()
	tok := r.FormValue("t")
	pass := r.FormValue("password")
	if len(pass) < 10 {
		a.redirect(w, r, "/reset/confirm?t="+url.QueryEscape(tok), "", "Use a password of at least 10 characters.")
		return
	}
	userID, err := a.useEmailToken(tok, "reset", true)
	if err != nil {
		a.redirect(w, r, "/reset", "", err.Error()+" Ask for a new one.")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer tx.Rollback()
	// A reset link reached the inbox, which confirms the address too, and
	// every session signed in with the old password ends.
	if _, err := tx.Exec("UPDATE users SET pass_hash = ?, email_verified_at = CASE WHEN email_verified_at = 0 THEN ? ELSE email_verified_at END WHERE id = ?",
		hashPassword(pass), now(), userID); err != nil {
		a.serverError(w, err)
		return
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
		a.serverError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/login", "Password changed. Sign in with the new one.", "")
}

// handleAdminResetLink makes a one-time reset link for an account, for an
// admin to pass on by hand. It is shown once, on the page this answers
// with, and never put in a URL.
func (a *App) handleAdminResetLink(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := getUserByID(a.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.serverError(w, err)
		return
	}
	tok, err := a.newEmailToken(u.ID, "reset", 24*time.Hour)
	if err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.render(w, r, "admin_resetlink", "Reset link", map[string]any{
		"User": u, "Link": a.publicURL(r) + "/reset/confirm?t=" + url.QueryEscape(tok),
	})
}
