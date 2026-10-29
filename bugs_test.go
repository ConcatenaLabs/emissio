package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// pngBytes is a minimal valid PNG header followed by padding.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 64)...)

func postBug(t *testing.T, c *http.Client, base, csrf string, fields map[string]string, file []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", csrf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	if file != nil {
		fw, _ := mw.CreateFormFile("screenshot", "shot.png")
		fw.Write(file)
	}
	mw.Close()
	resp, err := c.Post(base+"/bugs", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func register(t *testing.T, c *http.Client, base, email string) {
	t.Helper()
	resp, err := c.PostForm(base+"/register", url.Values{"email": {email}, "password": {"a-long-password"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestBugReports(t *testing.T) {
	app, srv, client := newTestServer(t)
	if body := get(t, client, srv.URL+"/bugs"); !strings.Contains(body, "</html>") || !strings.Contains(body, "Sign in") {
		t.Fatal("bug page broken for anonymous visitors")
	}
	register(t, client, srv.URL, "bugs@example.com")
	csrf := csrfOf(t, client, srv.URL+"/bugs")
	ok := map[string]string{"surface": "explorer", "title": "Anchor link 404s", "steps": "Open a block page, click the anchor", "expected": "testnet4 block", "actual": "a 404 page"}

	// Without the token nothing is stored.
	postBug(t, client, srv.URL, "", ok, nil)
	// A screenshot that is not an image is refused.
	postBug(t, client, srv.URL, csrf, ok, []byte("#!/bin/sh\necho hi"))
	// An unknown surface is refused.
	bad := map[string]string{}
	for k, v := range ok {
		bad[k] = v
	}
	bad["surface"] = "secret-product"
	postBug(t, client, srv.URL, csrf, bad, nil)
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM bugs").Scan(&n)
	if n != 0 {
		t.Fatalf("%d bugs stored from refused submissions", n)
	}
	postBug(t, client, srv.URL, csrf, ok, pngBytes)
	postBug(t, client, srv.URL, csrf, ok, nil) // the same bug again
	app.db.QueryRow("SELECT COUNT(*) FROM bugs").Scan(&n)
	if n != 2 {
		t.Fatalf("want 2 bugs, got %d", n)
	}
	page := get(t, client, srv.URL+"/bugs")
	if !strings.Contains(page, "Anchor link 404s") || !strings.Contains(page, "Block explorer") {
		t.Fatal("reporter does not see the report")
	}
	// The screenshot is served to the reporter, as an image.
	resp, _ := client.Get(srv.URL + "/bugs/1/screenshot")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("screenshot: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp.Body.Close()
	// ...and to nobody else.
	other := &http.Client{Jar: newJar()}
	register(t, other, srv.URL, "other@example.com")
	resp, _ = other.Get(srv.URL + "/bugs/1/screenshot")
	if resp.StatusCode != 404 {
		t.Fatalf("another account read the screenshot: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The admin sees the identical text flagged, accepts the first and merges the second.
	app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'bugs@example.com'")
	if d := get(t, client, srv.URL+"/admin/bugs/2"); !strings.Contains(d, "The same text as") {
		t.Fatal("identical report not flagged")
	}
	client.PostForm(srv.URL+"/admin/bugs/1/review", url.Values{"csrf": {csrf}, "status": {"accepted"}})
	var status string
	app.db.QueryRow("SELECT status FROM bugs WHERE id = 1").Scan(&status)
	if status != "new" {
		t.Fatal("accepted without a severity")
	}
	client.PostForm(srv.URL+"/admin/bugs/1/review", url.Values{"csrf": {csrf}, "status": {"accepted"}, "severity": {"major"}})
	client.PostForm(srv.URL+"/admin/bugs/1/review", url.Values{"csrf": {csrf}, "status": {"accepted"}, "severity": {"severe"}})
	client.PostForm(srv.URL+"/admin/bugs/2/review", url.Values{"csrf": {csrf}, "status": {"duplicate"}, "dup_of": {"1"}})
	var bal int64
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 25 {
		t.Fatalf("balance %d, want 25 (one major report)", bal)
	}
	var dup int64
	app.db.QueryRow("SELECT dup_of FROM bugs WHERE id = 2").Scan(&dup)
	if dup != 1 {
		t.Fatalf("duplicate not merged: dup_of %d", dup)
	}
	if d := get(t, client, srv.URL+"/admin/bugs/1"); !strings.Contains(d, "Merged into this report") {
		t.Fatal("merged duplicate not shown on the original")
	}
	if page := get(t, client, srv.URL+"/bugs"); !strings.Contains(page, "same bug as report #1") || !strings.Contains(page, "25 SEQ") {
		t.Fatal("reporter does not see the outcome")
	}
}

func intake(t *testing.T, base, ip, ct, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/problem-reports", strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("Origin", "chrome-extension://abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("status %d without the CORS header", resp.StatusCode)
	}
	return resp.StatusCode, out, resp.Header
}

func report(desc, code string) string {
	b, _ := json.Marshal(map[string]any{
		"v": 1, "wallet": "ambra-browser", "version": "1.2.3", "mode": "default", "description": desc,
		"account_code": code,
		"errors":       []map[string]string{{"at": "2026-10-29T12:00:00Z", "message": "fetch failed"}},
		"context":      map[string]string{"platform": "linux", "network": "testnet"},
		"attached":     map[string]any{"addresses": []string{"tb1qexample"}},
		"later_field":  true,
	})
	return string(b)
}

func TestProblemReportIntake(t *testing.T) {
	app, srv, _ := newTestServer(t)
	code := "0123456789"
	createUser(app.db, "wallet@example.com", "x", code, "")

	// The preflight a browser extension sends.
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/api/problem-reports", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		resp.Header.Get("Access-Control-Allow-Methods") != "POST" || resp.Header.Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()

	cases := []struct {
		ip, ct, body string
		status       int
		account      string
	}{
		{"10.0.0.1", "text/plain", report("x", ""), 415, ""},
		{"10.0.0.1", "application/json", "{not json", 400, ""},
		{"10.0.0.1", "application/json", `{"v":2,"wallet":"web-wallet","description":"x"}`, 400, ""},
		{"10.0.0.1", "application/json", `{"v":1,"wallet":"some-app","description":"x"}`, 400, ""},
		{"10.0.0.1", "application/json", `{"v":1,"wallet":"web-wallet","description":"   "}`, 400, ""},
		{"10.0.0.1", "application/json", `{"v":1,"wallet":"web-wallet","description":"` + strings.Repeat("a", 40<<10) + `"}`, 413, ""},
		{"10.0.0.1", "application/json; charset=utf-8", report("Send button does nothing", code), 201, "linked"},
		{"10.0.0.1", "application/json", report("Send button does nothing", code), 409, ""},
		{"10.0.0.1", "application/json", report("Balance shows zero", "ffffffffff"), 201, "unknown"},
		{"10.0.0.1", "application/json", report("Receive screen blank", ""), 201, "none"},
	}
	for i, c := range cases {
		status, out, _ := intake(t, srv.URL, c.ip, c.ct, c.body)
		if status != c.status {
			t.Fatalf("case %d: status %d, want %d (%v)", i, status, c.status, out)
		}
		if c.status == 201 && out["account"] != c.account {
			t.Fatalf("case %d: account %v, want %s", i, out["account"], c.account)
		}
		if c.status != 201 && (out["error"] == nil || out["error"] == "") {
			t.Fatalf("case %d: no error sentence", i)
		}
	}
	var userID int64
	var meta string
	app.db.QueryRow("SELECT user_id, meta FROM bugs WHERE title = 'Send button does nothing'").Scan(&userID, &meta)
	if userID != 1 || !strings.Contains(meta, "fetch failed") || !strings.Contains(meta, "tb1qexample") || strings.Contains(meta, "later_field") {
		t.Fatalf("linked report stored wrong: user %d meta %s", userID, meta)
	}
	app.db.QueryRow("SELECT user_id, meta FROM bugs WHERE title = 'Balance shows zero'").Scan(&userID, &meta)
	if userID != 0 || !strings.Contains(meta, "ffffffffff") {
		t.Fatalf("unknown-code report: user %d meta %s", userID, meta)
	}

	// Per address: 5 an hour, the sixth is refused with Retry-After.
	for i := 0; i < 2; i++ {
		if s, _, _ := intake(t, srv.URL, "10.0.0.1", "application/json", report(fmt.Sprintf("more %d", i), "")); s != 201 {
			t.Fatalf("report %d refused early: %d", i, s)
		}
	}
	s, out, h := intake(t, srv.URL, "10.0.0.1", "application/json", report("one too many", ""))
	if s != 429 || h.Get("Retry-After") == "" || out["error"] != "Too many reports from here. Try again later." {
		t.Fatalf("over the hourly limit: %d %v %q", s, out, h.Get("Retry-After"))
	}
	// Another address is unaffected.
	if s, _, _ := intake(t, srv.URL, "10.0.0.2", "application/json", report("from elsewhere", "")); s != 201 {
		t.Fatalf("second address refused: %d", s)
	}

	// The report reaches the account's page and the admin desk, and pays only on acceptance.
	var bal int64
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 0 {
		t.Fatal("intake credited something")
	}
	var linked, unknown int64
	app.db.QueryRow("SELECT id FROM bugs WHERE title = 'Send button does nothing'").Scan(&linked)
	app.db.QueryRow("SELECT id FROM bugs WHERE title = 'Balance shows zero'").Scan(&unknown)
	if err := reviewBug(app.db, linked, "accepted", "minor", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := reviewBug(app.db, unknown, "accepted", "severe", 0, ""); err != nil {
		t.Fatal(err)
	}
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger").Scan(&bal)
	if bal != 10 {
		t.Fatalf("ledger total %d, want 10: only the linked report pays", bal)
	}
}

func newJar() http.CookieJar {
	j, _ := cookiejar.New(nil)
	return j
}

func TestSlidingLimiter(t *testing.T) {
	l := newSlidingLimiter(limitRule{time.Hour, 2}, limitRule{24 * time.Hour, 3})
	t0 := time.Unix(1_000_000, 0)
	for i, want := range []bool{true, true, false} {
		if ok, _ := l.allow("k", t0.Add(time.Duration(i)*time.Minute)); ok != want {
			t.Fatalf("event %d: %v", i, ok)
		}
	}
	if ok, _ := l.allow("k", t0.Add(61*time.Minute)); !ok {
		t.Fatal("hourly window did not free a slot")
	}
	if ok, wait := l.allow("k", t0.Add(3*time.Hour)); ok || wait < 20*time.Hour {
		t.Fatalf("daily limit: ok %v wait %v", ok, wait)
	}
}
