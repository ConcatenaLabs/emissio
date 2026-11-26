package main

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// #87. However long a record cannot be read, it is asked for again within
// minutes, so a payment waits minutes once reads work again.
func TestRecordWaitIsMinutes(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.coin("late", "100000000", r.app.codeOf(1, "code"))
	os.WriteFile(filepath.Join(r.dir, "fail-record-late"), nil, 0o600)
	for i := 0; i < 30; i++ {
		r.app.db.Exec("UPDATE arca_coins SET next_at = 0") // each wait has passed
		r.app.followArca()
	}
	var nextAt int64
	r.app.db.QueryRow("SELECT next_at FROM arca_coins WHERE leaf_id = 'late'").Scan(&nextAt)
	if w := nextAt - now(); w > 15*60 {
		t.Fatalf("after 30 failures the next read is %d s away", w)
	}
}

// #88. A code marked paid with a coin the wallet still lists and has not
// read is not moved to a second payment read first.
func TestNoRepointWhileTheRecordedCoinIsUnread(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	c := r.app.codeOf(1, "code")
	r.coin("first", "100000000", c)
	r.coin("second", "5000", c)
	r.app.db.Exec("UPDATE arca_codes SET paid_at = 1764000000, paid_leaf = 'first', paid_asset = 'ab', paid_atoms = 100000000 WHERE id = ?", c.ID)
	os.WriteFile(filepath.Join(r.dir, "fail-record-first"), nil, 0o600)
	r.app.followArca()
	if _, leaf := r.paidAt(c.ID); leaf != "first" {
		t.Fatalf("the code moved to %q while its own coin was unread", leaf)
	}
	os.Remove(filepath.Join(r.dir, "fail-record-first"))
	r.app.db.Exec("UPDATE arca_coins SET next_at = 0")
	r.app.followArca()
	var first, second int
	r.app.db.QueryRow("SELECT counted FROM arca_coins WHERE leaf_id = 'first'").Scan(&first)
	r.app.db.QueryRow("SELECT counted FROM arca_coins WHERE leaf_id = 'second'").Scan(&second)
	if p, leaf := r.paidAt(c.ID); p != 1764000000 || leaf != "first" || first != 1 || second != 0 {
		t.Fatalf("after both reads: paid_at %d, paid_leaf %q, counted %d and %d", p, leaf, first, second)
	}
}

// #89. A coin that a paid code names as its payment, but whose own leaf
// pays another code, pays that code; the first code stays paid.
func TestCoinHeldByAnotherCodesRecord(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	a := r.app.codeOf(1, "code")
	c := r.addCode("swap", randHex(32), randHex(32))
	r.coin("x", "186718345", c)
	r.app.db.Exec("UPDATE arca_codes SET paid_at = 1764000000, paid_leaf = 'x', paid_asset = 'ab', paid_atoms = 100000000 WHERE id = ?", a.ID)
	r.app.followArca()
	pa, la := r.paidAt(a.ID)
	pc, lc := r.paidAt(c.ID)
	var readAt int64
	r.app.db.QueryRow("SELECT read_at FROM arca_coins WHERE leaf_id = 'x'").Scan(&readAt)
	if pc == 0 || lc != "x" || pa != 1764000000 || la == "x" || readAt == 0 {
		t.Fatalf("code A paid_at %d leaf %q; code C paid_at %d leaf %q; coin read %d", pa, la, pc, lc, readAt)
	}
	// A's own coin, accepted by a later sync, becomes its recorded payment.
	r.rec("a-own", synthRecord(t, nil, a.Owner, a.OwnerNonce, nil))
	b, _ := json.Marshal([]map[string]string{{"leaf_id": "a-own", "asset": strings.Repeat("ab", 32), "value": "100000000"}})
	os.WriteFile(filepath.Join(r.dir, "mailbox.json"), b, 0o600)
	r.app.followArca()
	if p, leaf := r.paidAt(a.ID); p != 1764000000 || leaf != "a-own" {
		t.Fatalf("A's own coin: paid_at %d, leaf %q", p, leaf)
	}
}

