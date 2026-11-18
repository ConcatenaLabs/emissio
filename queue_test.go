package main

// The review queue holds only what a check cannot decide: evidence that
// has not arrived yet waits and is credited by the poller, a failing check
// refuses at once, a clean verification is approved at once unless the
// account shares a signal, and reports from our own automated checks stay
// out of the queue.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// xff sends every request as if from one network address.
type xff struct{ ip string }

func (x xff) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", x.ip)
	return http.DefaultTransport.RoundTrip(r)
}

func fromIP(ip string) http.RoundTripper { return xff{ip} }

// ipFor gives each email a network address of its own.
func ipFor(email string) string {
	var h uint32 = 2166136261
	for _, c := range []byte(email) {
		h = (h ^ uint32(c)) * 16777619
	}
	return fmt.Sprintf("10.%d.%d.%d", h>>16&0xff, h>>8&0xff, h&0xff|1)
}

// switchExplorer is a stand-in explorer whose transactions a test can
// change between polls, and switch off.
type switchExplorer struct {
	mu   sync.Mutex
	txs  map[string]string
	down bool
}

func (e *switchExplorer) set(txid, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.txs[txid] = body
}

func (e *switchExplorer) setDown(d bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.down = d
}

func (e *switchExplorer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if body, ok := e.txs[strings.TrimPrefix(r.URL.Path, "/tx/")]; ok {
		w.Write([]byte(body))
		return
	}
	http.NotFound(w, r)
}

