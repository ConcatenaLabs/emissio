package main

import (
	"database/sql"
	"encoding/hex"
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

// addCode gives account 1 a code of purpose with this owner key and nonce.
func (r *arcaRig) addCode(purpose, owner, nonce string) *arcaCode {
	res, _ := r.app.db.Exec(`INSERT INTO arca_codes (user_id, purpose, request, owner, owner_nonce, created_at) VALUES (1, ?, 'arca:00', ?, ?, ?)`,
		purpose, owner, nonce, now())
	id, _ := res.LastInsertId()
	var c *arcaCode
	for _, x := range r.app.codesOf(1, purpose, 0) {
		if x.ID == id {
			c = x
		}
	}
	return c
}

func (r *arcaRig) paidAt(id int64) (paidAt int64, leaf string) {
	r.app.db.QueryRow("SELECT paid_at, paid_leaf FROM arca_codes WHERE id = ?", id).Scan(&paidAt, &leaf)
	return
}

// B1. A coin whose history holds a coin that paid code A pays the code its
// own leaf names, never A: on a real record whose input is a transfer.
func TestHistoryDoesNotShadowTheCode(t *testing.T) {
	r := newArcaRig(t)
	raw := realOf(t, "transfer", 1863)
	in := &recReader{b: raw, pos: 3}
	inner, _, _ := in.record(1)
	own, _, _ := ownLeaf(raw)
	a := r.addCode("code", inner.Owner, inner.Nonce) // the code its history paid
	c := r.addCode("swap", own.Owner, own.Nonce)     // the code it pays
	r.coin("real", "100000000", nil)
	r.rec("real", hex.EncodeToString(raw))
	r.app.followArca()
	if p, _ := r.paidAt(a.ID); p != 0 {
		t.Fatal("the coin paid the code of a coin in its history")
	}
	if p, leaf := r.paidAt(c.ID); p == 0 || leaf != "real" {
		t.Fatal("the code the coin pays stayed unpaid")
	}
	// And when the history's code was paid already, the coin is not "not counted".
	r2 := newArcaRig(t)
	a2 := r2.addCode("code", inner.Owner, inner.Nonce)
	r2.app.db.Exec("UPDATE arca_codes SET paid_at = 1, paid_leaf = 'earlier' WHERE id = ?", a2.ID)
	c2 := r2.addCode("swap", own.Owner, own.Nonce)
	r2.coin("real", "100000000", nil)
	r2.rec("real", hex.EncodeToString(raw))
	r2.app.followArca()
	if p, _ := r2.paidAt(c2.ID); p == 0 {
		t.Fatal("with the history's code paid, the coin's own code stayed unpaid")
	}
}

// B1. A payer's bytes outside the coin's leaf (an output script naming a
// code) pay nothing.
func TestCraftedScriptPaysNothing(t *testing.T) {
	r := newArcaRig(t)
	victim := r.addCode("code", randHex(32), randHex(32))
	mark, _ := hex.DecodeString(victim.Owner + victim.OwnerNonce)
	r.coin("crafted", "100000000", nil)
	r.rec("crafted", synthRecord(t, nil, randHex(32), randHex(32), mark))
	r.app.followArca()
	if p, _ := r.paidAt(victim.ID); p != 0 {
		t.Fatal("bytes in an output's script paid a code")
	}
}

// B2. Emissio never pays one of its own codes: not a code pasted in the
// round trip, not one a payment name resolves to.
func TestEmissioNeverPaysItsOwnCode(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	own := r.app.receiveCode(r.app.codeOf(1, "code"))
	if m := r.post("web-wallet-roundtrip", "wallet", url.Values{"code": {own}}); !strings.Contains(m, "one of Emissio's own receive codes") {
		t.Fatalf("own code: %q", m)
	}
	names := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": own, "fresh": true})
	}))
	defer names.Close()
	r.app.cfg.LnaddrURL, r.app.cfg.LnaddrDomain = names.URL, "sequentiatestnet.com"
	if m := r.post("web-wallet-name", "wallet", url.Values{"code": {"alice"}}); !strings.Contains(m, "one of Emissio's own receive codes") {
		t.Fatalf("own code by name: %q", m)
	}
	if r.calls("send") != 0 {
		t.Fatal("Emissio paid itself")
	}
}

