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
	// The web wallet's tasks are listed; every other task is summed up by place, linked to the
	// task page's section for it.
	if strings.Contains(page, "Make your first testnet transaction") {
		t.Error("the front page lists a task that is not the web wallet's")
	}
	for _, want := range []string{`href="/tasks#s-core"`, "Sequentia Core", "See all 20", `href="/tasks#s-pools"`} {
		if !strings.Contains(page, want) {
			t.Errorf("signed out: no %q", want)
		}
	}
	tasks := get(t, client, srv.URL+"/tasks")
	if i, j := strings.Index(tasks, `id="s-webwallet"`), strings.Index(tasks, `id="s-core"`); i < 0 || j < i || !strings.Contains(tasks, "Make your first testnet transaction") {
		t.Error("the task page does not group the tasks by place, the web wallet's first")
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
	if !strings.Contains(page, `<span class="figure">10</span> SEQ earned`) || !strings.Contains(page, "1 of ") || !strings.Contains(page, "Next: <a href=\"/tasks/web-wallet-pay\">") || !strings.Contains(page, "tasks, 0 done") || !strings.Contains(page, `<span class="tile-state ok">Done</span>`) {
		t.Fatalf("signed in after a task:\n%s", page)
	}
	// The task list shows the same progress: what each task credited, the
	// next one to do, and a way into every task still open.
	tasks = get(t, client, srv.URL+"/tasks")
	for _, want := range []string{"<strong>10 SEQ</strong> earned from tasks", "1 of ", `href="/tasks/web-wallet-pay">Do the next task`,
		`<span class="pill approved">Done</span>`, "+10 <small>SEQ</small>", `<a class="btn small" href="/tasks/web-wallet-pay">Start</a>`} {
		if !strings.Contains(tasks, want) {
			t.Errorf("task list signed in: no %q", want)
		}
	}
	if strings.Contains(tasks, `<a class="btn small" href="/tasks/link-web-wallet">Start</a>`) {
		t.Error("task list: a done task offers to start it")
	}
	// A task page leads back to the list, and says which notes are optional.
	tp := get(t, client, srv.URL+"/tasks/web-wallet-pay")
	if !strings.Contains(tp, `href="/tasks#s-webwallet">&larr; All tasks</a>`) {
		t.Error("the task page has no way back to the list")
	}
	tp = get(t, client, srv.URL+"/tasks/first-transaction")
	if !strings.Contains(tp, `Notes <span class="muted">(optional)</span>`) {
		t.Error("the notes of a task whose check does not read them are not marked optional")
	}
}
