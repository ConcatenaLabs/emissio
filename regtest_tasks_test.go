package main

// The counterparty tasks end to end, against a real Sequentia node on a
// private chain: Emissio's wallet pays through sendtoaddress, a tester's
// wallet returns the payment or unblinds it, and the checks read the
// transactions back. The explorer is a stand-in that serves the node's own
// view of each transaction in the explorer's shape.
//
// Skipped unless EMISSIO_REGTEST_TASKS_CLI names a sequentia-cli wrapper for
// an elementsregtest node (-con_any_asset_fees=1, -txindex=1) holding the
// wallets "emissio-tasks" (two issued assets, fee rates set for them),
// "tester" (some of each asset and the policy asset) and "gen" (to mine), and
// EMISSIO_REGTEST_TASKS_DATADIR names its data directory.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type regNode struct {
	t   *testing.T
	cli string
}

func (n regNode) call(args ...string) string {
	n.t.Helper()
	out, err := exec.Command(n.cli, args...).Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		n.t.Fatalf("%v: %v %s", args, err, msg)
	}
	return strings.TrimSpace(string(out))
}

func (n regNode) json(out any, args ...string) {
	n.t.Helper()
	d := json.NewDecoder(strings.NewReader(n.call(args...)))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		n.t.Fatalf("%v: %v", args, err)
	}
}

func (n regNode) mine() {
	n.call("-rpcwallet=gen", "generatetoaddress", "1", n.call("-rpcwallet=gen", "getnewaddress"))
}

// explorerShim answers /tx/<txid> from the node, in the explorer's shape.
func explorerShim(n regNode) *httptest.Server {
	type rawOut struct {
		Value           *json.Number `json:"value"`
		Asset           string       `json:"asset"`
		ValueCommitment string       `json:"valuecommitment"`
		ScriptPubKey    struct {
			Hex     string `json:"hex"`
			Address string `json:"address"`
			Type    string `json:"type"`
		} `json:"scriptPubKey"`
	}
	type rawTx struct {
		Txid string `json:"txid"`
		Vin  []struct {
			Txid     string `json:"txid"`
			Vout     int    `json:"vout"`
			Issuance *struct {
				Asset        string `json:"asset"`
				IsReissuance bool   `json:"isreissuance"`
			} `json:"issuance"`
		} `json:"vin"`
		Vout          []rawOut `json:"vout"`
		Confirmations int      `json:"confirmations"`
		Blockhash     string   `json:"blockhash"`
	}
	get := func(txid string) (*rawTx, error) {
		out, err := exec.Command(n.cli, "getrawtransaction", txid, "1").Output()
		if err != nil {
			return nil, err
		}
		d := json.NewDecoder(bytes.NewReader(out))
		d.UseNumber()
		var tx rawTx
		return &tx, d.Decode(&tx)
	}
	conv := func(o rawOut) map[string]any {
		typ := o.ScriptPubKey.Type
		switch typ {
		case "nulldata":
			typ = "op_return"
		case "witness_v0_keyhash":
			typ = "v0_p2wpkh"
		}
		m := map[string]any{"asset": o.Asset, "scriptpubkey": o.ScriptPubKey.Hex, "scriptpubkey_address": o.ScriptPubKey.Address, "scriptpubkey_type": typ}
		if o.Value != nil {
			v, _ := atomsOf(o.Value.String())
			m["value"] = v
		}
		if o.ValueCommitment != "" {
			m["valuecommitment"] = o.ValueCommitment
		}
		return m
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		txid, ok := strings.CutPrefix(r.URL.Path, "/tx/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		tx, err := get(txid)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var vin []map[string]any
		for _, in := range tx.Vin {
			m := map[string]any{"txid": in.Txid, "vout": in.Vout}
			if prev, err := get(in.Txid); err == nil && in.Vout < len(prev.Vout) {
				m["prevout"] = conv(prev.Vout[in.Vout])
			}
			if in.Issuance != nil {
				m["issuance"] = map[string]any{"asset_id": in.Issuance.Asset, "is_reissuance": in.Issuance.IsReissuance}
			}
			vin = append(vin, m)
		}
		var vout []map[string]any
		for _, o := range tx.Vout {
			vout = append(vout, conv(o))
		}
		status := map[string]any{"confirmed": tx.Confirmations > 0}
		if tx.Confirmations > 0 {
			var hdr struct {
				Height int64 `json:"height"`
			}
			out, _ := exec.Command(n.cli, "getblockheader", tx.Blockhash).Output()
			json.Unmarshal(out, &hdr)
			status["block_height"], status["block_hash"] = hdr.Height, tx.Blockhash
		}
		json.NewEncoder(w).Encode(map[string]any{"txid": tx.Txid, "vin": vin, "vout": vout, "status": status})
	}))
}

