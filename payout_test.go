package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The generator's x coordinate: the key of the BIP173 example address
// bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4.
const gX = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func TestPayoutKeys(t *testing.T) {
	hrps := []string{"tb"}
	if k, err := namedKey(gX, hrps); err != nil || k != gX {
		t.Fatalf("x-only: %v %v", k, err)
	}
	if k, err := namedKey("02"+gX, hrps); err != nil || k != gX {
		t.Fatalf("compressed: %v %v", k, err)
	}
	// The x coordinate 0..05 is not on the curve.
	if _, err := namedKey(strings.Repeat("0", 63)+"5", hrps); err == nil {
		t.Fatal("a point off the curve was taken")
	}
	if _, err := namedKey("tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", hrps); err == nil || !strings.Contains(err.Error(), "only its key's hash") {
		t.Fatalf("a v0 address: %v", err)
	}
	if _, err := namedKey("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", hrps); err == nil {
		t.Fatal("a mainnet address taken as a testnet key")
	}
	if k, why := launchKey("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "02"+gX); why != "" || k != gX {
		t.Fatalf("a P2WPKH address with its key: %v %v", k, why)
	}
	if _, why := launchKey("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "03"+gX); !strings.Contains(why, "not the key of the payout address") {
		t.Fatalf("another key for a P2WPKH address: %v", why)
	}
	if _, why := launchKey("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", ""); !strings.Contains(why, "names only its key's hash") {
		t.Fatalf("a P2WPKH address with no key: %v", why)
	}
	if _, why := launchKey("bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3", ""); !strings.Contains(why, "script address") {
		t.Fatalf("a P2WSH address: %v", why)
	}
}

// A payment service stand-in: what an operator publishes, and a payment's
// public status as the test sets it.
type fakeService struct {
	srv    *httptest.Server
	mu     sync.Mutex
	asset  string
	status map[string]any
}

func newFakeService(t *testing.T, asset string) *fakeService {
	f := &fakeService{asset: asset, status: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"assets": []any{map[string]any{"asset": f.asset, "min_leaf": "330",
			"fees": map[string]any{"refresh_ppm": 1000, "refresh_base": "10", "offboard_base": "100"}}}})
	})
	mux.HandleFunc("POST /v1/distribution_status", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"distribution_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		st, ok := f.status[req.ID]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "unknown_participation", "message": "no distribution " + req.ID}})
			return
		}
		json.NewEncoder(w).Encode(st)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type claim struct {
	key        string
	value      int64
	state      string
	returnedIn string
}

func (f *fakeService) set(id, state, asset string, cs []claim) {
	var out []any
	for i, c := range cs {
		var ret any
		if c.returnedIn != "" {
			ret = c.returnedIn
		}
		out = append(out, map[string]any{"index": i, "key": c.key, "value": fmt.Sprint(c.value), "state": c.state, "returned_in": ret})
	}
	f.mu.Lock()
	f.status[id] = map[string]any{"distribution_id": id, "state": state, "asset": asset, "round": strings.Repeat("a", 64),
		"deadline": 1900000000, "claims": out}
	f.mu.Unlock()
}

