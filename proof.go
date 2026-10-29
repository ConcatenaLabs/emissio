package main

// Proofs. A txid on its own proves that a transaction exists, not that the
// submitter made it. Every account therefore has a proof amount: eight
// decimals derived from its claim code, and a transaction counts as evidence
// only if one of its transparent outputs carries an amount ending in exactly
// those decimals. Any wallet can send an exact amount, so this needs no
// signing tools, and a stranger's transaction never matches by accident
// (one chance in a hundred million per output).
//
// On top of that, each task checks the property it is about, from the
// explorer's view of the transaction: the fee asset, the number of assets
// moved, a blinded output, an issuance or reissuance, a stake-sized output.
//
// Two tasks are not about transactions at all. Building from source and
// running a node are proven by the node itself: the public node's peer list
// shows every connected node's user agent, and a user agent carrying the
// account code is something only that account's owner would run. A rebuilt
// binary can change its client name, which a release binary cannot, so the
// build task requires the code as the client name; the uptime task accepts
// it as a user-agent comment and is judged on how continuously the node was
// seen over a week.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	proofAtoms = 100_000_000 // atoms per whole unit; the proof lives in the fractional part
	minStake   = 40_000 * proofAtoms
	// The uptime task: a week of sightings at this cadence, this share of them present.
	uptimeDays       = 7
	uptimeSlot       = 10 * time.Minute
	uptimeMinPresent = 0.8
)

// proofTag derives the eight proof decimals from a claim code.
func proofTag(claimCode string) string {
	n, _ := strconv.ParseUint(claimCode, 16, 64)
	return fmt.Sprintf("%08d", n%proofAtoms)
}

// The explorer's view of a transaction, the parts the checks read.
type chainVout struct {
	Value           *int64 `json:"value"`
	Asset           string `json:"asset"`
	ValueCommitment string `json:"valuecommitment"`
	Type            string `json:"scriptpubkey_type"`
	Script          string `json:"scriptpubkey"`
	Address         string `json:"scriptpubkey_address"`
}
type chainVin struct {
	Txid       string     `json:"txid"`
	Vout       int        `json:"vout"`
	IsCoinbase bool       `json:"is_coinbase"`
	Prevout    *chainVout `json:"prevout"`
	Issuance   *issuance  `json:"issuance"`
}
type chainTx struct {
	Txid   string      `json:"txid"`
	Vin    []chainVin  `json:"vin"`
	Vout   []chainVout `json:"vout"`
	Status struct {
		Confirmed   bool   `json:"confirmed"`
		BlockHeight int64  `json:"block_height"`
		BlockHash   string `json:"block_hash"`
	} `json:"status"`
}

var proofClient = &http.Client{Timeout: 8 * time.Second}

// fetchTx returns the transaction, or a human note when it cannot.
func (a *App) fetchTx(txid string) (*chainTx, string) {
	if a.cfg.EsploraURL == "" {
		return nil, "no explorer configured; manual review"
	}
	resp, err := proofClient.Get(strings.TrimRight(a.cfg.EsploraURL, "/") + "/tx/" + strings.ToLower(txid))
	if err != nil {
		return nil, "explorer unreachable; manual review"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "txid NOT FOUND on the testnet"
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Sprintf("explorer returned HTTP %d; manual review", resp.StatusCode)
	}
	var tx chainTx
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&tx); err != nil {
		return nil, "explorer response unreadable; manual review"
	}
	return &tx, ""
}