// B3. The bot credits nothing to an account whose email is not confirmed,
// when the site confirms email; it does once the email is confirmed.
func TestBotPassesTheEmailGate(t *testing.T) {
	g := newTgRig(t)
	_, code, id := g.account(t, "unconfirmed@example.com", "10.4.0.1")
	g.app.mailer = &memMailer{}
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if st, _, _ := g.verif(id); st != "" {
		t.Fatalf("an unconfirmed account was verified: %s", st)
	}
	var bal int64
	g.app.db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM ledger WHERE user_id = ?", id).Scan(&bal)
	if bal != 0 {
		t.Fatalf("ledger %d", bal)
	}
	if a := g.bot.last(oldTG); !strings.Contains(a, "Nothing was linked") {
		t.Fatalf("answer: %q", a)
	}
	g.app.db.Exec("UPDATE users SET email_verified_at = 1 WHERE id = ?", id)
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if st, _, _ := g.verif(id); st != "verified" {
		t.Fatalf("after confirming: %s", st)
	}
}

// B4. A coin whose record cannot be read is asked for again for as long as
// it takes, less often as failures grow, and the desk names it until then.
func TestRecordNeverGivenUp(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.post("web-wallet-pay", "submit", url.Values{})
	r.coin("stubborn", "100000000", r.app.codeOf(1, "code"))
	os.WriteFile(filepath.Join(r.dir, "fail-record"), nil, 0o600)
	r.app.followArca()
	before := r.calls("record")
	r.app.followArca() // backing off: not asked again at once
	if r.calls("record") != before {
		t.Fatal("a failed record was asked for again at once")
	}
	for i := 0; i < 30; i++ {
		r.app.db.Exec("UPDATE arca_coins SET next_at = 0") // the backoff has passed
		r.app.followArca()
	}
	r.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
	desk := get(t, r.client, r.srv.URL+"/admin/users/1")
	if strings.Contains(desk, "all read") || !strings.Contains(desk, "stubborn") || !strings.Contains(desk, "could not be read") {
		t.Fatal("the desk does not name the coin whose record cannot be read")
	}
	os.Remove(filepath.Join(r.dir, "fail-record"))
	r.app.db.Exec("UPDATE arca_coins SET next_at = 0")
	r.app.followArca()
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("after 31 failed reads the coin was given up")
	}
	desk = get(t, r.client, r.srv.URL+"/admin/users/1")
	if !strings.Contains(desk, "all read") {
		t.Fatal("the desk does not say all is read once it is")
	}
}

// B4. A record in a form this Emissio cannot read is kept unread and named,
// never matched by searching it.
func TestUnreadableRecordIsKept(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	c := r.app.codeOf(1, "code")
	r.coin("strange", "100000000", nil)
	r.rec("strange", "09"+c.Owner+c.OwnerNonce) // an unknown format that carries the code's key and nonce
	r.app.followArca()
	var readAt int64
	var errText string
	r.app.db.QueryRow("SELECT read_at, error FROM arca_coins WHERE leaf_id = 'strange'").Scan(&readAt, &errText)
	if r.app.codeOf(1, "code").PaidAt != 0 || readAt != 0 || !strings.Contains(errText, "format") {
		t.Fatalf("an unreadable record: paid %d, read %d, %q", r.app.codeOf(1, "code").PaidAt, readAt, errText)
	}
}