// account makes an account with a balance, verified or not, and a key.
func account(t *testing.T, a *App, email string, balance int64, verified bool, rehearsal string) int64 {
	id, err := createUser(a.db, email, "x", randomHex(5), "")
	if err != nil {
		t.Fatal(err)
	}
	a.db.Exec(`INSERT INTO ledger (user_id, amount, kind, note, created_at) VALUES (?,?,?,?,?)`, id, balance, "adjustment", "test", now())
	if verified {
		a.db.Exec(`INSERT INTO verifications (user_id, platform, handle, status, created_at) VALUES (?,?,?,?,?)`,
			id, "x", email, "verified", now())
	}
	if rehearsal != "" {
		if err := setKey(a.db, id, "rehearsal_key", rehearsal); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func payoutApp(t *testing.T, network string) (*App, *fakeService) {
	db := mustOpenDB(filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { db.Close() })
	seedDB(db)
	asset := strings.Repeat("5", 64)
	svc := newFakeService(t, asset)
	return NewApp(Config{PayoutURL: svc.srv.URL, PayoutNetwork: network, PayoutAsset: asset, PayoutAtomsPerSEQ: 1000,
		PayoutHRPs: []string{"tb"}}, db), svc
}

func key(i int) string {
	// Distinct valid x-only keys: the x coordinates of 1G..nG are on the
	// curve; these are G and the BIP340 test vectors' keys.
	return []string{gX,
		"dff1d77f2a671c5f36183726db2341be58feae1da2deced843240f7b502ba659",
		"dd308afec5777e13121fa72b9cc1b7cc0139715309b086c960e18fd969774eb8",
		"25d1dff95105f5253c4022f628a996ad3a0d95fbf21d468a1b33f8c160d8f517"}[i]
}

// TestPayoutRehearsal: the testnet rehearsal of the payout. A run takes
// every payable account at its key and leaves the rest out with the reason;
// the payment's reference is recorded only when it names exactly the list;
// then the run follows it to the end, and no balance moves.
func TestPayoutRehearsal(t *testing.T) {
	a, svc := payoutApp(t, "testnet")
	alice := account(t, a, "alice@example.com", 100, true, key(0))
	bob := account(t, a, "bob@example.com", 200, true, key(1))
	account(t, a, "carol@example.com", 300, false, key(2)) // not verified
	account(t, a, "dave@example.com", 400, true, "")       // no key
	account(t, a, "erin@example.com", 50, true, key(0))    // alice's key again
	p, err := a.openRun("rehearsal 1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Accounts != 2 || p.LeftOut != 3 || p.TotalAtoms != 300_000 || p.FeeBound == 0 {
		t.Fatalf("run: %+v", p)
	}
	rows, _ := rowsOf(a.db, p.ID)
	for _, r := range rows {
		t.Logf("account %d %s: %s %d atoms %s", r.UserID, r.Email, r.State, r.Atoms, r.Reason)
	}
	if _, err := a.openRun("again"); err == nil || !strings.Contains(err.Error(), "no account can be paid") {
		t.Fatalf("a second run while the first is open: %v", err)
	}
	t.Logf("summary: %s", runSummary(p))

	good := []claim{{key(0), 100_000, "pending", ""}, {key(1), 200_000, "pending", ""}}
	refuse := func(name, id, asset string, cs []claim, want string) {
		svc.set(id, "pending", asset, cs)
		err := a.recordReference(p.ID, id)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v, want %q", name, err, want)
		}
		t.Logf("refused, %s: %v", name, err)
	}
	refuse("an account named twice", strings.Repeat("1", 64), p.Asset, append(append([]claim{}, good...), good[0]), "twice")
	refuse("one atom short", strings.Repeat("2", 64), p.Asset, []claim{good[0], {key(1), 199_999, "pending", ""}}, "the list says")
	refuse("a stranger paid", strings.Repeat("3", 64), p.Asset, append(append([]claim{}, good...), claim{key(3), 1000, "pending", ""}), "not one of this run's")
	refuse("an account left out", strings.Repeat("4", 64), p.Asset, good[:1], "leaves out")
	refuse("another asset", strings.Repeat("5", 64), strings.Repeat("6", 64), good, "this run pays in")
	if err := a.recordReference(p.ID, strings.Repeat("7", 64)); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("an unknown payment: %v", err)
	}

	ref := strings.Repeat("f", 64)
	svc.set(ref, "pending", p.Asset, good)
	if err := a.recordReference(p.ID, ref); err != nil {
		t.Fatal(err)
	}
	good[0].state, good[1].state = "open", "open"
	svc.set(ref, "released", p.Asset, good)
	a.followRun(p.ID)
	p, _ = getRun(a.db, p.ID)
	if p.State != "paid" || p.Deadline == 0 {
		t.Fatalf("after release: %+v", p)
	}
	line := a.accountPayoutOf(&User{ID: alice}).Line
	if !strings.Contains(line, "paid to your wallet") || !strings.Contains(line, "tSEQ") {
		t.Fatalf("alice reads %q", line)
	}
	t.Logf("alice reads: %s", line)
	good[0].state = "claimed"
	good[1].state, good[1].returnedIn = "returned", strings.Repeat("b", 64)
	svc.set(ref, "released", p.Asset, good)
	a.followRun(p.ID)
	a.followRun(p.ID)
	p, _ = getRun(a.db, p.ID)
	if p.State != "complete" || p.CollectedAtoms != 100_000 || p.ReturnedAtoms != 200_000 || p.ReturnTxids != strings.Repeat("b", 64) {
		t.Fatalf("at the end: %+v", p)
	}
	t.Logf("end: %s", runSummary(p))
	for _, id := range []int64{alice, bob} {
		if bal, _ := balanceOf(a.db, id); (id == alice && bal != 100) || (id == bob && bal != 200) {
			t.Fatalf("the rehearsal moved account %d's balance to %d", id, bal)
		}
	}
	// Bob's allocation came back: he can be in the next run, and so can Alice,
	// since a rehearsal pays nothing that counts.
	p2, err := a.openRun("rehearsal 2")
	if err != nil || p2.Accounts != 2 {
		t.Fatalf("the next run: %+v %v", p2, err)
	}
}

