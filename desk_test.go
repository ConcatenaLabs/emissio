package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// from posts a form as a client at a network address.
func from(t *testing.T, c *http.Client, ip, u string, v url.Values) string {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	q, _ := url.ParseQuery(resp.Request.URL.RawQuery)
	return q.Get("e") + q.Get("m")
}

func TestRegistrationLimits(t *testing.T) {
	app, srv, _ := newTestServer(t)
	reg := func(c *http.Client, ip, email string) string {
		return from(t, c, ip, srv.URL+"/register", url.Values{"email": {email}, "password": {"a-long-password"}})
	}
	// Three accounts a day from one network address, each from its own browser.
	for i, e := range []string{"a@x.io", "b@x.io", "c@x.io"} {
		if m := reg(&http.Client{Jar: newJar()}, "10.1.1.1", e); strings.Contains(m, "Several accounts") {
			t.Fatalf("account %d refused: %s", i, m)
		}
	}
	if m := reg(&http.Client{Jar: newJar()}, "10.1.1.1", "d@x.io"); !strings.Contains(m, "Several accounts were created from your network today") {
		t.Fatalf("fourth from one network: %q", m)
	}
	// Three accounts ever from one browser, whatever the network.
	same := &http.Client{Jar: newJar()}
	for i, ip := range []string{"10.2.0.1", "10.2.0.2", "10.2.0.3"} {
		if m := reg(same, ip, "dev"+string(rune('a'+i))+"@x.io"); strings.Contains(m, "browser") {
			t.Fatalf("browser account %d refused: %s", i, m)
		}
		same.Jar.SetCookies(mustURL(srv.URL), []*http.Cookie{{Name: sessionCookie, Value: "", MaxAge: -1}})
	}
	if m := reg(same, "10.2.0.4", "devd@x.io"); !strings.Contains(m, "This browser has already created several accounts") {
		t.Fatalf("fourth from one browser: %q", m)
	}
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n != 6 {
		t.Fatalf("%d accounts, want 6", n)
	}
	// The three from one browser are linked to each other.
	if s := signalsOf(app.db, 4); s.SharedDevices != 2 {
		t.Fatalf("browser signal: %+v", s)
	}
	// The three from one network share it.
	if s := signalsOf(app.db, 1); s.SharedIPs != 2 {
		t.Fatalf("network signal: %+v", s)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestAdminDesk(t *testing.T) {
	app, srv, admin := newTestServer(t)
	register(t, admin, srv.URL, "admin@x.io")
	app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
	csrf := csrfOf(t, admin, srv.URL+"/admin/queue")
	tester := &http.Client{Jar: newJar()}
	register(t, tester, srv.URL, "tester@x.io")
	tcsrf := csrfOf(t, tester, srv.URL+"/account")
	// Work in every queue: evidence (no explorer, so it waits), a bug, a security report, a verification.
	tester.PostForm(srv.URL+"/tasks/build-from-source/submit", url.Values{"csrf": {tcsrf}, "notes": {"Debian 12"}})
	postBug(t, tester, srv.URL, tcsrf, map[string]string{"surface": "compages", "title": "Bridge shows NaN", "steps": "open", "actual": "NaN"}, nil)
	tester.PostForm(srv.URL+"/security/report", url.Values{"csrf": {tcsrf}, "title": {"RPC leak"}, "severity": {"low"}, "body": {"x"}})
	tester.PostForm(srv.URL+"/account/verify", url.Values{"csrf": {tcsrf}, "platform": {"telegram"}, "handle": {"someone"}})
	q := get(t, admin, srv.URL+"/admin/queue")
	for _, want := range []string{"Build Sequentia Core from source", "Bridge shows NaN", "RPC leak", "tester@x.io", "task · 1", "bug · 1", "security · 1", "verification · 1"} {
		if !strings.Contains(q, want) {
			t.Errorf("queue lacks %q", want)
		}
	}
	if q := get(t, admin, srv.URL+"/admin/queue?kind=bug"); strings.Contains(q, "RPC leak") || !strings.Contains(q, "Bridge shows NaN") {
		t.Error("queue filter")
	}
	// Approve the evidence from the queue, then revoke it: the reward goes and comes back visibly.
	admin.PostForm(srv.URL+"/admin/submissions/1/review", url.Values{"csrf": {csrf}, "action": {"approve"}, "back": {"/admin/queue"}})
	if m := from(t, admin, "127.0.0.1", srv.URL+"/admin/submissions/1/revoke", url.Values{"csrf": {csrf}, "note": {"short"}}); !strings.Contains(m, "Give the reason") {
		t.Fatalf("revoke without a reason: %q", m)
	}
	admin.PostForm(srv.URL+"/admin/submissions/1/revoke", url.Values{"csrf": {csrf}, "note": {"the peer was another account's node"}})
	if m := from(t, admin, "127.0.0.1", srv.URL+"/admin/submissions/1/revoke", url.Values{"csrf": {csrf}, "note": {"the peer was another account's node"}}); !strings.Contains(m, "not approved") {
		t.Fatalf("second revoke: %q", m)
	}
	var bal int64
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger WHERE user_id = 2").Scan(&bal)
	if bal != 0 {
		t.Fatalf("balance after revoke %d", bal)
	}
	if acct := get(t, tester, srv.URL+"/account"); !strings.Contains(acct, "Revoked: Build Sequentia Core from source (the peer was another account&#39;s node)") {
		t.Fatal("the tester does not see the revocation and its reason")
	}
	// Adjustments need a reason, and are logged under the admin.
	if m := from(t, admin, "127.0.0.1", srv.URL+"/admin/adjust", url.Values{"csrf": {csrf}, "user": {"2"}, "amount": {"40"}, "note": {"x"}}); !strings.Contains(m, "the reason") {
		t.Fatalf("adjustment without reason: %q", m)
	}
	admin.PostForm(srv.URL+"/admin/adjust", url.Values{"csrf": {csrf}, "user": {"2"}, "amount": {"40"}, "note": {"=HYPERLINK(\"x\") found a broken guide"}})
	// Hold needs a reason; held accounts show everywhere.
	if m := from(t, admin, "127.0.0.1", srv.URL+"/admin/users/2/hold", url.Values{"csrf": {csrf}, "hold": {"1"}}); !strings.Contains(m, "Say why") {
		t.Fatalf("hold without reason: %q", m)
	}
	admin.PostForm(srv.URL+"/admin/users/2/hold", url.Values{"csrf": {csrf}, "hold": {"1"}, "note": {"shares a funding address with #7"}})
	u, _ := getUserByID(app.db, 2)
	if !u.Held {
		t.Fatal("not held")
	}
	page := get(t, admin, srv.URL+"/admin/users/2")
	for _, want := range []string{"held", "Release", "revoke", "adjust", "admin@x.io", "shares a funding address with #7", "Bridge shows NaN"} {
		if !strings.Contains(page, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	// Search finds the account by code and by network address.
	if s := get(t, admin, srv.URL+"/admin/users?q="+u.ClaimCode); !strings.Contains(s, "tester@x.io") || strings.Contains(s, "admin@x.io</a>") {
		t.Error("search by code")
	}
	if s := get(t, admin, srv.URL+"/admin/users?q=127.0.0.1"); !strings.Contains(s, "tester@x.io") {
		t.Error("search by network address")
	}
	// Exports: every one answers, and a cell a spreadsheet would run is text.
	for _, name := range []string{"users.csv", "submissions.csv", "ledger.csv", "bugs.csv"} {
		body := get(t, admin, srv.URL+"/admin/export/"+name)
		if !strings.Contains(body, "tester@x.io") {
			t.Errorf("%s lacks the tester", name)
		}
	}
	if l := get(t, admin, srv.URL+"/admin/export/ledger.csv"); !strings.Contains(l, `'=HYPERLINK`) {
		t.Error("formula cell not neutralised")
	}
	// None of it is reachable by a tester.
	resp, _ := tester.Get(srv.URL + "/admin/export/users.csv")
	if resp.StatusCode != 404 {
		t.Fatalf("tester read an export: %d", resp.StatusCode)
	}
	resp.Body.Close()
}
