package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The fee-list tasks' values for the account with claim code 0123456789,
// whose proof decimals are 86718345: 86718345 mod 16 = 9, so the whitelist
// task's USDX price is 0.39 (rate 39,000,000); 86718345 / 16 mod 16 = 8, so
// the price-server task's is 0.63 (rate 63,000,000).
func TestFeeTaskValues(t *testing.T) {
	tag := proofTag("0123456789")
	if got := feeTaskRateFor("whitelist-fee", tag); got != 39_000_000 {
		t.Errorf("whitelist rate %d", got)
	}
	if got := feeTaskRateFor("price-server-fee", tag); got != 63_000_000 {
		t.Errorf("price-server rate %d", got)
	}
	if got := priceText(feeTaskCents("whitelist-fee", tag)); got != "0.39" {
		t.Errorf("whitelist price %s", got)
	}
	// The two ranges never meet, and both stay below the network's 1.
	for n := 0; n < 4096; n++ {
		tg := fmt.Sprintf("%08d", n*24413)
		a, b := feeTaskCents("whitelist-fee", tg), feeTaskCents("price-server-fee", tg)
		if a < 30 || a > 45 || b < 55 || b > 70 {
			t.Fatalf("%s: %d %d", tg, a, b)
		}
	}
	// The fee formula, as a node computes it (measured on regtest at rate
	// 62,000,000: vsize 326 at fee rate 1 paid 526 atoms, at fee rate 3
	// paid 1578; the task's fee rate 2 is between).
	if got := feeAt(62_000_000, 326); got != 1052 {
		t.Errorf("feeAt = %d", got)
	}
}

func feeTx(id string, weight int64, outs ...map[string]any) string {
	m := map[string]any{"txid": id, "vin": []map[string]any{{"is_coinbase": false}}, "vout": outs, "weight": weight,
		"status": map[string]any{"confirmed": true, "block_height": 42, "block_hash": "bh"}}
	b, _ := json.Marshal(m)
	return string(b)
}

// Every verdict of the two fee-list checks, and the slips a tester makes.
func TestFeeListChecks(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	usdx := "2a515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de"
	gold := "aa515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de"
	proof := vout(100000000+tag, usdx, "v0_p2wpkh")
	const w = 1302 // vsize 326
	txs := map[string]string{
		"listed":     feeTx("listed", w, proof, vout(feeAt(39_000_000, 326), usdx, "fee")),
		"listedbyte": feeTx("listedbyte", w, proof, vout(feeAt(39_000_000, 327), usdx, "fee")),
		"server":     feeTx("server", w, proof, vout(feeAt(63_000_000, 326), usdx, "fee")),
		"network":    feeTx("network", w, proof, vout(feeAt(100_000_000, 326), usdx, "fee")),
		"feerate3":   feeTx("feerate3", w, proof, vout(50000, usdx, "v0_p2wpkh"), vout(2508, usdx, "fee")),
		"gold":       feeTx("gold", w, proof, vout(feeAt(39_000_000, 326), gold, "fee")),
		"tseq":       feeTx("tseq", w, proof, vout(652, policy, "fee")),
		"nofee":      feeTx("nofee", w, proof),
		"nosize":     feeTx("nosize", 0, proof, vout(feeAt(39_000_000, 326), usdx, "fee")),
		"notag":      feeTx("notag", w, vout(100000000, usdx, "v0_p2wpkh"), vout(feeAt(39_000_000, 326), usdx, "fee")),
		// The reviewer's transaction: 191 vbytes, no change, 100 atoms of dust change added to the fee.
		"folded": feeTx("folded", 764, proof, vout(1418, usdx, "fee")),
		// The same overpayment with a change output is not the wallet's folding.
		"overpaid": feeTx("overpaid", w, proof, vout(50000, usdx, "v0_p2wpkh"), vout(feeAt(39_000_000, 326)+100, usdx, "fee")),
	}
	// A fee priced on the discounted vsize: the pass note names that size.
	disc := map[string]any{"txid": "disc", "vin": []map[string]any{{"is_coinbase": false}}, "weight": 2726 * 4, "discount_vsize": 461,
		"vout":   []map[string]any{proof, vout(feeAt(39_000_000, 461), usdx, "fee")},
		"status": map[string]any{"confirmed": true, "block_height": 42, "block_hash": "bh"}}
	b, _ := json.Marshal(disc)
	txs["disc"] = string(b)
	explorer := fakeExplorer(t, txs, nil, nil)
	defer explorer.Close()
	app, _, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.PolicyAsset = policy
	app.cfg.USDXAsset = usdx
	uid, _ := createUser(app.db, "fees@example.com", "x", code, "")
	user := &User{ID: uid, ClaimCode: code}
	for _, k := range []struct {
		kind, txid string
		v          verdict
		want       string
	}{
		{"whitelist-fee", "listed", vPass, "your node valued USDX at 0.39, rate 39000000 (OK)"},
		{"whitelist-fee", "listedbyte", vPass, "(OK)"},
		{"price-server-fee", "server", vPass, "your node valued USDX at 0.63, rate 63000000 (OK)"},
		{"whitelist-fee", "server", vFail, "follows from the price of the task \"Run the price server\" (0.63)"},
		{"price-server-fee", "listed", vFail, "follows from the price of the task \"List an asset for fees on your node\" (0.39)"},
		{"whitelist-fee", "network", vFail, "valued USDX at 1, the network's rate (100000000): list it at 39000000 first"},
		{"price-server-fee", "network", vFail, "list it at 63000000 first"},
		{"whitelist-fee", "feerate3", vFail, "this task asks for rate 39000000 (USDX at 0.39) and fee_rate=2"},
		{"whitelist-fee", "gold", vFail, "this task's fee is paid in USDX"},
		{"whitelist-fee", "tseq", vFail, "this task's fee is paid in USDX"},
		{"whitelist-fee", "nofee", vFail, "no fee output"},
		{"whitelist-fee", "nosize", vReview, "gives no size"},
		{"whitelist-fee", "notag", vFail, "NOT found"},
		{"whitelist-fee", "folded", vFail, "the transaction has no change, and its fee of 1418 USDX atoms is above the 980 your rate gives for its 191 vbytes"},
		{"whitelist-fee", "overpaid", vFail, "a rate of about"},
		{"whitelist-fee", "disc", vPass, fmt.Sprintf("a fee of %d USDX atoms for 461 vbytes", feeAt(39_000_000, 461))},
	} {
		got := app.evidenceCheck(k.kind, k.txid, "", user)
		if got.Verdict != k.v || !strings.Contains(got.Note, k.want) {
			t.Errorf("%s/%s: got (%d) %q, want (%d) ...%q...", k.kind, k.txid, got.Verdict, got.Note, k.v, k.want)
		}
	}
	if got := app.evidenceCheck("whitelist-fee", "notag", "", user); !got.Firm {
		t.Error("a transaction without the proof decimals is not a firm refusal")
	}
	if got := app.evidenceCheck("whitelist-fee", "folded", "", user); got.Firm || !strings.Contains(got.Note, "keeps a change output") {
		t.Errorf("folded change: %+v", got)
	}
}