// TestPayoutLaunchDebitsOnce: on mainnet a collected allocation is taken off
// the balance, once however often the run is followed; a returned one stays
// owed.
func TestPayoutLaunchDebitsOnce(t *testing.T) {
	a, svc := payoutApp(t, "mainnet")
	alice := account(t, a, "alice@example.com", 100, true, "")
	bob := account(t, a, "bob@example.com", 200, true, "")
	a.db.Exec(`UPDATE users SET mainnet_address = ? WHERE id = ?`, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", alice)
	setKey(a.db, alice, "payout_key", "02"+gX)
	// A taproot payout address names its key.
	tp := "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr"
	a.db.Exec(`UPDATE users SET mainnet_address = ? WHERE id = ?`, tp, bob)
	p, err := a.openRun("launch")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := rowsOf(a.db, p.ID)
	keys := map[int64]string{}
	for _, r := range rows {
		keys[r.UserID] = r.Key
		t.Logf("account %d: %s %s %s", r.UserID, r.State, r.Key, r.Reason)
	}
	if p.Accounts != 2 || keys[alice] != gX {
		t.Fatalf("launch run: %+v %v", p, keys)
	}
	ref := strings.Repeat("e", 64)
	cs := []claim{{keys[alice], 100_000, "pending", ""}, {keys[bob], 200_000, "pending", ""}}
	svc.set(ref, "pending", p.Asset, cs)
	if err := a.recordReference(p.ID, ref); err != nil {
		t.Fatal(err)
	}
	cs[0].state, cs[1].state, cs[1].returnedIn = "claimed", "returned", strings.Repeat("c", 64)
	svc.set(ref, "released", p.Asset, cs)
	a.followRun(p.ID)
	a.followRun(p.ID)
	if bal, _ := balanceOf(a.db, alice); bal != 0 {
		t.Fatalf("alice's balance after collecting: %d, want 0", bal)
	}
	if bal, _ := balanceOf(a.db, bob); bal != 200 {
		t.Fatalf("bob's balance after his allocation came back: %d, want 200", bal)
	}
	t.Logf("bob reads: %s", a.accountPayoutOf(&User{ID: bob, MainnetAddress: tp}).Line)
}

// TestPayoutAccountPage: the account page asks a bc1q account for its key,
// checks it against the address, and takes a testnet wallet key for the
// rehearsal; the page never names the payment service.
func TestPayoutAccountPage(t *testing.T) {
	app, srv, client := newTestServer(t)
	app.cfg.PayoutURL, app.cfg.PayoutNetwork, app.cfg.PayoutHRPs = "http://127.0.0.1:9", "testnet", []string{"tb"}
	resp, _ := client.PostForm(srv.URL+"/register", url.Values{"email": {"k@example.com"}, "password": {"a-long-password"}})
	resp.Body.Close()
	csrf := csrfOf(t, client, srv.URL+"/account")
	client.PostForm(srv.URL+"/account/address", url.Values{"csrf": {csrf}, "address": {"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"}})
	page := get(t, client, srv.URL+"/account")
	for _, want := range []string{"Public key of this address", "Testnet rehearsal"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the account page lacks %q", want)
		}
	}
	client.PostForm(srv.URL+"/account/payout-key", url.Values{"csrf": {csrf}, "key": {"03" + gX}})
	var u User
	app.db.QueryRow(`SELECT id FROM users WHERE email = 'k@example.com'`).Scan(&u.ID)
	if k, _ := keysOf(app.db, u.ID); k.PayoutKey != "" {
		t.Fatalf("a key that is not the address's was saved: %v", k)
	}
	client.PostForm(srv.URL+"/account/payout-key", url.Values{"csrf": {csrf}, "key": {"02" + gX}})
	client.PostForm(srv.URL+"/account/rehearsal-key", url.Values{"csrf": {csrf}, "key": {"02" + key(1)}})
	if k, _ := keysOf(app.db, u.ID); k.PayoutKey != "02"+gX || k.RehearsalKey != key(1) {
		t.Fatalf("keys saved: %+v", k)
	}
	page = get(t, client, srv.URL+"/account")
	if strings.Contains(strings.ToLower(page), "arca") {
		t.Fatal("the account page names the payment service")
	}
}
