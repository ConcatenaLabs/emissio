package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every verdict of the own-Bitcoin-node check, and the slips a tester
// makes: one node not running, a node not synced or connected too briefly or
// in one list only, several codes in one user agent, two addresses, a node
// still on the shared endpoint, a record too young or full, and an address
// that has vouched for another account.
func TestOwnBitcoinCheck(t *testing.T) {
	code := "0123456789"
	now := time.Unix(1_800_000_000, 0)
	// The shared endpoint's record: .7 quiet for an hour, .9 a minute ago.
	seen := map[string]int64{"198.51.100.7": now.Unix() - 3600, "198.51.100.9": now.Unix() - 60, "2001:db8::7": now.Unix() - 120}
	since, dropped := now.Unix()-86400, int64(0)
	oldProxy := false
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.URL.Query().Get("addr")
		out := map[string]any{"addr": a, "last": nil}
		if !oldProxy {
			out["since"], out["dropped_last"] = since, nil
			if dropped != 0 {
				out["dropped_last"] = dropped
			}
		}
		if v, ok := seen[a]; ok {
			out["last"] = v
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer shared.Close()
	app, _, _ := newTestServer(t)
	app.cfg.AnchorSeenURL = shared.URL + "/last-seen"
	app.cfg.NodeDatadir = t.TempDir()
	peersFile := filepath.Join(t.TempDir(), "peers.json")
	app.cfg.BtcPeersFile = peersFile
	uid, _ := createUser(app.db, "btc@example.com", "x", code, "")
	user := &User{ID: uid, ClaimCode: code}
	uid2, _ := createUser(app.db, "btc2@example.com", "x", "9876543210", "")
	user2 := &User{ID: uid2, ClaimCode: "9876543210"}

	type bp struct {
		addr, subver   string
		headers, block int64
		conn           time.Duration // connected this long before now
		inPrev         bool
	}
	good := func(addr string) bp {
		return bp{addr, "/Satoshi:28.1.0(emissio-" + code + ")/", 119998, 119996, 20 * time.Minute, true}
	}
	btc := func(updated int64, peers ...bp) {
		ps := []map[string]any{{"id": 1, "addr": "203.0.113.5:48333", "subver": "/Satoshi:27.0.0/", "inbound": false, "services": []string{"NETWORK"}, "conntime": now.Unix() - 9000,
			"synced_headers": 120001, "synced_blocks": 120001, "first_height": 119980, "best_headers": 120001, "best_blocks": 120001, "listed": 150}}
		for i, p := range peers {
			listed := 1
			if p.inPrev {
				listed = 12
			}
			// The node's height was 119998 when the connection was first
			// listed; it is 120001 now, and a follower's heights stay put.
			ps = append(ps, map[string]any{"id": 10 + i, "addr": p.addr, "subver": p.subver, "inbound": true, "services": []string{"NETWORK", "WITNESS"}, "conntime": now.Add(-p.conn).Unix(),
				"synced_headers": p.headers, "synced_blocks": p.block, "first_height": 119998, "best_headers": p.headers, "best_blocks": p.block, "listed": listed})
		}
		b, _ := json.Marshal(map[string]any{"updated": updated, "height": 120001, "peers": ps})
		os.WriteFile(peersFile, b, 0o600)
	}
	seq := func(addrs ...string) string {
		var ps []map[string]any
		for _, a := range addrs {
			ps = append(ps, map[string]any{"addr": a, "subver": "/Sequentia Core:25.2.7(emissio-" + code + ")/"})
		}
		b, _ := json.Marshal(ps)
		return string(b)
	}
	fresh := now.Unix() - 30
	unsynced := good("198.51.100.7:51234")
	unsynced.headers, unsynced.block = -1, -1
	young := good("198.51.100.7:51234")
	young.conn = 4 * time.Minute
	once := good("198.51.100.7:51234")
	once.inPrev = false
	several := good("198.51.100.7:51234")
	several.subver = "/Satoshi:28.1.0(emissio-" + code + "; emissio-9876543210)/"
	cases := []struct {
		name     string
		updated  int64
		btcPeers []bp
		seqPeers string
		v        verdict
		firm     bool
		want     string
	}{
		{"pass", fresh, []bp{good("198.51.100.7:51234")}, seq("198.51.100.7:40000"), vPass, false, "has had no call from that address for 15 minutes (OK)"},
		{"IPv6 for both, shared endpoint used", fresh, []bp{good("[2001:db8::7]:51234")}, seq("[2001:db8::7]:40000"), vFail, false, "was called from 2001:db8::7 2m0s ago"},
		{"no Bitcoin node", fresh, nil, seq("198.51.100.7:40000"), vFail, false, "the public Bitcoin node sees no peer announcing emissio-0123456789"},
		{"not synced (a handshake only)", fresh, []bp{unsynced}, seq("198.51.100.7:40000"), vFail, false, "does not count it as synced: on this connection it knows its headers to -1 and its blocks to -1, and its own height was 119998 when the connection was first listed"},
		{"connected four minutes", fresh, []bp{young}, seq("198.51.100.7:40000"), vFail, false, "connected 4m0s ago: the check wants one connection of 10 minutes. Keep it running and submit again in 6 minutes"},
		{"in one list only", fresh, []bp{once}, seq("198.51.100.7:40000"), vFail, false, "not on the same connection in the one before"},
		{"several codes", fresh, []bp{several}, seq("198.51.100.7:40000"), vFail, true, "announces several account codes"},
		{"the best of two peers is shown", fresh, []bp{unsynced, young}, seq("198.51.100.7:40000"), vFail, false, "connected 4m0s ago"},
		{"a good peer beside a bad one", fresh, []bp{unsynced, good("198.51.100.7:51235")}, seq("198.51.100.7:40000"), vPass, false, "(OK)"},
		{"no Sequentia node", fresh, []bp{good("198.51.100.7:51234")}, seq(), vFail, false, "no Sequentia node announcing emissio-0123456789 is connected"},
		{"two addresses", fresh, []bp{good("198.51.100.7:51234")}, seq("198.51.100.8:40000"), vFail, false, "connects from 198.51.100.7 and your Sequentia node from 198.51.100.8"},
		{"shared endpoint", fresh, []bp{good("198.51.100.9:51234")}, seq("198.51.100.9:40000"), vFail, false, "was called from 198.51.100.9 1m0s ago"},
		{"stale peer list", now.Unix() - 600, []bp{good("198.51.100.7:51234")}, seq("198.51.100.7:40000"), vReview, false, "out of date"},
	}
	for _, c := range cases {
		btc(c.updated, c.btcPeers...)
		app.cfg.NodeCLI = fakeCLI(t, c.seqPeers)
		got := app.ownBitcoinCheck("", user, now)
		if got.Verdict != c.v || got.Firm != c.firm || !strings.Contains(got.Note, c.want) {
			t.Errorf("%s: got (%d firm=%v) %q, want (%d firm=%v) ...%q...", c.name, got.Verdict, got.Firm, got.Note, c.v, c.firm, c.want)
		}
	}
	run := func(u *User, notes string) checkResult {
		btc(fresh, good("198.51.100.7:51234"))
		app.cfg.NodeCLI = fakeCLI(t, seq("198.51.100.7:40000"))
		return app.ownBitcoinCheck(notes, u, now)
	}
	// The address-mismatch refusal is the tester's to send to a reviewer.
	btc(fresh, good("198.51.100.7:51234"))
	app.cfg.NodeCLI = fakeCLI(t, seq("198.51.100.8:40000"))
	if got := app.ownBitcoinCheck("", user, now); got.Firm || !strings.Contains(got.Note, "send this to a reviewer") {
		t.Errorf("two addresses: %+v", got)
	}
	// A record that began five minutes ago cannot show 15 quiet minutes.
	since = now.Unix() - 300
	if got := run(user, ""); got.Verdict != vFail || !strings.Contains(got.Note, "began only 5m0s ago") || !strings.Contains(got.Note, "submit again in 10 minutes") {
		t.Errorf("young record: %+v", got)
	}
	since = now.Unix() - 86400
	// A record that was full three minutes ago may have missed the address.
	dropped = now.Unix() - 180
	if got := run(user, ""); got.Verdict != vReview || !strings.Contains(got.Note, "was full 3m0s ago") {
		t.Errorf("full record: %+v", got)
	}
	// Full twenty minutes ago: the last fifteen are whole again.
	dropped = now.Unix() - 1200
	if got := run(user, ""); got.Verdict != vPass {
		t.Errorf("full long ago: %+v", got)
	}
	dropped = 0
	// A proxy that does not say when its record began is not trusted.
	oldProxy = true
	if got := run(user, ""); got.Verdict != vReview || !strings.Contains(got.Note, "record is unavailable") {
		t.Errorf("record without a start: %+v", got)
	}
	oldProxy = false
	// The address vouched for the first account; a second account from it
	// goes to a reviewer, and the first still passes.
	code2 := user2.ClaimCode
	btc(fresh, bp{"198.51.100.7:51236", "/Satoshi:28.1.0(emissio-" + code2 + ")/", 119998, 119998, 30 * time.Minute, true})
	app.cfg.NodeCLI = fakeCLI(t, `[{"addr":"198.51.100.7:40001","subver":"/Sequentia Core:25.2.7(emissio-`+code2+`)/"}]`)
	if got := app.ownBitcoinCheck("", user2, now); got.Verdict != vReview || !strings.Contains(got.Note, "already been used for another account") {
		t.Errorf("second account: %+v", got)
	}
	if got := run(user, ""); got.Verdict != vPass {
		t.Errorf("first account again: %+v", got)
	}
	// A pasted status that shows a problem says what is wrong on a refusal,
	// and is no evidence on a pass.
	bad := `{"validateanchor": true, "tipheight": 5, "anchorstatus": "no_connection"}`
	if got := run(user, bad); got.Verdict != vPass {
		t.Errorf("a status is no evidence: %+v", got)
	}
	btc(fresh)
	app.cfg.NodeCLI = fakeCLI(t, seq("198.51.100.7:40000"))
	if got := app.ownBitcoinCheck("mine: "+bad, user, now); got.Verdict != vFail || !strings.HasPrefix(got.Note, `the status you pasted says anchorstatus "no_connection"`) || !strings.Contains(got.Note, "sees no peer announcing") {
		t.Errorf("diagnosis: %+v", got)
	}
	if got := app.ownBitcoinCheck(`{"validateanchor": false, "tipheight": 5, "anchorstatus": "not_validated"}`, user, now); !strings.Contains(got.Note, "validateanchor false") {
		t.Errorf("diagnosis, not validating: %+v", got)
	}
	// Not configured: a reviewer decides.
	app.cfg.BtcPeersFile = ""
	if got := app.ownBitcoinCheck("", user, now); got.Verdict != vReview {
		t.Errorf("unconfigured: %+v", got)
	}
}

// The status is found with text, other objects and stray braces around it.
func TestParseAnchorStatus(t *testing.T) {
	st := `{
  "validateanchor": true,
  "tipheight": 212898,
  "anchorheight": 161394,
  "anchorhash": "00ab",
  "anchorstatus": "ok",
  "recoveryset": []
}`
	for name, notes := range map[string]string{
		"alone":                st,
		"text around":          "my node says:\n" + st + "\nthanks",
		"braces before":        "{} and { not json\n" + st,
		"an object before":     `{"chain": "test", "blocks": 5}` + "\n" + st,
		"an object after":      st + "\n" + `{"other": 1}`,
		"braces in text after": st + " see {this}",
	} {
		got, ok := parseAnchorStatus(notes)
		if !ok || *got.TipHeight != 212898 || got.AnchorStatus != "ok" {
			t.Errorf("%s: %v %+v", name, ok, got)
		}
	}
	for name, notes := range map[string]string{"none": "it works", "another object only": `{"chain": "test"}`, "cut short": `{"validateanchor": true, "tipheight": 5`} {
		if _, ok := parseAnchorStatus(notes); ok {
			t.Errorf("%s: parsed", name)
		}
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
