package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var linkRe = regexp.MustCompile(`https?://\S+`)

func mailServer(t *testing.T) (*App, string, *http.Client, *memMailer) {
	app, srv, client := newTestServer(t)
	mm := &memMailer{}
	app.mailer = mm
	return app, srv.URL, client, mm
}

func pathOf(t *testing.T, link, base string) string {
	t.Helper()
	if !strings.HasPrefix(link, base) {
		t.Fatalf("link %q is not under %q", link, base)
	}
	return strings.TrimPrefix(link, base)
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestEmailVerificationGate(t *testing.T) {
	app, base, client, mm := mailServer(t)
	register(t, client, base, "new@example.com")
	m := mm.last()
	if m.To != "new@example.com" || !strings.Contains(m.Body, "/verify-email?t=") {
		t.Fatalf("no verification mail: %+v", m)
	}
	acct := get(t, client, base+"/account")
	if !strings.Contains(acct, "Confirm your email address to start earning") {
		t.Fatal("account page does not ask for confirmation")
	}
	csrf := csrfOf(t, client, base+"/account")
	// Unconfirmed: submissions are held back.
	client.PostForm(base+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {strings.Repeat("ab", 32)}})
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&n)
	if n != 0 {
		t.Fatal("an unconfirmed account submitted evidence")
	}
	// Resend works, within limits.
	for i := 0; i < 4; i++ {
		client.PostForm(base+"/account/verify-email", url.Values{"csrf": {csrf}})
	}
	if len(mm.sent) != 4 { // the registration mail plus three resends an hour
		t.Fatalf("%d mails, want 4", len(mm.sent))
	}
	link := linkRe.FindString(mm.last().Body)
	first := linkRe.FindString(mm.sent[0].Body)
	// An older link no longer works once a newer one was sent.
	get(t, client, base+pathOf(t, first, base))
	var verified int64
	app.db.QueryRow("SELECT email_verified_at FROM users WHERE email = 'new@example.com'").Scan(&verified)
	if verified != 0 {
		t.Fatal("a superseded link confirmed the address")
	}
	get(t, client, base+pathOf(t, link, base))
	app.db.QueryRow("SELECT email_verified_at FROM users WHERE email = 'new@example.com'").Scan(&verified)
	if verified == 0 {
		t.Fatal("the newest link did not confirm the address")
	}
	client.PostForm(base+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {strings.Repeat("ab", 32)}})
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&n)
	if n != 1 {
		t.Fatal("a confirmed account cannot submit")
	}
}

func TestPasswordReset(t *testing.T) {
	app, base, client, mm := mailServer(t)
	register(t, client, base, "forgot@example.com")
	before := len(mm.sent)

	anon := &http.Client{Jar: newJar()}
	// An unknown address gets the same answer and no mail.
	resp, _ := anon.PostForm(base+"/reset", url.Values{"email": {"nobody@example.com"}})
	resp.Body.Close()
	unknownURL := resp.Request.URL.String()
	resp, _ = anon.PostForm(base+"/reset", url.Values{"email": {"forgot@example.com"}})
	resp.Body.Close()
	if resp.Request.URL.String() != unknownURL {
		t.Fatalf("answers differ: %q vs %q", resp.Request.URL, unknownURL)
	}
	if len(mm.sent) != before+1 || mm.last().To != "forgot@example.com" {
		t.Fatalf("reset mail: %d sent, last %+v", len(mm.sent)-before, mm.last())
	}
	link := pathOf(t, linkRe.FindString(mm.last().Body), base)
	tok := strings.TrimPrefix(link, "/reset/confirm?t=")
	if page := get(t, anon, base+link); !strings.Contains(page, "Choose a new password") {
		t.Fatal("reset form missing")
	}
	// Too short is refused and the link still works.
	anon.PostForm(base+"/reset/confirm", url.Values{"t": {tok}, "password": {"short"}})
	anon.PostForm(base+"/reset/confirm", url.Values{"t": {tok}, "password": {"a-brand-new-password"}})
	// The old session is gone.
	resp, _ = client.Get(base + "/account")
	resp.Body.Close()
	if !strings.HasSuffix(resp.Request.URL.Path, "/login") {
		t.Fatalf("old session survived the reset: landed on %s", resp.Request.URL.Path)
	}
	// The new password works, the old one does not, and the link is spent.
	u, _ := getUserByEmail(app.db, "forgot@example.com")
	if !verifyPassword(u.PassHash, "a-brand-new-password") || verifyPassword(u.PassHash, "a-long-password") {
		t.Fatal("password not changed")
	}
	if u.EmailVerifiedAt == 0 {
		t.Fatal("a used reset link should confirm the address")
	}
	anon.PostForm(base+"/reset/confirm", url.Values{"t": {tok}, "password": {"a-third-password!"}})
	u, _ = getUserByEmail(app.db, "forgot@example.com")
	if !verifyPassword(u.PassHash, "a-brand-new-password") {
		t.Fatal("a spent reset link worked twice")
	}

	// An admin makes a reset link by hand; it is on the page, not in a URL.
	app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'forgot@example.com'")
	admin := &http.Client{Jar: newJar()}
	admin.PostForm(base+"/login", url.Values{"email": {"forgot@example.com"}, "password": {"a-brand-new-password"}})
	csrf := csrfOf(t, admin, base+"/admin/users")
	resp, err := admin.PostForm(base+"/admin/users/1/reset-link", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(resp)
	if resp.Request.Method != "POST" || !strings.Contains(body, "/reset/confirm?t=") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("admin reset link not shown once on the page")
	}
}