// #90. Chats that blocked the bot are given up at the first refusal, and do
// not keep a refused answer elsewhere from being sent again.
func TestBlockedChatsDoNotStarveRetries(t *testing.T) {
	g := newTgRig(t)
	_, code, _ := g.account(t, "public@example.com", "10.8.1.1")
	g.bot.mu.Lock()
	g.bot.blocked = map[string]bool{}
	for i := 0; i < 25; i++ {
		g.bot.blocked[fmt.Sprint(9100000000+i)] = true
	}
	g.bot.mu.Unlock()
	for i := 0; i < 25; i++ {
		g.bot.send(int64(9100000000+i), "", code, time.Minute)
	}
	g.app.pollTelegram(0)
	g.bot.mu.Lock()
	g.bot.failing = 1
	g.bot.mu.Unlock()
	g.bot.send(9200000000, "", code, 0)
	for i := 0; i < 5; i++ {
		g.app.pollTelegram(0)
		g.app.db.Exec("UPDATE tg_messages SET next_try_at = 0") // each wait has passed
	}
	if n := g.bot.answers(9200000000); n != 1 {
		t.Fatalf("the refused answer was sent %d times", n)
	}
	g.bot.mu.Lock()
	defer g.bot.mu.Unlock()
	for chat := range g.bot.blocked {
		if g.bot.sends[chat] != 1 {
			t.Fatalf("a blocked chat was asked %d times", g.bot.sends[chat])
		}
	}
}

// #91. A Telegram account that is not the one on file gets one answer
// whether the account was verified or held back.
func TestStrangerGetsOneAnswer(t *testing.T) {
	g := newTgRig(t)
	_, codeA, _ := g.account(t, "clean@example.com", "10.8.2.1")
	_, codeB, b := g.account(t, "held3@example.com", "10.8.2.2")
	g.app.db.Exec("UPDATE users SET held = 1 WHERE id = ?", b)
	g.bot.send(60000010, "s1", codeA, 0)
	g.bot.send(60000011, "s2", codeB, 0)
	g.poll(t)
	// Each answer names the sender's own username, which is theirs to know.
	a1 := strings.NewReplacer(codeA, "CODE", "@s1", "@NAME").Replace(g.bot.last(60000010))
	a2 := strings.NewReplacer(codeB, "CODE", "@s2", "@NAME").Replace(g.bot.last(60000011))
	if a1 == "" || a1 != a2 {
		t.Fatalf("two answers:\n%q\n%q", a1, a2)
	}
}

