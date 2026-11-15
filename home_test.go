package main

import (
	"sort"
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
	// The web wallet's tasks are listed; every other task is summed up by stage, linked to the
	// task page's section for it.
	if strings.Contains(page, "Make your first testnet transaction") {
		t.Error("the front page lists a task that is not the web wallet's")
	}
	for _, want := range []string{`href="/tasks#s-core"`, "Sequentia Core", "See all 23", `href="/tasks#s-staking"`, `href="/tasks#s-fees"`} {
		if !strings.Contains(page, want) {
			t.Errorf("signed out: no %q", want)
		}
	}
	tasks := get(t, client, srv.URL+"/tasks")
	if i, j := strings.Index(tasks, `id="s-webwallet"`), strings.Index(tasks, `id="s-core"`); i < 0 || j < i || !strings.Contains(tasks, "Make your first testnet transaction") {
		t.Error("the task page does not group the tasks by stage, the web wallet's first")
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

// The task page is one path: stages in order, each task after every task
// it needs. Every seeded task names a known surface (its label) and a stage
// (its category), every stage has a task, so no page shows an empty group,
// and the sort values rise along the path, so the dashboard's next task and
// the page agree.
func TestSeededTasksFollowThePath(t *testing.T) {
	pos := map[string]int{}
	stageIdx := map[string]int{}
	for i, st := range stages {
		stageIdx[st.Key] = i
	}
	used := map[string]int{}
	for _, task := range seedTasks {
		known := false
		for _, s := range surfaces {
			known = known || s.Key == task.surface
		}
		if !known {
			t.Errorf("task %s is on %q, which is not a surface", task.slug, task.surface)
		}
		st := stageOf(task.category)
		if st == "other" {
			t.Errorf("task %s is under %q, which is not a stage", task.slug, task.category)
		}
		used[st]++
	}
	for _, st := range stages {
		if used[st.Key] == 0 {
			t.Errorf("stage %s has no task", st.Key)
		}
	}
	// The path: by stage, then by sort, as the pages list it.
	path := make([]seedTask, len(seedTasks))
	copy(path, seedTasks)
	sort.SliceStable(path, func(i, j int) bool {
		si, sj := stageIdx[stageOf(path[i].category)], stageIdx[stageOf(path[j].category)]
		if si != sj {
			return si < sj
		}
		return path[i].sort < path[j].sort
	})
	for i, task := range path {
		if i > 0 && task.sort <= path[i-1].sort {
			t.Errorf("sort of %s (%d) does not rise after %s (%d)", task.slug, task.sort, path[i-1].slug, path[i-1].sort)
		}
		pos[task.slug] = i
	}
	for _, task := range seedTasks {
		for _, need := range task.needs {
			p, ok := pos[need]
			switch {
			case !ok:
				t.Errorf("%s needs %q, which is no seeded task", task.slug, need)
			case p >= pos[task.slug]:
				t.Errorf("%s comes before %s, which it needs", task.slug, need)
			}
		}
	}
	// The faults the path fixes stay fixed: the release is installed before
	// anything is done with the node, the unblinding task is with the node's,
	// unbonding ends the staking stage, and the fee tasks come after listing.
	for _, c := range [][2]string{{"install-release", "first-transaction"}, {"install-release", "issue-asset"},
		{"first-transaction", "emissio-unblind"}, {"fee-whitelist", "any-asset-fee"}, {"fee-whitelist", "emissio-return"},
		{"fee-whitelist", "price-server"}, {"redelegate", "unbond-stake"}} {
		if pos[c[0]] >= pos[c[1]] {
			t.Errorf("%s is not before %s", c[0], c[1])
		}
	}
	for _, task := range seedTasks {
		if task.slug == "unbond-stake" && task.category != "Staking" {
			t.Errorf("unbond-stake is under %q", task.category)
		}
		if task.slug == "emissio-unblind" && task.category != "Sequentia Core" {
			t.Errorf("emissio-unblind is under %q", task.category)
		}
	}
}

// reseed-tasks puts an existing database on the path: seeded tasks take the
// seed's stage and sort, an admin's reward stays, and a task an admin added
// keeps its own sort.
func TestReseedTasksOrder(t *testing.T) {
	app, _, _ := newTestServer(t)
	app.db.Exec("UPDATE tasks SET sort = 999, category = 'Infrastructure', reward = 77 WHERE slug = 'unbond-stake'")
	app.db.Exec("DELETE FROM tasks WHERE slug = 'fee-whitelist'")
	app.db.Exec("INSERT INTO tasks (slug, title, category, body, surface, check_kind, reward, cap, needs_txid, active, sort) VALUES ('admin-made', 'Admin task', 'Community', '', 'core', 'none', 5, 0, 0, 1, 3)")
	_, inserted, _, _ := reseedTasks(app.db)
	if inserted != 1 {
		t.Errorf("inserted %d, want 1", inserted)
	}
	var srt, reward int64
	var cat string
	app.db.QueryRow("SELECT sort, category, reward FROM tasks WHERE slug = 'unbond-stake'").Scan(&srt, &cat, &reward)
	if srt != 140 || cat != "Staking" || reward != 77 {
		t.Errorf("unbond-stake after reseed: sort %d, %q, reward %d", srt, cat, reward)
	}
	app.db.QueryRow("SELECT sort FROM tasks WHERE slug = 'admin-made'").Scan(&srt)
	if srt != 3 {
		t.Errorf("the admin's task's sort became %d", srt)
	}
	tasks, _ := listTasks(app.db, false)
	groups := taskGroupsFull(tasks)
	if last := groups[len(groups)-1]; last.Key != "other" || last.Name != "Other tasks" || last.Tasks[0].Slug != "admin-made" {
		t.Errorf("the admin's task is not last, under Other tasks: %+v", last)
	}
	if groups[0].Key != "webwallet" || groups[0].Tasks[0].Slug != "link-web-wallet" {
		t.Errorf("the path does not start with the web wallet's link task: %s", groups[0].Tasks[0].Slug)
	}
}
