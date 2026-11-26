package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// theirCode is a receive code of a tester's wallet for Emissio to pay.
func theirCode() string {
	det, _ := json.Marshal(map[string]any{"kind": "request", "operator": strings.Repeat("aa", 32), "owner": strings.Repeat("0c", 32), "owner_nonce": strings.Repeat("0d", 32), "until": 9999999999})
	return "arca:" + hex.EncodeToString(det)
}

// 1. A payment's place binds for as long as it stands, however long its
// request waits behind the wallet: three in flight for over half an hour
// still fill the address's limit. A start of the service removes the places
// a stop left reserved, and keeps every place whose payment was begun.
func TestReservationsBindUntilTheStart(t *testing.T) {
	r := newArcaRig(t)
	for i := 0; i < arcaSendsDay; i++ {
		u, _ := createUser(r.app.db, fmt.Sprintf("q%d@example.com", i), "h", fmt.Sprintf("00000002%02d", i), "")
		r.app.db.Exec("INSERT INTO arca_sends (user_id, purpose, target, asset, atoms, state, ref, ip, created_at) VALUES (?, 'roundtrip', '', '', 0, ?, '', '10.7.7.7', ?)",
			u, []string{"reserved", "begun", "reserved"}[i], now()-31*60)
	}
	if m := r.post("web-wallet-roundtrip", "wallet", url.Values{"code": {theirCode()}}); !strings.Contains(m, "from your network today") || r.calls("send") != 0 {
		t.Fatalf("a fourth account behind the address, with three requests in flight for 31 minutes: %q, %d payments", m, r.calls("send"))
	}
	r.restart()
	var reserved, paying int
	r.app.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE state = 'reserved'").Scan(&reserved)
	r.app.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE state = 'begun'").Scan(&paying)
	if reserved != 0 || paying != 1 {
		t.Fatalf("after a start: %d reserved, %d paying", reserved, paying)
	}
}

