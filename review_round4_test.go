package main

// Regression tests for the fourth review round: a referral decision read
// inside the payment's own transaction, so that no qualification already in
// flight can pay or queue a referral that was rejected or paid meanwhile;
// a hold on either side shown on the referral item and never paid; and the
// decision's wording.

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func referralPaid(app *App) (rows int, sum int64) {
	app.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(amount),0) FROM ledger WHERE kind IN ('referral','referral-welcome')").Scan(&rows, &sum)
	return
}

func referralStatus(app *App, referee int64) string {
	var s string
	if app.db.QueryRow("SELECT status FROM referral_reviews WHERE referee_id = ?", referee).Scan(&s) != nil {
		return "(no row)"
	}
	return s
}

// qualifyingPair makes a referrer and a referee that meet every referral
// condition, on one network address (linked) or on two.
func qualifyingPair(app *App, linked bool) (r, e int64) {
	r, _ = createUser(app.db, "referrer@example.com", "x", "1111111111", "")
	e, _ = createUser(app.db, "referee@example.com", "x", "2222222222", "")
	app.db.Exec("UPDATE users SET referred_by = ? WHERE id = ?", r, e)
	for i, id := range []int64{r, e} {
		ip := fmt.Sprintf("10.120.0.%d", i+1)
		if linked {
			ip = "10.120.0.99"
		}
		app.db.Exec("INSERT INTO user_ips (user_id, ip, first_seen, last_seen) VALUES (?, ?, 0, 0)", id, ip)
		app.db.Exec("INSERT INTO verifications (user_id, platform, handle, evidence, check_note, status, created_at) VALUES (?, 'x', ?, '', '', 'verified', 0)", id, fmt.Sprintf("h%d", id))
		app.db.Exec("INSERT INTO ledger (user_id, amount, kind, note, created_at) VALUES (?, 60, 'adjustment', 'work', 0)", id)
	}
	return
}

// waitFor polls until cond holds, for up to five seconds.
func waitFor(cond func() bool) bool {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// Many qualification checks of one linked referral run at once. While they
// are in flight the first sends it to a reviewer, the reviewer rejects it,
// and the pair's shared address is cleared. No check still in flight pays
// it: a rejection is final (KR98).
func TestRejectedReferralStaysUnpaidUnderRace(t *testing.T) {
	for round := 0; round < 12; round++ {
		app, _, _ := newTestServer(t)
		r, e := qualifyingPair(app, true)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); app.processReferrals(e) }()
		}
		if !waitFor(func() bool { return referralStatus(app, e) == "pending" }) {
			wg.Wait()
			t.Fatalf("round %d: the linked referral never reached the queue (status %s)", round, referralStatus(app, e))
		}
		if err := reviewReferral(app.db, e, false, "one person, two accounts"); err != nil {
			t.Fatalf("round %d: reject: %v", round, err)
		}
		for _, p := range [][2]int64{{r, e}, {e, r}} {
			app.db.Exec("INSERT INTO cleared_signals (user_id, other_id, kind, value, admin_id, created_at) VALUES (?, ?, 'ip', '10.120.0.99', 0, 0)", p[0], p[1])
		}
		wg.Wait()
		app.processReferrals(e)
		if rows, sum := referralPaid(app); rows != 0 || referralStatus(app, e) != "rejected" {
			t.Fatalf("round %d: a rejected referral was paid by checks in flight: %d rows, %d SEQ, status %s", round, rows, sum, referralStatus(app, e))
		}
	}
}

// Many qualification checks of one unlinked referral run at once, and the
// pair comes to share an address just after the first pays. No check still
// in flight leaves a review row for the referral already paid (RV91 R3-2).
func TestPaidReferralLeavesNoReviewRow(t *testing.T) {
	for round := 0; round < 12; round++ {
		app, _, _ := newTestServer(t)
		_, e := qualifyingPair(app, false)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); app.processReferrals(e) }()
		}
		if !waitFor(func() bool { rows, _ := referralPaid(app); return rows == 2 }) {
			wg.Wait()
			t.Fatalf("round %d: the unlinked referral was never paid", round)
		}
		app.db.Exec("INSERT INTO user_ips (user_id, ip, first_seen, last_seen) VALUES (?, '10.120.0.1', 0, 0)", e) // the referrer's address
		wg.Wait()
		if rows, _ := referralPaid(app); rows != 2 || referralStatus(app, e) != "(no row)" {
			t.Fatalf("round %d: paid rows %d, review row %s: a check in flight queued a referral already paid", round, rows, referralStatus(app, e))
		}
	}
}

