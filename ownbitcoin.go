package main

// The task "Validate anchors with your own Bitcoin node". Emissio cannot see
// into a tester's node, so the check puts together what the public side
// sees:
//
//   - the box's Bitcoin node has a peer announcing the account code in its
//     user agent (a -uacomment): its peer list, written every minute by a
//     timer on the box to a file Emissio reads (EMISSIO_BTC_PEERS);
//   - the public Sequentia node has a peer announcing the code from the same
//     address;
//   - the notes carry that node's getanchorstatus: anchors validated, the
//     tip's anchor found on Bitcoin, the tip current, and its anchor the one
//     the public chain records for that height;
//   - the shared Bitcoin endpoint, which a testnet node uses unless told
//     otherwise, has had no call from that address lately: its record of each
//     address's last call (EMISSIO_ANCHOR_SEEN, answered on the box only).
//
// What it cannot see: which endpoint the node really uses, beyond the
// absence of calls to the shared one, so a third-party endpoint passes too.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// btcPeersMaxAge is how old the box's Bitcoin peer list may be: the
	// timer writes it every minute.
	btcPeersMaxAge = 5 * time.Minute
	// tipSlack is how far behind the public tip the pasted status may be.
	tipSlack = 10
)

// sharedQuiet is how long the tester's address must not have called the
// shared endpoint: a validating node calls it every few seconds. A variable
// only so the regtest run need not wait a quarter of an hour.
var sharedQuiet = 15 * time.Minute

type btcPeerList struct {
	Updated int64 `json:"updated"`
	Peers   []struct {
		Addr    string `json:"addr"`
		Subver  string `json:"subver"`
		Inbound bool   `json:"inbound"`
	} `json:"peers"`
}

// peerHost is a peer address without its port.
func peerHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(addr, "[]"))
}

var anchorStatusRe = regexp.MustCompile(`(?s)\{.*\}`)

type anchorStatus struct {
	ValidateAnchor *bool  `json:"validateanchor"`
	TipHeight      *int64 `json:"tipheight"`
	AnchorHeight   int64  `json:"anchorheight"`
	AnchorHash     string `json:"anchorhash"`
	AnchorStatus   string `json:"anchorstatus"`
}

// parseAnchorStatus reads getanchorstatus output pasted in the notes.
func parseAnchorStatus(notes string) (*anchorStatus, bool) {
	m := anchorStatusRe.FindString(notes)
	if m == "" {
		return nil, false
	}
	var st anchorStatus
	if err := json.Unmarshal([]byte(m), &st); err != nil || st.ValidateAnchor == nil || st.TipHeight == nil {
		return nil, false
	}
	return &st, true
}

