package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldExplorer serves canned transactions, holding each /tx/ request until
// `want` requests have arrived or the hold times out: the window in which two
// submissions have both passed the claim queries and neither has inserted.
func heldExplorer(t *testing.T, txs map[string]string, want int32) (*httptest.Server, *atomic.Int32) {
	var arrived atomic.Int32
	all := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tx/") {
			if arrived.Add(1) >= want {
				once.Do(func() { close(all) })
			}
			select {
			case <-all:
			case <-time.After(2 * time.Second):
			}
		}
		body, ok := txs[strings.TrimPrefix(r.URL.Path, "/tx/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &arrived
}

type tester struct {
	client *http.Client
	csrf   string
}

func newTester(t *testing.T, app *App, base, email, code string) tester {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	register(t, c, base, email)
	app.db.Exec("UPDATE users SET claim_code = ? WHERE email = ?", code, email)
	return tester{c, csrfOf(t, c, base+"/account")}
}

func (te tester) submit(t *testing.T, base, slug, txid string) string {
	resp, err := te.client.PostForm(base+"/tasks/"+slug+"/submit", url.Values{"csrf": {te.csrf}, "txid": {txid}})
	if err != nil {
		t.Error(err)
		return ""
	}
	resp.Body.Close()
	return resp.Request.URL.RawQuery
}

// Two accounts submitting one txid at the same moment: the transaction
// carries both accounts' proof outputs, and the explorer holds both evidence
// requests until both have arrived. Exactly one account is credited.
func TestConcurrentTxidClaim(t *testing.T) {
	codeA, codeB := "0123456789", "00000000ff"
	tagA, tagB := proofTag(codeA), proofTag(codeB)
	atoms := func(tag string) int64 {
		var n int64
		for _, c := range tag {
			n = n*10 + int64(c-'0')
		}
		return 100000000 + n
	}
	both := strings.Repeat("ab", 32)
	plain := []map[string]any{{"is_coinbase": false}}
	txs := map[string]string{both: txJSONid(both, plain, []map[string]any{
		vout(atoms(tagA), policy, "v0_p2wpkh"), vout(atoms(tagB), policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true)}
	explorer, arrived := heldExplorer(t, txs, 2)
	app, srv, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	a := newTester(t, app, srv.URL, "a@example.com", codeA)
	b := newTester(t, app, srv.URL, "b@example.com", codeB)
	var wg sync.WaitGroup
	answers := make([]string, 2)
	for i, te := range []tester{a, b} {
		wg.Add(1)
		go func(i int, te tester) {
			defer wg.Done()
			answers[i] = te.submit(t, srv.URL, "first-transaction", both)
		}(i, te)
	}
	wg.Wait()
	var subs, approved, credits int
	app.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(status = 'approved'), 0) FROM submissions WHERE txid = ?", both).Scan(&subs, &approved)
	app.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'submission'").Scan(&credits)
	t.Logf("same txid: submissions=%d approved=%d credits=%d explorer requests=%d", subs, approved, credits, arrived.Load())
	if subs != 1 || approved != 1 || credits != 1 {
		t.Fatalf("one txid credited %d accounts (%d submissions)", credits, subs)
	}
	refused := 0
	for _, q := range answers {
		if strings.Contains(q, "already+been+submitted+by+another+account") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("the second account was not told the transaction is taken: %v", answers)
	}
}

// One account submitting one task twice at the same moment, with two of its
// own transactions, gets one submission for it.
func TestConcurrentTaskClaim(t *testing.T) {
	code := "0123456789"
	tx1, tx2 := strings.Repeat("c1", 32), strings.Repeat("c2", 32)
	plain := []map[string]any{{"is_coinbase": false}}
	txs := map[string]string{
		tx1: txJSONid(tx1, plain, []map[string]any{vout(186718345, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		tx2: txJSONid(tx2, plain, []map[string]any{vout(286718345, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
	}
	explorer, _ := heldExplorer(t, txs, 2)
	app, srv, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	te := newTester(t, app, srv.URL, "twice@example.com", code)
	var wg sync.WaitGroup
	for _, tx := range []string{tx1, tx2} {
		wg.Add(1)
		go func(tx string) { defer wg.Done(); te.submit(t, srv.URL, "first-transaction", tx) }(tx)
	}
	wg.Wait()
	var subs, credits int
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&subs)
	app.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'submission'").Scan(&credits)
	if subs != 1 || credits != 1 {
		t.Fatalf("one task: %d submissions, %d credits", subs, credits)
	}
}

// What stays allowed: one account using its own transaction for a second
// task that permits it, and submissions of different transactions by
// different accounts, which do not wait for each other.
func TestClaimLockKeepsWhatIsAllowed(t *testing.T) {
	codeA, codeB := "0123456789", "00000000ff"
	txA, txB := strings.Repeat("d1", 32), strings.Repeat("d2", 32)
	plain := []map[string]any{{"is_coinbase": false}}
	atomsB := int64(100000000)
	var tagB int64
	for _, c := range proofTag(codeB) {
		tagB = tagB*10 + int64(c-'0')
	}
	txs := map[string]string{
		txA: txJSONid(txA, plain, []map[string]any{vout(186718345, policy, "v0_p2wpkh"), vout(41, "aa"+policy[2:], "fee")}, true),
		txB: txJSONid(txB, plain, []map[string]any{vout(atomsB+tagB, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
	}
	// Two requests must meet at the explorer: only possible if the two
	// accounts' submissions of different txids run at the same time.
	explorer, arrived := heldExplorer(t, txs, 2)
	app, srv, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	a := newTester(t, app, srv.URL, "own@example.com", codeA)
	b := newTester(t, app, srv.URL, "other@example.com", codeB)
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.submit(t, srv.URL, "first-transaction", txA) }()
	go func() { defer wg.Done(); b.submit(t, srv.URL, "first-transaction", txB) }()
	wg.Wait()
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("submissions of different transactions waited for each other (%s, %d explorer requests)", took, arrived.Load())
	}
	// The same account, its own transaction, a second task that takes it.
	if q := a.submit(t, srv.URL, "any-asset-fee", txA); !strings.Contains(q, "Checked+and+credited") {
		t.Errorf("own txid for a second task: %s", q)
	}
	var credits int
	app.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'submission'").Scan(&credits)
	if credits != 3 {
		t.Errorf("%d credits, want 3", credits)
	}
}
