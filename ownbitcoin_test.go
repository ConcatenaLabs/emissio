package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every verdict of the own-Bitcoin-node check, and the slips a tester
// makes: one node not running, two addresses, the status of a node that
// does not validate or cannot reach its Bitcoin node, an old or edited
// status, and a node still on the shared endpoint.
func TestOwnBitcoinCheck(t *testing.T) {
	code := "0123456789"
	now := time.Unix(1_800_000_000, 0)
	anchor := "000000000469aa5964c35539a3b2a8bf00f1cab77c9d7ca1882d7a803f014845"
	explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blocks/tip/height":
			w.Write([]byte("212900"))
		case "/block-height/212898":
			w.Write([]byte("blockhash212898"))
		case "/block/blockhash212898":
			w.Write([]byte(`{"id":"blockhash212898","bitcoin_anchor":{"height":161394,"hash":"` + anchor + `","depth":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer explorer.Close()
	// The shared endpoint's record: 198.51.100.7 quiet for an hour, 198.51.100.9 a minute ago.
	seen := map[string]int64{"198.51.100.7": now.Unix() - 3600, "198.51.100.9": now.Unix() - 60}
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.URL.Query().Get("addr")
		out := map[string]any{"addr": a, "last": nil}
		if v, ok := seen[a]; ok {
			out["last"] = v
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer shared.Close()
	app, _, _ := newTestServer(t)
	app.cfg.EsploraURL = explorer.URL
	app.cfg.AnchorSeenURL = shared.URL + "/last-seen"
	app.cfg.NodeDatadir = t.TempDir()
	peersFile := filepath.Join(t.TempDir(), "peers.json")
	app.cfg.BtcPeersFile = peersFile
	btc := func(updated int64, addrs ...string) {
		var ps []map[string]any
		for _, a := range addrs {
			ps = append(ps, map[string]any{"addr": a, "subver": "/Satoshi:28.1.0(emissio-0123456789)/", "inbound": true})
		}
		ps = append(ps, map[string]any{"addr": "203.0.113.5:48333", "subver": "/Satoshi:27.0.0/", "inbound": false})
		b, _ := json.Marshal(map[string]any{"updated": updated, "peers": ps})
		os.WriteFile(peersFile, b, 0o600)
	}
	seq := func(addrs ...string) string {
		var ps []map[string]any
		for _, a := range addrs {
			ps = append(ps, map[string]any{"addr": a, "subver": "/Sequentia Core:25.2.7(emissio-0123456789)/"})
		}
		b, _ := json.Marshal(ps)
		return string(b)
	}
	status := func(validate bool, st string, tip int64, hash string) string {
		return fmt.Sprintf(`my node says:
{
  "validateanchor": %v,
  "tipheight": %d,
  "anchorheight": 161394,
  "anchorhash": "%s",
  "anchorstatus": "%s",
  "lastposfinalreject": 0,
  "recoveryset": []
}`, validate, tip, hash, st)
	}
	good := status(true, "ok", 212898, anchor)
	cases := []struct {
		name     string
		updated  int64
		btcPeers []string
		seqPeers string
		notes    string
		v        verdict
		want     string
	}{
		{"pass", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), good, vPass, "shared endpoint has had no call from that address for 15 minutes (OK)"},
		{"pass, IPv6 for both", now.Unix() - 30, []string{"[2001:db8::7]:51234"}, seq("[2001:db8::7]:40000"), good, vFail, "shared"},
		{"no Bitcoin node", now.Unix() - 30, nil, seq("198.51.100.7:40000"), good, vFail, "the public Bitcoin node sees no peer announcing emissio-0123456789"},
		{"no Sequentia node", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq(), good, vFail, "no Sequentia node announcing emissio-0123456789 is connected"},
		{"two addresses", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.8:40000"), good, vFail, "connects from 198.51.100.7 and your Sequentia node from 198.51.100.8"},
		{"no status", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), "it works", vFail, "paste the whole output of sequentia-cli getanchorstatus"},
		{"not validating", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), status(false, "not_validated", 212898, anchor), vFail, "validateanchor false"},
		{"no connection", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), status(true, "no_connection", 212898, anchor), vFail, `anchorstatus "no_connection"`},
		{"old status", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), status(true, "ok", 212800, anchor), vFail, "of block 212800 and the chain is at 212900"},
		{"edited anchor", now.Unix() - 30, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), status(true, "ok", 212898, strings.Repeat("ab", 32)), vFail, "but that block anchors to"},
		{"shared endpoint", now.Unix() - 30, []string{"198.51.100.9:51234"}, seq("198.51.100.9:40000"), good, vFail, "was called from 198.51.100.9 1m0s ago"},
		{"stale peer list", now.Unix() - 600, []string{"198.51.100.7:51234"}, seq("198.51.100.7:40000"), good, vReview, "out of date"},
	}
	seen["2001:db8::7"] = now.Unix() - 120
	for _, c := range cases {
		btc(c.updated, c.btcPeers...)
		app.cfg.NodeCLI = fakeCLI(t, c.seqPeers)
		got := app.ownBitcoinCheck(c.notes, code, now)
		if got.Verdict != c.v || !strings.Contains(got.Note, c.want) {
			t.Errorf("%s: got (%d) %q, want (%d) ...%q...", c.name, got.Verdict, got.Note, c.v, c.want)
		}
	}
	// The address-mismatch refusal is the tester's to send to a reviewer.
	btc(now.Unix()-30, "198.51.100.7:51234")
	app.cfg.NodeCLI = fakeCLI(t, seq("198.51.100.8:40000"))
	if got := app.ownBitcoinCheck(good, code, now); got.Firm || !strings.Contains(got.Note, "send this to a reviewer") {
		t.Errorf("two addresses: %+v", got)
	}
	// Not configured: a reviewer decides.
	app.cfg.BtcPeersFile = ""
	if got := app.ownBitcoinCheck(good, code, now); got.Verdict != vReview {
		t.Errorf("unconfigured: %+v", got)
	}
}

// The task is seeded switched off until its check's sources are live.
func TestOwnBitcoinTaskSeededOff(t *testing.T) {
	app, srv, client := newTestServer(t)
	var active int
	app.db.QueryRow("SELECT active FROM tasks WHERE slug = 'own-bitcoin-node'").Scan(&active)
	if active != 0 {
		t.Fatal("own-bitcoin-node is seeded active")
	}
	if strings.Contains(get(t, client, srv.URL+"/tasks"), "Validate anchors with your own Bitcoin node") {
		t.Error("the task page offers an inactive task")
	}
	app.db.Exec("UPDATE tasks SET active = 1 WHERE slug = 'own-bitcoin-node'")
	reseedTasks(app.db)
	app.db.QueryRow("SELECT active FROM tasks WHERE slug = 'own-bitcoin-node'").Scan(&active)
	if active != 1 {
		t.Error("reseed-tasks switched off a task an admin switched on")
	}
}