// M1. Requests in parallel make one code, and Emissio pays an account at
// most paymentsPerTask times however many requests run at once.
func TestParallelRequestsKeepTheLimits(t *testing.T) {
	r := newArcaRig(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.post("web-wallet-btc", "wallet", url.Values{}) }()
	}
	wg.Wait()
	var n int
	r.app.db.QueryRow("SELECT COUNT(*) FROM arca_codes WHERE purpose = 'btc'").Scan(&n)
	if n != 1 {
		t.Fatalf("20 parallel requests made %d codes", n)
	}
	det, _ := json.Marshal(map[string]any{"kind": "request", "operator": strings.Repeat("aa", 32), "owner": strings.Repeat("0c", 32), "owner_nonce": strings.Repeat("0d", 32), "until": 9999999999})
	theirs := "arca:" + hex.EncodeToString(det)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := url.Values{"code": {theirs}, "csrf": {r.csrf}}
			from(t, r.client, fmt.Sprintf("10.8.0.%d", i), r.srv.URL+"/tasks/web-wallet-roundtrip/wallet", v)
		}(i)
	}
	wg.Wait()
	if s := r.calls("send"); s > paymentsPerTask {
		t.Fatalf("12 parallel requests made %d payments", s)
	}
}

// M2. A stranger's message files nothing on a held account: the bot sends
// the sender to the form, and the owner's form files it for a reviewer.
func TestBotFilesNothingAReviewerMustJudge(t *testing.T) {
	g := newTgRig(t)
	c, code, id := g.account(t, "held2@example.com", "10.4.0.2")
	g.app.db.Exec("UPDATE users SET held = 1 WHERE id = ?", id)
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if st, _, _ := g.verif(id); st != "" {
		t.Fatalf("a message filed %s on a held account", st)
	}
	if a := g.bot.last(oldTG); !strings.Contains(a, "only the account's owner can ask") {
		t.Fatalf("answer: %q", a)
	}
	csrf := csrfOf(t, c, g.srv.URL+"/account")
	if m := from(t, c, "10.4.0.2", g.srv.URL+"/account/verify", url.Values{"csrf": {csrf}, "platform": {"telegram"}, "handle": {"old_tg"}}); !strings.Contains(m, "Verification request received") {
		t.Fatalf("form: %q", m)
	}
	if st, _, note := g.verif(id); st != "pending" || !strings.Contains(note, "the account is held") {
		t.Fatalf("form: %s %q", st, note)
	}
}

// L1. The coin's read and its code's payment are one write: a failure of
// the second leaves the coin unread, and the next follow pays the code.
func TestAttributionIsOneWrite(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.coin("atomic", "100000000", r.app.codeOf(1, "code"))
	r.app.db.Exec("CREATE TRIGGER no_pay BEFORE UPDATE OF paid_at ON arca_codes BEGIN SELECT RAISE(ABORT, 'refused'); END")
	r.app.followArca() // the write is interrupted between the code's payment and the coin's read mark
	var readAt, tries int64
	r.app.db.QueryRow("SELECT read_at, tries FROM arca_coins WHERE leaf_id = 'atomic'").Scan(&readAt, &tries)
	if readAt != 0 || tries != 1 || r.app.codeOf(1, "code").PaidAt != 0 {
		t.Fatalf("after the interrupted write: read_at %d, tries %d", readAt, tries)
	}
	r.app.db.Exec("DROP TRIGGER no_pay")
	r.app.db.Exec("UPDATE arca_coins SET next_at = 0")
	r.app.followArca()
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("a failed write dropped the coin")
	}
}

// L2. New coins arriving faster than a follow reads do not keep an older
// payment waiting: the oldest unread coins get a share of every follow.
func TestDustDoesNotStarveAPayment(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.coin("payment", "100000000", r.app.codeOf(1, "code"))
	for f := 0; f < 3; f++ {
		for i := 0; i < reconcileBatch; i++ {
			r.coin(fmt.Sprintf("dust%d-%d", f, i), "1", nil)
		}
		r.app.followArca()
	}
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("the payment waited behind three follows of new coins")
	}
}