func TestResetWithoutMail(t *testing.T) {
	_, srv, client := newTestServer(t)
	if page := get(t, client, srv.URL+"/reset"); !strings.Contains(page, "An admin sends you a reset link") {
		t.Fatal("reset page without mail")
	}
	// Without mail, nothing gates earning.
	register(t, client, srv.URL, "nomail@example.com")
	if strings.Contains(get(t, client, srv.URL+"/account"), "Confirm your email") {
		t.Fatal("confirmation asked for without mail")
	}
}

// TestSMTPMailer speaks SMTP with STARTTLS and PLAIN auth to a stand-in
// server, the way a submission service on port 587 is reached.
func TestSMTPMailer(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		var log strings.Builder
		rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
		say := func(s string) { rw.WriteString(s + "\r\n"); rw.Flush() }
		say("220 stub")
		inData := false
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				got <- log.String()
				return
			}
			log.WriteString(line)
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case inData:
				if strings.TrimSpace(line) == "." {
					inData = false
					say("250 queued")
				}
			case strings.HasPrefix(cmd, "EHLO"):
				if _, ok := c.(*tls.Conn); ok {
					say("250-stub\r\n250 AUTH PLAIN")
				} else {
					say("250-stub\r\n250 STARTTLS")
				}
			case cmd == "STARTTLS":
				say("220 go ahead")
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				if tc.Handshake() != nil {
					got <- "handshake failed"
					return
				}
				c = tc
				rw = bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
				log.WriteString("[tls]\n")
			case strings.HasPrefix(cmd, "AUTH PLAIN"):
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				inData = true
				say("354 go")
			case cmd == "QUIT":
				say("221 bye")
				got <- log.String()
				c.Close()
				return
			}
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	m := &smtpMailer{host: "127.0.0.1", port: port, user: "u", pass: "p", from: "Sequentia Emissio <emissio@example.com>"}
	// The stand-in's certificate is self-signed; trust it for this test.
	pool := x509.NewCertPool()
	c0, _ := x509.ParseCertificate(der)
	pool.AddCert(c0)
	m.roots = pool
	if err := m.Send("to@example.com", "Subject here", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	log := <-got
	tlsAt := strings.Index(log, "[tls]")
	authAt := strings.Index(log, "AUTH PLAIN")
	if tlsAt < 0 || authAt < tlsAt {
		t.Fatalf("credentials sent before TLS:\n%s", log)
	}
	for _, want := range []string{"MAIL FROM:<emissio@example.com>", "RCPT TO:<to@example.com>", "Subject: Subject here", "line one\r\nline two"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in:\n%s", want, log)
		}
	}
}