// 2. The intent is recorded before the act: a payment is begun only once
// its row reads `begun`, and from then on nothing removes the row. The
// wallet pays and every later write fails: after a restart the row is still
// there, counted, and on the desk. The `begun` write fails: the wallet is
// never asked to pay.
func TestPayingRowOutlivesEveryFailure(t *testing.T) {
	r := newArcaRig(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	r.app.db.Exec("CREATE TRIGGER no_fill BEFORE UPDATE ON arca_sends WHEN NEW.state != 'begun' BEGIN SELECT RAISE(ABORT, 'refused'); END")
	r.app.db.Exec("CREATE TRIGGER no_anew BEFORE INSERT ON arca_sends WHEN NEW.state != 'reserved' BEGIN SELECT RAISE(ABORT, 'refused'); END")
	r.post("web-wallet-roundtrip", "wallet", url.Values{"code": {theirCode()}})
	if r.calls("send") != 1 {
		t.Fatal("no payment made")
	}
	var id int64
	r.app.db.QueryRow("SELECT id FROM arca_sends").Scan(&id)
	if id == 0 || !strings.Contains(logs.String(), fmt.Sprintf("payment %d", id)) {
		t.Fatalf("row %d; log:\n%s", id, logs.String())
	}
	r.app.db.Exec("DROP TRIGGER no_fill")
	r.app.db.Exec("DROP TRIGGER no_anew")
	r.restart()
	var state string
	r.app.db.QueryRow("SELECT state FROM arca_sends WHERE id = ?", id).Scan(&state)
	if state != "begun" {
		t.Fatalf("after a restart the payment's row is %q", state)
	}
	r.app.db.Exec("UPDATE arca_sends SET created_at = created_at - 3600 WHERE id = ?", id)
	r.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
	if page := get(t, r.client, r.srv.URL+"/admin/queue"); !strings.Contains(page, fmt.Sprintf("Payment %d", id)) || !strings.Contains(page, "reconcile it by hand") {
		t.Fatal("the desk does not show the payment to reconcile")
	}
	r.app.db.Exec("UPDATE users SET is_admin = 0 WHERE id = 1")
	for i := 0; i < paymentsPerTask-1; i++ {
		r.post("web-wallet-roundtrip", "wallet", url.Values{"code": {theirCode()}})
	}
	if m := r.post("web-wallet-roundtrip", "wallet", url.Values{"code": {theirCode()}}); !strings.Contains(m, "paid you 3 times") || r.calls("send") != paymentsPerTask {
		t.Fatalf("the begun payment is not counted: %q, %d payments", m, r.calls("send"))
	}
	// The `begun` write fails: nothing is paid.
	r2 := newArcaRig(t)
	r2.app.db.Exec("CREATE TRIGGER no_paying BEFORE UPDATE ON arca_sends WHEN NEW.state = 'begun' BEGIN SELECT RAISE(ABORT, 'refused'); END")
	m := r2.post("web-wallet-roundtrip", "wallet", url.Values{"code": {theirCode()}})
	var n int
	r2.app.db.QueryRow("SELECT COUNT(*) FROM arca_sends").Scan(&n)
	if r2.calls("send") != 0 || n != 0 || !strings.Contains(m, "could not pay") {
		t.Fatalf("with the paying write refused: %d payments, %d rows, %q", r2.calls("send"), n, m)
	}
}

// 3. The deploy runs the new binary's guard-check, as the service's user,
// between the build and the install, and stops on a conflict; a box with no
// database yet has nothing to check.
func TestDeployRunsGuardCheck(t *testing.T) {
	script, err := os.ReadFile("deploy/install-on-box.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	build, check, install := strings.Index(s, "go build"), strings.Index(s, "guard-check-before-install.sh"), strings.Index(s, "/opt/emissio/emissio.new")
	if build < 0 || check < build || install < check || !strings.Contains(s, "guard-check-before-install.sh emissio /var/lib/emissio/emissio.db emissio /opt/emissio") {
		t.Fatalf("install-on-box.sh does not run the check between build and install (build %d, check %d, install %d)", build, check, install)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on the path to build the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "emissio")
	if out, err := exec.Command(gobin, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	me, _ := user.Current()
	run := func(db string) (string, error) {
		out, err := exec.Command("bash", "deploy/guard-check-before-install.sh", bin, db, me.Username, dir).CombinedOutput()
		return string(out), err
	}
	if out, err := run(filepath.Join(dir, "none.db")); err != nil || !strings.Contains(out, "no database") {
		t.Fatalf("no database: %v %q", err, out)
	}
	path := filepath.Join(dir, "live.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run(path); err != nil || !strings.Contains(out, "no conflicts") {
		t.Fatalf("clean: %v %q", err, out)
	}
	db.Exec("DROP INDEX arca_codes_paid_leaf")
	u, _ := createUser(db, "d@example.com", "h", "0123456789", "")
	for i := 0; i < 2; i++ {
		db.Exec(`INSERT INTO arca_codes (user_id, purpose, request, owner, owner_nonce, paid_leaf, paid_at, created_at) VALUES (?, 'code', 'r', 'o', 'n', 'same', 1, 0)`, u)
	}
	db.Close()
	out, err := run(path)
	if err == nil || !strings.Contains(out, "arca_codes_paid_leaf: rows 1, 2") || !strings.Contains(out, "not installed") {
		t.Fatalf("conflict: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "emissio.check")); !os.IsNotExist(err) {
		t.Fatal("the check's copy of the binary was left behind")
	}
}

// 4. With the guard's index standing, rows the backfill could not give an
// id are a warning, not a refusal: guard-check says the index exists and
// exits 0, as the start does, and the start logs each such row.
func TestGuardCheckWithTheIndexStanding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "standing.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := createUser(db, "a@example.com", "h", "0123456780", "")
	b, _ := createUser(db, "b@example.com", "h", "0123456781", "")
	db.Exec(`INSERT INTO verifications (user_id, platform, handle, evidence, status, check_note, created_at) VALUES (?, 'telegram', 'ha', 'telegram-id:42', 'verified', '', 0)`, a)
	db.Exec(`INSERT INTO verifications (user_id, platform, handle, status, check_note, created_at) VALUES (?, 'telegram', 'hb', 'verified', 'message verified from @hb (id 42)', 0)`, b)
	db.Close()
	var out bytes.Buffer
	if code := guardCheck(path, &out); code != 0 || !strings.Contains(out.String(), "verifications_telegram_id: index exists") || !strings.Contains(out.String(), "rows 1, 2") {
		t.Fatalf("guard-check: exit %d, %q", code, out.String())
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	db, err = openDB(path)
	if err != nil {
		t.Fatalf("the start refused what guard-check passed: %v", err)
	}
	db.Close()
	if !strings.Contains(logs.String(), "verification 2") {
		t.Fatalf("the failed backfill was not logged:\n%s", logs.String())
	}
}

// 5. A stranger learns nothing from a second or a third message: the same
// answer whether the first verified the account or was held back.
func TestStrangerLearnsNothingFromLaterMessages(t *testing.T) {
	g := newTgRig(t)
	_, codeA, _ := g.account(t, "cleana@example.com", "10.9.1.1")
	_, codeB, _ := g.account(t, "cleanb@example.com", "10.9.1.2")
	_, codeC, c := g.account(t, "heldc@example.com", "10.9.1.3")
	_, codeD, d := g.account(t, "heldd@example.com", "10.9.1.4")
	g.app.db.Exec("UPDATE users SET held = 1 WHERE id IN (?, ?)", c, d)
	run := func(chat int64, name string, codes ...string) []string {
		var out []string
		for _, code := range codes {
			g.bot.send(chat, name, code, 0)
			g.poll(t)
			out = append(out, strings.NewReplacer(codeA, "CODE", codeB, "CODE", codeC, "CODE", codeD, "CODE", "@"+name, "@NAME").Replace(g.bot.last(chat)))
		}
		return out
	}
	if codeA == "" || codeB == "" || codeC == "" || codeD == "" {
		t.Fatal("accounts not registered")
	}
	clean := run(60000030, "sa", codeA, codeA, codeB)
	held := run(60000031, "sb", codeC, codeC, codeD)
	for i := range clean {
		if clean[i] != held[i] {
			t.Fatalf("message %d:\n%q\n%q", i+1, clean[i], held[i])
		}
	}
}

// 5. The answer is sent before the work a verification brings (the
// referrals it may qualify), whose length depends on the outcome.
func TestBotAnswersBeforeCreditWork(t *testing.T) {
	g := newTgRig(t)
	_, code, _ := g.account(t, "order@example.com", "10.9.2.1")
	var mu sync.Mutex
	var order []string
	tgCreditWork = func(a *App, userID int64) {
		mu.Lock()
		order = append(order, fmt.Sprintf("work (%d answers)", g.bot.answers(oldTG)))
		mu.Unlock()
	}
	defer func() { tgCreditWork = func(a *App, userID int64) { a.processReferrals(userID) } }()
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if len(order) != 1 || order[0] != "work (1 answers)" {
		t.Fatalf("the work ran %v", order)
	}
}

// 6. Only the Bot API's own refusals that cannot change are final: a chat
// that blocked the bot and lifts the block gets its answer; a proxy's 403 is
// not the Bot API's; a chat that no longer exists is given up.
func TestOnlyFinalRefusalsGiveUp(t *testing.T) {
	g := newTgRig(t)
	_, code, _ := g.account(t, "final@example.com", "10.9.3.1")
	g.bot.mu.Lock()
	g.bot.blocked = map[string]bool{"9300000001": true}
	g.bot.proxy = map[string]bool{"9300000002": true}
	g.bot.gone = map[string]bool{"9300000003": true}
	g.bot.mu.Unlock()
	for i := int64(1); i <= 3; i++ {
		g.bot.send(9300000000+i, "", code, 0)
	}
	g.app.pollTelegram(0)
	g.bot.mu.Lock()
	g.bot.blocked, g.bot.proxy = nil, nil // the user lifts the block; the proxy lets it through
	g.bot.mu.Unlock()
	for i := 0; i < 3; i++ {
		g.app.db.Exec("UPDATE tg_messages SET next_try_at = 0")
		g.app.pollTelegram(0)
	}
	if g.bot.answers(9300000001) != 1 || g.bot.answers(9300000002) != 1 {
		t.Fatalf("after the block lifted: %d; after the proxy let it through: %d", g.bot.answers(9300000001), g.bot.answers(9300000002))
	}
	g.bot.mu.Lock()
	defer g.bot.mu.Unlock()
	if g.bot.sends["9300000003"] != 1 {
		t.Fatalf("a chat that no longer exists was asked %d times", g.bot.sends["9300000003"])
	}
}

// 7. A code's record moved to another coin takes that coin's amount and
// asset; moved to none, it keeps none, and the desk says its coin is not
// found yet.
func TestRepointCarriesTheCoinsAmount(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	a := r.app.codeOf(1, "code")
	c := r.addCode("swap", randHex(32), randHex(32))
	r.coin("x", "186718345", c)
	r.app.db.Exec("UPDATE arca_codes SET paid_at = 1764000000, paid_leaf = 'x', paid_asset = 'ab', paid_atoms = 186718345 WHERE id = ?", a.ID)
	r.app.followArca()
	if got := r.app.codeOf(1, "code"); got.PaidAtoms != 0 || got.PaidAsset != "" || got.PaidAt != 1764000000 {
		t.Fatalf("released: %d of %q, paid_at %d", got.PaidAtoms, got.PaidAsset, got.PaidAt)
	}
	r.post("web-wallet-pay", "submit", url.Values{})
	r.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
	if desk := get(t, r.client, r.srv.URL+"/admin/users/1"); !strings.Contains(desk, "its coin not found yet") {
		t.Fatal("the desk does not say the code's coin is not found yet")
	}
	r.rec("a-own", synthRecord(t, nil, a.Owner, a.OwnerNonce, nil))
	b, _ := json.Marshal([]map[string]string{{"leaf_id": "a-own", "asset": strings.Repeat("cd", 32), "value": "100000123"}})
	os.WriteFile(filepath.Join(r.dir, "mailbox.json"), b, 0o600)
	r.app.followArca()
	if got := r.app.codeOf(1, "code"); got.PaidAtoms != 100000123 || got.PaidAsset != strings.Repeat("cd", 32) {
		t.Fatalf("moved to its own coin: %d of %q", got.PaidAtoms, got.PaidAsset)
	}
}

// 8. The README says what the desk shows of coins not read: the ten waiting
// longest, and the count of all.
func TestReadmeNamesTheDeskLimit(t *testing.T) {
	b, _ := os.ReadFile("README.md")
	if strings.Contains(string(b), "every coin whose record could not be read yet") {
		t.Fatal("the README promises every coin, and the desk names ten")
	}
	_ = time.Now
}

// A one-shot command (createadmin, reseed-tasks) opens the database beside
// the serving process: a reservation of a request in flight there stands.
func TestCommandLeavesStandingReservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serving.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := createUser(db, "s@example.com", "h", "0123456789", "")
	db.Exec("INSERT INTO arca_sends (user_id, purpose, target, asset, atoms, state, ref, ip, created_at) VALUES (?, 'roundtrip', '', '', 0, 'reserved', '', '10.1.1.1', ?)", u, now())
	db.Close()
	db = mustOpenDB(path) // as main does before any command
	seedDB(db)
	runCommand(db, []string{"reseed-tasks"})
	var n int
	db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE state = 'reserved'").Scan(&n)
	db.Close()
	if n != 1 {
		t.Fatalf("a command removed a standing reservation (%d left)", n)
	}
}

// A payment whose reserved row is gone when it is filled in (whatever
// removed it) is recorded anew, counted, and logged with what was paid.
func TestFillWithItsRowGone(t *testing.T) {
	r := newArcaRig(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	slot := &paySlot{id: 4242, userID: 1, purpose: "roundtrip", ip: "10.7.7.7"}
	r.app.fillSlot(slot, "target", strings.Repeat("ab", 32), 123456, "sent", "t9")
	var n int
	var atoms int64
	r.app.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(atoms), 0) FROM arca_sends WHERE user_id = 1 AND purpose = 'roundtrip' AND state = 'sent'").Scan(&n, &atoms)
	if n != 1 || atoms != 123456 {
		t.Fatalf("the payment left %d rows (%d atoms)", n, atoms)
	}
	if !strings.Contains(logs.String(), "payment 4242") || !strings.Contains(logs.String(), "123456 atoms") {
		t.Fatalf("not logged:\n%s", logs.String())
	}
	// In a request: the reservation removed while the wallet pays.
	os.WriteFile(filepath.Join(r.dir, "slow-send"), []byte("1"), 0o600)
	go func() {
		time.Sleep(400 * time.Millisecond)
		r.app.db.Exec("DELETE FROM arca_sends WHERE state IN ('reserved', 'begun')") // whatever removed it
	}()
	r.post("web-wallet-roundtrip", "wallet", url.Values{"code": {theirCode()}})
	r.app.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE user_id = 1 AND purpose = 'roundtrip' AND state = 'sent'").Scan(&n)
	if r.calls("send") != 1 || n != 2 {
		t.Fatalf("a payment made while its row was removed: %d payments, %d sent rows", r.calls("send"), n)
	}
}