// #93. Several accounts behind one network address: Emissio pays at most
// the address's daily limit, and a request the limit refuses is answered at
// once, not after the payments under way.
func TestAddressLimitWithoutWaiting(t *testing.T) {
	r := newArcaRig(t)
	os.WriteFile(filepath.Join(r.dir, "slow-send"), []byte("1.5"), 0o600)
	det, _ := json.Marshal(map[string]any{"kind": "request", "operator": strings.Repeat("aa", 32), "owner": strings.Repeat("0c", 32), "owner_nonce": strings.Repeat("0d", 32), "until": 9999999999})
	theirs := "arca:" + hex.EncodeToString(det)
	type acct struct {
		c    *http.Client
		csrf string
	}
	var accts []acct
	for i := 0; i < 5; i++ {
		c := &http.Client{Jar: newJar()}
		from(t, c, fmt.Sprintf("10.8.3.%d", i+1), r.srv.URL+"/register", url.Values{"email": {fmt.Sprintf("shared%d@example.com", i)}, "password": {"a-long-password"}})
		var id int64
		r.app.db.QueryRow("SELECT id FROM users WHERE email = ?", fmt.Sprintf("shared%d@example.com", i)).Scan(&id)
		if _, err := r.app.db.Exec("INSERT INTO wallet_links (user_id, address, pubkey, created_at) VALUES (?, ?, ?, 0)", id, fmt.Sprintf("tb1qshared%d", i), fmt.Sprintf("02%02x", i)); err != nil || id == 0 {
			t.Fatalf("account %d: %v", i, err)
		}
		accts = append(accts, acct{c, csrfOf(t, c, r.srv.URL+"/account")})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var refusedAfter []time.Duration
	start := time.Now()
	for _, a := range accts {
		wg.Add(1)
		go func(a acct) {
			defer wg.Done()
			m := from(t, a.c, "10.9.9.9", r.srv.URL+"/tasks/web-wallet-roundtrip/wallet", url.Values{"csrf": {a.csrf}, "code": {theirs}})
			if strings.Contains(m, "from your network today") {
				mu.Lock()
				refusedAfter = append(refusedAfter, time.Since(start))
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	if s := r.calls("send"); s != arcaSendsDay {
		t.Fatalf("%d payments behind one address", s)
	}
	if len(refusedAfter) != 2 {
		t.Fatalf("%d requests refused", len(refusedAfter))
	}
	for _, d := range refusedAfter {
		if d > time.Second {
			t.Fatalf("a request the limit refuses waited %s for the payments under way", d)
		}
	}
}

// #94. Coins the follow's own sync accepts, fifty and more every follow, do
// not keep an older unread payment waiting.
func TestAcceptedStreamDoesNotStarve(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.coin("payment", "100000000", r.app.codeOf(1, "code"))
	for i := 0; i < 20; i++ {
		r.coin(fmt.Sprintf("older%d", i), "1", nil) // a backlog, older than the payment's place is not
	}
	for f := 0; f < 3; f++ {
		var mb []map[string]string
		for i := 0; i < reconcileBatch+10; i++ {
			leaf := fmt.Sprintf("accepted%d-%d", f, i)
			mb = append(mb, map[string]string{"leaf_id": leaf, "asset": strings.Repeat("ab", 32), "value": "1"})
			r.rec(leaf, synthRecord(t, nil, randHex(32), randHex(32), nil))
		}
		b, _ := json.Marshal(mb)
		os.WriteFile(filepath.Join(r.dir, "mailbox.json"), b, 0o600)
		r.app.followArca()
	}
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("the payment waited behind three follows of accepted coins")
	}
}

// #95. An answer sent but not recorded as sent is logged, and is sent again
// (at least once), never lost.
func TestAnswerNotRecordedIsLogged(t *testing.T) {
	g := newTgRig(t)
	_, code, _ := g.account(t, "twice2@example.com", "10.8.4.1")
	g.app.db.Exec("CREATE TRIGGER no_answered BEFORE UPDATE OF answered ON tg_messages BEGIN SELECT RAISE(ABORT, 'refused'); END")
	g.bot.send(oldTG, "old_tg", code, 0)
	g.app.pollTelegram(0)
	if !strings.Contains(g.logs.String(), "sent but not recorded") {
		t.Fatalf("an unrecorded answer was not logged:\n%s", g.logs.String())
	}
	g.app.db.Exec("DROP TRIGGER no_answered")
	g.app.db.Exec("UPDATE tg_messages SET next_try_at = 0")
	g.app.pollTelegram(0)
	g.app.pollTelegram(0)
	if n := g.bot.answers(oldTG); n != 2 {
		t.Fatalf("%d answers: an unrecorded answer is sent again once", n)
	}
}

// #96. The check run before a deploy (`emissio guard-check`) names exactly
// the rows the start-up refusal names, from the same extraction, including
// for a note whose first "(id " is not followed by digits.
func TestGuardCheckMatchesTheRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tg.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP INDEX verifications_telegram_id")
	notes := []string{"(id junk), then (id 42)", "message verified from @b (id 42); account created ~Sep 2014", "message verified from @c (id 43)"}
	for i, note := range notes {
		u, _ := createUser(db, fmt.Sprintf("g%d@example.com", i), "h", fmt.Sprintf("00000001%02d", i), "")
		db.Exec(`INSERT INTO verifications (user_id, platform, handle, status, check_note, created_at) VALUES (?, 'telegram', ?, 'verified', ?, 0)`,
			u, fmt.Sprintf("h%d", i), note)
	}
	db.Close()
	var out bytes.Buffer
	if code := guardCheck(path, &out); code == 0 || !strings.Contains(out.String(), "verifications_telegram_id: rows 1, 2") {
		t.Fatalf("guard-check: exit %d, %q", code, out.String())
	}
	if _, err := openDB(path); err == nil || !strings.Contains(err.Error(), "verifications rows 1, 2 ") {
		t.Fatalf("the refusal: %v", err)
	}
	// Read-only: the check changed nothing, and a corrected database passes.
	ro, _ := sql.Open("sqlite", path)
	ro.Exec("UPDATE verifications SET status = 'rejected' WHERE id = 1")
	ro.Close()
	out.Reset()
	if code := guardCheck(path, &out); code != 0 || !strings.Contains(out.String(), "no conflicts") {
		t.Fatalf("after the correction: exit %d, %q", code, out.String())
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	if db, err := openDB(path); err != nil {
		t.Fatalf("after the correction the refusal stands: %v", err)
	} else {
		db.Close()
	}
}
