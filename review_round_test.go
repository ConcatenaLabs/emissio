package main

// Regression tests for the findings of the two reviews of the
// queue-only-judgment change, each built from the reviewer's probe: one
// platform verification per account under concurrent requests; a hold, or
// a shared signal, that lands while a check runs or while a submission
// waits; a judgment case met by the waiting poller; a waiting submission of
// another tester's transaction; the test marker as a whole word.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two requests at once for one account and one platform, each with a
// different qualifying social account: one is verified and paid, the other
// refused (RV91 F2, KR93 high 1).
func TestConcurrentVerificationsOneAccount(t *testing.T) {
	app, srv, _ := newTestServer(t)
	old := strconv.FormatUint(uint64(time.Now().AddDate(-3, 0, 0).UnixMilli()-xSnowflakeEpoch)<<22, 10)
	c := &http.Client{Jar: newJar()}
	from(t, c, "10.8.0.1", srv.URL+"/register", url.Values{"email": {"race@example.com"}, "password": {"a-long-password"}})
	var code string
	var uid int64
	app.db.QueryRow("SELECT id, claim_code FROM users WHERE email = 'race@example.com'").Scan(&uid, &code)
	post := func(handle string) string {
		return fmt.Sprintf(`{"__typename":"Tweet","text":%q,"user":{"screen_name":%q,"id_str":%q}}`, "me "+code, handle, old)
	}
	posts := map[string]string{"21001": post("first_x"), "21002": post("second_x")}
	x := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		if b, ok := posts[r.URL.Query().Get("id")]; ok {
			w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	}))
	defer x.Close()
	app.cfg.XBase = x.URL
	csrf := csrfOf(t, c, srv.URL+"/account")
	var wg sync.WaitGroup
	answers := make([]string, 2)
	for i, link := range []string{"https://x.com/first_x/status/21001", "https://x.com/second_x/status/21002"} {
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			answers[i] = from(t, c, "10.8.0.1", srv.URL+"/account/verify", url.Values{"csrf": {csrf}, "platform": {"x"}, "post": {link}})
		}(i, link)
	}
	wg.Wait()
	var verified, live int
	var bonus int64
	app.db.QueryRow("SELECT COUNT(*) FROM verifications WHERE user_id = ? AND platform = 'x' AND status = 'verified'", uid).Scan(&verified)
	app.db.QueryRow("SELECT COUNT(*) FROM verifications WHERE user_id = ? AND platform = 'x' AND status != 'rejected'", uid).Scan(&live)
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger WHERE user_id = ? AND kind = 'verification'", uid).Scan(&bonus)
	if verified != 1 || live != 1 || bonus != verificationBonus {
		t.Fatalf("one account, two X requests at once: verified %d, live %d, bonus %d (answers %q)", verified, live, bonus, answers)
	}
	refused := 0
	for _, m := range answers {
		if strings.Contains(m, "You already have a X verification on file") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("the second request was not told one is on file: %q", answers)
	}
	// The database refuses a second live row whatever path writes it.
	if _, err := app.db.Exec(`INSERT INTO verifications (user_id, platform, handle, evidence, check_note, created_at)
		VALUES (?, 'x', 'third_x', '', '', 0)`, uid); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second live X verification was stored: %v", err)
	}
	// A rejected one does not count.
	app.db.Exec("UPDATE verifications SET status = 'rejected' WHERE user_id = ?", uid)
	if _, err := app.db.Exec(`INSERT INTO verifications (user_id, platform, handle, evidence, check_note, created_at)
		VALUES (?, 'x', 'third_x', '', '', 0)`, uid); err != nil {
		t.Fatalf("a new request after a rejection was refused: %v", err)
	}
}

// gate is a stand-in service that holds its next answer until released.
type gate struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{arrived: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) wait() {
	g.once.Do(func() { close(g.arrived) })
	<-g.release
}

