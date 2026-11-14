package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The X post must fit X's limit with the longest link this site makes, and
// carry the code itself: X shortens links, so the code inside the link alone
// would not reach the X verification check.
func TestShareTexts(t *testing.T) {
	code := "b4c0164766"
	link := "https://sequentiatestnet.com/emissio/r/" + code
	x, title, reddit := shareTexts(code, link)
	if n := xLength(x, link); n > 280 {
		t.Fatalf("the X post is %d characters as X counts them", n)
	}
	if !strings.Contains(strings.ReplaceAll(x, link, ""), code) {
		t.Fatal("the X post carries the code only inside the link")
	}
	for name, s := range map[string]string{"x": x, "reddit": reddit} {
		if !strings.Contains(s, link) {
			t.Errorf("the %s text has no referral link", name)
		}
	}
	if !strings.Contains(reddit, code) || title == "" {
		t.Error("the Reddit post lacks the code or a title")
	}
	for _, banned := range []string{"dollar", "euro", "free money", "guaranteed"} {
		if strings.Contains(strings.ToLower(x+title+reddit), banned) {
			t.Errorf("the share texts say %q", banned)
		}
	}
}

func TestShareKitOnPages(t *testing.T) {
	app, srv, client := newTestServer(t)
	client.PostForm(srv.URL+"/register", url.Values{"email": {"sharer@example.com"}, "password": {"a-long-password"}})
	var code string
	app.db.QueryRow("SELECT claim_code FROM users WHERE email = 'sharer@example.com'").Scan(&code)
	for _, path := range []string{"/", "/tasks", "/account"} {
		page := get(t, client, srv.URL+path)
		if !strings.Contains(page, "Post on X") || !strings.Contains(page, "x.com/intent/post?text=") ||
			!strings.Contains(page, qesc("My Emissio code: "+code)) {
			t.Errorf("%s has no ready-made post with the code", path)
		}
	}
	// Signed out, nothing to share.
	if anon := get(t, &http.Client{Jar: newJar()}, srv.URL+"/tasks"); strings.Contains(anon, "Post on X") {
		t.Error("a signed-out page shows the invite kit")
	}
	// A visitor from the referral link is told who invited them and why
	// this is not a scam, before being asked for anything.
	friend := &http.Client{Jar: newJar()}
	page := get(t, friend, srv.URL+"/r/"+code)
	if !strings.Contains(page, "You were invited by "+code) || !strings.Contains(page, "Nothing to buy or deposit") {
		t.Fatal("the referral landing does not explain the invitation")
	}
}