// The task pages give each account its own price and rate, and a signed-out
// reader a placeholder; the two older fee tasks point to the listing task.
func TestFeeTaskPages(t *testing.T) {
	app, srv, client := newTestServer(t)
	page := get(t, client, srv.URL+"/tasks/fee-whitelist")
	if !strings.Contains(page, "&lt;your rate&gt;") {
		t.Error("signed out: no placeholder for the rate")
	}
	register(t, client, srv.URL, "lister@x.io")
	var code string
	app.db.QueryRow("SELECT claim_code FROM users WHERE email = 'lister@x.io'").Scan(&code)
	tag := proofTag(code)
	page = get(t, client, srv.URL+"/tasks/fee-whitelist")
	for _, want := range []string{
		fmt.Sprintf("&#34;USDX&#34;: %d}", feeTaskRateFor("whitelist-fee", tag)),
		"values USDX at " + priceText(feeTaskCents("whitelist-fee", tag)),
		"fee_rate=2", "reference atoms per vByte", "A lower rate means your node values the asset less, so it pays more of it",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("whitelist task page: no %q", want)
		}
	}
	page = get(t, client, srv.URL+"/tasks/price-server")
	for _, want := range []string{
		fmt.Sprintf("&#34;USDX&#34;: %s}", priceText(feeTaskCents("price-server-fee", tag))),
		fmt.Sprintf("is %d: find it", feeTaskRateFor("price-server-fee", tag)), "--once --dry-run",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("price-server task page: no %q", want)
		}
	}
	for _, slug := range []string{"any-asset-fee", "emissio-return"} {
		if p := get(t, client, srv.URL+"/tasks/"+slug); !strings.Contains(p, "List an asset for fees on your node") {
			t.Errorf("%s does not point to the listing task", slug)
		}
	}
	for _, p := range []string{get(t, client, srv.URL+"/tasks/fee-whitelist"), get(t, client, srv.URL+"/tasks/price-server")} {
		if strings.Contains(p, "sat/vB") || strings.Contains(p, "sat/vbyte") {
			t.Error("a fee task names sat/vB")
		}
	}
}