// The coins this follow's own sync accepted are read first, ahead of a
// backlog on both sides.
func TestSyncAcceptedReadFirst(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	for i := 0; i < 30; i++ {
		r.coin(fmt.Sprintf("old%d", i), "1", nil)
	}
	r.coin("accepted", "100000000", r.app.codeOf(1, "code"))
	for i := 0; i < 200; i++ {
		r.coin(fmt.Sprintf("new%d", i), "1", nil)
	}
	b, _ := json.Marshal([]map[string]string{{"leaf_id": "accepted"}})
	os.WriteFile(filepath.Join(r.dir, "mailbox.json"), b, 0o600)
	r.app.followArca()
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("the coin the follow's own sync accepted was not read in that follow")
	}
}

// L3. With no code open, no coin unread and nothing accepted, a follow does
// not list the wallet's coins more than once an hour.
func TestQuietFollowsSkipTheListing(t *testing.T) {
	r := newArcaRig(t)
	r.app.followArca()
	n := r.calls("coins")
	r.app.followArca()
	if r.calls("coins") != n {
		t.Fatal("a quiet follow listed the coins again")
	}
	r.post("web-wallet-pay", "wallet", url.Values{}) // a code is open now
	r.app.followArca()
	if r.calls("coins") != n+1 {
		t.Fatal("a follow with a code open did not list the coins")
	}
}

// L4. Form and bot at once, for two accounts and one Telegram account, with
// the schema's guard gone: one verification at most.
func TestFormAndBotRaceForOneTelegramID(t *testing.T) {
	g := newTgRig(t)
	g.app.db.Exec("DROP INDEX verifications_telegram_id")
	// Both requests pass the check before either files, unless something
	// keeps them apart.
	tgAfterCheck = func() { time.Sleep(30 * time.Millisecond) }
	defer func() { tgAfterCheck = func() {} }()
	for round := 0; round < 20; round++ {
		_, _, a := g.account(t, fmt.Sprintf("ra%d@example.com", round), fmt.Sprintf("10.5.%d.1", round))
		_, _, b := g.account(t, fmt.Sprintf("rb%d@example.com", round), fmt.Sprintf("10.5.%d.2", round))
		ua, _ := getUserByID(g.app.db, a)
		ub, _ := getUserByID(g.app.db, b)
		if ua == nil || ub == nil {
			t.Fatalf("round %d: accounts not registered", round)
		}
		id := int64(60000000 + round)
		var wg sync.WaitGroup
		// The same Telegram account under two usernames (renamed between
		// its two messages), so the username's index does not catch it.
		for _, x := range []struct {
			u       *User
			name    string
			consent bool
		}{{ua, fmt.Sprintf("race%da", round), false}, {ub, fmt.Sprintf("race%db", round), true}} {
			wg.Add(1)
			go func(u *User, name string, consent bool) {
				defer wg.Done()
				g.app.tgVerify(u, tgSender{ID: id, Username: name, Sent: now()}, consent)
			}(x.u, x.name, x.consent)
		}
		wg.Wait()
		var n int
		g.app.db.QueryRow("SELECT COUNT(*) FROM verifications WHERE evidence = ? AND status != 'rejected'", tgEvidence(id)).Scan(&n)
		if n > 1 {
			t.Fatalf("round %d: one Telegram account has %d live requests", round, n)
		}
	}
	g.app.db.Exec("UPDATE users SET is_admin = 1")
	c := &http.Client{Jar: newJar()}
	from(t, c, "10.5.0.9", g.srv.URL+"/register", url.Values{"email": {"boss@example.com"}, "password": {"a-long-password"}})
	g.app.db.Exec("UPDATE users SET is_admin = 1 WHERE email = 'boss@example.com'")
	if page := get(t, c, g.srv.URL+"/admin/queue"); !strings.Contains(page, "verifications_telegram_id") {
		t.Fatal("the desk does not show the missing guard")
	}
}

