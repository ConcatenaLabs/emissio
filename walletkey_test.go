package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The BIP84 test mnemonic ("abandon … about") on the testnet: its account key
// at m/84'/1'/0', and the key of its first receive address,
// tb1q6rz28mcfaxtmd6v789l9rrlrusdprr9pqcpvkl (BIP84's own vector). The
// account key and the child key were derived independently in Python.
const (
	vecTpub = "tpubDC8msFGeGuwnKG9Upg7DM2b4DaRqg3CUZa5g8v2SRQ6K4NSkxUgd7HsL2XVWbVm39yBA4LAxysQAm397zwQSQoQgewGiYZqrA9DsP4zbQ1M"
	vecKey0 = "e7ab2537b5d49e970309aae06e9e49f36ce1c9febbd44ec8e0d1cca0b4f9c319"
	vecAddr = "tb1q6rz28mcfaxtmd6v789l9rrlrusdprr9pqcpvkl"
)

func TestFirstReceiveKey(t *testing.T) {
	for _, in := range []string{
		vecTpub,
		"[73c5da0a/84h/1h/0h]" + vecTpub,
		"[73c5da0a/84'/1'/0']" + vecTpub,
		// The web wallet's wpkh form: two descriptor lines, receive then change.
		"wpkh([73c5da0a/84h/1h/0h]" + vecTpub + "/0/*)#abcdefgh\nwpkh([73c5da0a/84h/1h/0h]" + vecTpub + "/1/*)#hgfedcba",
	} {
		k, err := firstReceiveKey(in)
		if err != nil || k != vecKey0 {
			t.Fatalf("%q: %v %v, want %s", in[:30], k, err, vecKey0)
		}
	}
	if _, err := firstReceiveKey("[73c5da0a/86h/1h/0h]" + vecTpub); err == nil || !strings.Contains(err.Error(), "84h/1h/0h") {
		t.Fatalf("another account path: %v", err)
	}
	if _, err := firstReceiveKey(vecTpub[:len(vecTpub)-1] + "N"); err == nil {
		t.Fatal("a bad checksum was taken")
	}
}

func TestWalletKeyFromAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /address/{a}/txs", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("a") != vecAddr {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		json.NewEncoder(w).Encode([]any{map[string]any{"vin": []any{map[string]any{
			"witness": []string{"3044aa", "02" + vecKey0},
			"prevout": map[string]any{"scriptpubkey_address": vecAddr},
		}}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a := &App{cfg: Config{EsploraURL: srv.URL, PayoutHRPs: []string{"tb"}}}
	if k, err := a.walletKey(vecAddr); err != nil || k != vecKey0 {
		t.Fatalf("an address that sent: %v %v", k, err)
	}
	if _, err := a.walletKey("tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"); err == nil || !strings.Contains(err.Error(), "has not sent anything yet") {
		t.Fatalf("an address that never sent: %v", err)
	}
	if k, err := a.walletKey("[73c5da0a/84h/1h/0h]" + vecTpub); err != nil || k != vecKey0 {
		t.Fatalf("the account key: %v %v", k, err)
	}
	if _, err := a.walletKey("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"); err == nil {
		t.Fatal("a mainnet address was taken")
	}
}

// TestRehearsalPanelOnlyWhenAsked: the account page shows the rehearsal only
// while an admin has it asking for wallets, or to an account that gave one.
func TestRehearsalPanelOnlyWhenAsked(t *testing.T) {
	app, srv, client := newTestServer(t)
	app.cfg.PayoutURL, app.cfg.PayoutNetwork, app.cfg.PayoutHRPs = "http://127.0.0.1:9", "testnet", []string{"tb"}
	resp, _ := client.PostForm(srv.URL+"/register", url.Values{"email": {"r@example.com"}, "password": {"a-long-password"}})
	resp.Body.Close()
	if page := get(t, client, srv.URL+"/account"); strings.Contains(page, "Testnet rehearsal") {
		t.Fatal("the rehearsal shows while nobody asked for wallets")
	}
	csrf := csrfOf(t, client, srv.URL+"/account")
	client.PostForm(srv.URL+"/account/rehearsal-key", url.Values{"csrf": {csrf}, "key": {vecTpub}})
	var id int64
	app.db.QueryRow(`SELECT id FROM users WHERE email = 'r@example.com'`).Scan(&id)
	if k, _ := keysOf(app.db, id); k.RehearsalKey != "" {
		t.Fatal("a wallet was saved while the rehearsal was not asking")
	}
	app.db.Exec(`INSERT INTO payout_settings (name, value) VALUES ('rehearsal_open', '1')`)
	page := get(t, client, srv.URL+"/account")
	if !strings.Contains(page, "Show xpub") {
		t.Fatal("the rehearsal does not say where the wallet shows its key")
	}
	client.PostForm(srv.URL+"/account/rehearsal-key", url.Values{"csrf": {csrf}, "key": {"[73c5da0a/84h/1h/0h]" + vecTpub}})
	if k, _ := keysOf(app.db, id); k.RehearsalKey != vecKey0 {
		t.Fatalf("saved %q, want the first receive key", k.RehearsalKey)
	}
	app.db.Exec(`UPDATE payout_settings SET value = '0'`)
	if page := get(t, client, srv.URL+"/account"); !strings.Contains(page, "Your testnet wallet is saved") {
		t.Fatal("an account that gave a wallet no longer sees it once the asking stops")
	}
}