// A hold placed while a check is running is honoured at the credit, on
// every automatic path: the waiting poller, the submit path and a
// verification (KR93 high 2).
func TestHoldDuringCheck(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	plain := []map[string]any{{"is_coinbase": false}}
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	balance := func(app *App) int64 {
		var b int64
		app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger").Scan(&b)
		return b
	}

	t.Run("waiting poller", func(t *testing.T) {
		txid := "71" + strings.Repeat("0", 62)
		var mu sync.Mutex
		confirmed := false
		var g *gate
		explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			c, gg := confirmed, g
			mu.Unlock()
			if gg != nil {
				gg.wait()
			}
			w.Write([]byte(txJSONid(txid, plain, tagged, c)))
		}))
		defer explorer.Close()
		app, srv, client := newTestServer(t)
		app.cfg.EsploraURL, app.cfg.PolicyAsset = explorer.URL, policy
		register(t, client, srv.URL, "hold1@example.com")
		app.db.Exec("UPDATE users SET claim_code = ?", code)
		csrf := csrfOf(t, client, srv.URL+"/account")
		resp, _ := client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
		resp.Body.Close()
		mu.Lock()
		confirmed, g = true, newGate()
		gg := g
		mu.Unlock()
		done := make(chan struct{})
		go func() { app.recheckWaiting(time.Now()); close(done) }()
		<-gg.arrived
		app.db.Exec("UPDATE users SET held = 1")
		close(gg.release)
		<-done
		var status string
		app.db.QueryRow("SELECT status FROM submissions").Scan(&status)
		if status != "pending" || balance(app) != 0 {
			t.Fatalf("held while the poller checked: status %s, balance %d", status, balance(app))
		}
	})

	t.Run("submit", func(t *testing.T) {
		txid := "72" + strings.Repeat("0", 62)
		g := newGate()
		explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.wait()
			w.Write([]byte(txJSONid(txid, plain, tagged, true)))
		}))
		defer explorer.Close()
		app, srv, client := newTestServer(t)
		app.cfg.EsploraURL, app.cfg.PolicyAsset = explorer.URL, policy
		register(t, client, srv.URL, "hold2@example.com")
		app.db.Exec("UPDATE users SET claim_code = ?", code)
		csrf := csrfOf(t, client, srv.URL+"/account")
		done := make(chan string)
		go func() {
			resp, err := client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
			if err != nil {
				done <- err.Error()
				return
			}
			resp.Body.Close()
			done <- resp.Request.URL.Query().Get("m")
		}()
		<-g.arrived
		app.db.Exec("UPDATE users SET held = 1")
		close(g.release)
		m := <-done
		var status string
		app.db.QueryRow("SELECT status FROM submissions").Scan(&status)
		if status != "pending" || balance(app) != 0 || strings.Contains(m, "credited:") {
			t.Fatalf("held while the submit check ran: status %s, balance %d, %q", status, balance(app), m)
		}
	})

	t.Run("verification", func(t *testing.T) {
		app, srv, _ := newTestServer(t)
		old := strconv.FormatUint(uint64(time.Now().AddDate(-3, 0, 0).UnixMilli()-xSnowflakeEpoch)<<22, 10)
		c := &http.Client{Jar: newJar()}
		from(t, c, "10.7.0.1", srv.URL+"/register", url.Values{"email": {"hold3@example.com"}, "password": {"a-long-password"}})
		var code string
		app.db.QueryRow("SELECT claim_code FROM users").Scan(&code)
		g := newGate()
		x := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.wait()
			fmt.Fprintf(w, `{"__typename":"Tweet","text":%q,"user":{"screen_name":"held_x","id_str":%q}}`, "me "+code, old)
		}))
		defer x.Close()
		app.cfg.XBase = x.URL
		csrf := csrfOf(t, c, srv.URL+"/account")
		done := make(chan string)
		go func() {
			done <- from(t, c, "10.7.0.1", srv.URL+"/account/verify", url.Values{"csrf": {csrf}, "platform": {"x"}, "post": {"https://x.com/held_x/status/31001"}})
		}()
		<-g.arrived
		app.db.Exec("UPDATE users SET held = 1")
		close(g.release)
		m := <-done
		var status, note string
		app.db.QueryRow("SELECT status, check_note FROM verifications").Scan(&status, &note)
		if status != "pending" || balance(app) != 0 || !strings.Contains(note, "waiting for a reviewer: the account is held") {
			t.Fatalf("held while the verification check ran: %s %q, balance %d, %q", status, note, balance(app), m)
		}
	})
}

