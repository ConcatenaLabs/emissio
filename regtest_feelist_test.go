package main

// The fee-list tasks end to end, on two elementsregtest nodes: a tester's
// node T, with the fee list a fresh node starts with (the policy asset
// only), and a producer P that lists the test USDX at the network's rate and
// mines. The tester lists USDX on T by hand, then through the real price
// server, sends with the fee in USDX, P mines it, and the checks read the
// transactions back from T through the explorer stand-in.
//
// Skipped unless these are set:
//   EMISSIO_REGTEST_FEELIST_TESTER    sequentia-cli wrapper for T (wallet "tester",
//                                     holding the policy asset and the test USDX; -txindex=1)
//   EMISSIO_REGTEST_FEELIST_PRODUCER  sequentia-cli wrapper for P (wallet "gen"), connected to T
//   EMISSIO_REGTEST_FEELIST_USDX      the test USDX's asset id
//   EMISSIO_REGTEST_FEELIST_RPC       T's RPC as host:port:user:password, for the price server
//   EMISSIO_REGTEST_PRICE_SERVER      path to contrib/price-server/price_server.py

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRegtestFeeList(t *testing.T) {
	tcli, pcli := os.Getenv("EMISSIO_REGTEST_FEELIST_TESTER"), os.Getenv("EMISSIO_REGTEST_FEELIST_PRODUCER")
	usdx, rpc, server := os.Getenv("EMISSIO_REGTEST_FEELIST_USDX"), os.Getenv("EMISSIO_REGTEST_FEELIST_RPC"), os.Getenv("EMISSIO_REGTEST_PRICE_SERVER")
	if tcli == "" || pcli == "" || usdx == "" || rpc == "" || server == "" {
		t.Skip("the EMISSIO_REGTEST_FEELIST_* and EMISSIO_REGTEST_PRICE_SERVER variables are not set")
	}
	T, P := regNode{t, tcli}, regNode{t, pcli}
	shim := explorerShim(T)
	defer shim.Close()
	app, _, _ := newTestServer(t)
	app.cfg.EsploraURL = shim.URL
	app.cfg.USDXAsset = usdx
	code := "0123456789"
	tag := proofTag(code)
	uid, _ := createUser(app.db, "lister@example.com", "x", code, "")
	user := &User{ID: uid, ClaimCode: code}
	var rates map[string]json.Number
	T.json(&rates, "getfeeexchangerates")
	var policyAsset string
	for k := range rates {
		policyAsset = k
	}
	var labels map[string]string
	T.json(&labels, "dumpassetlabels")
	if id, ok := labels[policyAsset]; ok {
		policyAsset = id
	}
	addr := T.call("-rpcwallet=tester", "getnewaddress")
	amount := "1." + tag

	// send sends 1.<proof> USDX with the fee in USDX at fee rate f from T,
	// has P mine it, and waits for T to see the block. It returns the txid,
	// or the node's refusal.
	send := func(f string) (string, string) {
		args := []string{"-rpcwallet=tester", "-named", "sendtoaddress", "address=" + addr, "amount=" + amount,
			"assetlabel=" + usdx, "fee_asset_label=" + usdx}
		if f != "" {
			args = append(args, "fee_rate="+f)
		}
		out, err := exec.Command(tcli, args...).CombinedOutput()
		if err != nil {
			return "", strings.TrimSpace(string(out))
		}
		txid := strings.TrimSpace(string(out))
		for i := 0; i < 60 && !strings.Contains(P.call("getrawmempool"), txid); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		if !strings.Contains(P.call("getrawmempool"), txid) {
			t.Fatalf("the producer never received %s", txid)
		}
		P.call("-rpcwallet=gen", "generatetoaddress", "1", P.call("-rpcwallet=gen", "getnewaddress"))
		want := P.call("getblockcount")
		for i := 0; i < 60 && T.call("getblockcount") != want; i++ {
			time.Sleep(500 * time.Millisecond)
		}
		return txid, ""
	}
	setRates := func(r map[string]int64) {
		b, _ := json.Marshal(r)
		T.call("setfeeexchangerates", string(b))
	}
	check := func(kind, txid string, v verdict, want string) {
		t.Helper()
		got := app.evidenceCheck(kind, txid, "", user)
		if got.Verdict != v || !strings.Contains(got.Note, want) {
			t.Errorf("%s/%s: got (%d) %q, want (%d) ...%q...", kind, txid, got.Verdict, got.Note, v, want)
		}
		t.Logf("%s %s: %s", kind, txid, got.Note)
	}
	defer setRates(map[string]int64{policyAsset: rateScale})

	// A fresh node refuses a fee in an asset it does not list.
	if _, refused := send(""); !strings.Contains(refused, "The chosen fee asset is not accepted") {
		t.Fatalf("unlisted USDX: %q", refused)
	}
	rateA := feeTaskRateFor("whitelist-fee", tag)
	setRates(map[string]int64{policyAsset: rateScale, usdx: rateA})
	listed, refused := send("2")
	if refused != "" {
		t.Fatal(refused)
	}
	check("whitelist-fee", listed, vPass, fmt.Sprintf("rate %d (OK)", rateA))
	check("price-server-fee", listed, vFail, "follows from the price of the task \"List an asset for fees on your node\"")
	// Negative: another fee rate at the right rate.
	wrongRate, _ := send("3")
	check("whitelist-fee", wrongRate, vFail, "this task asks for rate")
	// Negative: USDX listed at the network's rate.
	setRates(map[string]int64{policyAsset: rateScale, usdx: rateScale})
	network, _ := send("2")
	check("whitelist-fee", network, vFail, "the network's rate (100000000)")

	// ---- the price server ----
	dir := t.TempDir()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry.json":
			json.NewEncoder(w).Encode(map[string]any{
				usdx: []any{"example.org", "USDX", "USD test", 8, 1, 0}, policyAsset: []any{"example.org", "TSEQ", "Sequence", 8, 1, 0}})
		case "/prices":
			json.NewEncoder(w).Encode(map[string]any{
				"USDX": map[string]any{"price": 1.0, "market_cap": 1e9, "volume_24h": 1e7},
				"SEQ":  map[string]any{"price": 0.17, "market_cap": 1e9, "volume_24h": 1e7}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer feed.Close()
	parts := strings.SplitN(rpc, ":", 4)
	port, _ := strconv.Atoi(parts[1])
	priceB := feeTaskCents("price-server-fee", tag)
	cfg := map[string]any{
		"poll_interval_secs": 60, "source_timeout": 5,
		"node_rpc":           map[string]any{"host": parts[0], "port": port, "user": parts[2], "password": parts[3]},
		"registry_url":       feed.URL + "/registry.json",
		"source":             map[string]any{"url": feed.URL + "/prices", "quote_currency": "USD", "format": "sequentia", "mode": "except", "assets": []string{"USDX"}},
		"manual_prices":      map[string]any{"USDX": float64(priceB) / 100},
		"feed_aliases":       map[string]any{"TSEQ": "SEQ"},
		"default_thresholds": map[string]any{"require": "all"},
	}
	cfgPath := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, b, 0o600)
	runServer := func(args ...string) string {
		out, err := exec.Command("python3", append([]string{"-I", server, "--config", cfgPath, "--once"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("price server %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	rateB := feeTaskRateFor("price-server-fee", tag)
	dry := runServer("--dry-run")
	if !strings.Contains(dry, "dry-run: ") || !strings.Contains(dry, "would publish") || !strings.Contains(dry, fmt.Sprintf("%q: %d", usdx, rateB)) {
		t.Fatalf("the dry run would not publish USDX at %d:\n%s", rateB, dry)
	}
	T.json(&rates, "getfeeexchangerates")
	if r := rates[usdx].String(); r != strconv.FormatInt(rateScale, 10) {
		t.Fatalf("the dry run changed the node's list: USDX at %s", r)
	}
	pub := runServer()
	if !strings.Contains(pub, "to 1/1 node(s)") {
		t.Fatalf("the price server did not publish:\n%s", pub)
	}
	T.json(&rates, "getfeeexchangerates")
	if r := rates[usdx].String(); r != strconv.FormatInt(rateB, 10) {
		t.Fatalf("after the price server, USDX at %s, want %d", r, rateB)
	}
	for _, l := range strings.Split(pub, "\n") {
		if strings.Contains(l, "published") {
			t.Logf("price server: %s", l)
		}
	}
	served, refused := send("2")
	if refused != "" {
		t.Fatal(refused)
	}
	check("price-server-fee", served, vPass, fmt.Sprintf("rate %d (OK)", rateB))
	check("whitelist-fee", served, vFail, "follows from the price of the task \"Run the price server\"")
}
