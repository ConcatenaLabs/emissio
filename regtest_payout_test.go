package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRegtestPayout is the end-to-end proof of the payout run, against a REAL
// payment service (an Arca operator, run by arca's `arca_operator_for_browsers`
// harness on its anchored regtest chain), the REAL emissio binary driven over
// its pages, and the `arca` command line as the paying wallet and as every
// account's wallet. Skipped unless EMISSIO_REGTEST_CONTROL names the harness's
// control address; EMISSIO_REGTEST_ARCA names the arca binary. With
// EMISSIO_REGTEST_SHOT (a program taking URL, cookie, output path) and
// EMISSIO_REGTEST_SHOTS (a directory) it also screenshots the admin's page.
//
// The rehearsal: twelve accounts with balances; ten are payable (verified,
// with a testnet wallet key, five the key of an address, five the wallet's
// identity key), one is not verified, one gave no key. One run pays the ten in
// one payment from the paying wallet; seven wallets collect at their next
// sync; three never do, and at the deadline their allocations go back to the
// paying wallet in one transaction paying exactly their sum. No balance moves.
func TestRegtestPayout(t *testing.T) {
	control := strings.TrimRight(os.Getenv("EMISSIO_REGTEST_CONTROL"), "/")
	if control == "" {
		t.Skip("set EMISSIO_REGTEST_CONTROL (an arca operator harness) to run")
	}
	r := &reg{t: t, control: control, arca: os.Getenv("EMISSIO_REGTEST_ARCA"), work: t.TempDir()}
	if r.arca == "" {
		t.Fatal("EMISSIO_REGTEST_ARCA names the arca binary")
	}
	if p := os.Getenv("EMISSIO_REGTEST_EVIDENCE"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r.ev = f
	}
	r.run()
}

type reg struct {
	t                         *testing.T
	control, arca, work       string
	ev                        *os.File
	server, nodeURL, nodeUser string
	nodePwd, x                string
	base                      string
	db                        *sql.DB
}

func (r *reg) log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.t.Log(line)
	if r.ev != nil {
		fmt.Fprintln(r.ev, line)
	}
}