func TestWaitingSubmissions(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	plain := []map[string]any{{"is_coinbase": false}}
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	untagged := []map[string]any{vout(100000000, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	ex := &switchExplorer{txs: map[string]string{}}
	explorer := httptest.NewServer(ex)
	defer explorer.Close()
	app, srv, client := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	register(t, client, srv.URL, "wait@example.com")
	app.db.Exec("UPDATE users SET claim_code = ? WHERE email = 'wait@example.com'", code)
	csrf := csrfOf(t, client, srv.URL+"/account")
	submit := func(slug, txid string) string {
		resp, err := client.PostForm(srv.URL+"/tasks/"+slug+"/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		q := resp.Request.URL.Query()
		return q.Get("e") + q.Get("m")
	}
	type row struct {
		Status, Chain, Review string
		Auto                  int
	}
	sub := func(id int64) row {
		var r row
		app.db.QueryRow("SELECT status, chain_note, review_note, auto FROM submissions WHERE id = ?", id).Scan(&r.Status, &r.Chain, &r.Review, &r.Auto)
		return r
	}
	balance := func() int64 {
		var b int64
		app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger").Scan(&b)
		return b
	}
	queued := func() int {
		subs, err := pendingSubmissions(app.db)
		if err != nil {
			t.Fatal(err)
		}
		return len(subs)
	}
	txA := "a1" + strings.Repeat("0", 62)

	// Not confirmed yet: stored as waiting, with the reason, and not queued.
	ex.set(txA, txJSONid(txA, plain, tagged, false))
	if m := submit("first-transaction", txA); !strings.Contains(m, "Not there yet: the transaction is in the mempool and not yet confirmed") || !strings.Contains(m, "up to 3 days") {
		t.Fatalf("mempool submission: %q", m)
	}
	if r := sub(1); r.Status != "waiting" || queued() != 0 || balance() != 0 {
		t.Fatalf("mempool submission stored as %+v, queue %d, balance %d", r, queued(), balance())
	}
	if p := get(t, client, srv.URL+"/tasks/first-transaction"); !strings.Contains(p, "pill waiting") || !strings.Contains(p, "you need not submit again") {
		t.Fatal("the task page does not show the waiting submission")
	}
	// Still unconfirmed at the next poll: it keeps waiting.
	app.recheckWaiting(time.Now())
	if r := sub(1); r.Status != "waiting" || balance() != 0 {
		t.Fatalf("still unconfirmed: %+v", r)
	}
	// Confirmed: the poller credits it as the submit path does.
	ex.set(txA, txJSONid(txA, plain, tagged, true))
	app.recheckWaiting(time.Now())
	if r := sub(1); r.Status != "approved" || r.Auto != 1 || r.Review != "Checked automatically." || balance() != 10 {
		t.Fatalf("confirmed: %+v, balance %d", r, balance())
	}
	app.recheckWaiting(time.Now())
	if balance() != 10 {
		t.Fatalf("credited twice: %d", balance())
	}

	// An unconfirmed transaction without this account's proof decimals is
	// refused at once: it is not bound to the account, so it cannot wait.
	txB := "b1" + strings.Repeat("0", 62)
	ex.set(txB, txJSONid(txB, plain, untagged, false))
	app.db.Exec("UPDATE tasks SET check_kind = 'first-transaction' WHERE slug IN ('anchor-lookup', 'multi-asset-send', 'any-asset-fee')")
	if m := submit("anchor-lookup", txB); !strings.Contains(m, "Not accepted: proof amount .86718345 NOT found") {
		t.Fatalf("unbound mempool submission: %q", m)
	}
	// A check that now fails refuses the waiting submission with the reason.
	ex.set(txB, txJSONid(txB, plain, tagged, false))
	if m := submit("anchor-lookup", txB); !strings.Contains(m, "Not there yet") {
		t.Fatalf("second mempool submission: %q", m)
	}
	ex.set(txB, txJSONid(txB, plain, untagged, true))
	app.recheckWaiting(time.Now())
	if r := sub(2); r.Status != "rejected" || !strings.HasPrefix(r.Review, "Checked again: proof amount .86718345 NOT found") {
		t.Fatalf("now failing: %+v", r)
	}

	// A waiting submission lapses after its time with the check's note.
	txC := "c1" + strings.Repeat("0", 62)
	ex.set(txC, txJSONid(txC, plain, tagged, false))
	submit("multi-asset-send", txC)
	app.recheckWaiting(time.Now().Add(71 * time.Hour))
	if r := sub(3); r.Status != "waiting" {
		t.Fatalf("lapsed early: %+v", r)
	}
	app.recheckWaiting(time.Now().Add(73 * time.Hour))
	if r := sub(3); r.Status != "rejected" || !strings.Contains(r.Review, "Not there after 3 days: the transaction is in the mempool") {
		t.Fatalf("lapse: %+v", r)
	}

	// A check that cannot run keeps it waiting, and at the end of the time
	// sends it to a reviewer, since nothing automatic could decide it.
	txD := "d1" + strings.Repeat("0", 62)
	ex.set(txD, txJSONid(txD, plain, tagged, false))
	submit("any-asset-fee", txD)
	ex.setDown(true)
	app.recheckWaiting(time.Now())
	if r := sub(4); r.Status != "waiting" || queued() != 0 {
		t.Fatalf("explorer down: %+v", r)
	}
	app.recheckWaiting(time.Now().Add(73 * time.Hour))
	if r := sub(4); r.Status != "pending" || queued() != 1 {
		t.Fatalf("explorer down at the lapse: %+v, queue %d", r, queued())
	}
	ex.setDown(false)
}

// A new submission replaces a waiting one, and a held account's evidence
// that arrives goes to a reviewer, not to the ledger.
func TestWaitingReplacedAndHeld(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	plain := []map[string]any{{"is_coinbase": false}}
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	ex := &switchExplorer{txs: map[string]string{}}
	explorer := httptest.NewServer(ex)
	defer explorer.Close()
	app, srv, client := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	register(t, client, srv.URL, "held@example.com")
	app.db.Exec("UPDATE users SET claim_code = ? WHERE email = 'held@example.com'", code)
	csrf := csrfOf(t, client, srv.URL+"/account")
	submit := func(txid string) {
		resp, err := client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	tx1, tx2 := "e1"+strings.Repeat("0", 62), "e2"+strings.Repeat("0", 62)
	ex.set(tx1, txJSONid(tx1, plain, tagged, false))
	ex.set(tx2, txJSONid(tx2, plain, tagged, false))
	submit(tx1)
	submit(tx2)
	var s1, s2, note string
	app.db.QueryRow("SELECT status, review_note FROM submissions WHERE id = 1").Scan(&s1, &note)
	app.db.QueryRow("SELECT status FROM submissions WHERE id = 2").Scan(&s2)
	if s1 != "rejected" || note != "Replaced by a newer submission." || s2 != "waiting" {
		t.Fatalf("replace: %s %q, %s", s1, note, s2)
	}
	app.db.Exec("UPDATE users SET held = 1")
	ex.set(tx2, txJSONid(tx2, plain, tagged, true))
	app.recheckWaiting(time.Now())
	var bal int64
	app.db.QueryRow("SELECT status FROM submissions WHERE id = 2").Scan(&s2)
	app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger").Scan(&bal)
	if s2 != "pending" || bal != 0 {
		t.Fatalf("held: %s, balance %d", s2, bal)
	}
}

// fakeX answers X's embedded-post endpoint for the posts given.
func fakeX(t *testing.T, posts map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := posts[r.URL.Query().Get("id")]
		if r.URL.Path != "/tweet-result" || !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerificationsDecidedByTheCheck(t *testing.T) {
	app, srv, _ := newTestServer(t)
	old := strconv.FormatUint(uint64(time.Now().AddDate(-3, 0, 0).UnixMilli()-xSnowflakeEpoch)<<22, 10)
	young := strconv.FormatUint(uint64(time.Now().AddDate(-1, 0, 0).UnixMilli()-xSnowflakeEpoch)<<22, 10)
	post := func(handle, id, text string) string {
		return fmt.Sprintf(`{"__typename":"Tweet","text":%q,"user":{"screen_name":%q,"id_str":%q}}`, text, handle, id)
	}
	codes := map[string]string{}
	newAccount := func(email, ip string) (*http.Client, string) {
		c := &http.Client{Jar: newJar()}
		from(t, c, ip, srv.URL+"/register", url.Values{"email": {email}, "password": {"a-long-password"}})
		var code string
		app.db.QueryRow("SELECT claim_code FROM users WHERE email = ?", email).Scan(&code)
		codes[email] = code
		return c, csrfOf(t, c, srv.URL+"/account")
	}
	alice, csrfA := newAccount("alice@example.com", "10.1.0.1")
	bob, csrfB := newAccount("bob@example.com", "10.1.0.2")
	carol, csrfC := newAccount("carol@example.com", "10.1.0.2") // bob's network
	dave, csrfD := newAccount("dave@example.com", "10.1.0.4")
	x := fakeX(t, map[string]string{
		"11001": post("alice_x", old, "joining "+codes["alice@example.com"]),
		"11002": post("bob_x", old, "me "+codes["bob@example.com"]),
		"11003": post("carol_x", old, "me "+codes["carol@example.com"]),
		"11004": post("dave_x", young, "me "+codes["dave@example.com"]),
		"11005": post("dave_x", old, "no code here"),
	})
	app.cfg.XBase = x.URL
	verify := func(c *http.Client, csrf, ip, link string) string {
		return from(t, c, ip, srv.URL+"/account/verify", url.Values{"csrf": {csrf}, "platform": {"x"}, "post": {link}})
	}
	status := func(email string) (string, string) {
		var s, n string
		app.db.QueryRow(`SELECT v.status, v.check_note FROM verifications v JOIN users u ON u.id = v.user_id
			WHERE u.email = ? ORDER BY v.id DESC LIMIT 1`, email).Scan(&s, &n)
		return s, n
	}
	bonus := func(email string) int64 {
		var b int64
		app.db.QueryRow(`SELECT COALESCE(SUM(l.amount), 0) FROM ledger l JOIN users u ON u.id = l.user_id
			WHERE u.email = ? AND l.kind = 'verification'`, email).Scan(&b)
		return b
	}

	// A clean check on an account that shares nothing: approved at once.
	if m := verify(alice, csrfA, "10.1.0.1", "https://x.com/alice_x/status/11001"); !strings.Contains(m, "Verified: your X account is linked") {
		t.Fatalf("clean: %q", m)
	}
	if s, _ := status("alice@example.com"); s != "verified" || bonus("alice@example.com") != verificationBonus {
		t.Fatalf("clean: %s, bonus %d", s, bonus("alice@example.com"))
	}
	// A clean check on an account sharing a network address: a reviewer
	// decides, and the note says what it shares.
	if m := verify(bob, csrfB, "10.1.0.2", "https://x.com/bob_x/status/11002"); !strings.Contains(m, "A reviewer confirms it") {
		t.Fatalf("shared: %q", m)
	}
	s, n := status("bob@example.com")
	if s != "pending" || !strings.Contains(n, "for a reviewer: this account shares a network address with 1 other account") || bonus("bob@example.com") != 0 {
		t.Fatalf("shared: %s %q", s, n)
	}
	verify(carol, csrfC, "10.1.0.2", "https://x.com/carol_x/status/11003")
	if s, _ := status("carol@example.com"); s != "pending" {
		t.Fatalf("shared, the other account: %s", s)
	}
	pend, _ := pendingVerifications(app.db)
	if len(pend) != 2 {
		t.Fatalf("queue holds %d verifications, want the 2 that share a signal", len(pend))
	}
	// Deterministic failures: refused with the reason, never stored.
	if m := verify(dave, csrfD, "10.1.0.4", "https://x.com/dave_x/status/11004"); !strings.Contains(m, "Not verified: post by @dave_x contains the code; account created") || !strings.Contains(m, "UNDER 2 YEARS") {
		t.Fatalf("young: %q", m)
	}
	if m := verify(dave, csrfD, "10.1.0.4", "https://x.com/dave_x/status/11005"); !strings.Contains(m, "Not verified: code NOT found in the post") {
		t.Fatalf("no code: %q", m)
	}
	if m := verify(dave, csrfD, "10.1.0.4", "https://x.com/dave_x/status/11999"); !strings.Contains(m, "Not verified: post NOT FOUND") {
		t.Fatalf("no post: %q", m)
	}
	if s, _ := status("dave@example.com"); s != "" {
		t.Fatalf("a refused verification was stored: %s", s)
	}
	// A held account's clean check waits for a reviewer.
	app.db.Exec("UPDATE users SET held = 1 WHERE email = 'dave@example.com'")
	x2 := fakeX(t, map[string]string{"11006": post("dave_x", old, "me "+codes["dave@example.com"])})
	app.cfg.XBase = x2.URL
	verify(dave, csrfD, "10.1.0.4", "https://x.com/dave_x/status/11006")
	if s, _ := status("dave@example.com"); s != "pending" {
		t.Fatalf("held: %s", s)
	}
}

// The Telegram check reads a message sent to the bot, and the account page
// says exactly that, naming the bot.
func TestTelegramBotVerification(t *testing.T) {
	app, srv, _ := newTestServer(t)
	c := &http.Client{Jar: newJar()}
	from(t, c, "10.2.0.1", srv.URL+"/register", url.Values{"email": {"tg@example.com"}, "password": {"a-long-password"}})
	var code string
	app.db.QueryRow("SELECT claim_code FROM users WHERE email = 'tg@example.com'").Scan(&code)
	var mu sync.Mutex
	updates := []map[string]any{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			w.Write([]byte(`{"ok":true,"result":{"username":"EmissioCheckBot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	app.cfg.TgBotToken, app.cfg.TgAPIBase = "123:test", api.URL
	page := get(t, c, srv.URL+"/account")
	if !strings.Contains(page, "as a Telegram message to @EmissioCheckBot") || !strings.Contains(page, "not your profile or bio") || strings.Contains(page, "Telegram bio") {
		t.Fatal("the account page does not say the check reads a message to the bot")
	}
	csrf := csrfOf(t, c, srv.URL+"/account")
	verify := func(handle string) string {
		return from(t, c, "10.2.0.1", srv.URL+"/account/verify", url.Values{"csrf": {csrf}, "platform": {"telegram"}, "handle": {handle}})
	}
	msg := func(id int64, user, text string) map[string]any {
		return map[string]any{"message": map[string]any{"text": text, "from": map[string]any{"id": id, "username": user}}}
	}
	// No message to the bot: refused with what to do, nothing stored.
	if m := verify("old_tg"); !strings.Contains(m, "Not verified: no message with your account code from @old_tg reached the bot") {
		t.Fatalf("no message: %q", m)
	}
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM verifications").Scan(&n)
	if n != 0 {
		t.Fatal("a refused Telegram verification was stored")
	}
	// An id newer than the newest anchor gives no age the check can vouch
	// for: a reviewer decides.
	mu.Lock()
	updates = []map[string]any{msg(9000000000, "new_tg", "code "+code)}
	mu.Unlock()
	verify("new_tg")
	var status, note string
	app.db.QueryRow("SELECT status, check_note FROM verifications ORDER BY id DESC LIMIT 1").Scan(&status, &note)
	if status != "pending" || !strings.Contains(note, "younger than this estimate") {
		t.Fatalf("weak estimate: %s %q", status, note)
	}
	app.db.Exec("UPDATE verifications SET status = 'rejected'")
	// A message from an old account: approved at once.
	mu.Lock()
	updates = []map[string]any{msg(54845238, "old_tg", "here is "+code)}
	mu.Unlock()
	if m := verify("old_tg"); !strings.Contains(m, "Verified: your Telegram account is linked") {
		t.Fatalf("old account: %q", m)
	}
}

// Reports our own automated checks file never reach the queue or a reward.
func TestTestReportsStayOutOfTheQueue(t *testing.T) {
	app, srv, client := newTestServer(t)
	body := func(desc string, test bool) string {
		m := map[string]any{"v": 1, "wallet": "web-wallet", "description": desc}
		if test {
			m["test"] = true
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	s, out, _ := intake(t, srv.URL, "10.3.0.1", "application/json", body("end-to-end check of the button", true))
	if s != 201 || out["test"] != true {
		t.Fatalf("flagged intake: %d %v", s, out)
	}
	s, out, _ = intake(t, srv.URL, "10.3.0.2", "application/json", body("EMISSIO-TEST typed into the wallet's own box", false))
	if s != 201 || out["test"] != true {
		t.Fatalf("marked intake: %d %v", s, out)
	}
	s, out, _ = intake(t, srv.URL, "10.3.0.3", "application/json", body("the send button does nothing", false))
	if s != 201 || out["test"] != nil {
		t.Fatalf("real intake: %d %v", s, out)
	}
	register(t, client, srv.URL, "drive@example.com")
	csrf := csrfOf(t, client, srv.URL+"/bugs")
	postBug(t, client, srv.URL, csrf, map[string]string{"surface": "faucet", "title": "EMISSIO-TEST form drive", "steps": "s", "actual": "a"}, nil)
	queued, _ := queryBugs(app.db, "WHERE b.status = 'new' ORDER BY b.id")
	if len(queued) != 1 || queued[0].Title != "the send button does nothing" {
		t.Fatalf("queue holds %d reports, want only the real one", len(queued))
	}
	var tests int
	app.db.QueryRow("SELECT COUNT(*) FROM bugs WHERE status = 'test'").Scan(&tests)
	if tests != 3 {
		t.Fatalf("%d test reports, want 3", tests)
	}
	var id int64
	app.db.QueryRow("SELECT id FROM bugs WHERE status = 'test' AND user_id > 0").Scan(&id)
	if err := reviewBug(app.db, id, "accepted", "minor", 0, "x"); err == nil {
		t.Fatal("a test report was accepted")
	}
	var bal int64
	app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger").Scan(&bal)
	if bal != 0 {
		t.Fatalf("a test report earned %d", bal)
	}
}
