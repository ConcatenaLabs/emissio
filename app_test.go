package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*App, *httptest.Server, *http.Client) {
	t.Helper()
	db := mustOpenDB(filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { db.Close() })
	seedDB(db)
	app := NewApp(Config{BasePath: "", EsploraURL: ""}, db)
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	return app, srv, client
}

func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func csrfOf(t *testing.T, c *http.Client, pageURL string) string {
	t.Helper()
	body := get(t, c, pageURL)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf token on %s", pageURL)
	}
	return m[1]
}

func TestUserFlow(t *testing.T) {
	app, srv, client := newTestServer(t)

	// Public pages render.
	for _, p := range []string{"/", "/tasks", "/tasks/first-transaction", "/competitions",
		"/competitions/artwork-2026", "/security", "/guide", "/rules", "/login", "/register"} {
		body := get(t, client, srv.URL+p)
		if strings.Contains(body, "Something went wrong") {
			t.Fatalf("page %s errored", p)
		}
		// A render error truncates the body mid-template.
		if !strings.Contains(body, "</html>") {
			t.Fatalf("page %s truncated (template error)", p)
		}
	}

	// Register.
	resp, err := client.PostForm(srv.URL+"/register", url.Values{
		"email": {"tester@example.com"}, "password": {"a-long-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	acct := get(t, client, srv.URL+"/account")
	if !strings.Contains(acct, "Claim code") {
		t.Fatal("account page missing claim code after registration")
	}

	// Save a valid mainnet address; then a testnet one, which must be refused.
	csrf := csrfOf(t, client, srv.URL+"/account")
	client.PostForm(srv.URL+"/account/address", url.Values{
		"csrf": {csrf}, "address": {"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
	})
	acct = get(t, client, srv.URL+"/account")
	if !strings.Contains(acct, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4") {
		t.Fatal("valid address not saved")
	}
	client.PostForm(srv.URL+"/account/address", url.Values{
		"csrf": {csrf}, "address": {"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"},
	})
	acct = get(t, client, srv.URL+"/account")
	if !strings.Contains(acct, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4") {
		t.Fatal("testnet address overwrote the valid one")
	}

	// Submit task evidence (no explorer configured: chain note is advisory).
	txid := strings.Repeat("ab", 32)
	client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{
		"csrf": {csrf}, "txid": {txid}, "notes": {"claim code test"},
	})
	taskPage := get(t, client, srv.URL+"/tasks/first-transaction")
	if !strings.Contains(taskPage, "pending") {
		t.Fatal("submission not recorded as pending")
	}

	// A second submission for the same task must be refused.
	client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{
		"csrf": {csrf}, "txid": {txid},
	})
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 submission, got %d", n)
	}

	// CSRF: a POST without the token must not create anything.
	client.PostForm(srv.URL+"/security/report", url.Values{
		"title": {"x"}, "severity": {"low"}, "body": {"y"},
	})
	app.db.QueryRow("SELECT COUNT(*) FROM reports").Scan(&n)
	if n != 0 {
		t.Fatal("report created without csrf token")
	}

	// Admin approves the submission and the ledger credits once.
	app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'tester@example.com'")
	adminPage := get(t, client, srv.URL+"/admin/submissions")
	if !strings.Contains(adminPage, txid) {
		t.Fatal("pending submission not on admin queue")
	}
	client.PostForm(srv.URL+"/admin/submissions/1/review", url.Values{
		"csrf": {csrf}, "action": {"approve"},
	})
	var bal int64
	app.db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 10 {
		t.Fatalf("expected balance 10 after approval, got %d", bal)
	}
	// Second approval attempt must fail (already reviewed).
	client.PostForm(srv.URL+"/admin/submissions/1/review", url.Values{
		"csrf": {csrf}, "action": {"approve"},
	})
	app.db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 10 {
		t.Fatalf("double credit: balance %d", bal)
	}

	// Competition entry.
	client.PostForm(srv.URL+"/competitions/artwork-2026/enter", url.Values{
		"csrf": {csrf}, "url": {"https://example.com/art.png"}, "notes": {"my entry"},
	})
	app.db.QueryRow("SELECT COUNT(*) FROM entries").Scan(&n)
	if n != 1 {
		t.Fatal("entry not saved")
	}

	// Security report with csrf.
	client.PostForm(srv.URL+"/security/report", url.Values{
		"csrf": {csrf}, "title": {"Test bug"}, "severity": {"medium"}, "body": {"details"},
	})
	app.db.QueryRow("SELECT COUNT(*) FROM reports").Scan(&n)
	if n != 1 {
		t.Fatal("report not saved")
	}

	// Every admin page renders completely.
	for _, p := range []string{"/admin", "/admin/submissions", "/admin/tasks", "/admin/competitions",
		"/admin/competitions/artwork-2026", "/admin/reports", "/admin/reports/1", "/admin/users"} {
		body := get(t, client, srv.URL+p)
		if !strings.Contains(body, "</html>") {
			t.Fatalf("admin page %s truncated (template error)", p)
		}
	}

	// Accept the security report with an award; ledger credits once.
	client.PostForm(srv.URL+"/admin/reports/1/review", url.Values{
		"csrf": {csrf}, "status": {"accepted"}, "award": {"1000"}, "note": {"confirmed"},
	})
	app.db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 1010 {
		t.Fatalf("expected balance 1010 after report award, got %d", bal)
	}

	// Award the competition entry a place.
	client.PostForm(srv.URL+"/admin/competitions/artwork-2026/award", url.Values{
		"csrf": {csrf}, "entry": {"1"}, "place": {"1"},
	})
	app.db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 3010 {
		t.Fatalf("expected balance 3010 after prize, got %d", bal)
	}

	// Allocation export includes the user.
	csv := get(t, client, srv.URL+"/admin/allocations.csv")
	if !strings.Contains(csv, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4") || !strings.Contains(csv, ",3010") {
		t.Fatalf("allocation csv wrong:\n%s", csv)
	}
}

func TestBasePath(t *testing.T) {
	db := mustOpenDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	seedDB(db)
	app := NewApp(Config{BasePath: "/emissio"}, db)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{}
	// Both prefixed (direct) and unprefixed (proxy-stripped) paths must work.
	for _, p := range []string{"/emissio/tasks", "/tasks", "/emissio/", "/"} {
		body := get(t, client, srv.URL+p)
		if !strings.Contains(body, "</html>") {
			t.Fatalf("path %s truncated", p)
		}
		if !strings.Contains(body, `href="/emissio/static/style.css"`) {
			t.Fatalf("path %s: links not prefixed with base path", p)
		}
	}
}

func TestAnonymousCannotSubmit(t *testing.T) {
	app, srv, client := newTestServer(t)
	client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{
		"txid": {strings.Repeat("cd", 32)},
	})
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&n)
	if n != 0 {
		t.Fatal("anonymous submission accepted")
	}
	// Admin pages are hidden from anonymous users.
	resp, err := client.Get(srv.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("anonymous /admin: got %d, want 404", resp.StatusCode)
	}
}
