package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidateMainnetAddress(t *testing.T) {
	valid := []string{
		// BIP-173 reference P2WPKH
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4",
	}
	for _, a := range valid {
		if err := validateMainnetAddress(a); err != nil {
			t.Errorf("expected valid %q, got %v", a, err)
		}
	}

	// Round-trip a synthetic taproot (v1, 32-byte program) address.
	prog := make([]int, 0, 52)
	prog = append(prog, 1) // witness v1
	raw := make([]int, 0)
	for i := 0; i < 32; i++ {
		raw = append(raw, i%256)
	}
	conv := make([]int, 0)
	acc, bits := 0, 0
	for _, b := range raw {
		acc = acc<<8 | b
		bits += 8
		for bits >= 5 {
			bits -= 5
			conv = append(conv, (acc>>bits)&31)
		}
	}
	if bits > 0 {
		conv = append(conv, (acc<<(5-bits))&31)
	}
	prog = append(prog, conv...)
	taproot := bech32Encode("bc", prog, bech32mConst)
	if err := validateMainnetAddress(taproot); err != nil {
		t.Errorf("expected synthetic taproot %q valid, got %v", taproot, err)
	}
	// Same data with the wrong checksum constant must fail.
	badChecksum := bech32Encode("bc", prog, bech32Const)
	if err := validateMainnetAddress(badChecksum); err == nil {
		t.Errorf("expected v1-with-bech32 %q to be rejected", badChecksum)
	}

	invalid := map[string]string{
		"": "empty",
		"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx":   "testnet",
		"sqb1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq": "confidential",
		"lq1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq": "liquid",
		"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2":           "legacy",
		"3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy":           "legacy p2sh",
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5":   "bad checksum",
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3tb":   "bad checksum b",
		"notanaddress": "garbage",
	}
	for a, why := range invalid {
		if err := validateMainnetAddress(a); err == nil {
			t.Errorf("expected %s address %q to be rejected", why, a)
		}
	}
}

