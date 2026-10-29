package main

import (
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvoiceAmount(t *testing.T) {
	for inv, want := range map[string]int64{"lntb100u1pjxyz": 10_000, "lntb1m1pjxyz": 100_000, "lntbs25000n1pqq": 2_500, "lntb2500u1qq": 250_000} {
		if got, err := invoiceAmount(inv); err != nil || got != want {
			t.Errorf("%s: %d %v", inv, got, err)
		}
	}
	for _, bad := range []string{"lntb1pjxyz", "lntb15p1qq", "hello", "lntb0u1qq"} {
		if _, err := invoiceAmount(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestArcaTasks(t *testing.T) {
	app, srv, client := newTestServer(t)
	dir := t.TempDir()
	bin, _ := filepath.Abs("testdata/fake-arca.py")
	app.cfg.ArcaBin, app.cfg.ArcaDatadir = bin, dir
	register(t, client, srv.URL, "ww@x.io")
	app.db.Exec("UPDATE users SET claim_code = '0123456789' WHERE id = 1")
	csrf := csrfOf(t, client, srv.URL+"/account")
	post := func(slug string, v url.Values) string {
		v.Set("csrf", csrf)
		return from(t, client, "10.9.9.9", srv.URL+"/tasks/"+slug+"/"+map[bool]string{true: "submit", false: "wallet"}[v.Get("_submit") == "1"], v)
	}
	submit := func(slug string) string { return post(slug, url.Values{"_submit": {"1"}}) }
	// Without a linked wallet, Emissio pays nobody.
	if m := post("web-wallet-pay", url.Values{}); !strings.Contains(m, "Link your web wallet first") {
		t.Fatalf("unlinked: %q", m)
	}
	app.db.Exec("INSERT INTO wallet_links (user_id, address, pubkey, created_at) VALUES (1, 'tb1qlinked', '02ab', 0)")
	// The account's own code; a payment reaching it is attributed by the coin's record.
	if m := post("web-wallet-pay", url.Values{}); !strings.Contains(m, "Your receive code from Emissio is below") {
		t.Fatalf("code: %q", m)
	}
	page := get(t, client, srv.URL+"/tasks/web-wallet-pay")
	if !strings.Contains(page, "bitcoin:tb1qemissio") || !strings.Contains(page, "sequentia=arca%3A") {
		t.Fatal("the task page does not show the receive code")
	}
	if m := submit("web-wallet-pay"); !strings.Contains(m, "nothing has reached your Emissio code yet") {
		t.Fatalf("early submit: %q", m)
	}
	c := app.codeOf(1, "code")
	pay := func(leaf, asset, value string, c *arcaCode) {
		mb := []map[string]string{{"leaf_id": leaf, "asset": asset, "value": value}}
		b, _ := json.Marshal(mb)
		os.WriteFile(filepath.Join(dir, "mailbox.json"), b, 0o600)
		rec := "00ff"
		if c != nil {
			rec += c.Owner + c.OwnerNonce + "11"
		}
		b, _ = json.Marshal(rec)
		os.WriteFile(filepath.Join(dir, "rec-"+leaf+".json"), b, 0o600)
		app.followArca()
	}
	pay("stray", strings.Repeat("ab", 32), "100000000", nil) // a coin of nobody's request
	pay("leaf1", strings.Repeat("ab", 32), "100000000", c)
	if m := submit("web-wallet-pay"); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("paid code: %q", m)
	}

	// The round trip: Emissio pays the tester's code, the tester sends back exactly that.
	det, _ := json.Marshal(map[string]any{"kind": "request", "operator": strings.Repeat("aa", 32), "owner": strings.Repeat("0c", 32), "owner_nonce": strings.Repeat("0d", 32), "until": 9999999999})
	theirs := "bitcoin:tb1qtester?sequentia=" + url.QueryEscape("arca:"+hex.EncodeToString(det))
	other, _ := json.Marshal(map[string]any{"kind": "request", "operator": strings.Repeat("ee", 32), "owner": strings.Repeat("0c", 32), "owner_nonce": strings.Repeat("0d", 32), "until": 9999999999})
	if m := post("web-wallet-roundtrip", url.Values{"code": {"arca:" + hex.EncodeToString(other)}}); !strings.Contains(m, "another server") {
		t.Fatalf("other operator: %q", m)
	}
	if m := post("web-wallet-roundtrip", url.Values{"code": {theirs}}); !strings.Contains(m, "Emissio has paid your receive code") {
		t.Fatalf("roundtrip pay: %q", m)
	}
	var asset string
	var atoms int64
	app.db.QueryRow("SELECT asset, atoms FROM arca_sends WHERE purpose = 'roundtrip'").Scan(&asset, &atoms)
	if page := get(t, client, srv.URL+"/tasks/web-wallet-roundtrip"); strings.Contains(page, amountText(atoms)) || !strings.Contains(page, "Send exactly what arrived") {
		t.Fatal("the round trip page shows the amount, or no code to return it to")
	}
	back := app.codeOf(1, "roundtrip")
	pay("leaf2", asset, "1", back) // the wrong amount
	if m := submit("web-wallet-roundtrip"); !strings.Contains(m, "not what Emissio sent you") {
		t.Fatalf("wrong amount: %q", m)
	}
	app.db.Exec("UPDATE arca_codes SET paid_at = 0 WHERE id = ?", back.ID)
	pay("leaf3", asset, strings.TrimLeft(strings.Replace(amountText(atoms), ".", "", 1), "0"), back)
	if m := submit("web-wallet-roundtrip"); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("right amount: %q", m)
	}

	// BTC at the account's own ark address.
	post("web-wallet-btc", url.Values{})
	bc := app.codeOf(1, "btc")
	if bc == nil || bc.Ark == "" {
		t.Fatal("no ark address in the BTC code")
	}
	moves, _ := json.Marshal([]map[string]any{{"status": "successful", "received_on": []map[string]any{{"destination": map[string]string{"type": "ark", "value": bc.Ark}, "amount_sat": 1500}}}})
	os.WriteFile(filepath.Join(dir, "ark-"+bc.Ark+".json"), moves, 0o600)
	app.followArca()
	if m := submit("web-wallet-btc"); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("btc: %q", m)
	}

	// An invoice: too large is refused, a small one is paid.
	if m := post("web-wallet-invoice", url.Values{"code": {"lntb1m1pjxyzacd"}}); !strings.Contains(m, "up to 10000 sats") {
		t.Fatalf("large invoice: %q", m)
	}
	if m := post("web-wallet-invoice", url.Values{"code": {"bitcoin:tb1qx?amount=0.0001&lightning=lntb100u1pjxyzacd"}}); !strings.Contains(m, "Emissio paid your invoice") {
		t.Fatalf("invoice: %q", m)
	}
	if m := submit("web-wallet-invoice"); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("invoice submit: %q", m)
	}

	// The swap: the payment needs the proof decimals, in an asset other than tSEQ.
	post("web-wallet-swap", url.Values{})
	sc := app.codeOf(1, "swap")
	pay("leaf4", strings.Repeat("ab", 32), "100000000", sc)
	if m := submit("web-wallet-swap"); !strings.Contains(m, "does not end in your proof decimals") {
		t.Fatalf("swap without tag: %q", m)
	}
	app.db.Exec("UPDATE arca_codes SET paid_at = 0 WHERE id = ?", sc.ID)
	pay("leaf5", strings.Repeat("ab", 32), "186718345", sc)
	if m := submit("web-wallet-swap"); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("swap: %q", m)
	}
	var bal int64
	app.db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = 1").Scan(&bal)
	if bal != 15+25+20+20+25 {
		t.Fatalf("balance %d", bal)
	}
}