// L5. The bot tells a stranger nothing of another account's state.
func TestBotRevealsNoStateToAStranger(t *testing.T) {
	g := newTgRig(t)
	_, code, _ := g.account(t, "known@example.com", "10.6.0.1")
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	g.bot.send(60000001, "stranger", code, 0)
	g.poll(t)
	if a := g.bot.last(60000001); strings.Contains(a, "verified") || strings.Contains(a, "waiting") || !strings.Contains(a, "Nothing was linked") {
		t.Fatalf("to a stranger: %q", a)
	}
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if a := g.bot.last(oldTG); !strings.Contains(a, "already verified with this Telegram account") {
		t.Fatalf("to the owner: %q", a)
	}
}

// L6. A poller that cannot read the Bot API shows on the desk.
func TestDeadPollerShowsOnTheDesk(t *testing.T) {
	g := newTgRig(t)
	c, _, id := g.account(t, "admin@example.com", "10.6.0.2")
	g.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = ?", id)
	g.app.cfg.TgBotToken = "999:TEST-token-revoked"
	g.app.pollTelegram(0)
	page := get(t, c, g.srv.URL+"/admin/queue")
	if !strings.Contains(page, "has not read its messages") || !strings.Contains(page, "Unauthorized") || strings.Contains(page, "TEST-token") {
		t.Fatal("the desk does not show the dead poller")
	}
	g.app.cfg.TgBotToken = fakeToken
	g.app.pollTelegram(0)
	if page := get(t, c, g.srv.URL+"/admin/queue"); !strings.Contains(page, "The Telegram bot read its messages") {
		t.Fatal("the desk does not show the poller back")
	}
}

// The codes corrected by hand: marked paid with a coin whose history holds
// the payment (or with no coin). The reconciler keeps them paid, credits
// nothing again, and records the tester's real coin as the payment.
func TestHandCorrectedCodes(t *testing.T) {
	for _, handLeaf := range []string{"change", ""} {
		r := newArcaRig(t)
		r.post("web-wallet-pay", "wallet", url.Values{})
		c := r.app.codeOf(1, "code")
		pay := synthRecord(t, nil, c.Owner, c.OwnerNonce, nil)
		payRaw, _ := hex.DecodeString(pay)
		r.coin("tester-payment", "100000000", c)
		r.coin("change", "5000", nil)
		r.rec("change", synthRecord(t, payRaw[1:], randHex(32), randHex(32), nil)) // spends the payment
		r.app.db.Exec("UPDATE arca_codes SET paid_at = 1764000000, paid_leaf = ?, paid_asset = 'ab', paid_atoms = 100000000 WHERE id = ?", handLeaf, c.ID)
		r.post("web-wallet-pay", "submit", url.Values{})
		var credits int
		r.app.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'submission'").Scan(&credits)
		r.app.followArca()
		r.app.followArca()
		p, leaf := r.paidAt(c.ID)
		var after, counted int
		r.app.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'submission'").Scan(&after)
		r.app.db.QueryRow("SELECT counted FROM arca_coins WHERE leaf_id = 'tester-payment'").Scan(&counted)
		if p != 1764000000 || leaf != "tester-payment" || counted != 1 || after != credits || credits != 1 {
			t.Fatalf("hand leaf %q: paid_at %d, paid_leaf %q, counted %d, credits %d then %d", handLeaf, p, leaf, counted, credits, after)
		}
		r.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
		if desk := get(t, r.client, r.srv.URL+"/admin/users/1"); strings.Contains(desk, "Not counted") {
			t.Fatalf("hand leaf %q: the tester's coin is listed as a problem", handLeaf)
		}
		var odd int
		r.app.db.QueryRow(handCheckQuery).Scan(&odd)
		if odd != 0 {
			t.Fatalf("hand leaf %q: the check query finds %d codes", handLeaf, odd)
		}
	}
}