// fetchJSON reads one explorer endpoint into out.
func (a *App) fetchJSON(path string, out any) error {
	resp, err := proofClient.Get(strings.TrimRight(a.cfg.EsploraURL, "/") + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// proofOutput finds the transparent output carrying the proof decimals.
func proofOutput(tx *chainTx, tag string) (int, *chainVout) {
	want, _ := strconv.ParseInt(tag, 10, 64)
	for i := range tx.Vout {
		o := &tx.Vout[i]
		if o.Value == nil || o.Type == "fee" || o.Type == "op_return" {
			continue
		}
		if *o.Value%proofAtoms == want {
			return i, o
		}
	}
	return -1, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

var hashRe = regexp.MustCompile(`[0-9a-fA-F]{64}`)

// anchorCheck compares the Bitcoin block hash the user wrote in the notes
// with the anchor of the block that holds their transaction.
func (a *App) anchorCheck(tx *chainTx, notes string) (bool, string) {
	if !tx.Status.Confirmed {
		return false, "anchor unknown until the transaction confirms"
	}
	var block struct {
		Anchor json.RawMessage `json:"bitcoin_anchor"`
	}
	if err := a.fetchJSON("/block/"+tx.Status.BlockHash, &block); err != nil {
		return false, "block anchor unavailable"
	}
	anchor := strings.ToLower(string(block.Anchor))
	for _, h := range hashRe.FindAllString(notes, -1) {
		if h != tx.Txid && strings.Contains(anchor, strings.ToLower(h)) {
			return true, "anchor hash in the notes matches the block's Bitcoin anchor (OK)"
		}
	}
	return false, "no hash in the notes matches the block's Bitcoin anchor"
}

// ---- peers: the public node's view of who is connected ----

type peer struct {
	Addr   string `json:"addr"`
	Subver string `json:"subver"`
}

// peers reads the public node's peer list through its CLI.
func (a *App) peers() ([]peer, error) {
	if a.cfg.NodeCLI == "" || a.cfg.NodeDatadir == "" {
		return nil, fmt.Errorf("no node configured")
	}
	out, err := exec.Command(a.cfg.NodeCLI, "-datadir="+a.cfg.NodeDatadir, "getpeerinfo").Output()
	if err != nil {
		return nil, err
	}
	var ps []peer
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}

var peerCodeRe = regexp.MustCompile(`(?i)emissio-([0-9a-f]{10})`)

// peerCodes maps every account code seen in a user agent to that user agent.
func peerCodes(ps []peer) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		for _, m := range peerCodeRe.FindAllStringSubmatch(p.Subver, -1) {
			out[strings.ToLower(m[1])] = p.Subver
		}
	}
	return out
}

// builtClientName is what a rebuilt node announces when CLIENT_NAME is the
// account code: the name sits before the colon, where a comment cannot go.
func builtClientName(subver, code string) bool {
	return strings.HasPrefix(strings.ToLower(subver), "/emissio-"+strings.ToLower(code)+":")
}

func (a *App) buildCheck(code string) checkResult {
	ps, err := a.peers()
	if err != nil {
		return review("public node unreachable; manual review")
	}
	for _, p := range ps {
		if builtClientName(p.Subver, code) {
			return pass("seen as peer %s announcing %s (OK): a client name only a rebuilt node can carry", p.Addr, p.Subver)
		}
	}
	if sv, ok := peerCodes(ps)[strings.ToLower(code)]; ok {
		return fail("seen as a peer announcing %s, but only as a user-agent comment: the client name must be Emissio-%s, which needs a rebuild", sv, code)
	}
	return fail("no connected peer announces Emissio-%s; the node must be running and connected to the public node when you submit", code)
}

// recordSightings stores one row per code seen, at this moment.
func (a *App) recordSightings() {
	ps, err := a.peers()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for code := range peerCodes(ps) {
		a.db.Exec("INSERT INTO peer_sightings (code, seen_at) VALUES (?, ?)", code, now)
	}
}

// startPeerPoller records sightings at the uptime cadence.
func (a *App) startPeerPoller() {
	if a.cfg.NodeCLI == "" || a.cfg.NodeDatadir == "" {
		return
	}
	go func() {
		a.recordSightings()
		for range time.Tick(uptimeSlot) {
			a.recordSightings()
		}
	}()
}

// uptimeCheck judges a week of sightings: the span from first to last, and
// how many of the ten-minute slots in the last seven days had one.
func (a *App) uptimeCheck(code string, now time.Time) checkResult {
	if a.cfg.NodeCLI == "" || a.cfg.NodeDatadir == "" {
		return review("no public node configured; manual review")
	}
	note := uptimeVerdict(a.db, strings.ToLower(code), now)
	if strings.HasSuffix(note, "(OK)") {
		return pass("%s", note)
	}
	return fail("%s", note)
}

func uptimeVerdict(db *sql.DB, code string, now time.Time) string {
	since := now.Add(-uptimeDays * 24 * time.Hour).Unix()
	rows, err := db.Query("SELECT seen_at FROM peer_sightings WHERE code = ? AND seen_at >= ? ORDER BY seen_at", code, since)
	if err != nil {
		return "sightings unavailable; manual review"
	}
	defer rows.Close()
	slots := map[int64]bool{}
	var first, last int64
	for rows.Next() {
		var t int64
		if rows.Scan(&t) != nil {
			continue
		}
		if first == 0 {
			first = t
		}
		last = t
		slots[t/int64(uptimeSlot.Seconds())] = true
	}
	if first == 0 {
		return "node never seen: run it with -uacomment=emissio-" + code + " connected to the public node, and submit after a week"
	}
	total := int64(uptimeDays*24*time.Hour) / int64(uptimeSlot)
	present := float64(len(slots)) / float64(total)
	days := float64(last-first) / 86400
	verdict := "does NOT yet qualify"
	if days >= uptimeDays-0.5 && present >= uptimeMinPresent {
		verdict = "OK"
	}
	return fmt.Sprintf("node seen over %.1f days, present in %.0f%% of the last week's %d-minute slots (%s)",
		days, present*100, int(uptimeSlot.Minutes()), verdict)
}

var _ = log.Printf

// ---- competition entries and security reports ----

// normalizeURL makes two spellings of the same link compare equal.
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		host := rest
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			host = rest[:j]
		}
		s = strings.ToLower(s[:i+3]+host) + rest[len(host):]
	}
	return s
}