// A waiting submission whose account comes to share a network address, a
// browser or a chain address with another account before its evidence
// arrives goes to a reviewer with the signal in its note; a reviewer's
// clearance of those links lets the next credit through; and the immediate
// submit path weighs the signals the same way (KR93 high 3).
func TestSignalsAtTheCredit(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	fundedBy := "tb1qfunderaddress0000000000000000000000000"
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	vin := []map[string]any{{"is_coinbase": false, "txid": strings.Repeat("99", 32), "vout": 0, "prevout": map[string]any{"scriptpubkey_address": fundedBy, "value": 200000000}}}
	signalsCase := []struct {
		name, want string
		share      func(app *App, me, other int64)
	}{
		{"network address", "shares a network address with 1 other account", func(app *App, me, other int64) {
			app.db.Exec("INSERT INTO user_ips (user_id, ip, first_seen, last_seen) SELECT ?, ip, 0, 0 FROM user_ips WHERE user_id = ?", other, me)
		}},
		{"browser", "shares a browser with 1 other account", func(app *App, me, other int64) {
			app.db.Exec("INSERT INTO user_devices (user_id, device, first_seen, last_seen) SELECT ?, device, 0, 0 FROM user_devices WHERE user_id = ?", other, me)
		}},
		{"chain address", "shares a chain address with 1 other account", func(app *App, me, other int64) {
			app.db.Exec("INSERT INTO chain_links (user_id, address, role, created_at) VALUES (?, ?, 'funded-by', 0)", other, fundedBy)
		}},
	}
	for i, sc := range signalsCase {
		t.Run(sc.name, func(t *testing.T) {
			ex := &switchExplorer{txs: map[string]string{}}
			explorer := httptest.NewServer(ex)
			defer explorer.Close()
			app, srv, client := newTestServer(t)
			app.cfg.EsploraURL, app.cfg.PolicyAsset = explorer.URL, policy
			register(t, client, srv.URL, "me@example.com")
			app.db.Exec("UPDATE users SET claim_code = ? WHERE email = 'me@example.com'", code)
			other, _ := createUser(app.db, "other@example.com", "x", "ffffffffff", "")
			var me int64
			app.db.QueryRow("SELECT id FROM users WHERE email = 'me@example.com'").Scan(&me)
			csrf := csrfOf(t, client, srv.URL+"/account")
			txid := fmt.Sprintf("8%d", i) + strings.Repeat("0", 62)
			ex.set(txid, txJSONid(txid, vin, tagged, false))
			resp, _ := client.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
			resp.Body.Close()
			sc.share(app, me, other)
			ex.set(txid, txJSONid(txid, vin, tagged, true))
			app.recheckWaiting(time.Now())
			var status, note string
			var bal int64
			app.db.QueryRow("SELECT status, chain_note FROM submissions").Scan(&status, &note)
			app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger").Scan(&bal)
			if status != "pending" || bal != 0 || !strings.Contains(note, "waiting for a reviewer: this account "+sc.want) {
				t.Fatalf("signal acquired while waiting: %s %q, balance %d", status, note, bal)
			}
			// The submit path weighs the same signals.
			app.db.Exec("UPDATE tasks SET check_kind = 'first-transaction' WHERE slug = 'multi-asset-send'")
			txid3 := fmt.Sprintf("a%d", i) + strings.Repeat("0", 62)
			ex.set(txid3, txJSONid(txid3, vin, tagged, true))
			resp, _ = client.PostForm(srv.URL+"/tasks/multi-asset-send/submit", url.Values{"csrf": {csrf}, "txid": {txid3}})
			resp.Body.Close()
			if m := resp.Request.URL.Query().Get("m"); !strings.Contains(m, "A reviewer confirms it") {
				t.Fatalf("the submit path credited an account sharing a %s: %q", sc.name, m)
			}
			// A reviewer clears the links: the next credit goes through.
			app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = ?", me)
			admin := csrfOf(t, client, srv.URL+fmt.Sprintf("/admin/users/%d", me))
			resp, _ = client.PostForm(srv.URL+fmt.Sprintf("/admin/users/%d/clear-links", me), url.Values{"csrf": {admin}, "note": {"one household, two people"}})
			resp.Body.Close()
			if m := resp.Request.URL.Query().Get("m"); !strings.Contains(m, "shared signals with 1 accounts") {
				t.Fatalf("clear links: %q", m)
			}
			app.db.Exec("UPDATE tasks SET check_kind = 'first-transaction' WHERE slug = 'confidential-tx'")
			txid4 := fmt.Sprintf("b%d", i) + strings.Repeat("0", 62)
			ex.set(txid4, txJSONid(txid4, vin, tagged, true))
			resp, _ = client.PostForm(srv.URL+"/tasks/confidential-tx/submit", url.Values{"csrf": {csrf}, "txid": {txid4}})
			resp.Body.Close()
			if m := resp.Request.URL.Query().Get("m"); !strings.Contains(m, "Checked and credited") {
				t.Fatalf("after clearing: %q", m)
			}
			// A link to a new account still holds the next one back.
			third, _ := createUser(app.db, "third@example.com", "x", "eeeeeeeeee", "")
			sc.share(app, me, third)
			if sig := unclearedSignals(app.db, me, []string{fundedBy}); !sig.Any() {
				t.Fatalf("a new link after the clearance is not weighed: %+v", sig)
			}
		})
	}
}

