package main

import (
	"strings"
	"testing"
)

// The front page: what Sequentia is, a way to start testing that needs no
// account, the web wallet's tasks first, no counters; signed in, the tester's
// progress and next task.
func TestFrontPage(t *testing.T) {
	app, srv, client := newTestServer(t)
	app.cfg.WalletURL = "https://example.test/wallet/"
	page := get(t, client, srv.URL+"/")
	for _, want := range []string{
		"a Bitcoin sidechain for asset tokenization and disintermediated exchanges",
		"No account needed.", `href="https://example.test/wallet/"`, "Open the web wallet",
		"Join Emissio and earn for what you do", "Sequence tokens (SEQ)", "Create your account",
		"Web wallet tasks", "Get paid at your payment name", "Bug reports", "Vulnerability reports", "Competitions", "Referrals",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("signed out: no %q", want)
		}
	}
	for _, banned := range []string{"Participants", "Issued so far", "claimed", "txid", "proof decimals", "Ambra", "SeqDEX", "Arca", "Lightning", "decentralized", "the Bitcoin sidechain"} {
		if strings.Contains(page, banned) {
			t.Errorf("signed out: shows %q", banned)
		}
	}
	// The web wallet's tasks come before every other task.
	if strings.Index(page, "Make your first testnet transaction") < strings.Index(page, "Get paid at your payment name") {
		t.Error("a non-web-wallet task is listed before the web wallet's")
	}

	register(t, client, srv.URL, "front@x.io")
	page = get(t, client, srv.URL+"/")
	if !strings.Contains(page, "Your progress") || !strings.Contains(page, "0 of ") || !strings.Contains(page, "Next: <a href=\"/tasks/link-web-wallet\">Link your web wallet</a>") {
		t.Fatalf("signed in: no progress or next task:\n%s", page)
	}
	if !strings.Contains(page, "verify one social account and add your mainnet address") {
		t.Error("signed in: no payout reminder")
	}
	// A task done moves the next one on and is marked done.
	var id int64
	app.db.QueryRow("SELECT id FROM tasks WHERE slug = 'link-web-wallet'").Scan(&id)
	app.db.Exec("INSERT INTO submissions (user_id, task_id, status, created_at) VALUES (1, ?, 'approved', 0)", id)
	app.db.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, created_at) VALUES (1, 10, 'submission', 1, 0)")
	page = get(t, client, srv.URL+"/")
	if !strings.Contains(page, `<span class="figure">10</span> SEQ earned`) || !strings.Contains(page, "1 of ") || !strings.Contains(page, "Next: <a href=\"/tasks/web-wallet-pay\">") || !strings.Contains(page, `<span class="sub ok">Done</span>`) {
		t.Fatalf("signed in after a task:\n%s", page)
	}
}
