package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// arcaRig is an Emissio over a database file and a stand-in wallet
// directory that both outlive a restart.
type arcaRig struct {
	t      *testing.T
	dbPath string
	dir    string
	app    *App
	srv    *httptest.Server
	client *http.Client
	csrf   string
}

func newArcaRig(t *testing.T) *arcaRig {
	r := &arcaRig{t: t, dbPath: filepath.Join(t.TempDir(), "emissio.db"), dir: t.TempDir()}
	jar, _ := cookiejar.New(nil)
	r.client = &http.Client{Jar: jar}
	r.start()
	seedDB(r.app.db)
	register(t, r.client, r.srv.URL, "rec@x.io")
	r.app.db.Exec("INSERT INTO wallet_links (user_id, address, pubkey, created_at) VALUES (1, 'tb1qlinked', '02ab', 0)")
	r.csrf = csrfOf(t, r.client, r.srv.URL+"/account")
	return r
}

// start opens the database and serves it, as the binary does on start.
func (r *arcaRig) start() {
	db := mustOpenDB(r.dbPath)
	bin, _ := filepath.Abs("testdata/fake-arca.py")
	r.app = NewApp(Config{RedditBase: "http://127.0.0.1:9", TelegramBase: "http://127.0.0.1:9", XBase: "http://127.0.0.1:9",
		ArcaBin: bin, ArcaDatadir: r.dir}, db)
	if r.srv == nil {
		r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { r.app.routes().ServeHTTP(w, q) }))
		r.t.Cleanup(func() { r.srv.Close(); r.app.db.Close() })
	}
}

// restart stops Emissio and starts it again over the same database.
func (r *arcaRig) restart() {
	r.app.db.Close()
	r.start()
}

func (r *arcaRig) post(slug, action string, v url.Values) string {
	v.Set("csrf", r.csrf)
	return from(r.t, r.client, "10.7.7.7", r.srv.URL+"/tasks/"+slug+"/"+action, v)
}

// coin puts a coin in the wallet as another command's sync would have
// accepted it: in the wallet's list of coins, never in the follower's own
// sync. Its record names code c, or no code.
func (r *arcaRig) coin(leaf, value string, c *arcaCode) {
	var coins []map[string]string
	if b, err := os.ReadFile(filepath.Join(r.dir, "coins.json")); err == nil {
		json.Unmarshal(b, &coins)
	}
	coins = append(coins, map[string]string{"leaf_id": leaf, "asset": strings.Repeat("ab", 32), "value": value, "kind": "transfer", "state": "live"})
	b, _ := json.Marshal(coins)
	os.WriteFile(filepath.Join(r.dir, "coins.json"), b, 0o600)
	rec := synthRecord(r.t, nil, randHex(32), randHex(32), nil)
	if c != nil {
		rec = synthRecord(r.t, nil, c.Owner, c.OwnerNonce, nil)
	}
	r.rec(leaf, rec)
}

// rec sets the record the wallet gives for a coin.
func (r *arcaRig) rec(leaf, rec string) {
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(r.dir, "rec-"+leaf+".json"), b, 0o600)
}

