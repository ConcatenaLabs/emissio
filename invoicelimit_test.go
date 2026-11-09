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

// seqInvoice builds a Sequentia testnet invoice for amount (the prefix's
// amount part) whose `a` field names asset. Signature and checksum are
// filler: Emissio reads the fields, the node checks the rest.
func seqInvoice(amount, asset string) string {
	var groups []int
	acc, bits := 0, 0
	for i := 0; i < len(asset); i += 2 {
		var b int
		for _, c := range asset[i : i+2] {
			b = b<<4 | strings.IndexRune("0123456789abcdef", c)
		}
		acc = acc<<8 | b
		bits += 8
		for bits >= 5 {
			bits -= 5
			groups = append(groups, (acc>>bits)&31)
		}
	}
	if bits > 0 {
		groups = append(groups, (acc<<(5-bits))&31)
	}
	vals := make([]int, 7) // timestamp
	vals = append(vals, 1, 1, 20)
	vals = append(vals, make([]int, 52)...) // payment hash
	vals = append(vals, 29, len(groups)>>5, len(groups)&31)
	vals = append(vals, groups...)
	vals = append(vals, make([]int, 104+6)...)
	var b strings.Builder
	b.WriteString("lntsqt" + amount + "1")
	for _, v := range vals {
		b.WriteByte(bech32Charset[v])
	}
	return b.String()
}

func TestInvoiceAsset(t *testing.T) {
	asset := strings.Repeat("2a51", 16)
	if got, err := invoiceAsset(seqInvoice("100u", asset)); err != nil || got != asset {
		t.Fatalf("sequentia invoice: %q %v", got, err)
	}
	if got, err := invoiceAsset("lntb100u1pjxyzacd"); err != nil || got != "btc" {
		t.Fatalf("bitcoin invoice: %q %v", got, err)
	}
	noAsset := "lntsqt100u1" + strings.Repeat("q", 7+104+6)
	if _, err := invoiceAsset(noAsset); err == nil {
		t.Fatal("a Sequentia invoice without an asset is read")
	}
}

func TestAssetCap(t *testing.T) {
	for _, c := range []struct {
		price float64
		text  string
		atoms int64
	}{
		{1, "10 USDX", 1_000_000_000},
		{1.1143, "5 USDX", 500_000_000},
		{4074.9, "0.002 USDX", 200_000},
		{51.942, "0.1 USDX", 10_000_000},
		{0.1752, "50 USDX", 5_000_000_000},
		{0.0001, "100000 USDX", 10_000_000_000_000},
	} {
		got, ok := assetCap("x", "USDX", 8, c.price)
		if !ok || got.Text != c.text || got.Atoms != c.atoms {
			t.Errorf("price %v: %+v %v", c.price, got, ok)
		}
	}
	if _, ok := assetCap("x", "Z", 0, 4074.9); ok {
		t.Error("a limit below one atom")
	}
	if b := btcCap(); b.Text != "0.0001 BTC" || b.Atoms != 10_000 {
		t.Errorf("btc: %+v", b)
	}
}

func TestInvoiceRefusal(t *testing.T) {
	app, _, _ := newTestServer(t)
	bin, _ := filepath.Abs("testdata/fake-arca.py")
	app.cfg.ArcaBin, app.cfg.ArcaDatadir = bin, t.TempDir()
	usdx := strings.Repeat("ab", 32) // the asset the stand-in arca holds
	feeds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prices":
			json.NewEncoder(w).Encode(map[string]any{"USDX": map[string]any{"price": 1.0}})
		case "/registry/" + usdx:
			json.NewEncoder(w).Encode(map[string]any{"contract": map[string]any{"ticker": "USDX", "precision": 8}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer feeds.Close()
	app.cfg.PricesURL, app.cfg.RegistryURL = feeds.URL+"/prices", feeds.URL+"/registry"
	if why := app.invoiceRefusal(usdx, 1_000_000_000); why != "" {
		t.Fatalf("10 USDX refused: %s", why)
	}
	why := app.invoiceRefusal(usdx, 1_000_000_001)
	if !strings.Contains(why, "in USDX of up to 10 USDX") || !strings.Contains(why, "0.0001 BTC (10,000 sats), 10 USDX") {
		t.Fatalf("over the limit: %s", why)
	}
	if why := app.invoiceRefusal(strings.Repeat("cd", 32), 1); !strings.Contains(why, "does not pay invoices in that asset") {
		t.Fatalf("unknown asset: %s", why)
	}
	if got := capsText(app.shownCaps()); got != "0.0001 BTC (10,000 sats), 10 USDX" {
		t.Fatalf("shown: %s", got)
	}
}

// An invoice in a Sequentia asset: Emissio pays it, the receiving wallet
// takes it later, and the follower brings the payment's state up to date,
// which opens the task.
func TestInvoicePaidLater(t *testing.T) {
	app, srv, client := newTestServer(t)
	dir := t.TempDir()
	bin, _ := filepath.Abs("testdata/fake-arca.py")
	app.cfg.ArcaBin, app.cfg.ArcaDatadir = bin, dir
	gold := strings.Repeat("ab", 32) // the asset the stand-in arca holds
	feeds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prices":
			json.NewEncoder(w).Encode(map[string]any{"GOLD": map[string]any{"price": 4074.9}})
		case "/registry/" + gold:
			json.NewEncoder(w).Encode(map[string]any{"contract": map[string]any{"ticker": "GOLD", "precision": 8}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer feeds.Close()
	app.cfg.PricesURL, app.cfg.RegistryURL = feeds.URL+"/prices", feeds.URL+"/registry"
	register(t, client, srv.URL, "gold@x.io")
	app.db.Exec("INSERT INTO wallet_links (user_id, address, pubkey, created_at) VALUES (1, 'tb1qlinked', '02ab', 0)")
	csrf := csrfOf(t, client, srv.URL+"/account")
	post := func(path string, v url.Values) string {
		v.Set("csrf", csrf)
		return from(t, client, "10.9.9.8", srv.URL+path, v)
	}
	if m := post("/tasks/web-wallet-invoice/wallet", url.Values{"code": {"bitcoin:tb1qx?lightning=" + seqInvoice("3m", gold)}}); !strings.Contains(m, "in GOLD of up to 0.002 GOLD") {
		t.Fatalf("0.003 GOLD: %q", m)
	}
	if m := post("/tasks/web-wallet-invoice/wallet", url.Values{"code": {"bitcoin:tb1qx?lightning=" + seqInvoice("1m", gold)}}); !strings.Contains(m, "on its way") {
		t.Fatalf("0.001 GOLD: %q", m)
	}
	if m := post("/tasks/web-wallet-invoice/wallet", url.Values{"code": {"bitcoin:tb1qx?lightning=" + seqInvoice("1m", gold)}}); !strings.Contains(m, "already paying that invoice") || strings.Contains(m, "{") {
		t.Fatalf("the same invoice again: %q", m)
	}
	if m := post("/tasks/web-wallet-invoice/submit", url.Values{}); !strings.Contains(m, "is paying, not paid") {
		t.Fatalf("submit while paying: %q", m)
	}
	b, _ := json.Marshal(map[string]any{strings.Repeat("ef", 32): map[string]string{"state": "paid"}})
	os.WriteFile(filepath.Join(dir, "payments.json"), b, 0o600)
	app.followArca()
	if m := post("/tasks/web-wallet-invoice/submit", url.Values{}); !strings.Contains(m, "Checked and credited") {
		t.Fatalf("submit once paid: %q", m)
	}
}
