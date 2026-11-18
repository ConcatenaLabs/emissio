package main

// The task "Validate anchors with your own Bitcoin node". Emissio cannot see
// into a tester's node, so the check puts together what the public side
// sees:
//
//   - the box's Bitcoin node has a peer announcing the account code in its
//     user agent (a -uacomment), and only that one code; the node counts it
//     as synced (its headers and blocks within a few of the box node's
//     height when the connection began), it has been connected for ten
//     minutes, and the same connection is in two successive peer lists. The
//     lists are written every minute by a timer on the box to a file Emissio
//     reads (EMISSIO_BTC_PEERS), each carrying the one before it on;
//   - the public Sequentia node has a peer announcing the code from the same
//     address;
//   - the shared Bitcoin endpoint, which a testnet node uses unless told
//     otherwise, has had no call from that address for the quiet interval,
//     by its record of each address's last call (EMISSIO_ANCHOR_SEEN,
//     answered on the box only), and that record covers the whole interval;
//   - the address has not vouched for another account before: a Bitcoin
//     node's address vouches for one account, ever, as a social handle does,
//     and a second account from it goes to a reviewer.
//
// What it cannot see: the node's settings, and which endpoint the tester's
// Sequentia node really asks beyond the absence of calls to the shared one.
// A peer's user agent is what the peer announces; a program that completes
// the handshake and answers for headers can announce it too. The
// getanchorstatus a tester pastes is not evidence: it is read only to tell a
// tester whose node reports a problem what is wrong.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// btcPeersMaxAge is how old the box's Bitcoin peer list may be: the
	// timer writes it every minute.
	btcPeersMaxAge = 5 * time.Minute
	// btcSyncSlack is how many blocks below the box node's height, when the
	// connection was first listed, a peer's headers and blocks may be for the
	// node to count it as synced.
	btcSyncSlack = 3
)

// sharedQuiet is how long the tester's address must not have called the
// shared endpoint: a validating node calls it every few seconds. btcMinConn
// is how long the tester's Bitcoin node must have been connected. Variables
// only so the regtest run need not wait a quarter of an hour.
var (
	sharedQuiet = 15 * time.Minute
	btcMinConn  = 10 * time.Minute
)

const btcNodeSchema = `
CREATE TABLE IF NOT EXISTS btc_node_hosts (
	host TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	created_at INTEGER NOT NULL
);
`

// btcPeer is one peer of the box's Bitcoin node, as btc-peers.py writes it.
// Bitcoin Core learns a peer's height when it connects and when it announces
// a block, and a node that only follows never announces one; so the list
// follows each connection (ID and Conntime) from one list to the next:
// FirstHeight is the box node's height when the connection was first
// listed, BestHeaders and BestBlocks the highest synced heights seen on it,
// and Listed how many successive lists carry it.
type btcPeer struct {
	ID          int64    `json:"id"`
	Addr        string   `json:"addr"`
	Subver      string   `json:"subver"`
	Inbound     bool     `json:"inbound"`
	Services    []string `json:"services"`
	Conntime    int64    `json:"conntime"`
	FirstHeight int64    `json:"first_height"`
	BestHeaders int64    `json:"best_headers"`
	BestBlocks  int64    `json:"best_blocks"`
	Listed      int      `json:"listed"`
}

type btcPeerList struct {
	Updated int64     `json:"updated"`
	Height  *int64    `json:"height"`
	Peers   []btcPeer `json:"peers"`
}

// peerHost is a peer address without its port.
func peerHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(addr, "[]"))
}

var emissioCodeRe = regexp.MustCompile(`(?i)emissio-([0-9a-z]+)`)

// agentCodes are the distinct account codes a user agent announces.
func agentCodes(subver string) []string {
	var codes []string
	seen := map[string]bool{}
	for _, m := range emissioCodeRe.FindAllStringSubmatch(subver, -1) {
		c := strings.ToLower(m[1])
		if !seen[c] {
			seen[c] = true
			codes = append(codes, c)
		}
	}
	return codes
}

func announces(subver, code string) bool {
	for _, c := range agentCodes(subver) {
		if c == code {
			return true
		}
	}
	return false
}

type anchorStatus struct {
	ValidateAnchor *bool  `json:"validateanchor"`
	TipHeight      *int64 `json:"tipheight"`
	AnchorStatus   string `json:"anchorstatus"`
}

// parseAnchorStatus finds getanchorstatus output pasted in the notes: the
// first JSON object, decoded from any opening brace, that has the status's
// fields. Text and other objects or braces around it are passed over.
func parseAnchorStatus(notes string) (*anchorStatus, bool) {
	for i := 0; i < len(notes); i++ {
		if notes[i] != '{' {
			continue
		}
		var st anchorStatus
		if json.NewDecoder(strings.NewReader(notes[i:])).Decode(&st) == nil && st.ValidateAnchor != nil && st.TipHeight != nil {
			return &st, true
		}
	}
	return nil, false
}