func (a *App) fetchText(path string) (string, error) {
	resp, err := proofClient.Get(strings.TrimRight(a.cfg.EsploraURL, "/") + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return strings.TrimSpace(string(b)), err
}

// sharedLastCall is the time the shared endpoint last saw a call from host,
// or zero when it has none on record.
func (a *App) sharedLastCall(host string) (time.Time, error) {
	resp, err := proofClient.Get(a.cfg.AnchorSeenURL + "?addr=" + url.QueryEscape(host))
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var r struct {
		Last *int64 `json:"last"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return time.Time{}, err
	}
	if r.Last == nil {
		return time.Time{}, nil
	}
	return time.Unix(*r.Last, 0), nil
}

func (a *App) ownBitcoinCheck(notes, code string, now time.Time) checkResult {
	if a.cfg.BtcPeersFile == "" || a.cfg.AnchorSeenURL == "" {
		return review("the Bitcoin peer list or the shared endpoint's record is not configured; manual review")
	}
	code = strings.ToLower(code)
	// 1. The tester's Bitcoin node, among the box Bitcoin node's peers.
	raw, err := os.ReadFile(a.cfg.BtcPeersFile)
	var list btcPeerList
	if err != nil || json.Unmarshal(raw, &list) != nil {
		return review("the public Bitcoin node's peer list is unavailable; manual review")
	}
	if now.Sub(time.Unix(list.Updated, 0)) > btcPeersMaxAge {
		return review("the public Bitcoin node's peer list is out of date; manual review")
	}
	btcHost, btcAgent := "", ""
	for _, p := range list.Peers {
		if strings.Contains(strings.ToLower(p.Subver), "emissio-"+code) {
			btcHost, btcAgent = peerHost(p.Addr), p.Subver
			break
		}
	}
	if btcHost == "" {
		return fail("the public Bitcoin node sees no peer announcing emissio-%s: start Bitcoin Core with -testnet4 -uacomment=emissio-%s -addnode=159.195.15.140:48333, wait until it is connected, and submit while it is running", code, code)
	}
	// 2. The tester's Sequentia node, from the same address.
	ps, err := a.peers()
	if err != nil {
		return review("Bitcoin node seen (%s); public Sequentia node unreachable, manual review", btcAgent)
	}
	seqHost := ""
	for _, p := range ps {
		if strings.Contains(strings.ToLower(p.Subver), "emissio-"+code) {
			seqHost = peerHost(p.Addr)
			if seqHost == btcHost {
				break
			}
		}
	}
	if seqHost == "" {
		return fail("your Bitcoin node is seen (%s), but no Sequentia node announcing emissio-%s is connected to the public node: run it with -uacomment=emissio-%s -addnode=159.195.15.140:18444 and submit while it is connected", btcAgent, code, code)
	}
	if seqHost != btcHost {
		return fail("your Bitcoin node connects from %s and your Sequentia node from %s: the check needs both on one address. If they run on one machine and reach the internet by different addresses (IPv6 for one and IPv4 for the other, a VPN), connect both with the IPv4 addnode the task gives, or send this to a reviewer", btcHost, seqHost)
	}
	// 3. The node's own report: anchors validated, against a current tip.
	st, ok := parseAnchorStatus(notes)
	if !ok {
		return fail("both nodes are seen from %s; paste the whole output of sequentia-cli getanchorstatus in the notes", btcHost)
	}
	if !*st.ValidateAnchor {
		return fail("your node reports validateanchor false: it does not check anchors at all. Point mainchainrpchost, mainchainrpcport and mainchainrpccookiefile at your Bitcoin node and restart it")
	}
	if st.AnchorStatus != "ok" {
		return fail("your node reports anchorstatus %q: it cannot check the tip's anchor against its Bitcoin node (no_connection means it does not reach it). Fix the connection, then paste a fresh getanchorstatus", st.AnchorStatus)
	}
	var tip int64
	if err := a.fetchJSON("/blocks/tip/height", &tip); err != nil {
		return review("both nodes are seen and the status says ok; the explorer is unreachable, manual review")
	}
	if *st.TipHeight < tip-tipSlack || *st.TipHeight > tip {
		return fail("the pasted status is of block %d and the chain is at %d: paste a fresh getanchorstatus from a synced node", *st.TipHeight, tip)
	}
	hash, err := a.fetchText("/block-height/" + strconv.FormatInt(*st.TipHeight, 10))
	var block struct {
		Anchor struct {
			Height int64  `json:"height"`
			Hash   string `json:"hash"`
		} `json:"bitcoin_anchor"`
	}
	if err != nil || a.fetchJSON("/block/"+hash, &block) != nil {
		return review("the explorer has no block %d yet; manual review", *st.TipHeight)
	}
	if !strings.EqualFold(block.Anchor.Hash, st.AnchorHash) {
		return fail("the pasted status names anchor %s for block %d, but that block anchors to %s: paste the status exactly as your node prints it", short(st.AnchorHash), *st.TipHeight, short(block.Anchor.Hash))
	}
	// 4. Not the shared endpoint.
	last, err := a.sharedLastCall(btcHost)
	if err != nil {
		return review("both nodes are seen and the anchor is validated; the shared endpoint's record is unavailable, manual review")
	}
	if !last.IsZero() && now.Sub(last) < sharedQuiet {
		return fail("the shared Bitcoin endpoint was called from %s %s ago: your Sequentia node still checks its anchors there. Set mainchainrpchost, mainchainrpcport and mainchainrpccookiefile to your own Bitcoin node, restart it, and submit again after %d quiet minutes (another node on the same address counts too)", btcHost, now.Sub(last).Round(time.Second), int(sharedQuiet.Minutes()))
	}
	return pass("your Bitcoin node (%s) and your Sequentia node are seen from %s; your node validates the anchor of block %d (%s), and the shared endpoint has had no call from that address for %d minutes (OK)", btcAgent, btcHost, *st.TipHeight, short(block.Anchor.Hash), int(sharedQuiet.Minutes()))
}