// A case only a person can judge, met by the waiting poller, goes to the
// queue at once and never lapses as rejected; a check that could not run
// keeps waiting (RV91 F1).
func TestPollerSendsJudgmentToTheQueue(t *testing.T) {
	code := "0123456789"
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"addr": r.URL.Query().Get("addr"), "last": nil, "since": time.Now().Unix() - 86400, "dropped_last": nil})
	}))
	defer shared.Close()
	app, _, _ := newTestServer(t)
	app.cfg.AnchorSeenURL = shared.URL + "/last-seen"
	app.cfg.NodeDatadir = t.TempDir()
	peersFile := filepath.Join(t.TempDir(), "peers.json")
	app.cfg.BtcPeersFile = peersFile
	uid, _ := createUser(app.db, "own@example.com", "x", code, "")
	now := time.Now()
	writePeers := func(updated int64) {
		ps := []map[string]any{{"id": 10, "addr": "198.51.100.7:51234", "subver": "/Satoshi:28.1.0(emissio-" + code + ")/", "inbound": true, "services": []string{"NETWORK"},
			"conntime": now.Add(-20 * time.Minute).Unix(), "synced_headers": 119998, "synced_blocks": 119996, "first_height": 119998, "best_headers": 119998, "best_blocks": 119996, "listed": 12}}
		b, _ := json.Marshal(map[string]any{"updated": updated, "height": 120001, "peers": ps})
		os.WriteFile(peersFile, b, 0o600)
	}
	app.db.Exec("UPDATE tasks SET active = 1 WHERE check_kind = 'own-bitcoin-node'")
	var taskID int64
	app.db.QueryRow("SELECT id FROM tasks WHERE check_kind = 'own-bitcoin-node'").Scan(&taskID)
	insert := func() {
		app.db.Exec(`INSERT INTO submissions (user_id, task_id, txid, notes, status, chain_note, created_at) VALUES (?,?,?,?,?,?,?)`,
			uid, taskID, "", "", "waiting", "not yet", now.Unix())
	}
	status := func() (s, n string) {
		app.db.QueryRow("SELECT status, chain_note FROM submissions ORDER BY id DESC LIMIT 1").Scan(&s, &n)
		return
	}
	// A stale peer list: the check could not run. It keeps waiting.
	insert()
	writePeers(now.Unix() - 600)
	app.cfg.NodeCLI = fakeCLI(t, `[{"addr":"198.51.100.8:40000","subver":"/Sequentia Core:25.2.7(emissio-`+code+`)/"}]`)
	app.recheckWaiting(now)
	if s, n := status(); s != "waiting" || !strings.Contains(n, "out of date") {
		t.Fatalf("could not run: %s %q", s, n)
	}
	app.db.Exec("UPDATE submissions SET status = 'rejected'")
	// Two addresses: a person must judge. To the queue at the first poll.
	insert()
	writePeers(now.Unix())
	app.recheckWaiting(now)
	if s, n := status(); s != "pending" || !strings.Contains(n, "A reviewer looks at this one") {
		t.Fatalf("judgment case after one poll: %s %q", s, n)
	}
	queued, _ := pendingSubmissions(app.db)
	if len(queued) != 1 {
		t.Fatalf("queue holds %d submissions, want the judgment case", len(queued))
	}
	// It is no longer the poller's: a later poll past the lapse leaves it.
	app.cfg.NodeCLI = fakeCLI(t, `[]`)
	app.recheckWaiting(now.Add(73 * time.Hour))
	if s, _ := status(); s != "pending" {
		t.Fatalf("judgment case at the lapse: %s", s)
	}
}