func (r *reg) ctl(path string, body any) map[string]any {
	r.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Post(r.control+path, "application/json", bytes.NewReader(b))
	if err != nil {
		r.t.Fatalf("control %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		r.t.Fatalf("control %s: %d %v", path, resp.StatusCode, out)
	}
	return out
}

// wallet is an arca command-line wallet.
type wallet struct {
	r    *reg
	name string
	dir  string
}

func (r *reg) wallet(name string) *wallet {
	w := &wallet{r: r, name: name, dir: filepath.Join(r.work, "wallets", strings.ReplaceAll(name, " ", "-"))}
	w.ok("create", "--server", r.server, "--node-url", r.nodeURL, "--node-user", r.nodeUser)
	return w
}

func (w *wallet) run(args ...string) (bool, any, string) {
	cmd := exec.Command(w.r.arca, append([]string{"--datadir", w.dir, "--witness-patience", "120"}, args...)...)
	cmd.Env = append(os.Environ(), "ARCA_NODE_PASSWORD="+w.r.nodePwd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var v any
	json.Unmarshal(out.Bytes(), &v)
	return err == nil, v, strings.TrimSpace(errb.String())
}

func (w *wallet) ok(args ...string) map[string]any {
	w.r.t.Helper()
	ok, v, text := w.run(args...)
	if !ok {
		w.r.t.Fatalf("%s: arca %v: %s", w.name, args, text)
	}
	m, _ := v.(map[string]any)
	return m
}

func (w *wallet) list(args ...string) []any {
	w.r.t.Helper()
	ok, v, text := w.run(args...)
	if !ok {
		w.r.t.Fatalf("%s: arca %v: %s", w.name, args, text)
	}
	a, _ := v.([]any)
	return a
}

func (w *wallet) refused(want string, args ...string) string {
	w.r.t.Helper()
	ok, _, text := w.run(args...)
	if ok || !strings.Contains(text, want) {
		w.r.t.Fatalf("%s: arca %v: want a refusal with %q, got %v %s", w.name, args, want, ok, text)
	}
	return strings.SplitN(text, "\n", 2)[0]
}

func (w *wallet) liveX() int64 {
	var total int64
	for _, c := range w.list("coins") {
		m := c.(map[string]any)
		if m["state"] == "live" && m["asset"] == w.r.x {
			v, _ := strconv.ParseInt(fmt.Sprint(m["value"]), 10, 64)
			total += v
		}
	}
	return total
}

// browser is one person's session on the site.
type browser struct {
	r    *reg
	c    *http.Client
	ip   string
	csrf string
}

func (r *reg) browser(ip string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{r: r, ip: ip, c: &http.Client{Jar: jar, Timeout: time.Minute}}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (b *browser) get(path string) string {
	b.r.t.Helper()
	req, _ := http.NewRequest("GET", b.r.base+path, nil)
	req.Header.Set("X-Forwarded-For", b.ip)
	resp, err := b.c.Do(req)
	if err != nil {
		b.r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := csrfField.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return string(body)
}

// post submits a form and answers the page it lands on, with its flash.
func (b *browser) post(path string, form url.Values) (string, string) {
	b.r.t.Helper()
	if b.csrf != "" {
		form.Set("csrf", b.csrf)
	}
	req, _ := http.NewRequest("POST", b.r.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", b.ip)
	resp, err := b.c.Do(req)
	if err != nil {
		b.r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	q := resp.Request.URL.Query()
	return string(body), q.Get("m") + q.Get("e")
}

func (b *browser) cookie() string {
	u, _ := url.Parse(b.r.base + "/")
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

func (r *reg) shot(b *browser, path, name string) {
	prog, dir := os.Getenv("EMISSIO_REGTEST_SHOT"), os.Getenv("EMISSIO_REGTEST_SHOTS")
	if prog == "" || dir == "" {
		return
	}
	out := filepath.Join(dir, name+".png")
	cmd := exec.Command(prog, r.base+path, b.cookie(), out)
	o, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("screenshot %s: %v %s", name, err, o)
	}
	r.log("  screenshot %s", out)
}

func (r *reg) startEmissio() {
	bin := filepath.Join(r.work, "emissio")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		r.t.Fatalf("go build: %v %s", err, out)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	dbPath := filepath.Join(r.work, "emissio.db")
	env := append(os.Environ(), "EMISSIO_LISTEN="+addr, "EMISSIO_DB="+dbPath, "EMISSIO_PAYOUT_URL="+r.server,
		"EMISSIO_PAYOUT_NETWORK=testnet", "EMISSIO_PAYOUT_ASSET="+r.x, "EMISSIO_PAYOUT_ATOMS_PER_SEQ=10000",
		"EMISSIO_PAYOUT_HRPS=ert,tb", "EMISSIO_PAYOUT_POLL_SECS=2")
	admin := exec.Command(bin, "createadmin", "admin@example.com")
	admin.Env = env
	admin.Stdin = strings.NewReader("a-regtest-password\n")
	if out, err := admin.CombinedOutput(); err != nil {
		r.t.Fatalf("createadmin: %v %s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = env
	logf, _ := os.Create(filepath.Join(r.work, "emissio.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if b, err := os.ReadFile(filepath.Join(r.work, "emissio.log")); err == nil && r.ev != nil {
			fmt.Fprintf(r.ev, "\n--- emissio log ---\n%s", b)
		}
	})
	r.base = "http://" + addr
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(r.base + "/"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		r.t.Fatal(err)
	}
	r.db = db
	r.log("emissio %s (pid %d): EMISSIO_PAYOUT_NETWORK=testnet, the payment service %s, asset X, 10,000 atoms per SEQ of balance",
		r.base, cmd.Process.Pid, r.server)
}

func (r *reg) runState(id int64) *PayoutRun {
	p, err := getRun(r.db, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *reg) waitRun(id int64, cond func(*PayoutRun) bool, what string) *PayoutRun {
	for i := 0; i < 120; i++ {
		if p := r.runState(id); cond(p) {
			return p
		}
		time.Sleep(time.Second)
	}
	r.t.Fatalf("run %d never %s: %+v", id, what, r.runState(id))
	return nil
}

func (r *reg) run() {
	t := r.t
	st := r.ctl("/state", nil)
	r.server, r.nodeURL = st["server"].(string), st["node_url"].(string)
	r.nodeUser, r.nodePwd, r.x = st["node_user"].(string), st["node_password"].(string), st["x"].(string)
	r.log("operator harness: server %s, node %s (elementsregtest, anchored), X %s (tSEQ here)", r.server, r.nodeURL, r.x)
	r.startEmissio()

	admin := r.browser("10.0.0.1")
	admin.get("/login")
	if _, msg := admin.post("/login", url.Values{"email": {"admin@example.com"}, "password": {"a-regtest-password"}}); msg != "" {
		r.log("admin login: %s", msg)
	}
	admin.get("/admin/payouts")
	if _, msg := admin.post("/admin/payouts/collection", url.Values{"open": {"1"}}); !strings.Contains(msg, "are asked") {
		t.Fatalf("opening the rehearsal for wallets: %q", msg)
	}
	r.log("the admin has the rehearsal ask accounts for their testnet wallet")
	admin.get("/admin/users")

	// --- twelve accounts, each with its own wallet ---
	var accts []*racct
	for i := 0; i < 12; i++ {
		email := fmt.Sprintf("acct%02d@example.com", i+1)
		b := r.browser(fmt.Sprintf("10.0.1.%d", i+1))
		b.get("/register")
		b.post("/register", url.Values{"email": {email}, "password": {"a-long-password"}})
		var id int64
		if err := r.db.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&id); err != nil {
			t.Fatalf("%s did not register: %v", email, err)
		}
		a := &racct{b: b, id: id, email: email, bal: int64(100 * (i + 1))}
		admin.get("/admin/users")
		if _, msg := admin.post("/admin/adjust", url.Values{"user": {fmt.Sprint(id)}, "amount": {fmt.Sprint(a.bal)},
			"note": {"regtest balance"}}); !strings.Contains(msg, "Adjusted") {
			t.Fatalf("adjust %s: %q", email, msg)
		}
		if i != 10 {
			// A verified platform: a reviewer's decision, recorded as the review
			// path records it.
			r.db.Exec(`INSERT INTO verifications (user_id, platform, handle, status, created_at, reviewed_at) VALUES (?,?,?,?,?,?)`,
				id, "x", fmt.Sprintf("acct%02d", i+1), "verified", now(), now())
		}
		if i != 11 {
			a.w = r.wallet(fmt.Sprintf("wallet %02d", i+1))
			given := ""
			if i%2 == 0 {
				given = a.w.ok("address")["key"].(string)
			} else {
				given = a.w.ok("openamp-key")["key"].(string)
			}
			a.key = given
			b.get("/account")
			if _, msg := b.post("/account/rehearsal-key", url.Values{"key": {given}}); !strings.Contains(msg, "Saved") {
				t.Fatalf("%s's key: %q", email, msg)
			}
		}
		accts = append(accts, a)
		kind := map[bool]string{true: "an address key", false: "its identity (OpenAMP) key"}[i%2 == 0]
		switch {
		case i == 10:
			r.log("  account %d %s: balance %d SEQ, not verified", id, email, a.bal)
		case i == 11:
			r.log("  account %d %s: balance %d SEQ, verified, no wallet key given", id, email, a.bal)
		default:
			r.log("  account %d %s: balance %d SEQ, verified, testnet wallet key %s (%s)", id, email, a.bal, a.key, kind)
		}
	}

	// --- the paying wallet: the treasury's, holding X off the chain ---
	payer := r.wallet("paying wallet")
	addr := payer.ok("address")
	r.ctl("/fund", map[string]any{"script": addr["script_pubkey"], "asset": r.x, "amount": 120_000_000})
	r.ctl("/produce", nil)
	payer.ok("board", r.x, "100000000")
	r.ctl("/produce", nil)
	r.ctl("/bury", nil)
	for i := 0; i < 60; i++ {
		b := payer.list("boards")
		if len(b) > 0 && b[0].(map[string]any)["server"].(map[string]any)["state"] == "credited" {
			break
		}
		time.Sleep(time.Second)
	}
	payer.ok("sync")
	r.log("the paying wallet boards 100,000,000 X and holds %d X live", payer.liveX())

	// --- the run ---
	r.log("")
	r.log("== the payout run ==")
	admin.get("/admin/payouts")
	page, msg := admin.post("/admin/payouts/new", url.Values{"note": {"regtest rehearsal"}})
	r.log("  opened: %s", msg)
	var runID int64
	r.db.QueryRow(`SELECT MAX(id) FROM payout_runs`).Scan(&runID)
	p := r.runState(runID)
	r.log("  the admin's page: %s", runSummary(p))
	rows, _ := rowsOf(r.db, runID)
	for _, row := range rows {
		r.log("    account %d: %s, %d atoms %s %s", row.UserID, row.State, row.Atoms, row.Key, row.Reason)
	}
	if p.Accounts != 10 || p.LeftOut != 2 || !strings.Contains(page, "Pay 10 accounts") {
		t.Fatalf("run %+v", p)
	}
	r.shot(admin, fmt.Sprintf("/admin/payouts/%d", runID), "6-payout-planned")
	list := admin.get(fmt.Sprintf("/admin/payouts/%d/list.txt", runID))
	file := filepath.Join(r.work, "payout.txt")
	os.WriteFile(file, []byte(list), 0o600)
	lines := strings.Split(strings.TrimSpace(list), "\n")
	twice := filepath.Join(r.work, "twice.txt")
	os.WriteFile(twice, []byte(lines[1]+"\n"+lines[1]+"\n"), 0o600)
	r.log("  an account named twice: %s", payer.refused("is named twice", "distribute", twice, "--asset", r.x, "--quote"))
	beyond := filepath.Join(r.work, "beyond.txt")
	os.WriteFile(beyond, []byte(strings.Fields(lines[1])[0]+" 999000000\n"), 0o600)
	r.log("  a total the wallet does not hold: %s", payer.refused("and the claims and the fee take", "distribute", beyond, "--asset", r.x))
	quote := payer.ok("distribute", file, "--asset", r.x, "--quote")
	d := payer.ok("distribute", file, "--asset", r.x, "--shown", fmt.Sprint(quote["fee"]))
	ref := d["distribution"].(string)
	if fee, _ := strconv.ParseInt(fmt.Sprint(quote["fee"]), 10, 64); fee > p.FeeBound {
		t.Fatalf("the wallet's fee %d is above the bound the page showed, %d", fee, p.FeeBound)
	}
	r.log("  the paying wallet: total %v, fee %v (the page's bound %d), reference %s", quote["total"], quote["fee"], p.FeeBound, ref)
	// A reference of another payment is refused: it names other keys.
	admin.get(fmt.Sprintf("/admin/payouts/%d", runID))
	_, msg = admin.post(fmt.Sprintf("/admin/payouts/%d/reference", runID), url.Values{"reference": {strings.Repeat("0", 64)}})
	r.log("  an unknown reference: %s", msg)
	_, msg = admin.post(fmt.Sprintf("/admin/payouts/%d/reference", runID), url.Values{"reference": {ref}})
	r.log("  the reference: %s", msg)
	if !strings.Contains(msg, "Payment recorded") {
		t.Fatalf("reference: %q", msg)
	}
	round := r.ctl("/round", nil)
	r.log("  the round %v: %v vB", round["round"], round["vsize"])
	payer.ok("sync")
	p = r.waitRun(runID, func(p *PayoutRun) bool { return p.State == "paid" }, "paid")
	r.log("  the admin's page: %s", runSummary(p))

	// Seven wallets collect at their next sync; three never open.
	var collectors []*racct
	for i, a := range accts[:10] {
		if i >= 7 {
			break
		}
		found := a.w.list("claims")
		if len(found) != 1 {
			t.Fatalf("%s: %v", a.email, found)
		}
		a.w.list("claim")
		collectors = append(collectors, a)
		r.log("  %s's wallet found its allocation (named as %v) and collected it; it holds %d X", a.email,
			found[0].(map[string]any)["named_as"], a.w.liveX())
	}
	p = r.waitRun(runID, func(p *PayoutRun) bool { return p.CollectedAtoms > 0 && countState(r, runID, "collected") == 7 }, "7 collected")
	r.log("  the admin's page: %s", runSummary(p))
	line := r.accountLine(accts[0])
	r.log("  %s's account page: %s", accts[0].email, line)

	// --- the deadline ---
	r.log("")
	r.log("== the deadline: three allocations nobody collected go back to the paying wallet ==")
	var expiry int64
	st2 := r.status(ref)
	expiry = int64(st2["expiry"].(float64))
	now0 := int64(r.ctl("/advance", map[string]any{"seconds": 1})["median_time"].(float64))
	if wait := expiry - 4*86400 - 12*3600 - now0; wait > 0 {
		r.ctl("/advance", map[string]any{"seconds": wait})
	}
	for _, a := range collectors {
		a.w.ok("sync")
	}
	payer.ok("sync")
	r.ctl("/round", nil)
	for _, a := range collectors {
		a.w.ok("sync")
	}
	now0 = int64(r.ctl("/advance", map[string]any{"seconds": 1})["median_time"].(float64))
	r.ctl("/advance", map[string]any{"seconds": expiry - now0 + 3600})
	r.ctl("/settle", nil)
	r.ctl("/advance", map[string]any{"seconds": 37 * 3600})
	for i := 0; i < 20; i++ {
		r.ctl("/settle", nil)
		time.Sleep(3 * time.Second)
		if r.runState(runID).State == "complete" {
			break
		}
	}
	p = r.waitRun(runID, func(p *PayoutRun) bool { return p.State == "complete" }, "complete")
	r.log("  the admin's page: %s", runSummary(p))
	rows, _ = rowsOf(r.db, runID)
	var want int64
	for _, row := range rows {
		r.log("    account %d: %s %d atoms %s", row.UserID, row.State, row.Atoms, row.ReturnedIn)
		if row.State == "returned" {
			want += row.Atoms
		}
	}
	if countState(r, runID, "returned") != 3 || p.ReturnedAtoms != want || p.ReturnTxids == "" || strings.Contains(p.ReturnTxids, ",") {
		t.Fatalf("returns: %+v", p)
	}
	st2 = r.status(ref)
	rs := st2["return_script"].(string)
	tx := r.ctl("/rpc", map[string]any{"method": "getrawtransaction", "params": []any{p.ReturnTxids, true}})
	var paid int64
	for _, o := range tx["vout"].([]any) {
		m := o.(map[string]any)
		if m["scriptPubKey"].(map[string]any)["hex"] == rs && m["asset"] == r.x {
			v, _ := strconv.ParseFloat(fmt.Sprint(m["value"]), 64)
			paid += int64(v*1e8 + 0.5)
		}
	}
	r.log("  the return %s on the chain: %v vB, pays the paying wallet's return script %s exactly %d X", p.ReturnTxids, tx["vsize"], rs, paid)
	if paid != want {
		t.Fatalf("the return pays %d, want %d", paid, want)
	}
	r.shot(admin, fmt.Sprintf("/admin/payouts/%d", runID), "7-payout-complete")
	r.log("  %s's account page: %s", accts[8].email, r.accountLine(accts[8]))
	r.shot(accts[0].b, "/account", "8-account-collected")
	// The rehearsal moved no balance.
	for _, a := range accts {
		if bal, _ := balanceOf(r.db, a.id); bal != a.bal {
			t.Fatalf("%s's balance is %d, want %d: a rehearsal moves nothing", a.email, bal, a.bal)
		}
	}
	r.log("  every account's balance is what it was: the rehearsal moved nothing in the ledger")
	page = admin.get(fmt.Sprintf("/admin/payouts/%d", runID)) + accts[0].b.get("/account")
	if strings.Contains(strings.ToLower(page), "arca") {
		t.Fatal("a page names the payment service")
	}
}

func countState(r *reg, runID int64, state string) int {
	var n int
	r.db.QueryRow(`SELECT COUNT(*) FROM payout_rows WHERE run_id = ? AND state = ?`, runID, state).Scan(&n)
	return n
}

var lineRe = regexp.MustCompile(`data-payout-line>([^<]*)<`)

// racct is one account of the proof: its session, its wallet, its balance.
type racct struct {
	b     *browser
	w     *wallet
	id    int64
	email string
	key   string
	bal   int64
}

// accountLine is what the account's own page says about its payout.
func (r *reg) accountLine(a *racct) string {
	m := lineRe.FindStringSubmatch(a.b.get("/account"))
	if m == nil {
		r.t.Fatalf("%s's account page says nothing of the payout", a.email)
	}
	return html.UnescapeString(m[1])
}

func (r *reg) status(ref string) map[string]any {
	b, _ := json.Marshal(map[string]any{"distribution_id": ref})
	resp, err := http.Post(r.server+"/v1/distribution_status", "application/json", bytes.NewReader(b))
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}
