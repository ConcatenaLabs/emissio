package main

// The own-Bitcoin-node task end to end on a private network: two regtest
// Bitcoin nodes (the box's, and the tester's announcing the account code
// and connected to it), the shared endpoint (the node repository's
// testnet-rpc-cache-proxy.py) in front of the box's, a public Sequentia node,
// and the tester's Sequentia node, announcing the code, anchoring first
// through the shared endpoint and then through its own Bitcoin node. The
// peer list is written by the ops repository's bin/btc-peers.py, as on the
// box. Then: the endpoint restarted (a record too young to show the quiet
// interval), restarted full (review), a program that only completes the
// Bitcoin handshake (refused), and a second account from the same address
// (review).
//
// Skipped unless these are set:
//   EMISSIO_REGTEST_ANCHOR_PUBLIC   sequentia-cli wrapper for the public node
//   EMISSIO_REGTEST_ANCHOR_DATADIR  its data directory
//   EMISSIO_REGTEST_ANCHOR_TESTER   sequentia-cli wrapper for the tester's node
//   EMISSIO_REGTEST_ANCHOR_SEEN     the proxy's /last-seen URL
//   EMISSIO_REGTEST_BTC_PEERS       the peer-list file
//   EMISSIO_REGTEST_BTC_PEERS_CMD   a command that writes it (btc-peers.py)
//   EMISSIO_REGTEST_TESTER_SHARED   a command (re)starting the tester's nodes, Sequentia on the shared endpoint
//   EMISSIO_REGTEST_TESTER_OWN      a command restarting the tester's Sequentia node on its own Bitcoin node
//   EMISSIO_REGTEST_PROXY_RESTART   a command restarting the shared endpoint
//   EMISSIO_REGTEST_PROXY_FULL      a command restarting it with a record of no room, then calling it
//   EMISSIO_REGTEST_FAKE_PEER       a command connecting a handshake-only program announcing emissio-1111111111
//   EMISSIO_REGTEST_SECOND          a command restarting both tester's nodes announcing emissio-9876543210

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRegtestOwnBitcoinNode(t *testing.T) {
	env := map[string]string{}
	for _, k := range []string{"ANCHOR_PUBLIC", "ANCHOR_DATADIR", "ANCHOR_TESTER", "ANCHOR_SEEN", "BTC_PEERS", "BTC_PEERS_CMD", "TESTER_SHARED", "TESTER_OWN", "PROXY_RESTART", "PROXY_FULL", "FAKE_PEER", "SECOND"} {
		env[k] = os.Getenv("EMISSIO_REGTEST_" + k)
		if env[k] == "" {
			t.Skip("the EMISSIO_REGTEST_ANCHOR_*, _BTC_PEERS*, _TESTER_*, _PROXY_*, _FAKE_PEER and _SECOND variables are not set")
		}
	}
	T := regNode{t, env["ANCHOR_TESTER"]}
	sh := func(cmd string) {
		t.Helper()
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
	}
	app, _, _ := newTestServer(t)
	app.cfg.NodeCLI = env["ANCHOR_PUBLIC"]
	app.cfg.NodeDatadir = env["ANCHOR_DATADIR"]
	app.cfg.BtcPeersFile = env["BTC_PEERS"]
	app.cfg.AnchorSeenURL = env["ANCHOR_SEEN"]
	mk := func(email, code string) *User {
		id, err := createUser(app.db, email, "x", code, "")
		if err != nil {
			t.Fatal(err)
		}
		return &User{ID: id, ClaimCode: code}
	}
	user, second, faker := mk("own@example.com", "0123456789"), mk("second@example.com", "9876543210"), mk("fake@example.com", "1111111111")
	quiet, conn := sharedQuiet, btcMinConn
	sharedQuiet, btcMinConn = 8*time.Second, 30*time.Second
	defer func() { sharedQuiet, btcMinConn = quiet, conn }()
	// Two successive lists, as the timer writes them a minute apart.
	lists := func() {
		sh(env["BTC_PEERS_CMD"])
		time.Sleep(1100 * time.Millisecond)
		sh(env["BTC_PEERS_CMD"])
	}
	check := func(u *User, want string, v verdict) {
		t.Helper()
		lists()
		got := app.ownBitcoinCheck(T.call("getanchorstatus"), u, time.Now())
		t.Logf("%s: %s", u.ClaimCode, got.Note)
		if got.Verdict != v || !strings.Contains(got.Note, want) {
			t.Errorf("got (%d) %q, want (%d) ...%q...", got.Verdict, got.Note, v, want)
		}
	}
	// The tester's nodes start; its Sequentia node on the default, the
	// shared endpoint. The Bitcoin node is first seen too briefly.
	sh(env["TESTER_SHARED"])
	check(user, "the check wants one connection of", vFail)
	time.Sleep(btcMinConn)
	check(user, "still checks its anchors there", vFail)
	// The same node on its own Bitcoin node; the shared endpoint's record of
	// the address is still fresh, then goes quiet.
	sh(env["TESTER_OWN"])
	check(user, "still checks its anchors there", vFail)
	time.Sleep(sharedQuiet + 2*time.Second)
	check(user, "(OK)", vPass)
	// The endpoint restarted: its record is younger than the quiet interval.
	sh(env["PROXY_RESTART"])
	check(user, "submit again in 1 minute", vFail)
	// Restarted with no room, and called: the record may have missed a call.
	sh(env["PROXY_FULL"])
	check(user, "may have missed a call from that address", vReview)
	sh(env["PROXY_RESTART"])
	time.Sleep(sharedQuiet + 2*time.Second)
	check(user, "(OK)", vPass)
	// A program that only completes the handshake, announcing a code.
	sh(env["FAKE_PEER"])
	time.Sleep(btcMinConn)
	check(faker, "does not count it as synced: on this connection it knows its headers to -1 and its blocks to -1", vFail)
	// A second account from the address that vouched for the first.
	sh(env["SECOND"])
	time.Sleep(btcMinConn)
	check(second, "already been used for another account", vReview)
}