// handCheckQuery counts the paid codes whose recorded coin is not a coin
// read from the wallet whose own leaf names that code.
const handCheckQuery = `SELECT COUNT(*) FROM arca_codes c WHERE c.paid_at != 0 AND NOT EXISTS
	(SELECT 1 FROM arca_coins k WHERE k.leaf_id = c.paid_leaf AND k.code_id = c.id AND k.counted = 1)`

var _ = strconv.Itoa

// B1 on records the real wallet library made: a coin of a transfer that
// spent a coin which paid code C pays only the code of its own leaf; the
// change coin of the transfer that paid C pays nothing; the coin that paid C
// pays C. In format version 1, and in version 2 with a batch leaf two
// coins back in the history of a transfer that spends a fill.
func TestRealChainAndChange(t *testing.T) {
	for _, set := range [][3]string{{"paid", "spends-paid", "change-of-paid"}, {"in-spends-fill", "spends-fill", ""}} {
		r := newArcaRig(t)
		paid, spends := realNamed(t, set[0]), realNamed(t, set[1])
		if !strings.Contains(hex.EncodeToString(spends), hex.EncodeToString(paid[1:])) {
			t.Fatalf("%s does not carry %s whole", set[1], set[0])
		}
		lp, okp, _ := ownLeaf(paid)
		ls, _, _ := ownLeaf(spends)
		if !okp || lp.Owner == "" {
			t.Fatalf("%s names no owner", set[0])
		}
		c := r.addCode("code", lp.Owner, lp.Nonce)
		d := r.addCode("swap", ls.Owner, ls.Nonce)
		r.coin("spends", "100", nil)
		r.rec("spends", hex.EncodeToString(spends))
		if set[2] != "" {
			r.coin("change", "100", nil)
			r.rec("change", hex.EncodeToString(realNamed(t, set[2])))
		}
		r.app.followArca()
		if p, _ := r.paidAt(c.ID); p != 0 {
			t.Fatalf("%s: the spending coin or the change paid the code of the coin they come from", set[1])
		}
		if p, leaf := r.paidAt(d.ID); p == 0 || leaf != "spends" {
			t.Fatalf("%s: its own code stayed unpaid", set[1])
		}
		var changeCode int64 = -1
		r.app.db.QueryRow("SELECT code_id FROM arca_coins WHERE leaf_id = 'change'").Scan(&changeCode)
		if set[2] != "" && changeCode != 0 {
			t.Fatalf("the change coin was matched to code %d", changeCode)
		}
		r.coin("paid", "100", nil)
		r.rec("paid", hex.EncodeToString(paid))
		r.app.followArca()
		if p, leaf := r.paidAt(c.ID); p == 0 || leaf != "paid" {
			t.Fatalf("%s: did not pay its own code", set[0])
		}
	}
}

// One coin whose record names two codes (one in its history, one as its
// own leaf, a third in an output's script) pays exactly one, its own; and
// the schema refuses a second code that names the same coin.
func TestOneCoinNamingThreeCodes(t *testing.T) {
	r := newArcaRig(t)
	own, hist, script := r.addCode("code", randHex(32), randHex(32)), r.addCode("code", randHex(32), randHex(32)), r.addCode("code", randHex(32), randHex(32))
	input, _ := hex.DecodeString(synthRecord(t, nil, hist.Owner, hist.OwnerNonce, nil))
	mark, _ := hex.DecodeString(script.Owner + script.OwnerNonce)
	r.coin("three", "100000000", nil)
	r.rec("three", synthRecord(t, input[1:], own.Owner, own.OwnerNonce, mark))
	r.app.followArca()
	for _, c := range []*arcaCode{own, hist, script} {
		p, _ := r.paidAt(c.ID)
		if (p != 0) != (c == own) {
			t.Fatalf("code %d paid %v", c.ID, p != 0)
		}
	}
	if _, err := r.app.db.Exec("UPDATE arca_codes SET paid_leaf = 'three', paid_at = 1 WHERE id = ?", hist.ID); err == nil {
		t.Fatal("the schema let two codes name one coin")
	}
}

