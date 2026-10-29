package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Staking scripts as the node builds them (getstakescript, getdelegationscript
// on an elementsregtest node with -con_pos=1).
const (
	stakeKey       = "0322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749"
	stakeHeight    = "5ab275210322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749ac"
	stakeTime      = "03a90040b275210322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749ac"
	stakeVesting   = "5ab2750400b33f71b175210322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749ac"
	stakeBLSVestKy = "039b6347398505f5ec93826dc61c19f47c66c0283ee9be980e29ce325a0f4679ef"
	stakeBLSVest   = "03a90040b27530ad1a5b2cdc8cdbdd559fe0f0e2cd5e1f06223f635635f5689849bf721232916d5932093f88698b4b2d0713a04ecfe7a8754c6091d00729b7292ebd500ea1e91fdd0f9f359f261eafc5c2fbe60b33e01df27dd2168929435c9c95273142c94195078aff02fc4b376c63ae2b77a1e187a9070a46d0b038672056a230c59de5ba0275a8bd55177d073e9299221d02daef079a4ed5750400b33f71b17521039b6347398505f5ec93826dc61c19f47c66c0283ee9be980e29ce325a0f4679efac"
	poolA          = "02b4632d08485ff1df2db55b9dafd23347d1c47a457072a1e87be26896549a8737"
	poolB          = "039b149049555a2247d861d1ff0a46fd43635c80a836773837be0a785e0f6fce0c"
	delegToA       = "0653455144454c752102b4632d08485ff1df2db55b9dafd23347d1c47a457072a1e87be26896549a873775210322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749ac"
)

func delegScript(signer, controller string) string {
	return "0653455144454c7521" + signer + "7521" + controller + "ac"
}

func unbondScript(key string) string { return "0953455155" + "4e424f4e44" + "7521" + key + "ac" }

func TestScriptParsers(t *testing.T) {
	for name, c := range map[string]struct{ script, key string }{
		"height": {stakeHeight, stakeKey}, "time": {stakeTime, stakeKey},
		"vesting": {stakeVesting, stakeKey}, "bls+vesting": {stakeBLSVest, stakeBLSVestKy},
	} {
		if k, ok := parseStakeScript(c.script); !ok || k != c.key {
			t.Errorf("stake %s: %q %v", name, k, ok)
		}
	}
	for name, bad := range map[string]string{
		"p2wpkh":         "0014d092c99f7ad847287b809d92e66a84f3317ec1eb",
		"truncated":      stakeHeight[:len(stakeHeight)-2],
		"trailing":       stakeHeight + "75",
		"no csv":         "75210322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749ac",
		"delegation":     delegToA,
		"short key":      "5ab27520" + stakeKey[2:] + "ac",
		"cltv, no drop":  "5ab2750400b33f71b1210322502cadaed599cc6f50a0a2c03bffb755dbf443cca0f6cc8dfeaa29385b8749ac",
		"not hex at all": "zz",
	} {
		if _, ok := parseStakeScript(bad); ok {
			t.Errorf("stake parser accepted %s", name)
		}
	}
	if s, c, ok := parseDelegation(delegToA); !ok || s != poolA || c != stakeKey {
		t.Errorf("delegation: %s %s %v", s, c, ok)
	}
	if _, _, ok := parseDelegation(stakeHeight); ok {
		t.Error("delegation parser accepted a stake")
	}
	if k, ok := parseUnbond(unbondScript(stakeKey)); !ok || k != stakeKey {
		t.Errorf("unbond: %s %v", k, ok)
	}
	if _, ok := parseUnbond(delegToA); ok {
		t.Error("unbond parser accepted a delegation")
	}
}

func txJSONid(id string, vin []map[string]any, vout []map[string]any, confirmed bool) string {
	m := map[string]any{"txid": id, "vin": vin, "vout": vout,
		"status": map[string]any{"confirmed": confirmed, "block_height": 42, "block_hash": "bh"}}
	b, _ := json.Marshal(m)
	return string(b)
}

func spend(txid string, vout int, prev map[string]any) map[string]any {
	return map[string]any{"txid": txid, "vout": vout, "prevout": prev}
}

func TestChecks(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	usdx := "2a515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de"
	mine := "5bb1a1f2cfea8ad1f4b79f058005fa1ddd0d8b4a623e59974b99abbbf1fa80e4"
	faucet := "tb1phruj5gk2nqrd4atn58rjr7msakm64fxsh5famwwerraz5wu29qnqkk43tk"
	plain := []map[string]any{{"is_coinbase": false}}
	fee := vout(41, policy, "fee")
	txs := map[string]string{
		"first":    txJSONid("first", plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), fee}, true),
		"notag":    txJSONid("notag", plain, []map[string]any{vout(100000000, policy, "v0_p2wpkh"), fee}, true),
		"feeonly":  txJSONid("feeonly", plain, []map[string]any{vout(100000000, policy, "v0_p2wpkh"), vout(tag, policy, "fee")}, true),
		"mempool":  txJSONid("mempool", plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh")}, false),
		"assetfee": txJSONid("assetfee", plain, []map[string]any{vout(100000000+tag, usdx, "v0_p2wpkh"), vout(41, usdx, "fee")}, true),
		"multi":    txJSONid("multi", plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(5, usdx, "v0_p2wpkh"), fee}, true),
		"blinded":  txJSONid("blinded", plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), {"valuecommitment": "08aa", "scriptpubkey_type": "v0_p2wpkh"}, fee}, true),
		"issue": txJSONid("issue", []map[string]any{{"issuance": map[string]any{"asset_id": mine, "is_reissuance": false}}},
			[]map[string]any{vout(100000000000+tag, mine, "v0_p2wpkh"), fee}, true),
		"reissue": txJSONid("reissue", []map[string]any{{"issuance": map[string]any{"asset_id": mine, "is_reissuance": true}}},
			[]map[string]any{vout(50000000000+tag, mine, "v0_p2wpkh"), fee}, true),
		"burn":     txJSONid("burn", plain, []map[string]any{vout(100000000+tag, mine, "op_return"), fee}, true),
		"burnusdx": txJSONid("burnusdx", plain, []map[string]any{vout(100000000+tag, usdx, "op_return"), fee}, true),
		"stake":    txJSONid("stake", plain, []map[string]any{voutS(40000*100000000+tag, policy, "unknown", stakeTime, ""), fee}, true),
		"stakep2w": txJSONid("stakep2w", plain, []map[string]any{voutS(40000*100000000+tag, policy, "v0_p2wpkh", "0014aa", "tb1q"), fee}, true),
		"smallstk": txJSONid("smallstk", plain, []map[string]any{voutS(39999*100000000+tag, policy, "unknown", stakeTime, ""), fee}, true),
		"deleg":    txJSONid("deleg", plain, []map[string]any{voutS(1000, policy, "unknown", delegToA, ""), fee}, true),
		"delegnp":  txJSONid("delegnp", plain, []map[string]any{voutS(1000, policy, "unknown", delegScript(stakeKey, poolA), ""), fee}, true),
		"redeleg": txJSONid("redeleg", []map[string]any{spend("deleg", 0, voutS(1000, policy, "unknown", delegToA, ""))},
			[]map[string]any{voutS(900, policy, "unknown", delegScript(poolB, stakeKey), ""), fee}, true),
		"redelsame": txJSONid("redelsame", []map[string]any{spend("deleg", 0, voutS(1000, policy, "unknown", delegToA, ""))},
			[]map[string]any{voutS(900, policy, "unknown", delegToA, ""), fee}, true),
		"unbond": txJSONid("unbond", []map[string]any{spend("stake", 0, voutS(40000*100000000+tag, policy, "unknown", stakeTime, ""))},
			[]map[string]any{voutS(40000*100000000, policy, "unknown", unbondScript(stakeKey), ""), fee}, true),
		"unbondelse": txJSONid("unbondelse", []map[string]any{spend("first", 0, vout(100000000+tag, policy, "v0_p2wpkh"))},
			[]map[string]any{voutS(100000000, policy, "unknown", unbondScript(stakeKey), ""), fee}, true),
		// the faucet: two payments from its address, and a spend of both
		"fpay1": txJSONid("fpay1", []map[string]any{spend("res", 0, voutS(9, policy, "v1_p2tr", "51", faucet))}, []map[string]any{vout(5000000000000, policy, "v0_p2wpkh"), fee}, true),
		"fpay2": txJSONid("fpay2", []map[string]any{spend("res", 1, voutS(9, policy, "v1_p2tr", "51", faucet))}, []map[string]any{vout(5000000000000, policy, "v0_p2wpkh"), fee}, true),
		"consol": txJSONid("consol", []map[string]any{spend("fpay1", 0, nil), spend("fpay2", 0, nil)},
			[]map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), fee}, true),
		"consol1": txJSONid("consol1", []map[string]any{spend("fpay1", 0, nil), spend("first", 0, nil)},
			[]map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), fee}, true),
		// Compages: a delivery, a spend of it, and returns to redemption addresses
		"delivery":   txJSONid("delivery", plain, []map[string]any{vout(5000000, usdx, "v0_p2wpkh"), fee}, true),
		"bridged":    txJSONid("bridged", []map[string]any{spend("delivery", 0, nil)}, []map[string]any{vout(tag, usdx, "v0_p2wpkh"), fee}, true),
		"solbridged": txJSONid("solbridged", []map[string]any{spend("soldelivery", 0, nil)}, []map[string]any{vout(tag, usdx, "v0_p2wpkh"), fee}, true),
		"returned":   txJSONid("returned", plain, []map[string]any{voutS(tag, usdx, "v0_p2wpkh", "0014", "tb1qredeem"), fee}, true),
		"waiting":    txJSONid("waiting", plain, []map[string]any{voutS(tag, usdx, "v0_p2wpkh", "0014", "tb1qredeem2"), fee}, true),
		"elsewhere":  txJSONid("elsewhere", plain, []map[string]any{voutS(tag, usdx, "v0_p2wpkh", "0014", "tb1qnobody"), fee}, true),
	}
	assets := map[string]string{mine: `{"issuance_txin":{"txid":"issue"}}`, usdx: `{"issuance_txin":{"txid":"someoneelse"}}`}
	blocks := map[string]string{"bh": `{"bitcoin_anchor":{"hash":"00000000ABCDEF00000000000000000000000000000000000000000000000000","height":7}}`}
	explorer := fakeExplorer(t, txs, assets, blocks)
	defer explorer.Close()
	services := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pools.json":
			w.Write([]byte(`{"pools":[{"signer":"` + poolA + `"},{"signer":"` + poolB + `"}]}`))
		case "/bridge/deposit/tx/0x" + strings.Repeat("ab", 32):
			w.Write([]byte(`[{"status":"minted","seqTxid":"delivery"}]`))
		case "/bridge/sol/wrap/So1anaDepositAddress1111111111111111111":
			w.Write([]byte(`{"deposits":[{"status":"minted","seqTxid":"soldelivery"}]}`))
		case "/bridge/redeem/tb1qredeem":
			w.Write([]byte(`{"redemptions":[{"txid":"returned","status":"released"}]}`))
		case "/bridge/redeem/tb1qredeem2":
			w.Write([]byte(`{"redemptions":[{"txid":"waiting","status":"awaiting_finality"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer services.Close()
	txs["soldelivery"] = txJSONid("soldelivery", plain, []map[string]any{vout(5000000, usdx, "v0_p2wpkh"), fee}, true)

	app, _, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	app.cfg.PoolsURL = services.URL + "/pools.json"
	app.cfg.CompagesURL = services.URL + "/bridge"
	app.faucetAddr = map[string]bool{faucet: true}
	uid, _ := createUser(app.db, "prover@example.com", "x", code, "")
	user := &User{ID: uid, ClaimCode: code}
	approve := func(slug, txid, notes string) {
		if _, err := app.db.Exec("INSERT INTO submissions (user_id, task_id, txid, notes, status, created_at) SELECT ?, id, ?, ?, 'approved', 0 FROM tasks WHERE slug = ?", uid, txid, notes, slug); err != nil {
			t.Fatal(err)
		}
	}

	type c struct {
		kind, txid, notes string
		v                 verdict
		want              string
	}
	run := func(cases []c) {
		t.Helper()
		for _, k := range cases {
			got := app.evidenceCheck(k.kind, k.txid, k.notes, user)
			if got.Verdict != k.v || !strings.Contains(got.Note, k.want) {
				t.Errorf("%s/%s: got (%d) %q, want (%d) ...%q...", k.kind, k.txid, got.Verdict, got.Note, k.v, k.want)
			}
		}
	}
	// Before any prior task is on file.
	run([]c{
		{"first-transaction", "first", "", vPass, "proof amount found (output 0"},
		{"first-transaction", "notag", "", vFail, "proof amount .86718345 NOT found"},
		{"first-transaction", "feeonly", "", vFail, "NOT found"},
		{"first-transaction", "mempool", "", vFail, "not yet confirmed"},
		{"first-transaction", "missing", "", vFail, "txid NOT FOUND"},
		{"first-transaction", "", "", vFail, "no txid submitted"},
		{"anchor-lookup", "first", "anchor is 00000000abcdef00000000000000000000000000000000000000000000000000", vPass, "matches the block's Bitcoin anchor (OK)"},
		{"anchor-lookup", "first", "no idea", vFail, "no hash in the notes matches"},
		{"any-asset-fee", "assetfee", "", vPass, "fee paid in asset 2a515539da5e"},
		{"any-asset-fee", "first", "", vFail, "fee paid in tSEQ"},
		{"multi-asset-send", "multi", "", vPass, "2 assets"},
		{"multi-asset-send", "first", "", vFail, "only one asset"},
		{"confidential-tx", "blinded", "", vPass, "1 blinded output(s) (OK)"},
		{"confidential-tx", "first", "", vFail, "no blinded output"},
		{"issue-asset", "issue", "", vPass, "issues asset 5bb1a1f2cfea"},
		{"issue-asset", "reissue", "", vFail, "this is a reissuance"},
		{"reissue-asset", "reissue", "", vFail, "did not submit"},
		{"burn-asset", "burn", "", vFail, "complete the issuance task first"},
		{"stake", "stake", "", vPass, "staking output of 40,000 tSEQ for key 0322502cadae"},
		{"stake", "stakep2w", "", vFail, "not a staking script"},
		{"stake", "smallstk", "", vFail, "under 40,000"},
		{"delegate-stake", "deleg", "", vFail, "no staking output on file"},
		{"unbond-stake", "unbond", "", vFail, "no staking output on file"},
		{"faucet-consolidate", "consol", "", vPass, "from 2 faucet payments (OK)"},
		{"faucet-consolidate", "consol1", "", vFail, "from 1 faucet payment(s)"},
		{"bridge-in", "bridged", "deposit 0x" + strings.Repeat("ab", 32), vPass, "spends the bridge's delivery for a ethereum deposit (OK)"},
		{"bridge-in", "bridged", "deposit So1anaDepositAddress1111111111111111111", vFail, "does not spend the bridge's delivery for that solana deposit"},
		{"bridge-in", "solbridged", "So1anaDepositAddress1111111111111111111", vPass, "solana deposit (OK)"},
		{"bridge-in", "bridged", "no deposit named", vFail, "name no Ethereum deposit hash"},
		{"bridge-in", "first", "0x" + strings.Repeat("ab", 32), vFail, "does not spend the bridge's delivery"},
		{"bridge-both", "solbridged", "So1anaDepositAddress1111111111111111111", vFail, "complete the first bridge task"},
		{"bridge-out", "returned", "", vPass, "released on the source chain (OK)"},
		{"bridge-out", "waiting", "", vFail, "the redemption is awaiting finality"},
		{"bridge-out", "elsewhere", "", vFail, "not a Compages redemption address"},
		{"none", "first", "", vReview, "reviewed by hand"},
	})
	// With the earlier tasks on file, the dependent checks bind to them.
	approve("issue-asset", "issue", "")
	approve("stake", "stake", "")
	approve("bridge-in", "bridged", "deposit 0x"+strings.Repeat("ab", 32))
	run([]c{
		{"reissue-asset", "reissue", "", vPass, "issued by this account (OK)"},
		{"burn-asset", "burn", "", vPass, "issued by this account (OK)"},
		{"burn-asset", "burnusdx", "", vFail, "which this account did not issue"},
		{"delegate-stake", "deleg", "", vPass, "to pool 02b4632d0848"},
		{"delegate-stake", "delegnp", "", vFail, "no delegation record whose controller is this account's stake key"},
		{"redelegate", "redeleg", "", vFail, "no delegation on file"},
		{"unbond-stake", "unbond", "", vPass, "unbonding output of key 0322502cadae"},
		{"unbond-stake", "unbondelse", "", vFail, "spends none of this account's staking outputs"},
		{"bridge-both", "solbridged", "So1anaDepositAddress1111111111111111111", vPass, "solana deposit (OK)"},
		{"bridge-both", "bridged", "0x" + strings.Repeat("ab", 32), vFail, "your first bridge already came from ethereum"},
	})
	approve("delegate-stake", "deleg", "")
	run([]c{
		{"redelegate", "redeleg", "", vPass, "from 02b4632d0848… to pool 039b14904955"},
		{"redelegate", "redelsame", "", vFail, "names the same pool"},
	})
	// A service that is down sends evidence to a reviewer, never refuses it.
	app.cfg.PoolsURL = "http://127.0.0.1:9/pools.json"
	app.cfg.CompagesURL = "http://127.0.0.1:9"
	run([]c{
		{"delegate-stake", "deleg", "", vReview, "pool board unavailable"},
		{"bridge-out", "returned", "", vReview, "bridge unavailable"},
	})
	app.cfg.EsploraURL = "http://127.0.0.1:9"
	run([]c{{"first-transaction", "first", "", vReview, "explorer unreachable"}})
}

// fakeCLI writes a stand-in sequentia-cli that answers getpeerinfo.
func fakeCLI(t *testing.T, peers string) string {
	dir := t.TempDir()
	p := filepath.Join(dir, "cli")
	os.WriteFile(filepath.Join(dir, "peers.json"), []byte(peers), 0o600)
	os.WriteFile(p, []byte("#!/bin/sh\ncat "+filepath.Join(dir, "peers.json")+"\n"), 0o700)
	return p
}

func TestReleaseCheck(t *testing.T) {
	code := "0123456789"
	sum := strings.Repeat("c3", 32)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sum + "  sequentia-core-25.2.4-linux-x86_64.tar.gz\n" + strings.Repeat("d4", 32) + "  sequentia-core-25.2.4-win64-setup.exe\n"))
	}))
	defer site.Close()
	app, _, _ := newTestServer(t)
	app.cfg.SumsURL = site.URL
	app.cfg.NodeDatadir = t.TempDir()
	cases := []struct {
		peers, notes string
		v            verdict
		want         string
	}{
		{`[{"addr":"1.2.3.4:5","subver":"/Sequentia Core:25.2.4(emissio-0123456789)/"}]`, "sha256: " + sum, vPass, "runs it: /Sequentia Core:25.2.4(emissio-0123456789)/ (OK)"},
		{`[{"addr":"1.2.3.4:5","subver":"/Sequentia Core:25.2.3(emissio-0123456789)/"}]`, sum, vFail, "not release 25.2.4"},
		{`[]`, sum, vFail, "no connected node announces emissio-0123456789"},
		{`[]`, strings.Repeat("ee", 32), vFail, "no checksum in the notes matches"},
	}
	for i, c := range cases {
		app.cfg.NodeCLI = fakeCLI(t, c.peers)
		got := app.releaseCheck(c.notes, code)
		if got.Verdict != c.v || !strings.Contains(got.Note, c.want) {
			t.Errorf("case %d: (%d) %q", i, got.Verdict, got.Note)
		}
	}
}

// TestSubmitVerdicts drives the submission form: a pass is credited at once,
// a fail is refused unless sent to a reviewer, a held account always waits.
func TestSubmitVerdicts(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	plain := []map[string]any{{"is_coinbase": false}}
	txs := map[string]string{
		"aa" + strings.Repeat("0", 62): txJSONid("good", plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		"bb" + strings.Repeat("0", 62): txJSONid("bad", plain, []map[string]any{vout(100000000, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
	}
	explorer := fakeExplorer(t, txs, nil, nil)
	defer explorer.Close()
	app, srv, client := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	register(t, client, srv.URL, "auto@example.com")
	app.db.Exec("UPDATE users SET claim_code = ? WHERE email = 'auto@example.com'", code)
	csrf := csrfOf(t, client, srv.URL+"/account")
	submit := func(slug, txid string, extra url.Values) string {
		v := url.Values{"csrf": {csrf}, "txid": {txid}}
		for k, x := range extra {
			v[k] = x
		}
		resp, err := client.PostForm(srv.URL+"/tasks/"+slug+"/submit", v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Request.URL.RawQuery
	}
	bad := "bb" + strings.Repeat("0", 62)
	good := "aa" + strings.Repeat("0", 62)
	if q := submit("first-transaction", bad, nil); !strings.Contains(q, "Not+accepted") {
		t.Fatalf("a failing check was not refused: %s", q)
	}
	var n int
	app.db.QueryRow("SELECT COUNT(*) FROM submissions").Scan(&n)
	if n != 0 {
		t.Fatal("a refused submission was stored")
	}
	// Sent to a reviewer anyway, it waits as pending with the check's note.
	submit("first-transaction", bad, url.Values{"review": {"1"}})
	var status, note string
	app.db.QueryRow("SELECT status, chain_note FROM submissions ORDER BY id DESC LIMIT 1").Scan(&status, &note)
	if status != "pending" || !strings.Contains(note, "NOT found") {
		t.Fatalf("for review: %s %q", status, note)
	}
	app.db.Exec("UPDATE submissions SET status = 'rejected'")
	// A pass is approved and credited at once.
	if q := submit("first-transaction", good, nil); !strings.Contains(q, "Checked+and+credited") {
		t.Fatalf("pass not credited: %s", q)
	}
	var bal int64
	var auto int
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger").Scan(&bal)
	app.db.QueryRow("SELECT auto FROM submissions WHERE status = 'approved'").Scan(&auto)
	if bal != 10 || auto != 1 {
		t.Fatalf("balance %d auto %d", bal, auto)
	}
	// A held account's passing evidence waits for a reviewer.
	app.db.Exec("UPDATE users SET held = 1")
	submit("anchor-lookup", good, url.Values{"notes": {"x"}, "review": {"1"}})
	app.db.QueryRow("SELECT status FROM submissions ORDER BY id DESC LIMIT 1").Scan(&status)
	if status != "pending" {
		t.Fatalf("held account: %s", status)
	}
	// The dashboard shows the progress, the open tasks, the earnings and the reports.
	postBug(t, client, srv.URL, csrf, map[string]string{"surface": "faucet", "title": "Claim page hangs", "steps": "claim twice", "actual": "spinner"}, nil)
	acct := get(t, client, srv.URL+"/account")
	for _, want := range []string{"1 of 21 tasks done", "<strong>Sequentia Core</strong>", "1 of 12 tasks done", "1 waiting for review",
		"What you earned from", "Open tasks", "Combine two faucet payments", "Claim page hangs", "progress"} {
		if !strings.Contains(acct, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(acct, ">Make your first testnet transaction</a><span class=\"sub\">") {
		t.Error("a completed task is listed as open")
	}
}

func TestSeedCatalogue(t *testing.T) {
	seen := map[string]bool{}
	var total int64
	for _, s := range seedTasks {
		if seen[s.slug] {
			t.Errorf("duplicate slug %s", s.slug)
		}
		seen[s.slug] = true
		if !knownCheck(s.check) {
			t.Errorf("%s: unknown check %q", s.slug, s.check)
		}
		if surfaceTitle(s.surface) == s.surface {
			t.Errorf("%s: unknown surface %q", s.slug, s.surface)
		}
		if s.reward < 10 || s.reward > 50 {
			t.Errorf("%s: reward %d outside 10..50", s.slug, s.reward)
		}
		total += s.reward
		for _, hidden := range []string{"SeqDEX", "web wallet", "Ambra", "Lightning", "SeqLN", "Pignus", "Levo", "SeqPal", "decentralized", "the Bitcoin sidechain", "—", "SEQ chain"} {
			if strings.Contains(s.body+s.title, hidden) {
				t.Errorf("%s names %q", s.slug, hidden)
			}
		}
	}
	for _, s := range surfaces {
		n := 0
		for _, t := range seedTasks {
			if t.surface == s.Key {
				n++
			}
		}
		if n == 0 {
			t.Errorf("no task on surface %s", s.Key)
		}
	}
	t.Logf("%d tasks, %d SEQ for the full track", len(seedTasks), total)
}

// TestSeedBackfill: a database from before tasks had a surface and a check
// gets them for its seeded tasks on start, with its copy and amounts untouched.
func TestSeedBackfill(t *testing.T) {
	app, _, _ := newTestServer(t)
	app.db.Exec("UPDATE tasks SET check_kind = '', surface = '', reward = 77, body = 'old copy' WHERE slug = 'stake'")
	app.db.Exec("UPDATE tasks SET check_kind = 'none' WHERE slug = 'first-transaction'")
	seedDB(app.db)
	tk, _ := taskBySlug(app.db, "stake")
	if tk.CheckKind != "stake" || tk.Surface != "core" || tk.Reward != 77 || tk.Body != "old copy" {
		t.Fatalf("backfill: %+v", tk)
	}
	if tk, _ := taskBySlug(app.db, "first-transaction"); tk.CheckKind != "none" {
		t.Fatal("an admin's choice of check was overwritten")
	}
}
