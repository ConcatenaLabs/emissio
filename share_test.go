package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// On X the kit writes the link and the code and nothing else: posts that
// read alike across accounts look coordinated to X. The code must stand
// outside the link, since X shortens links and the verification check reads
// the post's text.
func TestShareTexts(t *testing.T) {
	code := "b4c0164766"
	link := "https://sequentiatestnet.com/emissio/r/" + code
	x, reddit, tg := shareTexts(code, link)
	rest := strings.TrimSpace(strings.Replace(strings.Replace(x, link, "", 1), code, "", 1))
	if rest != "" || !strings.Contains(x, link) || !strings.Contains(x, code) {
		t.Fatalf("the X text is not just the link and the code: %q", x)
	}
	// Room for the tester's words, with the official post quoted below.
	if n := xLength(x, link) + 1 + xURLWeight; n > 80 {
		t.Fatalf("the X text takes %d of the 280 characters", n)
	}
	// On Reddit the link alone: the title and the words are the tester's,
	// and the link carries the referral by itself.
	if strings.TrimSpace(reddit) != link {
		t.Errorf("the Reddit text is not just the link: %q", reddit)
	}
	// The Telegram line goes under the link, in a chat the tester chooses:
	// it names Sequentia as what it is and promises nothing.
	if !strings.Contains(tg, "a Bitcoin sidechain") || strings.Contains(tg, "Bitcoin wallet for") {
		t.Errorf("the Telegram line does not say what Sequentia is: %q", tg)
	}
	for _, banned := range []string{"dollar", "euro", "free money", "guaranteed"} {
		if strings.Contains(strings.ToLower(tg), banned) {
			t.Errorf("the Telegram line says %q", banned)
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
			!strings.Contains(page, qesc("\n"+code)) {
			t.Errorf("%s has no X post with the code", path)
		}
		if !strings.Contains(page, "Any post of yours on X that contains your account code verifies") {
			t.Errorf("%s does not say that any post with the code verifies X", path)
		}
	}
	// The Reddit button opens a text post with the link alone and no title;
	// no ready-made text is offered to paste.
	page := get(t, client, srv.URL+"/account")
	if !strings.Contains(page, "reddit.com/submit?type=TEXT&amp;text="+qesc("\n\n"+srv.URL+"/r/"+code)+`"`) {
		t.Error("the Reddit button does not open a text post with the link alone")
	}
	for _, gone := range []string{"title=", "share-reddit", "Why this is not a scam"} {
		if strings.Contains(page, gone) {
			t.Errorf("the kit still carries %q", gone)
		}
	}
	if !strings.Contains(page, "The same post in several communities is what Reddit removes as spam") {
		t.Error("the kit does not say where a Reddit post belongs")
	}
	// Signed out, nothing to share.
	if anon := get(t, &http.Client{Jar: newJar()}, srv.URL+"/tasks"); strings.Contains(anon, "Post on X") {
		t.Error("a signed-out page shows the invite kit")
	}
	// A visitor from the referral link is told who invited them and why
	// this is not a scam, before being asked for anything.
	friend := &http.Client{Jar: newJar()}
	page = get(t, friend, srv.URL+"/r/"+code)
	if !strings.Contains(page, "You were invited by "+code) || !strings.Contains(page, "Nothing to buy or deposit") {
		t.Fatal("the referral landing does not explain the invitation")
	}
}

// The quote button appears only with the official post configured, and only
// for a link that is a post on X.
func TestQuoteOfficialPost(t *testing.T) {
	if got := xQuotePost("https://twitter.com/SequentiaSEQ/status/1234567890?s=20"); got != "https://x.com/SequentiaSEQ/status/1234567890" {
		t.Fatalf("canonical post link: %q", got)
	}
	if got := xQuotePost("https://sequentia.io/news"); got != "" {
		t.Fatalf("a link that is not a post became the quote target: %q", got)
	}
	app, srv, client := newTestServer(t)
	client.PostForm(srv.URL+"/register", url.Values{"email": {"quoter@example.com"}, "password": {"a-long-password"}})
	if page := get(t, client, srv.URL+"/account"); strings.Contains(page, "Quote the official post") {
		t.Fatal("a quote button without an official post")
	}
	app.cfg.XQuotePost = xQuotePost("https://x.com/SequentiaSEQ/status/1234567890")
	page := get(t, client, srv.URL+"/account")
	if !strings.Contains(page, "Quote the official post on X") || !strings.Contains(page, "&amp;url="+qesc("https://x.com/SequentiaSEQ/status/1234567890")) {
		t.Fatal("no quote button for the configured post")
	}
	// A tester who has never quoted a post is told how, button or by hand,
	// to write their own words, and that the quote's link verifies their X
	// account like any other post with the code.
	for _, step := range []string{"press the repost icon", "choose <strong>Quote</strong>", "<strong>Copy link</strong>",
		`href="https://x.com/SequentiaSEQ/status/1234567890"`, `data-copy-from="share-x"`, "write your own words"} {
		if !strings.Contains(page, step) {
			t.Errorf("the quote steps lack %q", step)
		}
	}
}