// A failed answer is sent again on a later read, once.
func TestUnansweredAnswerIsSentAgain(t *testing.T) {
	g := newTgRig(t)
	_, code, id := g.account(t, "silence@example.com", "10.7.0.1")
	g.bot.mu.Lock()
	g.bot.failing = 2 // the answer, and its first retry
	g.bot.mu.Unlock()
	g.bot.send(oldTG, "old_tg", code, 0)
	g.app.pollTelegram(0)
	if st, _, _ := g.verif(id); st != "verified" || g.bot.answers(oldTG) != 0 {
		t.Fatalf("first read: %s, %d answers", st, g.bot.answers(oldTG))
	}
	for i := 0; i < 3; i++ {
		g.app.db.Exec("UPDATE tg_messages SET next_try_at = 0") // each wait has passed
		g.app.pollTelegram(0)
	}
	if n := g.bot.answers(oldTG); n != 1 {
		t.Fatalf("%d answers after the Bot API recovered", n)
	}
}

// A database whose rows break a new guard of the ledger does not start:
// the refusal names the rows by id only, and the read-only query a deploy
// runs first names the same rows.
func TestRefusesToStartOverBrokenGuards(t *testing.T) {
	// One coin as the payment of two codes.
	path := filepath.Join(t.TempDir(), "codes.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := createUser(db, "x@example.com", "h", "0123456789", "")
	db.Exec("DROP INDEX arca_codes_paid_leaf")
	for i := 0; i < 3; i++ {
		db.Exec(`INSERT INTO arca_codes (user_id, purpose, request, owner, owner_nonce, paid_leaf, paid_at, created_at) VALUES (?, 'code', 'r', 'o', 'n', ?, 1, 0)`,
			uid, map[bool]string{true: "secret-leaf", false: "other"}[i < 2])
	}
	found, _ := guardConflicts(db)
	before := rowList(found["arca_codes_paid_leaf"])
	db.Close()
	if _, err := openDB(path); err == nil || !strings.Contains(err.Error(), "arca_codes rows 1, 2 ") || strings.Contains(err.Error(), "secret-leaf") || before != "1, 2" {
		t.Fatalf("codes: %v (the query before: %q)", err, before)
	}
	// One Telegram account verifying two accounts, under two usernames, the
	// id read from the notes as the migration reads it.
	path = filepath.Join(t.TempDir(), "tg.db")
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP INDEX verifications_telegram_id")
	for i, name := range []string{"name_a", "name_b", "name_c"} {
		u, _ := createUser(db, name+"@example.com", "h", fmt.Sprintf("00000000%02d", i), "")
		tgID := map[bool]string{true: "777", false: "778"}[i < 2]
		db.Exec(`INSERT INTO verifications (user_id, platform, handle, status, check_note, created_at) VALUES (?, 'telegram', ?, 'verified', ?, 0)`,
			u, name, "message verified from @"+name+" (id "+tgID+"); account created ~Sep 2014")
	}
	found, _ = guardConflicts(db)
	before = rowList(found["verifications_telegram_id"])
	db.Close()
	if _, err := openDB(path); err == nil || !strings.Contains(err.Error(), "verifications rows 1, 2 ") || strings.Contains(err.Error(), "name_a") || before != "1, 2" {
		t.Fatalf("telegram: %v (the query before: %q)", err, before)
	}
	// Corrected, it starts.
	db, _ = sql.Open("sqlite", path)
	db.Exec("UPDATE verifications SET status = 'rejected' WHERE id = 2")
	db.Close()
	if db, err := openDB(path); err != nil {
		t.Fatalf("after the correction: %v", err)
	} else {
		db.Close()
	}
}