// anchorDiagnosis is what a pasted status says is wrong, or "".
func anchorDiagnosis(notes string) string {
	st, ok := parseAnchorStatus(notes)
	switch {
	case !ok:
		return ""
	case !*st.ValidateAnchor:
		return "the status you pasted says validateanchor false: your node does not check anchors at all. Point mainchainrpchost, mainchainrpcport and mainchainrpccookiefile at your Bitcoin node and restart it"
	case st.AnchorStatus != "ok":
		return fmt.Sprintf("the status you pasted says anchorstatus %q: your node cannot check the tip's anchor against its Bitcoin node (no_connection means it does not reach it). Fix the connection and wait until getanchorstatus says ok", st.AnchorStatus)
	}
	return ""
}

// sharedRecord is the shared endpoint's answer about one address: its last
// call (zero when none is on record), when the record began (the proxy's
// start), and when it last had to leave a new caller out because it was
// full (zero when never).
type sharedRecord struct {
	Last, Since, Dropped time.Time
}

func unixOrZero(p *int64) time.Time {
	if p == nil {
		return time.Time{}
	}
	return time.Unix(*p, 0)
}

func (a *App) sharedLastCall(host string) (sharedRecord, error) {
	resp, err := proofClient.Get(a.cfg.AnchorSeenURL + "?addr=" + url.QueryEscape(host))
	if err != nil {
		return sharedRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return sharedRecord{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var r struct {
		Last    *int64 `json:"last"`
		Since   *int64 `json:"since"`
		Dropped *int64 `json:"dropped_last"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return sharedRecord{}, err
	}
	if r.Since == nil {
		return sharedRecord{}, fmt.Errorf("the record does not say when it began")
	}
	return sharedRecord{Last: unixOrZero(r.Last), Since: unixOrZero(r.Since), Dropped: unixOrZero(r.Dropped)}, nil
}

// minutesUntil is a wait in whole minutes, rounded up, in words.
func minutesUntil(d time.Duration) string {
	if n := int(math.Ceil(d.Minutes())); n != 1 {
		return fmt.Sprintf("%d minutes", n)
	}
	return "1 minute"
}

// btcPeerVerdict judges one peer announcing the code: "" when it is the
// node the task asks for, otherwise what is missing, and how far it got
// (for choosing which refusal to show when several peers announce the code).
func btcPeerVerdict(p btcPeer, code string, now time.Time) (string, int) {
	if codes := agentCodes(p.Subver); len(codes) > 1 {
		return fmt.Sprintf("your Bitcoin node announces several account codes (%s): announce only yours, with one -uacomment=emissio-%s", p.Subver, code), 0
	}
	if p.BestHeaders < p.FirstHeight-btcSyncSlack || p.BestBlocks < p.FirstHeight-btcSyncSlack {
		return fmt.Sprintf("your Bitcoin node is seen (%s), but the public Bitcoin node does not count it as synced: on this connection it knows its headers to %d and its blocks to %d, and its own height was %d when the connection was first listed. Let your node finish syncing; once it has, restart it so that it connects afresh: it can pass %d minutes later", p.Subver, p.BestHeaders, p.BestBlocks, p.FirstHeight, int(btcMinConn.Minutes())), 1
	}
	if up := now.Sub(time.Unix(p.Conntime, 0)); up < btcMinConn {
		return fmt.Sprintf("your Bitcoin node is seen (%s), connected %s ago: the check wants one connection of %d minutes. Keep it running: it can pass in %s", p.Subver, up.Round(time.Second), int(btcMinConn.Minutes()), minutesUntil(btcMinConn-up)), 2
	}
	if p.Listed < 2 {
		return fmt.Sprintf("your Bitcoin node is seen (%s) in the latest peer list but not on the same connection in the one before: keep it connected: it can pass in two minutes", p.Subver), 3
	}
	return "", 4
}

func (a *App) ownBitcoinCheck(notes string, user *User, now time.Time) checkResult {
	res := a.ownBitcoinObserved(user, now)
	if res.Verdict == vFail || res.Verdict == vWait {
		if d := anchorDiagnosis(notes); d != "" {
			res.Note = d + ". What the public side sees: " + res.Note
		}
	}
	return res
}

func (a *App) ownBitcoinObserved(user *User, now time.Time) checkResult {
	if a.cfg.BtcPeersFile == "" || a.cfg.AnchorSeenURL == "" {
		return review("the Bitcoin peer list or the shared endpoint's record is not configured; manual review")
	}
	code := strings.ToLower(user.ClaimCode)
	// 1. The tester's Bitcoin node, among the box Bitcoin node's peers.
	raw, err := os.ReadFile(a.cfg.BtcPeersFile)
	var list btcPeerList
	if err != nil || json.Unmarshal(raw, &list) != nil || list.Height == nil {
		return review("the public Bitcoin node's peer list is unavailable; manual review")
	}
	if now.Sub(time.Unix(list.Updated, 0)) > btcPeersMaxAge {
		return review("the public Bitcoin node's peer list is out of date; manual review")
	}
	var btc *btcPeer
	why, best := "", -1
	for i, p := range list.Peers {
		if !announces(p.Subver, code) {
			continue
		}
		w, stage := btcPeerVerdict(p, code, now)
		if w == "" {
			btc = &list.Peers[i]
			break
		}
		if stage > best {
			why, best = w, stage
		}
	}
	if btc == nil {
		if best == 0 {
			return firmFail("%s", why)
		}
		if why != "" {
			return wait("%s", why)
		}
		return wait("the public Bitcoin node sees no peer announcing emissio-%s yet: start Bitcoin Core with -testnet4 -uacomment=emissio-%s -addnode=159.195.15.140:48333 and let it sync", code, code)
	}
	btcHost, btcAgent := peerHost(btc.Addr), btc.Subver
	// 2. The tester's Sequentia node, from the same address.
	ps, err := a.peers()
	if err != nil {
		return review("Bitcoin node seen (%s); public Sequentia node unreachable, manual review", btcAgent)
	}
	seqHost := ""
	for _, p := range ps {
		if !announces(p.Subver, code) {
			continue
		}
		if len(agentCodes(p.Subver)) > 1 {
			return firmFail("your Sequentia node announces several account codes (%s): announce only yours, with one -uacomment=emissio-%s", p.Subver, code)
		}
		seqHost = peerHost(p.Addr)
		if seqHost == btcHost {
			break
		}
	}
	if seqHost == "" {
		return wait("your Bitcoin node is seen (%s), but no Sequentia node announcing emissio-%s is connected to the public node yet: run it with -uacomment=emissio-%s -addnode=159.195.15.140:18444", btcAgent, code, code)
	}
	if seqHost != btcHost {
		return review("your Bitcoin node connects from %s and your Sequentia node from %s: the check needs both on one address. If they run on one machine and reach the internet by different addresses (IPv6 for one and IPv4 for the other, a VPN), connect both with the IPv4 addnode the task gives. A reviewer looks at this one", btcHost, seqHost)
	}
	// 3. Not the shared endpoint, by a record that covers the whole interval.
	rec, err := a.sharedLastCall(btcHost)
	if err != nil {
		return review("both nodes are seen from %s; the shared endpoint's record is unavailable, manual review", btcHost)
	}
	if !rec.Last.IsZero() && now.Sub(rec.Last) < sharedQuiet {
		return wait("the shared Bitcoin endpoint was called from %s %s ago: your Sequentia node still checks its anchors there. Set mainchainrpchost, mainchainrpcport and mainchainrpccookiefile to your own Bitcoin node, and restart it: it passes after %d quiet minutes (another node on the same address counts too)", btcHost, now.Sub(rec.Last).Round(time.Second), int(sharedQuiet.Minutes()))
	}
	// An address the record holds is followed even while the record is
	// full (the endpoint updates an entry it has), so its silence is known.
	// Only an address the record does not hold could have called unseen:
	// while the record was full, or before it began.
	known := !rec.Last.IsZero()
	if !known && !rec.Dropped.IsZero() && now.Sub(rec.Dropped) < sharedQuiet {
		return review("both nodes are seen from %s; the shared endpoint's record was full %s ago and may have missed a call from that address, manual review", btcHost, now.Sub(rec.Dropped).Round(time.Second))
	}
	if age := now.Sub(rec.Since); !known && age < sharedQuiet {
		return wait("both nodes are seen from %s, but the shared endpoint's record began only %s ago and cannot yet show %d quiet minutes: keep both nodes running; it can pass in %s", btcHost, age.Round(time.Second), int(sharedQuiet.Minutes()), minutesUntil(sharedQuiet-age))
	}
	// 4. One account per Bitcoin node address, ever.
	a.db.Exec("INSERT OR IGNORE INTO btc_node_hosts (host, user_id, created_at) VALUES (?,?,?)", btcHost, user.ID, now.Unix())
	var owner int64
	if err := a.db.QueryRow("SELECT user_id FROM btc_node_hosts WHERE host = ?", btcHost).Scan(&owner); err != nil {
		return review("both nodes are seen from %s; the record of addresses could not be read, manual review", btcHost)
	}
	if owner != user.ID {
		return review("a Bitcoin node from %s has already been used for another account; a reviewer decides", btcHost)
	}
	return pass("a synced Bitcoin node announcing your code (%s), connected for %s, and your Sequentia node are seen from %s, and the shared endpoint has had no call from that address for %d minutes (OK)", btcAgent, now.Sub(time.Unix(btc.Conntime, 0)).Round(time.Minute), btcHost, int(sharedQuiet.Minutes()))
}