// calls counts the wallet commands run so far, by name.
func (r *arcaRig) calls(name string) int {
	b, _ := os.ReadFile(filepath.Join(r.dir, "calls.log"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l == name {
			n++
		}
	}
	return n
}

func (r *arcaRig) credits(subID int64) int {
	var n int
	r.app.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'submission' AND ref_id = ?", subID).Scan(&n)
	return n
}

// A payment another command accepted, never in the follower's own sync, is
// matched on the next follow, across a restart, and its waiting submission
// is credited once.
func TestReconcileCoinAcceptedElsewhere(t *testing.T) {
	r := newArcaRig(t)
	if m := r.post("web-wallet-pay", "wallet", url.Values{}); !strings.Contains(m, "Your receive code from Emissio is below") {
		t.Fatalf("code: %q", m)
	}
	c := r.app.codeOf(1, "code")
	if m := r.post("web-wallet-pay", "submit", url.Values{}); !strings.Contains(m, "Emissio checks again every few minutes") {
		t.Fatalf("waiting submit: %q", m)
	}
	var subID int64
	var status string
	r.app.db.QueryRow("SELECT id, status FROM submissions ORDER BY id DESC LIMIT 1").Scan(&subID, &status)
	if status != "waiting" {
		t.Fatalf("submission %s", status)
	}
	r.app.followArca() // nothing yet
	// The tester pays; the wallet accepts the coin in another command.
	r.coin("elsewhere1", "100000000", c)
	r.restart()
	r.app.followArca()
	if c2 := r.app.codeOf(1, "code"); c2.PaidAt == 0 || c2.PaidAtoms != 100000000 {
		t.Fatalf("the coin accepted elsewhere was not matched: %+v", c2)
	}
	r.app.recheckWaiting(time.Now())
	r.app.db.QueryRow("SELECT status FROM submissions WHERE id = ?", subID).Scan(&status)
	if status != "approved" || r.credits(subID) != 1 {
		t.Fatalf("after the follow: %s, %d credits", status, r.credits(subID))
	}
	records := r.calls("record")
	// Again, and across another restart: counted once, its record read once.
	for i := 0; i < 2; i++ {
		r.app.followArca()
		r.app.recheckWaiting(time.Now())
		r.restart()
	}
	r.app.followArca()
	if r.credits(subID) != 1 || r.calls("record") != records {
		t.Fatalf("again: %d credits, %d record calls (was %d)", r.credits(subID), r.calls("record"), records)
	}
	var counted int
	r.app.db.QueryRow("SELECT counted FROM arca_coins WHERE leaf_id = 'elsewhere1'").Scan(&counted)
	if counted != 1 {
		t.Fatal("the coin is not kept as the code's payment")
	}
}

// A tester whose code is unpaid can get a new one once the code is ten
// minutes old, at most five a day; a payment to the older code still counts;
// one coin pays one code; a second coin to a paid code is not counted, and
// the reviewer sees it.
func TestNewCodeAndTwoCodes(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	c1 := r.app.codeOf(1, "code")
	page := get(t, r.client, r.srv.URL+"/tasks/web-wallet-pay")
	if strings.Contains(page, "Get a new receive code") || !strings.Contains(page, "If your wallet will not pay this code, a new one can be had from") {
		t.Fatal("a code minutes old offers a new one, or does not say when")
	}
	if m := r.post("web-wallet-pay", "wallet", url.Values{}); !strings.Contains(m, "A new one can be had from") || !strings.Contains(m, "any of your codes for this task counts") {
		t.Fatalf("early renew: %q", m)
	}
	r.app.db.Exec("UPDATE arca_codes SET created_at = created_at - 700")
	page = get(t, r.client, r.srv.URL+"/tasks/web-wallet-pay")
	if !strings.Contains(page, "Paid this code, or your wallet will not pay it?") || !strings.Contains(page, "a payment to any of your codes for this task counts") {
		t.Fatal("an unpaid code ten minutes old offers no new one")
	}
	if m := r.post("web-wallet-pay", "wallet", url.Values{}); !strings.Contains(m, "Your receive code from Emissio is below.") {
		t.Fatalf("renew: %q", m)
	}
	c2 := r.app.codeOf(1, "code")
	if c2.ID == c1.ID || c2.Request == c1.Request {
		t.Fatal("the new code is the old one")
	}
	if !strings.Contains(get(t, r.client, r.srv.URL+"/tasks/web-wallet-pay"), url.QueryEscape(c2.Request)) {
		t.Fatal("the page does not show the newest code")
	}
	// At most five a day.
	for i := 0; i < 6; i++ {
		r.app.db.Exec("UPDATE arca_codes SET created_at = created_at - 700")
		r.post("web-wallet-pay", "wallet", url.Values{})
	}
	var n int
	r.app.db.QueryRow("SELECT COUNT(*) FROM arca_codes WHERE purpose = 'code'").Scan(&n)
	if n != codesPerDay {
		t.Fatalf("%d codes in a day", n)
	}
	// The tester had paid the first code: it counts.
	r.coin("old-code", "100000000", c1)
	r.app.followArca()
	var paid int
	r.app.db.QueryRow("SELECT COUNT(*) FROM arca_codes WHERE paid_at != 0").Scan(&paid)
	if paid != 1 {
		t.Fatalf("one coin paid %d codes", paid)
	}
	// A second coin to the paid code: not counted, and the desk says so.
	r.coin("again", "5000", c1)
	r.app.followArca()
	if r.app.codesOf(1, "code", 0)[len(r.app.codesOf(1, "code", 0))-1].PaidAtoms != 100000000 {
		t.Fatal("the second coin replaced the first")
	}
	r.post("web-wallet-pay", "submit", url.Values{})
	var status string
	r.app.db.QueryRow("SELECT status FROM submissions ORDER BY id DESC LIMIT 1").Scan(&status)
	if status != "approved" {
		t.Fatalf("a payment to the older code: %s", status)
	}
	r.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
	desk := get(t, r.client, r.srv.URL+"/admin/users/1")
	for _, want := range []string{"code #" + itoa(c1.ID) + " made", "paid", "by coin old-code", "Not counted: coin again", "which another coin paid", "The wallet&#39;s coins were all read"} {
		if !strings.Contains(desk, want) {
			t.Fatalf("the desk does not say %q", want)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A reviewer on a waiting or rejected submission sees whether the wallet's
// coins were read since, and never "nothing arrived" while some are not.
func TestDeskSeesTheWallet(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.post("web-wallet-pay", "submit", url.Values{})
	r.app.db.Exec("UPDATE users SET is_admin = 1 WHERE id = 1")
	desk := get(t, r.client, r.srv.URL+"/admin/users/1")
	if !strings.Contains(desk, ": unpaid.") || !strings.Contains(desk, "have not been read yet: do not say that nothing arrived") {
		t.Fatal("before any follow the desk does not warn")
	}
	// A wallet with more coins than one follow reads.
	for i := 0; i < reconcileBatch+5; i++ {
		r.coin("stray"+itoa(int64(i)), "1", nil)
	}
	r.app.followArca()
	desk = get(t, r.client, r.srv.URL+"/admin/users/1")
	if !strings.Contains(desk, "with 5 records still to read") {
		t.Fatal("the desk does not say the coins are not all read")
	}
	r.app.followArca()
	r.app.db.Exec("UPDATE submissions SET status = 'rejected'")
	desk = get(t, r.client, r.srv.URL+"/admin/users/1")
	if !strings.Contains(desk, ": unpaid.") || !strings.Contains(desk, "coins were all read") {
		t.Fatal("a rejected submission does not show the wallet")
	}
}

// What a follow costs on a wallet with a thousand coins: one list of coins,
// and a record only for coins not read before, at most reconcileBatch, the
// newest first; a coin of no code is logged once.
func TestReconcileCost(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	for i := 0; i < 1000; i++ {
		r.coin("c"+itoa(int64(i)), "1", nil)
	}
	r.app.followArca()
	if got := r.calls("record"); got != reconcileBatch {
		t.Fatalf("first follow read %d records", got)
	}
	// What arrives now is read on the next follow, ahead of the backlog.
	r.coin("fresh", "100000000", r.app.codeOf(1, "code"))
	r.app.followArca()
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("a new payment waited behind the backlog")
	}
	for i := 0; i < 30; i++ {
		r.app.followArca()
	}
	if got := r.calls("record"); got != 1001 {
		t.Fatalf("%d records read for 1001 coins", got)
	}
	r.addCode("btc", randHex(32), randHex(32)) // a code open: the follow lists the coins
	coins, records := r.calls("coins"), r.calls("record")
	start := time.Now()
	r.app.followArca()
	took := time.Since(start)
	if r.calls("coins") != coins+1 || r.calls("record") != records {
		t.Fatal("a follow with nothing new read a record")
	}
	if n := strings.Count(logs.String(), "paid no code"); n != 1000 {
		t.Fatalf("%d log lines for 1000 coins of no code", n)
	}
	t.Logf("a follow over 1001 coins read before: %s (one coins call, no record calls)", took)
}

// A record the wallet cannot give is asked for again on later follows, and
// the coin is matched once it can.
func TestReconcileRecordFails(t *testing.T) {
	r := newArcaRig(t)
	r.post("web-wallet-pay", "wallet", url.Values{})
	r.coin("flaky", "100000000", r.app.codeOf(1, "code"))
	os.WriteFile(filepath.Join(r.dir, "fail-record"), nil, 0o600)
	r.app.followArca()
	if r.app.codeOf(1, "code").PaidAt != 0 {
		t.Fatal("matched without a record")
	}
	os.Remove(filepath.Join(r.dir, "fail-record"))
	r.app.db.Exec("UPDATE arca_coins SET next_at = 0") // its backoff has passed
	// The sync failing does not stop the reconciliation.
	os.WriteFile(filepath.Join(r.dir, "fail-sync"), nil, 0o600)
	r.app.followArca()
	if r.app.codeOf(1, "code").PaidAt == 0 {
		t.Fatal("not matched once the record could be read")
	}
}
