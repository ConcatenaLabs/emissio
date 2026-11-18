package main

// Regression tests for the third review round of the credit boundary: a
// clearance covers the signals a reviewer judged and nothing else; the
// funding an automatic credit weighs comes from the transaction the check
// judged, and with none at hand the credit does not go ahead; a referral is
// an automatic credit like any other.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func ledgerTotal(app *App) int64 {
	var b int64
	app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger").Scan(&b)
	return b
}

// A reviewer clears the network address two accounts share. A new value
// the same pair comes to share later, of each kind (another network
// address, a browser, a chain address that funds the evidence), sends the
// next credit to a reviewer; the cleared address alone does not.
func TestClearanceCoversOnlyTheSignalsJudged(t *testing.T) {
	tag := int64(86718345)
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	plain := []map[string]any{{"is_coinbase": false}}
	shared := "tb1qsharedfunder000000000000000000000000000"
	fundedByShared := []map[string]any{{"is_coinbase": false, "txid": strings.Repeat("99", 32), "vout": 0, "prevout": map[string]any{"scriptpubkey_address": shared, "value": 200000000}}}
	cases := []struct {
		name  string
		share func(app *App, a, b int64)
		vin   []map[string]any
		want  string
	}{
		{"network address", func(app *App, a, b int64) {
			for _, id := range []int64{a, b} {
				app.db.Exec("INSERT INTO user_ips (user_id, ip, first_seen, last_seen) VALUES (?, '10.250.0.9', 0, 0)", id)
			}
		}, plain, "shares a network address with 1 other account"},
		{"browser", func(app *App, a, b int64) {
			for _, id := range []int64{a, b} {
				app.db.Exec("INSERT INTO user_devices (user_id, device, first_seen, last_seen) VALUES (?, 'feedfacefeedfacefeedfacefeedface', 0, 0)", id)
			}
		}, plain, "shares a browser with 1 other account"},
		{"chain address", func(app *App, a, b int64) {
			app.db.Exec("INSERT INTO chain_links (user_id, address, role, created_at) VALUES (?, ?, 'funded-by', 0)", a, shared)
		}, fundedByShared, "shares a chain address with 1 other account"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := &switchExplorer{txs: map[string]string{}}
			explorer := httptest.NewServer(ex)
			defer explorer.Close()
			app, srv, _ := newTestServer(t)
			app.cfg.EsploraURL, app.cfg.PolicyAsset = explorer.URL, policy
			b := &http.Client{Jar: newJar(), Transport: fromIP("10.81.0.1")}
			register(t, b, srv.URL, "b@example.com")
			app.db.Exec("UPDATE users SET claim_code = '0123456789' WHERE email = 'b@example.com'")
			var bID int64
			app.db.QueryRow("SELECT id FROM users WHERE email = 'b@example.com'").Scan(&bID)
			aID, _ := createUser(app.db, "a@example.com", "x", "ffffffffff", "")
			app.db.Exec("INSERT INTO user_ips (user_id, ip, first_seen, last_seen) VALUES (?, '10.81.0.1', 0, 0)", aID)
			// The reviewer judges the shared router and clears it.
			app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = ?", bID)
			adm := csrfOf(t, b, srv.URL+fmt.Sprintf("/admin/users/%d", bID))
			resp, _ := b.PostForm(srv.URL+fmt.Sprintf("/admin/users/%d/clear-links", bID), url.Values{"csrf": {adm}, "note": {"two flatmates, one router"}})
			resp.Body.Close()
			if m := resp.Request.URL.Query().Get("m"); !strings.Contains(m, "Cleared 1 shared signals with 1 accounts") {
				t.Fatalf("clear: %q", m)
			}
			app.db.Exec("UPDATE users SET is_admin = 0 WHERE id = ?", bID)
			// What was cleared: that address, between that pair, both ways,
			// under the admin who judged it; and the judgment is logged.
			var rows, byAdmin int
			app.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(admin_id = ?), 0) FROM cleared_signals
				WHERE kind = 'ip' AND value = '10.81.0.1' AND ((user_id = ? AND other_id = ?) OR (user_id = ? AND other_id = ?))`,
				bID, bID, aID, aID, bID).Scan(&rows, &byAdmin)
			var action, logged string
			var logAdmin int64
			app.db.QueryRow("SELECT action, note, admin_id FROM admin_log ORDER BY id DESC LIMIT 1").Scan(&action, &logged, &logAdmin)
			if rows != 2 || byAdmin != 2 || action != "clear-links" || logAdmin != bID || !strings.Contains(logged, "two flatmates, one router") || ledgerTotal(app) != 0 {
				t.Fatalf("cleared rows %d (by the admin %d), log %s %q by %d, ledger %d", rows, byAdmin, action, logged, logAdmin, ledgerTotal(app))
			}
			csrf := csrfOf(t, b, srv.URL+"/account")
			submit := func(slug, txid string, vin []map[string]any) string {
				ex.set(txid, txJSONid(txid, vin, tagged, true))
				resp, err := b.PostForm(srv.URL+"/tasks/"+slug+"/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp.Request.URL.Query().Get("m")
			}
			// The cleared address alone: credited.
			if m := submit("first-transaction", fmt.Sprintf("c%d", i)+strings.Repeat("0", 62), plain); !strings.Contains(m, "Checked and credited") {
				t.Fatalf("the cleared signal alone held the credit back: %q", m)
			}
			// A new value between the same pair: a reviewer looks again.
			c.share(app, aID, bID)
			app.db.Exec("UPDATE tasks SET check_kind = 'first-transaction' WHERE slug = 'multi-asset-send'")
			txid := fmt.Sprintf("d%d", i) + strings.Repeat("0", 62)
			if m := submit("multi-asset-send", txid, c.vin); !strings.Contains(m, "A reviewer confirms it") {
				t.Fatalf("a new %s between the cleared pair was not weighed: %q, ledger %d", c.name, m, ledgerTotal(app))
			}
			var note string
			app.db.QueryRow("SELECT chain_note FROM submissions WHERE txid = ?", txid).Scan(&note)
			if !strings.Contains(note, "waiting for a reviewer: this account "+c.want) || ledgerTotal(app) != 10 {
				t.Fatalf("note %q, ledger %d", note, ledgerTotal(app))
			}
		})
	}
}

// The funding of the evidence comes from the transaction the check judged:
// an explorer that answers once and fails after cannot drop it, on the
// submit path or the poller's, and the credit is held back for the shared
// address it shows.
func TestFundingComesFromTheCheckedTransaction(t *testing.T) {
	tag := int64(86718345)
	tagged := []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}
	shared := "tb1qsharedfunder000000000000000000000000000"
	vin := []map[string]any{{"is_coinbase": false, "txid": strings.Repeat("99", 32), "vout": 0, "prevout": map[string]any{"scriptpubkey_address": shared, "value": 200000000}}}
	for _, path := range []string{"submit", "poller"} {
		t.Run(path, func(t *testing.T) {
			txid := "d3" + strings.Repeat("0", 62)
			var mu sync.Mutex
			calls, answers := 0, 1
			confirmed := path == "submit"
			explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				ok, c := calls <= answers, confirmed
				mu.Unlock()
				if strings.HasPrefix(r.URL.Path, "/tx/") && !ok {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				w.Write([]byte(txJSONid(txid, vin, tagged, c)))
			}))
			defer explorer.Close()
			app, srv, _ := newTestServer(t)
			app.cfg.EsploraURL, app.cfg.PolicyAsset = explorer.URL, policy
			c := &http.Client{Jar: newJar(), Transport: fromIP("10.93.0.1")}
			register(t, c, srv.URL, "f@example.com")
			app.db.Exec("UPDATE users SET claim_code = '0123456789' WHERE email = 'f@example.com'")
			var me int64
			app.db.QueryRow("SELECT id FROM users WHERE email = 'f@example.com'").Scan(&me)
			farm, _ := createUser(app.db, "farm@example.com", "x", "ffffffffff", "")
			app.db.Exec("INSERT INTO chain_links (user_id, address, role, created_at) VALUES (?, ?, 'funded-by', 0)", farm, shared)
			csrf := csrfOf(t, c, srv.URL+"/account")
			resp, _ := c.PostForm(srv.URL+"/tasks/first-transaction/submit", url.Values{"csrf": {csrf}, "txid": {txid}})
			resp.Body.Close()
			m := resp.Request.URL.Query().Get("m")
			if path == "poller" {
				if !strings.Contains(m, "Not there yet") {
					t.Fatalf("mempool submission: %q", m)
				}
				mu.Lock()
				confirmed, calls, answers = true, 0, 1
				mu.Unlock()
				app.recheckWaiting(time.Now())
			}
			var status, note string
			app.db.QueryRow("SELECT status, chain_note FROM submissions").Scan(&status, &note)
			var mine int
			app.db.QueryRow("SELECT COUNT(*) FROM chain_links WHERE user_id = ? AND address = ?", me, shared).Scan(&mine)
			if status != "pending" || ledgerTotal(app) != 0 || !strings.Contains(note, "shares a chain address with 1 other account") || mine != 1 {
				t.Fatalf("%s path, explorer failing after the check's read: %s %q, ledger %d, this account's link %d (%q)", path, status, note, ledgerTotal(app), mine, m)
			}
		})
	}
}

// With no checked transaction at hand for a transaction check, the funding
// is unknown and the credit does not go ahead; a check whose evidence is
// not a transaction has no funding to read. readsTx agrees with which
// checks read a transaction.
func TestFundingFailsClosed(t *testing.T) {
	app, _, _ := newTestServer(t)
	txid := strings.Repeat("ab", 32)
	if _, ok := app.fundingOf("first-transaction", txid, checkResult{Verdict: vPass}); ok {
		t.Fatal("a pass with no checked transaction gave a funding to credit on")
	}
	if addrs, ok := app.fundingOf("wallet-link", txid, checkResult{Verdict: vPass}); !ok || addrs != nil {
		t.Fatalf("a check that reads no transaction: %v %v", addrs, ok)
	}
	if _, ok := app.fundingOf("first-transaction", "", checkResult{Verdict: vPass}); !ok {
		t.Fatal("no txid, nothing to read")
	}
	var mu sync.Mutex
	hits := 0
	explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tx/"+txid) {
			mu.Lock()
			hits++
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	defer explorer.Close()
	app.cfg.EsploraURL = explorer.URL
	user := &User{ID: 1, ClaimCode: "0123456789"}
	for _, k := range checkKinds {
		mu.Lock()
		hits = 0
		mu.Unlock()
		app.evidenceCheck(k.Key, txid, "", user)
		mu.Lock()
		read := hits > 0
		mu.Unlock()
		if read != readsTx(k.Key) {
			t.Errorf("check %s: reads the transaction %v, readsTx says %v", k.Key, read, readsTx(k.Key))
		}
	}
}

// A referral between accounts that share an uncleared signal is not paid
// automatically: it waits in the queue, a reviewer approves it and both
// sides are paid once, or rejects it and it is never paid.
func TestReferralThroughTheBoundary(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		t.Run(decision, func(t *testing.T) {
			app, srv, admin := newTestServer(t)
			r, _ := createUser(app.db, "ref@example.com", "x", "1111111111", "")
			e, _ := createUser(app.db, "ree@example.com", "x", "2222222222", "")
			app.db.Exec("UPDATE users SET referred_by = ? WHERE id = ?", r, e)
			for _, id := range []int64{r, e} {
				app.db.Exec("INSERT INTO user_ips (user_id, ip, first_seen, last_seen) VALUES (?, '10.94.0.1', 0, 0)", id)
				app.db.Exec("INSERT INTO verifications (user_id, platform, handle, evidence, check_note, status, created_at) VALUES (?, 'x', ?, '', '', 'verified', 0)", id, fmt.Sprintf("h%d", id))
				app.db.Exec("INSERT INTO ledger (user_id, amount, kind, note, created_at) VALUES (?, 60, 'adjustment', 'reviewer-approved work', 0)", id)
			}
			paid := func() int64 {
				var p int64
				app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger WHERE kind IN ('referral','referral-welcome')").Scan(&p)
				return p
			}
			app.processReferrals(e)
			app.processReferrals(r)
			var status, note string
			app.db.QueryRow("SELECT status, note FROM referral_reviews WHERE referee_id = ?", e).Scan(&status, &note)
			if paid() != 0 || status != "pending" || !strings.Contains(note, "the referee shares a network address with 1 other account") {
				t.Fatalf("a referral between linked accounts: paid %d, review %s %q", paid(), status, note)
			}
			register(t, admin, srv.URL, "boss@example.com")
			app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'boss@example.com'")
			q := get(t, admin, srv.URL+"/admin/queue?kind=referral")
			if !strings.Contains(q, fmt.Sprintf("Referral by account #%d of account #%d", r, e)) || !strings.Contains(q, fmt.Sprintf("/admin/referrals/%d/review", e)) {
				t.Fatal("the queue does not show the referral with its decision")
			}
			csrf := csrfOf(t, admin, srv.URL+"/admin/queue")
			post := func() string {
				resp, err := admin.PostForm(srv.URL+fmt.Sprintf("/admin/referrals/%d/review", e), url.Values{"csrf": {csrf}, "action": {decision}, "note": {"looked at both"}})
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp.Request.URL.Query().Get("e") + resp.Request.URL.Query().Get("m")
			}
			post()
			want := int64(0)
			if decision == "approve" {
				want = 2 * referralBonus
			}
			if paid() != want {
				t.Fatalf("%s: paid %d, want %d", decision, paid(), want)
			}
			if m := post(); !strings.Contains(m, "already") {
				t.Fatalf("a second decision: %q", m)
			}
			app.db.Exec("INSERT INTO ledger (user_id, amount, kind, note, created_at) VALUES (?, 5, 'adjustment', 'more work', 0)", e)
			app.processReferrals(e)
			if paid() != want {
				t.Fatalf("after the decision a later credit paid the referral again: %d", paid())
			}
		})
	}
}