// A review row left for a referral already paid (as an older build could
// leave one) is closed as paid by either decision: approving does not show
// a raw ledger error, and rejecting does not mark a paid referral rejected.
func TestReviewOfAPaidReferralClosesIt(t *testing.T) {
	for _, approve := range []bool{true, false} {
		app, _, _ := newTestServer(t)
		r, e := qualifyingPair(app, false)
		app.processReferrals(e)
		app.db.Exec("INSERT INTO referral_reviews (referee_id, referrer_id, status, note, created_at) VALUES (?, ?, 'pending', 'stale', 0)", e, r)
		err := reviewReferral(app.db, e, approve, "x")
		rows, _ := referralPaid(app)
		var note string
		app.db.QueryRow("SELECT review_note FROM referral_reviews WHERE referee_id = ?", e).Scan(&note)
		if err != nil || rows != 2 || referralStatus(app, e) != "approved" || !strings.Contains(note, "already paid") {
			t.Fatalf("approve=%v on a paid referral: err %v, rows %d, status %s, note %q", approve, err, rows, referralStatus(app, e), note)
		}
	}
}

// A hold on either side shows on the referral item, and approving it pays
// nothing while a side is held; released, the approval pays (RV91 R3-1).
// The decision reads "rejected" (RV91 R3-3).
func TestReferralItemAndHolds(t *testing.T) {
	for _, side := range []string{"referrer", "referee"} {
		t.Run(side, func(t *testing.T) {
			app, srv, _ := newTestServer(t)
			r, e := qualifyingPair(app, true)
			app.processReferrals(e)
			heldID := r
			if side == "referee" {
				heldID = e
			}
			app.db.Exec("UPDATE users SET held = 1 WHERE id = ?", heldID)
			c := &http.Client{Jar: newJar(), Transport: fromIP("10.130.0.1")}
			register(t, c, srv.URL, "boss@example.com")
			app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'boss@example.com'")
			page := get(t, c, srv.URL+"/admin/queue?kind=referral")
			item := page[strings.Index(page, "Referral by account"):]
			item = item[:strings.Index(item, "</details>")]
			if !strings.Contains(item, `<span class="pill rejected">held</span>`) || !strings.Contains(item, fmt.Sprintf("the %s, account #%d, is held", side, heldID)) {
				t.Fatalf("the item does not show the %s's hold: %s", side, item)
			}
			csrf := csrfOf(t, c, srv.URL+"/admin/queue")
			decide := func(action string) string {
				return from(t, c, "10.130.0.1", srv.URL+fmt.Sprintf("/admin/referrals/%d/review", e), map[string][]string{"csrf": {csrf}, "action": {action}, "note": {"looked"}})
			}
			if m := decide("approve"); !strings.Contains(m, fmt.Sprintf("account %d is held", heldID)) {
				t.Fatalf("approving with the %s held: %q", side, m)
			}
			if rows, _ := referralPaid(app); rows != 0 || referralStatus(app, e) != "pending" {
				t.Fatalf("a held %s was paid: %d rows, status %s", side, rows, referralStatus(app, e))
			}
			app.db.Exec("UPDATE users SET held = 0 WHERE id = ?", heldID)
			if m := decide("approve"); !strings.Contains(m, "approved") {
				t.Fatalf("approving once released: %q", m)
			}
			if rows, _ := referralPaid(app); rows != 2 {
				t.Fatalf("released and approved: %d rows", rows)
			}
		})
	}
	app, srv, _ := newTestServer(t)
	_, e := qualifyingPair(app, true)
	app.processReferrals(e)
	c := &http.Client{Jar: newJar(), Transport: fromIP("10.130.0.2")}
	register(t, c, srv.URL, "boss2@example.com")
	app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'boss2@example.com'")
	csrf := csrfOf(t, c, srv.URL+"/admin/queue")
	if m := from(t, c, "10.130.0.2", srv.URL+fmt.Sprintf("/admin/referrals/%d/review", e), map[string][]string{"csrf": {csrf}, "action": {"reject"}, "note": {"same person"}}); m != fmt.Sprintf("Referral of account %d rejected.", e) {
		t.Fatalf("reject message: %q", m)
	}
}