func TestRegtestCounterparty(t *testing.T) {
	cli, datadir := os.Getenv("EMISSIO_REGTEST_TASKS_CLI"), os.Getenv("EMISSIO_REGTEST_TASKS_DATADIR")
	if cli == "" || datadir == "" {
		t.Skip("EMISSIO_REGTEST_TASKS_CLI and EMISSIO_REGTEST_TASKS_DATADIR not set")
	}
	n := regNode{t, cli}
	shim := explorerShim(n)
	defer shim.Close()
	app, srv, client := newTestServer(t)
	app.cfg.EsploraURL = shim.URL
	app.cfg.NodeCLI, app.cfg.NodeDatadir, app.cfg.Wallet = cli, datadir, "emissio-tasks"
	app.cfg.WalletDailyCap = 100
	register(t, client, srv.URL, "tester@example.com")
	app.db.Exec(`INSERT INTO submissions (user_id, task_id, txid, status, created_at)
		SELECT 1, id, 'x', 'approved', 0 FROM tasks WHERE slug = 'first-transaction'`)
	csrf := csrfOf(t, client, srv.URL+"/account")
	post := func(path string, v url.Values) string {
		v.Set("csrf", csrf)
		resp, err := client.PostForm(srv.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		q, _ := url.ParseQuery(resp.Request.URL.RawQuery)
		return q.Get("m") + q.Get("e")
	}
	tw := "-rpcwallet=tester"
	transparent := n.call(tw, "getnewaddress")
	blinded := n.call(tw, "getnewaddress", "", "blech32")

	// Address types are checked against the task.
	if m := post("/tasks/emissio-return/request", url.Values{"address": {blinded}}); !strings.Contains(m, "transparent address") {
		t.Fatalf("blinded address for the return task: %q", m)
	}
	if m := post("/tasks/emissio-unblind/request", url.Values{"address": {transparent}}); !strings.Contains(m, "confidential address") {
		t.Fatalf("transparent address for the unblind task: %q", m)
	}
	// A wallet name that reads as a treasury is never used.
	app.cfg.Wallet = "treasury2026"
	if m := post("/tasks/emissio-return/request", url.Values{"address": {transparent}}); !strings.Contains(m, "cannot pay") {
		t.Fatalf("treasury wallet: %q", m)
	}
	app.cfg.Wallet = "emissio-tasks"

	// ---- the return task ----
	if m := post("/tasks/emissio-return/request", url.Values{"address": {transparent}}); !strings.Contains(m, "Emissio has paid you") {
		t.Fatalf("payment request: %q", m)
	}
	p := app.latestPayment(1, "emissio-return")
	if p == nil || p.Atoms < minPaymentAtoms || p.Atoms >= minPaymentAtoms+paymentAtomsRange {
		t.Fatalf("payment: %+v", p)
	}
	t.Logf("Emissio paid %s of %s in %s", amountText(p.Atoms), p.Asset, p.Txid)
	// The payment's fee is in the asset it pays, never a default.
	var paid struct {
		Fee map[string]json.Number `json:"fee"`
	}
	n.json(&paid, "getrawtransaction", p.Txid, "1")
	if _, ok := paid.Fee[p.Asset]; !ok || len(paid.Fee) != 1 {
		t.Fatalf("payment fee not in the paid asset: %v", paid.Fee)
	}
	if m := post("/tasks/emissio-return/request", url.Values{"address": {transparent}}); !strings.Contains(m, "less than a day ago") {
		t.Fatalf("second request within the day: %q", m)
	}
	n.mine()
	ret, _ := app.returnAddress()
	amount := amountText(p.Atoms)

	// returnTx builds and broadcasts a return from the tester's wallet.
	returnTx := func(inputs []map[string]any, feeAsset string) string {
		in, _ := json.Marshal(inputs)
		outs, _ := json.Marshal([]map[string]any{{ret: amount, "asset": p.Asset}})
		raw := n.call(tw, "createrawtransaction", string(in), string(outs))
		opts, _ := json.Marshal(map[string]any{"fee_asset": feeAsset})
		var funded struct {
			Hex string `json:"hex"`
		}
		n.json(&funded, tw, "fundrawtransaction", raw, string(opts))
		var signed struct {
			Hex string `json:"hex"`
		}
		n.json(&signed, tw, "signrawtransactionwithwallet", funded.Hex)
		txid := n.call("sendrawtransaction", signed.Hex)
		n.mine()
		return txid
	}
	var vout int
	var ptx struct {
		Vout []struct {
			N            int `json:"n"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	n.json(&ptx, "getrawtransaction", p.Txid, "1")
	for _, o := range ptx.Vout {
		if o.ScriptPubKey.Address == transparent {
			vout = o.N
		}
	}
	// Negative: the right amount back, from another coin of the asset.
	var coins []struct {
		Txid  string `json:"txid"`
		Vout  int    `json:"vout"`
		Asset string `json:"asset"`
	}
	n.json(&coins, tw, "listunspent", "1")
	var other map[string]any
	for _, c := range coins {
		if c.Asset == p.Asset && c.Txid != p.Txid {
			other = map[string]any{"txid": c.Txid, "vout": c.Vout}
		}
	}
	if other == nil {
		t.Fatal("tester has no other coin of the paid asset")
	}
	n.call(tw, "lockunspent", "false", `[{"txid":"`+p.Txid+`","vout":`+jsonInt(vout)+`}]`)
	wrongCoin := returnTx([]map[string]any{other}, p.Asset)
	n.call(tw, "lockunspent", "true")
	if m := post("/tasks/emissio-return/submit", url.Values{"txid": {wrongCoin}}); !strings.Contains(m, "does not spend Emissio's payment") {
		t.Fatalf("return from another coin: %q", m)
	}
	// Positive: spends Emissio's coin, exact amount, fee in that asset.
	good := returnTx([]map[string]any{{"txid": p.Txid, "vout": vout}}, p.Asset)
	if m := post("/tasks/emissio-return/submit", url.Values{"txid": {good}}); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("good return: %q", m)
	}
	var bal int64
	app.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 25 {
		t.Fatalf("balance %d after the return", bal)
	}
	t.Logf("return %s credited", good)

	// Negative: a fresh payment returned with the fee in another asset.
	app.db.Exec("UPDATE emissio_payments SET created_at = created_at - 200000")
	app.db.Exec("UPDATE submissions SET status = 'rejected' WHERE status = 'approved' AND task_id = (SELECT id FROM tasks WHERE slug = 'emissio-return')")
	app.db.Exec("DELETE FROM ledger WHERE kind = 'submission'")
	if m := post("/tasks/emissio-return/request", url.Values{"address": {transparent}}); !strings.Contains(m, "Emissio has paid you") {
		t.Fatalf("second payment: %q", m)
	}
	p = app.latestPayment(1, "emissio-return")
	amount = amountText(p.Atoms)
	n.mine()
	n.json(&ptx, "getrawtransaction", p.Txid, "1")
	for _, o := range ptx.Vout {
		if o.ScriptPubKey.Address == transparent {
			vout = o.N
		}
	}
	var policyAsset string
	var labels map[string]string
	n.json(&labels, "dumpassetlabels")
	policyAsset = labels["bitcoin"]
	otherFee := returnTx([]map[string]any{{"txid": p.Txid, "vout": vout}}, policyAsset)
	if m := post("/tasks/emissio-return/submit", url.Values{"txid": {otherFee}}); !strings.Contains(m, "the fee is paid in") {
		t.Fatalf("fee in another asset: %q", m)
	}

	// ---- the unblind task ----
	if m := post("/tasks/emissio-unblind/request", url.Values{"address": {blinded}}); !strings.Contains(m, "Emissio has paid you") {
		t.Fatalf("confidential payment request: %q", m)
	}
	u := app.latestPayment(1, "emissio-unblind")
	n.mine()
	// The amount is blinded on the chain.
	var blindedTx struct {
		Vout []struct {
			Value           *json.Number `json:"value"`
			ValueCommitment string       `json:"valuecommitment"`
		} `json:"vout"`
	}
	n.json(&blindedTx, "getrawtransaction", u.Txid, "1")
	hidden := false
	for _, o := range blindedTx.Vout {
		hidden = hidden || (o.Value == nil && o.ValueCommitment != "")
	}
	if !hidden {
		t.Fatal("Emissio's confidential payment has no blinded output")
	}
	// The tester's wallet unblinds it.
	var wtx struct {
		Details []struct {
			Category string      `json:"category"`
			Amount   json.Number `json:"amount"`
			Asset    string      `json:"asset"`
		} `json:"details"`
	}
	n.json(&wtx, tw, "gettransaction", u.Txid)
	seen := ""
	for _, d := range wtx.Details {
		if d.Category == "receive" && d.Asset == u.Asset {
			seen = d.Amount.String()
		}
	}
	if v, ok := atomsOf(seen); !ok || v != u.Atoms {
		t.Fatalf("wallet sees %q, Emissio paid %s", seen, amountText(u.Atoms))
	}
	page := get(t, client, srv.URL+"/tasks/emissio-unblind")
	if strings.Contains(page, amountText(u.Atoms)) {
		t.Fatal("the task page shows the confidential amount")
	}
	if m := post("/tasks/emissio-unblind/submit", url.Values{"notes": {"0.12345678"}}); !strings.Contains(m, "do not name the amount") {
		t.Fatalf("wrong amount: %q", m)
	}
	if m := post("/tasks/emissio-unblind/submit", url.Values{"notes": {"The wallet shows " + seen + " received"}}); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("right amount: %q", m)
	}
	t.Logf("unblinded %s of %s from %s, credited", seen, u.Asset, u.Txid)
}

func jsonInt(i int) string { b, _ := json.Marshal(i); return string(b) }
