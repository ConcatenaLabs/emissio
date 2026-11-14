package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestEmailKey(t *testing.T) {
	for in, want := range map[string]string{
		"Alberto+uxtest@Outlook.com": "alberto@outlook.com",
		" alberto@outlook.com ":      "alberto@outlook.com",
		"a.l.b+x+y@gmail.com":        "alb@gmail.com",
		"Al.Berto@googlemail.com":    "alberto@gmail.com",
		"al.berto@outlook.com":       "al.berto@outlook.com", // dots matter outside Gmail
		"+tag@example.com":           "+tag@example.com",     // nothing before the tag: left as typed
		"no-at-sign":                 "no-at-sign",
	} {
		if got := emailKey(in); got != want {
			t.Errorf("emailKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// A second account on the same mailbox under another spelling is refused;
// the account that exists is untouched and still signs in.
func TestOneAccountPerMailbox(t *testing.T) {
	app, srv, _ := newTestServer(t)
	reg := func(email string) {
		c := &http.Client{Jar: newJar()}
		c.PostForm(srv.URL+"/register", url.Values{"email": {email}, "password": {"a-long-password"}})
	}
	count := func() (n int) {
		app.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
		return
	}
	reg("someone@outlook.com")
	reg("someone+second@outlook.com")
	reg("some.one@gmail.com")
	reg("someone+x@gmail.com")
	reg("s.o.m.e.o.n.e@googlemail.com")
	if n := count(); n != 2 {
		t.Fatalf("%d accounts, want 2 (one per mailbox)", n)
	}
	// Accounts made before the key existed get it at start-up, and the admin
	// desk names the mailbox they share.
	app.db.Exec("INSERT INTO users (email, pass_hash, claim_code, created_at) VALUES ('someone+old@outlook.com', 'x', 'c0ffee0001', 0)")
	backfillEmailKeys(app.db)
	var first, old int64
	app.db.QueryRow("SELECT id FROM users WHERE email = 'someone@outlook.com'").Scan(&first)
	app.db.QueryRow("SELECT id FROM users WHERE email = 'someone+old@outlook.com'").Scan(&old)
	links, err := linkedAccounts(app.db, first)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range links {
		if l.ID == old && l.Via == "mailbox" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the desk does not link accounts on one mailbox: %+v", links)
	}
}