// A waiting submission cannot park another tester's unconfirmed
// transaction: one without this account's proof decimals is refused at
// once, and a waiting row never keeps the transaction's owner out (RV91 F3).
func TestWaitingCannotSquatATxid(t *testing.T) {
	tag := int64(86718345)
	plain := []map[string]any{{"is_coinbase": false}}
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	ex := &switchExplorer{txs: map[string]string{}}
	explorer := httptest.NewServer(ex)
	defer explorer.Close()
	app, srv, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	owner := tester{client: &http.Client{Jar: newJar(), Transport: fromIP("10.6.0.1")}}
	squat := tester{client: &http.Client{Jar: newJar(), Transport: fromIP("10.6.0.2")}}
	register(t, owner.client, srv.URL, "owner@example.com")
	register(t, squat.client, srv.URL, "squat@example.com")
	app.db.Exec("UPDATE users SET claim_code = '0123456789' WHERE email = 'owner@example.com'")
	owner.csrf, squat.csrf = csrfOf(t, owner.client, srv.URL+"/account"), csrfOf(t, squat.client, srv.URL+"/account")
	txid := "f1" + strings.Repeat("0", 62)
	ex.set(txid, txJSONid(txid, plain, tagged, false))
	if q := squat.submit(t, srv.URL, "first-transaction", txid); !strings.Contains(q, "Not+accepted") || !strings.Contains(q, "NOT+found") {
		t.Fatalf("squatter: %s", q)
	}
	if q := owner.submit(t, srv.URL, "first-transaction", txid); !strings.Contains(q, "Not+there+yet") {
		t.Fatalf("owner of the unconfirmed transaction: %s", q)
	}
	ex.set(txid, txJSONid(txid, plain, tagged, true))
	app.recheckWaiting(time.Now())
	var status string
	app.db.QueryRow("SELECT s.status FROM submissions s JOIN users u ON u.id = s.user_id WHERE u.email = 'owner@example.com'").Scan(&status)
	if status != "approved" {
		t.Fatalf("owner after confirmation: %s", status)
	}

	// A check that cannot bind an unconfirmed transaction (no proof
	// decimals) can leave a waiting row: it is no claim on the transaction.
	// The owner is credited, and the waiting row, when its check passes, is
	// refused because another account claimed the transaction meanwhile.
	tx2 := "f2" + strings.Repeat("0", 62)
	var squatID, taskID int64
	var squatCode string
	app.db.QueryRow("SELECT id, claim_code FROM users WHERE email = 'squat@example.com'").Scan(&squatID, &squatCode)
	squatTag, _ := strconv.ParseInt(proofTag(squatCode), 10, 64)
	app.db.QueryRow("SELECT id FROM tasks WHERE slug = 'anchor-lookup'").Scan(&taskID)
	app.db.Exec("UPDATE tasks SET check_kind = 'first-transaction' WHERE id = ?", taskID)
	app.db.Exec(`INSERT INTO submissions (user_id, task_id, txid, notes, status, chain_note, created_at) VALUES (?,?,?,'','waiting','',?)`, squatID, taskID, tx2, time.Now().Unix())
	ex.set(tx2, txJSONid(tx2, plain, append([]map[string]any{vout(200000000+squatTag, policy, "v0_p2wpkh")}, tagged...), true))
	if q := owner.submit(t, srv.URL, "anchor-lookup", tx2); !strings.Contains(q, "Checked+and+credited") {
		t.Fatalf("a waiting row of another account kept the owner out: %s", q)
	}
	app.recheckWaiting(time.Now())
	var sq, note string
	app.db.QueryRow("SELECT status, review_note FROM submissions WHERE user_id = ? AND txid = ?", squatID, tx2).Scan(&sq, &note)
	if sq != "rejected" || note != "This transaction has been submitted by another account." {
		t.Fatalf("the waiting row of a claimed transaction: %s %q", sq, note)
	}
}

// The test marker is a whole word: a real report whose title only begins
// with the same letters is a real report (RV91 F4).
func TestTestMarkerIsAWord(t *testing.T) {
	for s, want := range map[string]bool{
		"EMISSIO-TEST":                    true,
		"emissio-test drive of the form":  true,
		"  EMISSIO-TEST: button":          true,
		"EMISSIO-TEST\nsteps":             true,
		"Emissio-testnet faucet is empty": false,
		"EMISSIO-TESTING":                 false,
		"EMISSIO-TEST-2":                  false,
		"a report about EMISSIO-TEST":     false,
	} {
		if got := isTestText(s); got != want {
			t.Errorf("isTestText(%q) = %v, want %v", s, got, want)
		}
	}
}
