package main

// The own-Bitcoin-node task end to end on a private network: two regtest
// Bitcoin nodes (the box's, and the tester's announcing the account code
// and connected to it), the shared endpoint (the node repository's
// testnet-rpc-cache-proxy.py) in front of the box's, a public Sequentia node
// anchoring through it, and the tester's Sequentia node, announcing the code,
// anchoring first through the shared endpoint and then through its own
// Bitcoin node. The peer list is written by the ops repository's
// bin/btc-peers.py, as on the box.
//
// Skipped unless these are set:
//   EMISSIO_REGTEST_ANCHOR_PUBLIC   sequentia-cli wrapper for the public node
//   EMISSIO_REGTEST_ANCHOR_DATADIR  its data directory
//   EMISSIO_REGTEST_ANCHOR_TESTER   sequentia-cli wrapper for the tester's node
//   EMISSIO_REGTEST_ANCHOR_SEEN     the proxy's /last-seen URL
//   EMISSIO_REGTEST_BTC_PEERS       the peer-list file
//   EMISSIO_REGTEST_BTC_PEERS_CMD   a command that writes it (btc-peers.py)
//   EMISSIO_REGTEST_TESTER_SHARED   a command restarting the tester's node on the shared endpoint
//   EMISSIO_REGTEST_TESTER_OWN      a command restarting it on its own Bitcoin node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRegtestOwnBitcoinNode(t *testing.T) {
	env := map[string]string{}
	for _, k := range []string{"ANCHOR_PUBLIC", "ANCHOR_DATADIR", "ANCHOR_TESTER", "ANCHOR_SEEN", "BTC_PEERS", "BTC_PEERS_CMD", "TESTER_SHARED", "TESTER_OWN"} {
		env[k] = os.Getenv("EMISSIO_REGTEST_" + k)
		if env[k] == "" {
			t.Skip("the EMISSIO_REGTEST_ANCHOR_*, _BTC_PEERS* and _TESTER_* variables are not set")
		}
	}
	P, T := regNode{t, env["ANCHOR_PUBLIC"]}, regNode{t, env["ANCHOR_TESTER"]}
	sh := func(cmd string) {
		t.Helper()
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
	}
	// The explorer, from the public node.
	explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/blocks/tip/height":
			w.Write([]byte(P.call("getblockcount")))
		case strings.HasPrefix(r.URL.Path, "/block-height/"):
			w.Write([]byte(P.call("getblockhash", strings.TrimPrefix(r.URL.Path, "/block-height/"))))
		case strings.HasPrefix(r.URL.Path, "/block/"):
			var h struct {
				Height       int64  `json:"height"`
				AnchorHeight int64  `json:"anchorheight"`
				AnchorHash   string `json:"anchorhash"`
			}
			P.json(&h, "getblockheader", strings.TrimPrefix(r.URL.Path, "/block/"))
			json.NewEncoder(w).Encode(map[string]any{"height": h.Height, "bitcoin_anchor": map[string]any{"height": h.AnchorHeight, "hash": h.AnchorHash}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer explorer.Close()
	app, _, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.NodeCLI = env["ANCHOR_PUBLIC"]
	app.cfg.NodeDatadir = env["ANCHOR_DATADIR"]
	app.cfg.BtcPeersFile = env["BTC_PEERS"]
	app.cfg.AnchorSeenURL = env["ANCHOR_SEEN"]
	code := "0123456789"
	quiet := sharedQuiet
	sharedQuiet = 8 * time.Second
	defer func() { sharedQuiet = quiet }()
	mine := func() { P.call("-rpcwallet=gen", "generatetoaddress", "1", P.call("-rpcwallet=gen", "getnewaddress")) }
	waitSynced := func() {
		want := P.call("getblockcount")
		for i := 0; i < 60 && T.call("getblockcount") != want; i++ {
			time.Sleep(500 * time.Millisecond)
		}
	}
	check := func(want string, v verdict) {
		t.Helper()
		sh(env["BTC_PEERS_CMD"])
		mine()
		waitSynced()
		got := app.ownBitcoinCheck(T.call("getanchorstatus"), code, time.Now())
		t.Logf("%s", got.Note)
		if got.Verdict != v || !strings.Contains(got.Note, want) {
			t.Errorf("got (%d) %q, want (%d) ...%q...", got.Verdict, got.Note, v, want)
		}
	}
	// The tester's node on the default: the shared endpoint.
	sh(env["TESTER_SHARED"])
	check("still checks its anchors there", vFail)
	// The same node on its own Bitcoin node; the shared endpoint's record of
	// the address is still fresh, then goes quiet.
	sh(env["TESTER_OWN"])
	check("still checks its anchors there", vFail)
	time.Sleep(sharedQuiet + 2*time.Second)
	check("(OK)", vPass)
	// A status edited by hand: another anchor.
	var st map[string]any
	json.Unmarshal([]byte(T.call("getanchorstatus")), &st)
	st["anchorhash"] = strings.Repeat("ab", 32)
	edited, _ := json.Marshal(st)
	if got := app.ownBitcoinCheck(string(edited), code, time.Now()); got.Verdict != vFail || !strings.Contains(got.Note, "but that block anchors to") {
		t.Errorf("edited status: %+v", got)
	}
	// An old status.
	for i := 0; i < tipSlack+1; i++ {
		mine()
	}
	stale, _ := json.Marshal(st)
	if got := app.ownBitcoinCheck(string(stale), code, time.Now()); got.Verdict != vFail || !strings.Contains(got.Note, "paste a fresh getanchorstatus") {
		t.Errorf("old status: %+v", got)
	}
}
