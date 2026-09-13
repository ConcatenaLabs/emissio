package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const policy = "c8eccacf0953e1931cd31e434d8319101cc36e6c38b0e2104d8687552fae3e40"

func TestProofTag(t *testing.T) {
	if got := proofTag("0000000000"); got != "00000000" {
		t.Errorf("zero code: %q", got)
	}
	tag := proofTag("0123456789")
	if len(tag) != 8 || tag != "00000000" && tag == proofTag("0123456788") {
		t.Errorf("tag %q", tag)
	}
	// 0x0123456789 = 4886718345 -> % 1e8 = 86718345
	if tag != "86718345" {
		t.Errorf("proofTag(0123456789) = %q, want 86718345", tag)
	}
}

// A fake explorer serving canned transactions, assets and blocks.
func fakeExplorer(t *testing.T, txs map[string]string, assets map[string]string, blocks map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		var ok bool
		switch {
		case strings.HasPrefix(r.URL.Path, "/tx/"):
			body, ok = txs[strings.TrimPrefix(r.URL.Path, "/tx/")]
		case strings.HasPrefix(r.URL.Path, "/asset/"):
			body, ok = assets[strings.TrimPrefix(r.URL.Path, "/asset/")]
		case strings.HasPrefix(r.URL.Path, "/block/"):
			body, ok = blocks[strings.TrimPrefix(r.URL.Path, "/block/")]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
}

func vout(value int64, asset, typ string) map[string]any {
	return map[string]any{"value": value, "asset": asset, "scriptpubkey_type": typ}
}

func txJSON(t *testing.T, vin []map[string]any, vout []map[string]any, confirmed bool) string {
	m := map[string]any{"txid": "aa", "vin": vin, "vout": vout,
		"status": map[string]any{"confirmed": confirmed, "block_height": 42, "block_hash": "bh"}}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestEvidenceCheck(t *testing.T) {
	code := "0123456789"
	tag := int64(86718345)
	usdx := "2a515539da5e6a60caa7766ecd65bac0c10d15717ddd2088844ba58f4d04b9de"
	other := "5bb1a1f2cfea8ad1f4b79f058005fa1ddd0d8b4a623e59974b99abbbf1fa80e4"
	plain := []map[string]any{{"is_coinbase": false}}
	txs := map[string]string{
		"first":    txJSON(t, plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		"notag":    txJSON(t, plain, []map[string]any{vout(100000000, policy, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		"feeonly":  txJSON(t, plain, []map[string]any{vout(100000000, policy, "v0_p2wpkh"), vout(tag, policy, "fee")}, true),
		"assetfee": txJSON(t, plain, []map[string]any{vout(100000000+tag, usdx, "v0_p2wpkh"), vout(41, usdx, "fee")}, true),
		"multi":    txJSON(t, plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), vout(5, usdx, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		"blinded":  txJSON(t, plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh"), {"valuecommitment": "08aa", "scriptpubkey_type": "v0_p2wpkh"}, vout(41, policy, "fee")}, true),
		"issue": txJSON(t, []map[string]any{{"issuance": map[string]any{"asset_id": other, "is_reissuance": false}}},
			[]map[string]any{vout(100000000000+tag, other, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		"reissue": txJSON(t, []map[string]any{{"issuance": map[string]any{"asset_id": other, "is_reissuance": true}}},
			[]map[string]any{vout(50000000000+tag, other, "v0_p2wpkh"), vout(41, policy, "fee")}, true),
		"stake":    txJSON(t, plain, []map[string]any{vout(40000*100000000+tag, policy, "unknown"), vout(41, policy, "fee")}, true),
		"smallstk": txJSON(t, plain, []map[string]any{vout(39999*100000000+tag, policy, "unknown"), vout(41, policy, "fee")}, true),
		"mempool":  txJSON(t, plain, []map[string]any{vout(100000000+tag, policy, "v0_p2wpkh")}, false),
	}
	assets := map[string]string{other: `{"issuance_txin":{"txid":"ISSUE"}}`}
	blocks := map[string]string{"bh": `{"bitcoin_anchor":{"hash":"00000000ABCDEF00000000000000000000000000000000000000000000000000","height":7}}`}
	srv := fakeExplorer(t, txs, assets, blocks)
	defer srv.Close()

	app, _, _ := newTestServer(t)
	app.cfg.EsploraURL = srv.URL
	app.cfg.PolicyAsset = policy
	uid, err := createUser(app.db, "prover@example.com", "x", code, "")
	if err != nil {
		t.Fatal(err)
	}
	user := &User{ID: uid, ClaimCode: code}
	// The reissue check looks for this account's issue-asset submission.
	if _, err := app.db.Exec("INSERT INTO submissions (user_id, task_id, txid, status, created_at) SELECT ?, id, 'issue', 'approved', 0 FROM tasks WHERE slug = 'issue-asset'", uid); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ slug, txid, notes, want string }{
		{"first-transaction", "first", "", "proof amount found (output 0, asset c8eccacf0953…); confirmed at height 42"},
		{"first-transaction", "notag", "", "proof amount .86718345 NOT found in any transparent output; confirmed at height 42"},
		{"first-transaction", "feeonly", "", "proof amount .86718345 NOT found in any transparent output; confirmed at height 42"},
		{"first-transaction", "mempool", "", "proof amount found (output 0, asset c8eccacf0953…); found in the mempool, not yet confirmed"},
		{"first-transaction", "missing", "", "txid NOT FOUND on the testnet"},
		{"any-asset-fee", "assetfee", "", "proof amount found (output 0, asset 2a515539da5e…); fee paid in asset 2a515539da5e… (OK); confirmed at height 42"},
		{"any-asset-fee", "first", "", "proof amount found (output 0, asset c8eccacf0953…); fee paid in tSEQ: does NOT qualify; confirmed at height 42"},
		{"multi-asset-send", "multi", "", "proof amount found (output 0, asset c8eccacf0953…); 2 assets in the outputs (OK); confirmed at height 42"},
		{"multi-asset-send", "first", "", "proof amount found (output 0, asset c8eccacf0953…); only one asset in the outputs: does NOT qualify; confirmed at height 42"},
		{"confidential-tx", "blinded", "", "proof amount found (output 0, asset c8eccacf0953…); 1 blinded output(s) (OK); confirmed at height 42"},
		{"confidential-tx", "first", "", "proof amount found (output 0, asset c8eccacf0953…); no blinded output: does NOT qualify; confirmed at height 42"},
		{"issue-asset", "issue", "", "proof amount found (output 0, asset 5bb1a1f2cfea…); issues asset 5bb1a1f2cfea… (OK); confirmed at height 42"},
		{"issue-asset", "reissue", "", "proof amount found (output 0, asset 5bb1a1f2cfea…); this is a reissuance, not a new issuance; confirmed at height 42"},
		{"reissue-asset", "reissue", "", "proof amount found (output 0, asset 5bb1a1f2cfea…); reissues asset 5bb1a1f2cfea…; issued by this account (OK); confirmed at height 42"},
		{"reissue-asset", "first", "", "proof amount found (output 0, asset c8eccacf0953…); no issuance in this transaction: does NOT qualify; confirmed at height 42"},
		{"stake", "stake", "", "proof amount found (output 0, asset c8eccacf0953…); proof output holds 40,000 tSEQ (OK); reviewer confirms the staking script; confirmed at height 42"},
		{"stake", "smallstk", "", "proof amount found (output 0, asset c8eccacf0953…); proof output is under 40,000 tSEQ: does NOT qualify; confirmed at height 42"},
		{"anchor-lookup", "first", "anchor is 00000000abcdef00000000000000000000000000000000000000000000000000 at 7", "proof amount found (output 0, asset c8eccacf0953…); anchor hash in the notes matches the block's Bitcoin anchor (OK); confirmed at height 42"},
		{"anchor-lookup", "first", "no idea", "proof amount found (output 0, asset c8eccacf0953…); no hash in the notes matches the block's Bitcoin anchor; confirmed at height 42"},
		{"bridge-in", "assetfee", "", "proof amount found (output 0, asset 2a515539da5e…); moves asset 2a515539da5e…; reviewer confirms it is a Compages asset; confirmed at height 42"},
		{"bridge-in", "first", "", "proof amount found (output 0, asset c8eccacf0953…); proof output is tSEQ; it must move the bridged asset; confirmed at height 42"},
		{"report-bug", "", "", "a maintainer confirms the linked issue"},
	}
	for _, c := range cases {
		if got := app.evidenceCheck(c.slug, c.txid, c.notes, user); got != c.want {
			t.Errorf("%s/%s:\n got %q\nwant %q", c.slug, c.txid, got, c.want)
		}
	}
}

func TestPeerCodes(t *testing.T) {
	ps := []peer{
		{Addr: "1.2.3.4:1", Subver: "/Sequentia Core:24.7.10(emissio-0123456789)/"},
		{Addr: "1.2.3.5:1", Subver: "/Emissio-ABCDEFabcd:24.7.10/"},
		{Addr: "1.2.3.6:1", Subver: "/Sequentia Core:24.7.10/"},
	}
	codes := peerCodes(ps)
	if len(codes) != 2 || codes["0123456789"] == "" || codes["abcdefabcd"] == "" {
		t.Errorf("peerCodes = %v", codes)
	}
	if !builtClientName(ps[1].Subver, "abcdefABCD") || builtClientName(ps[0].Subver, "0123456789") {
		t.Errorf("builtClientName wrong")
	}
}

func TestUptimeVerdict(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("CREATE TABLE peer_sightings (code TEXT NOT NULL, seen_at INTEGER NOT NULL)")
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if got := uptimeVerdict(db, "abc", now); !strings.HasPrefix(got, "node never seen") {
		t.Errorf("empty: %q", got)
	}
	// A full week at the slot cadence, with one slot in ten missing.
	start := now.Add(-7 * 24 * time.Hour)
	for i := 0; i < 7*24*6; i++ {
		if i%10 == 3 {
			continue
		}
		db.Exec("INSERT INTO peer_sightings VALUES ('abc', ?)", start.Add(time.Duration(i)*uptimeSlot).Unix())
	}
	got := uptimeVerdict(db, "abc", now)
	if !strings.HasSuffix(got, "(OK)") || !strings.Contains(got, "90%") {
		t.Errorf("full week: %q", got)
	}
	// Two days only.
	db.Exec("DELETE FROM peer_sightings")
	for i := 0; i < 2*24*6; i++ {
		db.Exec("INSERT INTO peer_sightings VALUES ('abc', ?)", start.Add(time.Duration(i)*uptimeSlot).Unix())
	}
	if got := uptimeVerdict(db, "abc", now); !strings.HasSuffix(got, "(does NOT yet qualify)") {
		t.Errorf("two days: %q", got)
	}
}