func TestFormatSEQ(t *testing.T) {
	cases := map[int64]string{0: "0", 10: "10", 1000: "1,000", 1000000: "1,000,000", -2500: "-2,500"}
	for in, want := range cases {
		if got := formatSEQ(in); got != want {
			t.Errorf("formatSEQ(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatUSD(t *testing.T) {
	if got := formatUSD(1000000); got != "175,000" {
		t.Errorf("formatUSD(1000000) = %q", got)
	}
	if got := formatUSD(215); got != "37.63" {
		t.Errorf("formatUSD(215) = %q", got)
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse battery staple")
	if !verifyPassword(h, "correct horse battery staple") {
		t.Error("valid password rejected")
	}
	if verifyPassword(h, "wrong password") {
		t.Error("wrong password accepted")
	}
	if !strings.HasPrefix(h, "argon2id$") {
		t.Errorf("unexpected hash format %q", h)
	}
}

func TestParseXPost(t *testing.T) {
	good := map[string][2]string{
		"https://x.com/Some_User/status/1234567890123456789":            {"some_user", "https://x.com/Some_User/status/1234567890123456789"},
		"https://twitter.com/Some_User/status/1234567890123456789?s=20": {"some_user", "https://x.com/Some_User/status/1234567890123456789"},
		"x.com/abc/status/12345/photo/1":                                {"abc", "https://x.com/abc/status/12345"},
		"  https://mobile.x.com/abc/status/12345  ":                     {"abc", "https://x.com/abc/status/12345"},
	}
	for in, want := range good {
		h, c, ok := parseXPost(in)
		if !ok || h != want[0] || c != want[1] {
			t.Errorf("parseXPost(%q) = %q, %q, %v; want %q, %q", in, h, c, ok, want[0], want[1])
		}
	}
	for _, in := range []string{"", "https://x.com/abc", "https://x.com/abc/status/", "https://example.com/abc/status/123", "https://x.com/a b/status/123", "@handle"} {
		if _, _, ok := parseXPost(in); ok {
			t.Errorf("parseXPost(%q) accepted", in)
		}
	}
	if got := xPostID("https://x.com/abc/status/12345"); got != "12345" {
		t.Errorf("xPostID = %q", got)
	}
}

func TestXAccountCreated(t *testing.T) {
	if _, ok := xAccountCreated(44196397); ok {
		t.Errorf("sequential id treated as snowflake")
	}
	// A post id is a snowflake with the same epoch: "the bird is freed".
	created, ok := xAccountCreated(1585841080431321088)
	if !ok || created.UTC().Format("2006-01-02T15:04:05") != "2022-10-28T03:49:11" {
		t.Errorf("snowflake decode = %v, %v", created.UTC(), ok)
	}
	if tok := xToken("20"); tok == "" || strings.ContainsAny(tok, "0.") {
		t.Errorf("xToken(20) = %q", tok)
	}
}

func TestCheckX(t *testing.T) {
	young := strconv.FormatUint(uint64(time.Now().AddDate(-1, 0, 0).UnixMilli()-xSnowflakeEpoch)<<22, 10)
	old := strconv.FormatUint(uint64(time.Now().AddDate(-3, 0, 0).UnixMilli()-xSnowflakeEpoch)<<22, 10)
	posts := map[string]string{
		"1": `{"__typename":"Tweet","text":"my code is CODE123 thanks","user":{"screen_name":"Alice_X","id_str":"12345"}}`,
		"2": `{"__typename":"Tweet","text":"my code is CODE123","user":{"screen_name":"mallory","id_str":"12345"}}`,
		"3": `{"__typename":"Tweet","text":"nothing here","user":{"screen_name":"alice_x","id_str":"` + young + `"}}`,
		"4": `{"__typename":"Tweet","text":"CODE123","user":{"screen_name":"alice_x","id_str":"` + old + `"}}`,
		"5": `{"__typename":"TweetTombstone"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tweet-result" || r.URL.Query().Get("token") == "" {
			http.NotFound(w, r)
			return
		}
		body, ok := posts[r.URL.Query().Get("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	cases := map[string]string{
		"1": "post by @Alice_X contains the code; account id predates 2016 (age OK)",
		"2": "post is by @mallory, NOT @alice_x: someone else's post",
		"3": "code NOT found in the post; account created " + time.Now().AddDate(-1, 0, 0).Format("Jan 2006") + " (age UNDER 2 YEARS)",
		"4": "post by @alice_x contains the code; account created " + time.Now().AddDate(-3, 0, 0).Format("Jan 2006") + " (age OK)",
		"5": "post unavailable (deleted, protected account, or wrong link)",
		"9": "post NOT FOUND (deleted, protected account, or wrong link)",
	}
	for id, want := range cases {
		if got := checkX(srv.URL, "alice_x", id, "CODE123"); got != want {
			t.Errorf("checkX(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestCheckReddit(t *testing.T) {
	old := float64(time.Now().AddDate(-3, 0, 0).Unix())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/access_token":
			if u, p, ok := r.BasicAuth(); !ok || u != "id" || p != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"access_token":"tok","token_type":"bearer"}`))
		case "/user/old_account/about":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprintf(w, `{"data":{"created_utc":%f,"subreddit":{"public_description":"hello CODE123"}}}`, old)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	want := "code found in profile description; account created " + time.Now().AddDate(-3, 0, 0).Format("Jan 2006") + " (age OK)"
	if got := checkReddit(srv.URL, srv.URL, "id", "secret", "old_account", "CODE123"); got != want {
		t.Errorf("checkReddit = %q, want %q", got, want)
	}
	if got := checkReddit(srv.URL, srv.URL, "id", "wrong", "old_account", "CODE123"); !strings.HasPrefix(got, "reddit token failed") {
		t.Errorf("bad credentials: %q", got)
	}
	if got := checkReddit(srv.URL, srv.URL, "id", "secret", "nobody", "CODE123"); got != "reddit user NOT FOUND" {
		t.Errorf("missing user: %q", got)
	}
}

func TestRedditToken(t *testing.T) {
	// Token produced by the Reddit app's token.ts for secret "s3cret",
	// u/Alice_X created 2019-05-04, code 0123456789, issued 2026-09-13T12:00Z.
	const tok = "ERV1.eyJ1IjoiQWxpY2VfWCIsImMiOjE1NTY5NjQwMDAsImUiOiIwMTIzNDU2Nzg5IiwiaSI6MTc4OTMwMDgwMCwieCI6MTc4OTM4NzIwMH0.DLr9rBudVVjoVsOYe08MWhORJ-hiOdktfvcHEOF6XSM"
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	p, err := parseRedditToken("s3cret", tok, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Username != "Alice_X" || p.Code != "0123456789" || p.Created != 1556964000 {
		t.Fatalf("claims %+v", p)
	}
	handle, note, ok := redditTokenCheck(p, "0123456789")
	if !ok || handle != "alice_x" || !strings.HasPrefix(note, "signed by the r/sequentia app for u/Alice_X; account created May 2019 (age OK)") {
		t.Fatalf("check = %q %q %v", handle, note, ok)
	}
	if _, _, ok := redditTokenCheck(p, "ffffffffff"); ok {
		t.Fatalf("token accepted for another account code")
	}
	if _, err := parseRedditToken("other", tok, now); err == nil {
		t.Fatalf("wrong secret accepted")
	}
	if _, err := parseRedditToken("s3cret", tok, now.AddDate(0, 0, 2)); err == nil {
		t.Fatalf("expired token accepted")
	}
	if _, err := parseRedditToken("s3cret", tok[:len(tok)-2]+"AA", now); err == nil {
		t.Fatalf("tampered signature accepted")
	}
}