// entryCheck looks for the account code where the entrant was told to put
// it: in the link itself, or in the text of the linked page. Work inside an
// image cannot be read here, which the note says, so the judges look.
func (a *App) entryCheck(entryURL, claimCode string) string {
	code := strings.ToLower(claimCode)
	if strings.Contains(strings.ToLower(entryURL), code) {
		return "account code is in the link (OK)"
	}
	req, err := http.NewRequest("GET", entryURL, nil)
	if err != nil {
		return "link unreadable"
	}
	req.Header.Set("User-Agent", "emissio-verifier/1.0 (sequentiatestnet.com)")
	resp, err := proofClient.Do(req)
	if err != nil {
		return "link unreachable; the judges check that the work carries the account code " + claimCode
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("link returned HTTP %d; the judges check that the work carries the account code %s", resp.StatusCode, claimCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
		return "link is a file (" + strings.SplitN(ct, ";", 2)[0] + "); the judges check that the work itself carries the account code " + claimCode
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if strings.Contains(strings.ToLower(string(body)), code) {
		return "account code found on the linked page (OK)"
	}
	return "account code NOT found on the linked page; the judges check that the work itself carries the account code " + claimCode
}

// reportCheck binds a security report to its account: the first line must
// be the account code, inside the encrypted block when the report is
// encrypted, so a report that leaks or is decrypted later still names the
// account that sent it. It also flags a body identical to another account's,
// the cheap form of copying.
func (a *App) reportCheck(userID int64, body, claimCode string) string {
	trimmed := strings.TrimSpace(body)
	encrypted := strings.HasPrefix(trimmed, "-----BEGIN PGP MESSAGE-----")
	sum := sha256.Sum256([]byte(trimmed))
	digest := fmt.Sprintf("%x", sum[:])
	var dupID int64
	a.db.QueryRow("SELECT id FROM reports WHERE body_hash = ? AND user_id != ? ORDER BY id LIMIT 1", digest, userID).Scan(&dupID)
	var parts []string
	if encrypted {
		parts = append(parts, "encrypted: after decrypting, confirm the first line is the account code "+claimCode)
	} else {
		first := strings.TrimSpace(strings.SplitN(trimmed, "\n", 2)[0])
		if strings.EqualFold(first, claimCode) {
			parts = append(parts, "first line carries the account code (OK)")
		} else {
			parts = append(parts, "account code NOT on the first line")
		}
	}
	if dupID > 0 {
		parts = append(parts, fmt.Sprintf("IDENTICAL to report #%d from another account", dupID))
	}
	return strings.Join(parts, "; ")
}
