package main

import (
	"net/url"
	"strings"
	"testing"
)

// An inactive task, switched off by an admin or retired, answers on its own
// address as closed: no instructions and no form, signed in or not, and a
// submission to it is refused and stores nothing.
func TestInactiveTaskIsClosed(t *testing.T) {
	app, srv, client := newTestServer(t)
	var body string
	app.db.QueryRow("SELECT body FROM tasks WHERE slug = 'first-transaction'").Scan(&body)
	instructions := strings.SplitN(body, "\n", 2)[0]
	app.db.Exec("UPDATE tasks SET active = 0 WHERE slug = 'first-transaction'")
	// A retired task, as an older database holds it once reseed-tasks has run.
	app.db.Exec(`INSERT INTO tasks (slug, title, category, body, surface, check_kind, reward, cap, needs_txid, active, sort)
		VALUES ('seqdex-swap', 'Swap on SeqDEX', 'Trading', 'Swap two assets and paste the txid.', 'dex', 'none', 20, 100, 1, 1, 900)`)
	for _, slug := range retiredTasks {
		app.db.Exec("UPDATE tasks SET active = 0 WHERE slug = ? AND active = 1", slug)
	}
	page := func(slug, who, gone string) {
		t.Helper()
		p := get(t, client, srv.URL+"/tasks/"+slug)
		if !strings.Contains(p, "This task is closed") {
			t.Errorf("%s, %s: the page does not say the task is closed", slug, who)
		}
		for _, s := range []string{gone, "/submit\"", "Submit evidence", "Sign in</a> or"} {
			if strings.Contains(p, s) {
				t.Errorf("%s, %s: the closed page shows %q", slug, who, s)
			}
		}
	}
	page("first-transaction", "anonymous", instructions)
	page("seqdex-swap", "anonymous", "Swap two assets and paste the txid.")
	register(t, client, srv.URL, "closed@example.com")
	page("first-transaction", "signed in", instructions)
	page("seqdex-swap", "signed in", "Swap two assets and paste the txid.")
	csrf := csrfOf(t, client, srv.URL+"/account")
	for _, slug := range []string{"first-transaction", "seqdex-swap"} {
		resp, err := client.PostForm(srv.URL+"/tasks/"+slug+"/submit", url.Values{
			"csrf": {csrf}, "txid": {strings.Repeat("ab", 32)}, "notes": {"done"}, "review": {"1"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Request.URL.Query().Get("e"); got != "This task is closed." {
			t.Errorf("%s: POST answered %q", slug, got)
		}
	}
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&n)
	if n != 0 {
		t.Errorf("%d submissions stored for closed tasks", n)
	}
	// Switched on again, the page is the task's own.
	app.db.Exec("UPDATE tasks SET active = 1 WHERE slug = 'first-transaction'")
	if p := get(t, client, srv.URL+"/tasks/first-transaction"); strings.Contains(p, "This task is closed") || !strings.Contains(p, instructions) {
		t.Error("an active task's page is not its own")
	}
}
