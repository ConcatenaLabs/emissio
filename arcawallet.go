package main

// Emissio's own web-wallet-grade wallet: the `arca` command line on a
// directory of its own (EMISSIO_ARCA, EMISSIO_ARCA_DATADIR), as the faucet
// runs its leaf wallet. With it Emissio is the counterparty of the web
// wallet tasks:
//
//   - each account gets its own receive code from Emissio,
//     bitcoin:<address>?ark=<a Bitcoin ark address of its own>&sequentia=<a
//     receive request of its own>, so whatever arrives at either part is
//     known to come from that account's wallet;
//   - Emissio pays an account's receive code an amount it does not show,
//     and the account sends back what arrived, which only a wallet
//     that received it can know;
//   - Emissio pays an invoice the account's wallet made;
//   - Emissio pays the wallet at its payment name, through the name service
//     (EMISSIO_LNADDR), and the account sends back what arrived.
//
// Every arca call is serialised: the wallet's store is not safe for two
// processes at once, and its Bitcoin side takes an exclusive lock. A
// follower syncs on the wallet's own schedule, reads each coin the mailbox
// accepted and attributes it to the request whose owner and nonce its record
// carries.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const arcaSchema = `
CREATE TABLE IF NOT EXISTS arca_codes (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	purpose TEXT NOT NULL,
	request TEXT NOT NULL,
	owner TEXT NOT NULL,
	owner_nonce TEXT NOT NULL,
	ark_address TEXT NOT NULL DEFAULT '',
	want_asset TEXT NOT NULL DEFAULT '',
	want_atoms INTEGER NOT NULL DEFAULT 0,
	paid_leaf TEXT NOT NULL DEFAULT '',
	paid_asset TEXT NOT NULL DEFAULT '',
	paid_atoms INTEGER NOT NULL DEFAULT 0,
	paid_at INTEGER NOT NULL DEFAULT 0,
	btc_sats INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS arca_codes_user ON arca_codes(user_id, purpose);
CREATE TABLE IF NOT EXISTS arca_sends (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	purpose TEXT NOT NULL,
	target TEXT NOT NULL,
	asset TEXT NOT NULL,
	atoms INTEGER NOT NULL,
	state TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS arca_sends_user ON arca_sends(user_id, purpose);
`

const (
	arcaSendsDay = 3 // payments per network address per day
)

func (a *App) arcaOn() bool { return a.cfg.ArcaBin != "" && a.cfg.ArcaDatadir != "" }

// arca runs one arca command under the wallet's lock and decodes its JSON.
// A refusal comes back as an error carrying the wallet's own message.
func (a *App) arca(timeout time.Duration, out any, args ...string) error {
	a.arcaMu.Lock()
	defer a.arcaMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.cfg.ArcaBin, append([]string{"--datadir", a.cfg.ArcaDatadir}, args...)...)
	raw, err := cmd.Output()
	var refusal struct {
		Error *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &refusal) == nil && refusal.Error != nil {
		return fmt.Errorf("%s", refusal.Error.Message)
	}
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n")
			msg = lines[len(lines)-1]
		}
		return fmt.Errorf("arca %s: %v %s", args[0], err, msg)
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = strings.TrimSpace(string(raw))
		return nil
	}
	return json.Unmarshal(raw, out)
}

// requestDetails is what a receive request names.
type requestDetails struct {
	Kind       string `json:"kind"`
	Operator   string `json:"operator"`
	Owner      string `json:"owner"`
	OwnerNonce string `json:"owner_nonce"`
	Until      int64  `json:"until"`
	Asset      string `json:"asset"`
	Value      string `json:"value"`
}

// decodeRequest reads an arca:<hex of JSON> receive request.
func decodeRequest(req string) (*requestDetails, error) {
	h, ok := strings.CutPrefix(strings.TrimSpace(req), "arca:")
	if !ok {
		return nil, errors.New("not a receive request")
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, errors.New("not a receive request")
	}
	var d requestDetails
	if json.Unmarshal(b, &d) != nil || d.Kind != "request" || len(d.Owner) != 64 || len(d.OwnerNonce) != 64 {
		return nil, errors.New("not a receive request")
	}
	return &d, nil
}

// codeParts splits a receive code into its address, ark, sequentia and
// lightning parts (receivecode.go). A bare receive request is its sequentia
// part, and anything else its address.
func codeParts(code string) (addr, ark, req, invoice string) {
	code = strings.TrimSpace(code)
	if c, ok := readReceiveCode(code); ok {
		return c.Address, c.Ark, c.Sequentia, c.Lightning
	}
	if strings.HasPrefix(code, "arca:") || strings.HasPrefix(strings.ToLower(code), "sqr1") {
		return "", "", code, ""
	}
	return code, "", "", ""
}

// newCode makes an account's receive code for one purpose: a fresh receive
// request and, when the Bitcoin side is set up, a Bitcoin ark address of
// its own. A request may name the asset and amount it asks for.
func (a *App) newCode(userID int64, purpose, asset string, atoms int64) (string, error) {
	args := []string{"receive"}
	if asset != "" {
		args = append(args, "--asset", asset, "--amount", strconv.FormatInt(atoms, 10))
	}
	var r struct {
		Request string         `json:"request"`
		Details requestDetails `json:"details"`
	}
	if err := a.arca(2*time.Minute, &r, args...); err != nil {
		return "", err
	}
	var ark string
	if purpose == "btc" {
		if err := a.arca(2*time.Minute, &ark, "bitcoin", "--quiet", "address"); err != nil {
			return "", err
		}
	}
	_, err := a.db.Exec(`INSERT INTO arca_codes (user_id, purpose, request, owner, owner_nonce, ark_address, want_asset, want_atoms, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, userID, purpose, r.Request, r.Details.Owner, r.Details.OwnerNonce, ark, asset, atoms, now())
	return r.Request, err
}

type arcaCode struct {
	ID                int64
	Purpose, Request  string
	Owner, OwnerNonce string
	Ark               string
	WantAsset         string
	WantAtoms         int64
	PaidAsset         string
	PaidAtoms, PaidAt int64
	BtcSats           int64
	CreatedAt         int64
}

func (a *App) codeOf(userID int64, purpose string) *arcaCode {
	var c arcaCode
	err := a.db.QueryRow(`SELECT id, purpose, request, owner, owner_nonce, ark_address, want_asset, want_atoms, paid_asset, paid_atoms, paid_at, btc_sats, created_at
		FROM arca_codes WHERE user_id = ? AND purpose = ? ORDER BY id DESC LIMIT 1`, userID, purpose).
		Scan(&c.ID, &c.Purpose, &c.Request, &c.Owner, &c.OwnerNonce, &c.Ark, &c.WantAsset, &c.WantAtoms, &c.PaidAsset, &c.PaidAtoms, &c.PaidAt, &c.BtcSats, &c.CreatedAt)
	if err != nil {
		return nil
	}
	return &c
}

// codesOf lists the account's codes for one purpose made at or after
// since, newest first.
func (a *App) codesOf(userID int64, purpose string, since int64) []*arcaCode {
	rows, err := a.db.Query(`SELECT id, purpose, request, owner, owner_nonce, ark_address, want_asset, want_atoms, paid_asset, paid_atoms, paid_at, btc_sats, created_at
		FROM arca_codes WHERE user_id = ? AND purpose = ? AND created_at >= ? ORDER BY id DESC`, userID, purpose, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*arcaCode
	for rows.Next() {
		var c arcaCode
		if rows.Scan(&c.ID, &c.Purpose, &c.Request, &c.Owner, &c.OwnerNonce, &c.Ark, &c.WantAsset, &c.WantAtoms, &c.PaidAsset, &c.PaidAtoms, &c.PaidAt, &c.BtcSats, &c.CreatedAt) == nil {
			out = append(out, &c)
		}
	}
	return out
}

// Which code counts. Every code is the account's own: its owner key was made
// for that account and task alone, so a payment to any of them shows the
// account's wallet paid Emissio, and the newest is only the one the page
// shows. A tester who paid a code and then asked for a new one (because the
// page had not seen the payment yet, or the wallet would not pay the old
// one) keeps the credit. For the round trip the return is of the newest
// payment Emissio made, so the codes made since that payment count.
const (
	codeRenewAfter = 10 * 60 // seconds an unpaid code is kept before a new one can be had
	codesPerDay    = 5       // codes per account and task per day
)

// renewable says whether an unpaid code can be replaced now, or from when.
func (a *App) renewable(userID int64, purpose string, c *arcaCode) (ok bool, from int64) {
	if c == nil {
		return true, 0
	}
	var today int64
	a.db.QueryRow("SELECT COUNT(*) FROM arca_codes WHERE user_id = ? AND purpose = ? AND created_at > ?", userID, purpose, now()-86400).Scan(&today)
	if today >= codesPerDay {
		var oldest int64
		a.db.QueryRow("SELECT MIN(created_at) FROM (SELECT created_at FROM arca_codes WHERE user_id = ? AND purpose = ? AND created_at > ? ORDER BY created_at DESC LIMIT ?)",
			userID, purpose, now()-86400, codesPerDay).Scan(&oldest)
		return false, oldest + 86400
	}
	if c.PaidAt == 0 && c.BtcSats == 0 && now()-c.CreatedAt < codeRenewAfter {
		return false, c.CreatedAt + codeRenewAfter
	}
	return true, 0
}

// receiveCode is the code shown to the account: Emissio's on-chain address
// first, as any Bitcoin wallet reads it, then the ark and Sequentia parts.
func (a *App) receiveCode(c *arcaCode) string {
	addr := a.arcaAddress()
	q := url.Values{}
	if c.Ark != "" {
		q.Set("ark", c.Ark)
	}
	q.Set("sequentia", c.Request)
	return "bitcoin:" + addr + "?" + q.Encode()
}

// ---------- the follower ----------

// followArca syncs the wallet and attributes what arrived.
func (a *App) followArca() time.Duration {
	var s struct {
		Mailbox struct {
			Accepted []struct {
				LeafID string `json:"leaf_id"`
			} `json:"accepted"`
		} `json:"mailbox"`
		Schedule struct {
			Now        int64  `json:"now"`
			NextSyncAt *int64 `json:"next_sync_at"`
		} `json:"schedule"`
	}
	wait := time.Minute
	var first []string
	if err := a.arca(10*time.Minute, &s, "sync"); err != nil {
		// What another command accepted is reconciled all the same.
		log.Printf("arca: sync: %v", err)
	} else {
		for _, c := range s.Mailbox.Accepted {
			first = append(first, c.LeafID)
		}
	}
	a.reconcileCoins(first)
	a.followArk()
	a.followInvoices()
	if s.Schedule.NextSyncAt != nil {
		if d := time.Duration(*s.Schedule.NextSyncAt-s.Schedule.Now) * time.Second; d > wait {
			wait = d
		}
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute // a tester waits on the credit
	}
	return wait
}

// The reconciliation. Whichever command accepted a coin (the follower's own
// sync, a send's, a sync run by hand) and whenever, the coin is in the
// wallet's list of every coin it holds or held. A follow reads that list
// (`arca coins`), and each coin not read before has its record read (`arca
// record`) and kept in arca_coins with the code it pays: the code whose
// owner key and nonce are those of the coin's own leaf, read from the record
// field by field (arcarecord.go), never found by searching it, since a
// transfer's record carries every coin of its history ahead of its own leaf.
// A code takes the first coin that pays it, and a coin pays one code, once
// (arca_codes_paid_leaf). The coin's read and its code's payment are one
// write. A record the wallet does not give, or that this reader cannot
// read, is asked for again on later follows for as long as it takes, less
// often as its failures grow, and the desk names it meanwhile.
//
// Per follow: one listing, and at most reconcileBatch records, those the
// follow's own sync accepted first, then reconcileOldest of the oldest
// unread and the newest for the rest, so a payment is read in the follow
// it arrives in and no backlog keeps an older one waiting. The listing is
// the wallet's whole history, with work per coin in the wallet, so a follow
// with no code open, nothing unread and nothing accepted lists it at most
// once an hour.
const arcaCoinSchema = `
CREATE TABLE IF NOT EXISTS arca_coins (
	leaf_id TEXT PRIMARY KEY,
	code_id INTEGER NOT NULL DEFAULT 0,
	counted INTEGER NOT NULL DEFAULT 0,
	asset TEXT NOT NULL DEFAULT '',
	atoms INTEGER NOT NULL DEFAULT 0,
	tries INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	next_at INTEGER NOT NULL DEFAULT 0,
	read_at INTEGER NOT NULL DEFAULT 0,
	seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS arca_coins_code ON arca_coins(code_id) WHERE code_id != 0;
CREATE INDEX IF NOT EXISTS arca_coins_unread ON arca_coins(read_at) WHERE read_at = 0;
`

const (
	reconcileBatch  = 50         // records read per follow
	reconcileOldest = 10         // of them, the oldest unread
	recordBackoff   = 15 * 60    // seconds, the longest wait before a failed record is asked for again
	codeLife        = 30 * 86400 // seconds a code is followed as open (a code works for 20 days)
	quietListing    = 3600       // seconds between listings when nothing can be owed
)

// recordWait is how long to wait after a coin's record failed n times: a
// minute, doubling up to recordBackoff.
func recordWait(n int) int64 {
	return min(int64(60)<<min(max(n-1, 0), 20), recordBackoff)
}

// owedNow says whether a coin may be owed to a code: a code open, a coin
// unread at the last listing, or a coin this follow accepted.
func (a *App) owedNow(first []string) bool {
	if _, unread := a.reconciled(); len(first) > 0 || unread > 0 {
		return true
	}
	var n int64
	a.db.QueryRow(`SELECT (SELECT COUNT(*) FROM arca_coins WHERE read_at = 0)
		+ (SELECT COUNT(*) FROM arca_codes WHERE paid_at = 0 AND created_at > ?)`, now()-codeLife).Scan(&n)
	return n > 0
}

// reconcileCoins matches the wallet's coins not read before to the codes
// they pay. first are the coins this follow's own sync accepted.
func (a *App) reconcileCoins(first []string) {
	var listed int64
	a.db.QueryRow("SELECT CAST(value AS INTEGER) FROM kv WHERE key = 'arca_listed'").Scan(&listed)
	if !a.owedNow(first) && now()-listed < quietListing {
		return
	}
	var coins []struct {
		LeafID string `json:"leaf_id"`
		Asset  string `json:"asset"`
		Value  string `json:"value"`
	}
	if err := a.arca(5*time.Minute, &coins, "coins"); err != nil {
		log.Printf("arca: coins: %v", err)
		return
	}
	a.db.Exec("INSERT INTO kv (key, value) VALUES ('arca_listed', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", strconv.FormatInt(now(), 10))
	type state struct {
		read   bool
		tries  int
		nextAt int64
	}
	known := map[string]state{}
	rows, err := a.db.Query("SELECT leaf_id, read_at, tries, next_at FROM arca_coins")
	if err != nil {
		return
	}
	for rows.Next() {
		var leaf string
		var readAt, nextAt int64
		var n int
		rows.Scan(&leaf, &readAt, &n, &nextAt)
		known[leaf] = state{readAt != 0, n, nextAt}
	}
	rows.Close()
	type coin struct{ leaf, asset, value string }
	var unread []coin // oldest first, as the wallet lists them
	inList := map[string]bool{}
	for _, c := range coins {
		if c.LeafID == "" || inList[c.LeafID] || known[c.LeafID].read {
			continue
		}
		inList[c.LeafID] = true
		unread = append(unread, coin{c.LeafID, c.Asset, c.Value})
	}
	due := func(c coin) bool { return known[c.leaf].nextAt <= now() }
	var batch []coin
	taken := map[string]bool{}
	add := func(c coin) {
		if len(batch) < reconcileBatch && !taken[c.leaf] && due(c) {
			taken[c.leaf] = true
			batch = append(batch, c)
		}
	}
	byLeaf := map[string]coin{}
	for _, c := range unread {
		byLeaf[c.leaf] = c
	}
	for _, l := range first {
		if len(batch) >= reconcileBatch-reconcileOldest {
			break // the oldest keep their places whatever the sync accepted
		}
		if c, ok := byLeaf[l]; ok {
			add(c)
		}
	}
	for i, n := 0, 0; i < len(unread) && n < reconcileOldest; i++ {
		if !taken[unread[i].leaf] && due(unread[i]) {
			add(unread[i])
			n++
		}
	}
	for i := len(unread) - 1; i >= 0; i-- {
		add(unread[i])
	}
	left := len(unread)
	for _, c := range batch {
		atoms, _ := strconv.ParseInt(c.value, 10, 64)
		a.db.Exec("INSERT OR IGNORE INTO arca_coins (leaf_id, asset, atoms, seen_at) VALUES (?,?,?,?)", c.leaf, c.asset, atoms, now())
		var rec struct {
			Record string `json:"record"`
		}
		err := a.arca(time.Minute, &rec, "record", c.leaf)
		var raw []byte
		if err == nil {
			if raw, err = hex.DecodeString(rec.Record); err != nil {
				err = errors.New("the wallet gave a record that is not hex")
			}
		}
		if err == nil {
			var own recLeaf
			var named bool
			if own, named, err = ownLeaf(raw); err == nil {
				if err = a.attribute(c.leaf, c.asset, atoms, own, named, func(l string) bool { return inList[l] }); err == nil {
					left--
					continue
				}
			} else {
				err = errors.New("a record in a format this Emissio does not read: update Emissio")
			}
		}
		n := known[c.leaf].tries + 1
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		a.db.Exec("UPDATE arca_coins SET tries = ?, error = ?, next_at = ? WHERE leaf_id = ? AND read_at = 0", n, msg, now()+recordWait(n), c.leaf)
		if n&(n-1) == 0 { // the 1st, 2nd, 4th, 8th... failure
			log.Printf("arca: coin %s not read (%d times so far, asked for again later): %s", c.leaf, n, msg)
		}
	}
	a.db.Exec("INSERT INTO kv (key, value) VALUES ('arca_reconciled', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		fmt.Sprintf("%d %d", now(), left))
}

// reconciled says when the wallet's coins were last read, and how many it
// had then whose records were still to read.
func (a *App) reconciled() (at int64, unread int64) {
	var v string
	a.db.QueryRow("SELECT value FROM kv WHERE key = 'arca_reconciled'").Scan(&v)
	fmt.Sscanf(v, "%d %d", &at, &unread)
	return at, unread
}

// attribute keeps, in one write, that a coin's record was read and the code
// its own leaf pays, if any. A code takes its first coin only. Two cases
// move a code's record of its payment, and only its record: the code stays
// paid as it was, so nothing is un-paid and nothing is credited, here or
// again (a credit is a ledger row of the submission, made once).
//   - A code whose recorded coin is not a coin read from the wallet whose own
//     leaf pays it (a code marked paid by hand, or by an earlier matcher that
//     searched the record) takes the coin that does pay it, unless its
//     recorded coin is still listed by the wallet and unread
//     (unreadListed): that coin may yet prove to be its payment, and a
//     second payment read first must not take its place.
//   - A coin that a paid code records as its payment, whose own leaf pays
//     another code or none, is released from that code: the code's record
//     moves to a coin read from the wallet that does pay it, or to none, and
//     the coin pays the code its own leaf names, in the same write, so the
//     one-coin-one-code guard never holds it unread.
func (a *App) attribute(leaf, asset string, atoms int64, own recLeaf, named bool, unreadListed func(string) bool) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if named && own.Owner != "" {
		tx.QueryRow("SELECT id FROM arca_codes WHERE lower(owner) = ? AND lower(owner_nonce) = ? ORDER BY id LIMIT 1", own.Owner, own.Nonce).Scan(&id)
	}
	var released []string
	rows, err := tx.Query("SELECT id FROM arca_codes WHERE paid_leaf = ? AND id != ?", leaf, id)
	if err != nil {
		return err
	}
	var holders []int64
	for rows.Next() {
		var h int64
		rows.Scan(&h)
		holders = append(holders, h)
	}
	rows.Close()
	for _, h := range holders {
		// The record moves with the coin's own amount and asset, or, to no
		// coin, with none: never the amount of a coin that pays another code.
		var alt, altAsset string
		var altAtoms int64
		tx.QueryRow("SELECT leaf_id, asset, atoms FROM arca_coins WHERE code_id = ? AND read_at != 0 AND leaf_id != ? ORDER BY read_at, leaf_id LIMIT 1", h, leaf).Scan(&alt, &altAsset, &altAtoms)
		if _, err := tx.Exec("UPDATE arca_codes SET paid_leaf = ?, paid_asset = ?, paid_atoms = ? WHERE id = ?", alt, altAsset, altAtoms, h); err != nil {
			return err
		}
		if alt != "" {
			if _, err := tx.Exec("UPDATE arca_coins SET counted = 1 WHERE leaf_id = ?", alt); err != nil {
				return err
			}
		}
		released = append(released, fmt.Sprintf("code %d (its record now names coin %q)", h, alt))
	}
	counted := false
	if id != 0 {
		res, err := tx.Exec("UPDATE arca_codes SET paid_leaf = ?, paid_asset = ?, paid_atoms = ?, paid_at = ? WHERE id = ? AND paid_at = 0",
			leaf, asset, atoms, now(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			counted = true
		} else {
			var paidLeaf string
			var genuine int
			tx.QueryRow("SELECT paid_leaf FROM arca_codes WHERE id = ?", id).Scan(&paidLeaf)
			tx.QueryRow("SELECT COUNT(*) FROM arca_coins WHERE leaf_id = ? AND code_id = ? AND read_at != 0", paidLeaf, id).Scan(&genuine)
			switch {
			case paidLeaf == leaf:
				counted = true
			case genuine == 0 && !(paidLeaf != "" && unreadListed(paidLeaf)):
				if _, err := tx.Exec("UPDATE arca_codes SET paid_leaf = ?, paid_asset = ?, paid_atoms = ? WHERE id = ?", leaf, asset, atoms, id); err != nil {
					return err
				}
				if paidLeaf != "" {
					if _, err := tx.Exec("UPDATE arca_coins SET counted = 0 WHERE leaf_id = ?", paidLeaf); err != nil {
						return err
					}
				}
				counted = true
			}
		}
	}
	if _, err := tx.Exec("UPDATE arca_coins SET code_id = ?, counted = ?, read_at = ?, error = '', next_at = 0 WHERE leaf_id = ?",
		id, counted, now(), leaf); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, r := range released {
		log.Printf("arca: coin %s was recorded as the payment of %s, but pays code %d: that code stays paid", leaf, r, id)
	}
	switch {
	case id == 0 && named:
		log.Printf("arca: coin %s (%d atoms of %s) paid no code", leaf, atoms, short(asset))
	case id != 0 && !counted:
		log.Printf("arca: coin %s paid code %d, which another coin paid or may pay: not counted", leaf, id)
	}
	return nil
}

// followArk reads what arrived at each account's Bitcoin ark address.
func (a *App) followArk() {
	rows, err := a.db.Query("SELECT id, ark_address FROM arca_codes WHERE ark_address != '' AND btc_sats = 0")
	if err != nil {
		return
	}
	type open struct {
		id   int64
		addr string
	}
	var opens []open
	for rows.Next() {
		var o open
		rows.Scan(&o.id, &o.addr)
		opens = append(opens, o)
	}
	rows.Close()
	if len(opens) == 0 {
		return
	}
	// history syncs Bark; the lookups then read what it recorded.
	var hist []json.RawMessage
	if err := a.arca(5*time.Minute, &hist, "bitcoin", "--quiet", "history"); err != nil {
		log.Printf("arca: bitcoin history: %v", err)
		return
	}
	for _, o := range opens {
		var moves []struct {
			Status     string `json:"status"`
			ReceivedOn []struct {
				Destination struct {
					Value string `json:"value"`
				} `json:"destination"`
				AmountSat int64 `json:"amount_sat"`
			} `json:"received_on"`
		}
		if err := a.arca(time.Minute, &moves, "bitcoin", "--quiet", "address", "lookup", "--address", o.addr); err != nil {
			continue
		}
		var sats int64
		for _, m := range moves {
			if m.Status != "successful" {
				continue
			}
			for _, r := range m.ReceivedOn {
				if r.Destination.Value == o.addr {
					sats += r.AmountSat
				}
			}
		}
		if sats > 0 {
			a.db.Exec("UPDATE arca_codes SET btc_sats = ? WHERE id = ?", sats, o.id)
		}
	}
}

// followInvoices brings the state of each invoice payment in a Sequentia
// asset still under way up to date: the wallet's own record of it, which
// `sync` moves on. A payment the receiving wallet has not taken yet answers
// `paying` when it is made, and the invoice task's check reads the state
// recorded here.
func (a *App) followInvoices() {
	rows, err := a.db.Query(`SELECT id, ref FROM arca_sends WHERE purpose = 'invoice' AND asset != 'btc' AND ref != ''
		AND state NOT IN ('paid', 'returned', 'failed') AND created_at > ?`, now()-7*24*3600)
	if err != nil {
		return
	}
	type open struct {
		id   int64
		hash string
	}
	var opens []open
	for rows.Next() {
		var o open
		rows.Scan(&o.id, &o.hash)
		opens = append(opens, o)
	}
	rows.Close()
	if len(opens) == 0 {
		return
	}
	var pays map[string]struct {
		State string `json:"state"`
	}
	if err := a.arca(time.Minute, &pays, "lightning", "payments"); err != nil {
		log.Printf("arca: lightning payments: %v", err)
		return
	}
	for _, o := range opens {
		if p, ok := pays[o.hash]; ok && p.State != "" {
			a.db.Exec("UPDATE arca_sends SET state = ? WHERE id = ?", p.State, o.id)
		}
	}
}

func (a *App) startArcaFollower() {
	if !a.arcaOn() {
		return
	}
	go func() {
		for {
			time.Sleep(a.followArca())
		}
	}()
	go func() { // the invoice limits a task page shows (invoicelimit.go)
		for {
			a.invoiceCaps()
			time.Sleep(capsFresh)
		}
	}()
}

// ---------- invoices ----------

var bolt11Re = regexp.MustCompile(`(?i)^ln([a-z]+?)(\d+)([munp]?)1[02-9ac-hj-np-z]+$`)

// invoiceAmount reads the amount an invoice names, in sats (atoms for an
// asset invoice), from its human-readable part. An invoice naming no
// amount is refused: Emissio pays only a known amount.
func invoiceAmount(inv string) (int64, error) {
	m := bolt11Re.FindStringSubmatch(strings.TrimSpace(inv))
	if m == nil {
		return 0, errors.New("not an invoice that names an amount")
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("not an invoice that names an amount")
	}
	// amount in whole coins times the multiplier, in units of 1e-8
	switch strings.ToLower(m[3]) {
	case "":
		return n * 100_000_000, nil
	case "m":
		return n * 100_000, nil
	case "u":
		return n * 100, nil
	case "n":
		if n%10 != 0 {
			return 0, errors.New("an invoice for a fraction of a sat")
		}
		return n / 10, nil
	case "p":
		if n%10_000 != 0 {
			return 0, errors.New("an invoice for a fraction of a sat")
		}
		return n / 10_000, nil
	}
	return 0, errors.New("not an invoice that names an amount")
}

// ---------- the request handler ----------

// handleArcaRequest serves the web wallet tasks' buttons: make the
// account's receive code, pay the account's receive code, or pay its
// invoice.
func (a *App) handleArcaRequest(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	task, err := taskBySlug(a.db, r.PathValue("slug"))
	if err != nil || !isArcaTask(task.CheckKind) {
		http.NotFound(w, r)
		return
	}
	back := "/tasks/" + task.Slug
	if !a.gate(w, r, user, back) {
		return
	}
	if !task.Active || (task.Cap > 0 && task.Awarded >= task.Cap) {
		a.redirect(w, r, back, "", "This task is closed.")
		return
	}
	if !a.arcaOn() {
		a.redirect(w, r, back, "", "Emissio's wallet is not taking part on this site right now.")
		return
	}
	if walletLinkOf(a.db, user.ID) == nil {
		a.redirect(w, r, back, "", "Link your web wallet first (the Link your web wallet task).")
		return
	}
	var done int64
	a.db.QueryRow("SELECT COUNT(*) FROM submissions WHERE user_id = ? AND task_id = ? AND status = 'approved'", user.ID, task.ID).Scan(&done)
	if done > 0 {
		a.redirect(w, r, back, "", "You have completed this task.")
		return
	}
	purpose := arcaPurpose(task.CheckKind)
	// One request of an account's task at a time, from its limits' check to
	// the code or payment it makes; and one paying request of a network
	// address at a time, for the address's daily limit.
	defer a.claims.lock(fmt.Sprintf("arca:%d:%s", user.ID, purpose))()
	switch task.CheckKind {
	case "arca-pay-code", "arca-pay-btc", "arca-swap":
		if ok, from := a.renewable(user.ID, purpose, a.codeOf(user.ID, purpose)); !ok {
			a.redirect(w, r, back, "", "Your receive code from Emissio is below. A new one can be had from "+time.Unix(from, 0).UTC().Format("15:04 UTC")+"; a payment to any of your codes for this task counts.")
			return
		}
		if _, err := a.newCode(user.ID, purpose, "", 0); err != nil {
			log.Printf("arca: code for user %d: %v", user.ID, err)
			a.redirect(w, r, back, "", "Emissio could not make a receive code just now. Try again later.")
			return
		}
		a.redirect(w, r, back, "Your receive code from Emissio is below.", "")
	case "arca-roundtrip", "arca-invoice", "arca-name":
		if r.FormValue("renew") == "1" && task.CheckKind != "arca-invoice" {
			a.renewReturnCode(w, r, user, back, purpose)
			return
		}
		// The address's limit is checked and the payment's place taken under
		// the address's lock, which is released before the wallet calls: those
		// queue behind the follower's sync, and must not hold up the next
		// request from the same address. Every place counts, however long its
		// request waits behind the wallet. One whose request was never begun
		// is given back by that request, or, if a stop ended it, by the next
		// start of the service (clearReservations); one whose payment was
		// begun (marked `begun`, then filled in) is never removed.
		unlockIP := a.claims.lock("arca-ip:" + clientIP(r))
		var mine, ipToday int64
		a.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE user_id = ? AND purpose = ?", user.ID, purpose).Scan(&mine)
		a.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE ip = ? AND created_at > ?", clientIP(r), now()-86400).Scan(&ipToday)
		if mine >= paymentsPerTask {
			unlockIP()
			a.redirect(w, r, back, "", fmt.Sprintf("Emissio has paid you %d times for this task, the most it pays one account.", paymentsPerTask))
			return
		}
		if ipToday >= arcaSendsDay {
			unlockIP()
			a.redirect(w, r, back, "", "Emissio has paid enough accounts from your network today. Try again tomorrow.")
			return
		}
		res, err := a.db.Exec("INSERT INTO arca_sends (user_id, purpose, target, asset, atoms, state, ref, ip, created_at) VALUES (?, ?, '', '', 0, 'reserved', '', ?, ?)",
			user.ID, purpose, clientIP(r), now())
		unlockIP()
		if err != nil {
			a.serverError(w, err)
			return
		}
		id, _ := res.LastInsertId()
		slot := &paySlot{id: id, userID: user.ID, purpose: purpose, ip: clientIP(r)}
		// Given back only while the payment was never begun: a `begun` row
		// is never removed.
		defer a.db.Exec("DELETE FROM arca_sends WHERE id = ? AND state = 'reserved'", slot.id)
		switch task.CheckKind {
		case "arca-roundtrip":
			a.payCode(w, r, user, back, purpose, r.FormValue("code"), "Emissio has paid your receive code. Send back what arrived, to Emissio's code below.", slot)
		case "arca-name":
			a.payName(w, r, user, back, purpose, slot)
		default:
			a.payInvoice(w, r, user, back, purpose, slot)
		}
	}
}

// renewReturnCode makes a new code to return Emissio's newest payment to,
// for a wallet that will not pay the one shown. No new payment is made.
func (a *App) renewReturnCode(w http.ResponseWriter, r *http.Request, user *User, back, purpose string) {
	var sentAt int64
	if a.db.QueryRow("SELECT created_at FROM arca_sends WHERE user_id = ? AND purpose = ? AND state NOT IN ('reserved', 'begun') ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&sentAt) != nil {
		a.redirect(w, r, back, "", "Emissio has not paid you yet for this task.")
		return
	}
	if ok, from := a.renewable(user.ID, purpose, a.codeOf(user.ID, purpose)); !ok {
		a.redirect(w, r, back, "", "A new code can be had from "+time.Unix(from, 0).UTC().Format("15:04 UTC")+"; a return to any of your codes since Emissio's payment counts.")
		return
	}
	if _, err := a.newCode(user.ID, purpose, "", 0); err != nil {
		log.Printf("arca: return code for user %d: %v", user.ID, err)
		a.redirect(w, r, back, "", "Emissio could not make a code just now. Try again later.")
		return
	}
	a.redirect(w, r, back, "Your new code is below. Send back what arrived to it; a return to the earlier one still counts.", "")
}

// paySlot is the place a paying request took in arca_sends.
type paySlot struct {
	id      int64
	userID  int64
	purpose string
	ip      string
}

// clearReservations removes the payment places that requests a stop ended
// left behind before they began a payment. Only the process about to serve
// calls it, immediately before it listens: one process serves a database,
// so nothing can still hold one of them. A one-shot command (createadmin,
// reseed-tasks) runs beside the serving process and never does, as a request
// in flight there holds its place. Only `reserved` rows: a payment begun
// (`begun`) is never removed.
func clearReservations(db *sql.DB) {
	if res, err := db.Exec("DELETE FROM arca_sends WHERE state = 'reserved'"); err != nil {
		log.Printf("arca: reserved payment places not cleared: %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("arca: %d payment places left by a stop cleared", n)
	}
}

// beginPayment records the intent before the act: the request's place is
// moved to `begun` by a checked write before the first wallet call that
// can pay, and the wallet is not asked if that write did not land. From
// then on nothing removes the row: it counts toward both limits, the
// readers of the newest payment skip it, and while it is not filled in
// (the wallet's answer lost, a stop, a write that failed) the desk shows it
// as a payment to reconcile by hand.
func (a *App) beginPayment(slot *paySlot) error {
	res, err := a.db.Exec("UPDATE arca_sends SET state = 'begun', created_at = ? WHERE id = ? AND state = 'reserved'", now(), slot.id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("the payment's place is gone")
	}
	return nil
}

// fillSlot records a payment Emissio made in its `begun` row. If the row
// cannot be written, it stays `begun`, counted and shown on the desk, and
// the log line carries the row's id and what was paid; if it is gone,
// whatever removed it, the payment is written anew.
func (a *App) fillSlot(slot *paySlot, target, asset string, atoms int64, state, ref string) {
	what := fmt.Sprintf("%d atoms of %s, state %s, ref %s", atoms, asset, state, ref)
	res, err := a.db.Exec("UPDATE arca_sends SET target = ?, asset = ?, atoms = ?, state = ?, ref = ?, created_at = ? WHERE id = ? AND state = 'begun'",
		target, asset, atoms, state, ref, now(), slot.id)
	if err != nil {
		log.Printf("arca: payment %d made but not recorded (%v): %s; its row stays begun, to reconcile by hand", slot.id, err, what)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return
	}
	log.Printf("arca: payment %d made, and its row was gone: %s; recording it anew", slot.id, what)
	a.recordAnew(slot, target, asset, atoms, state, ref, what)
}

// recordAnew writes a payment the wallet made in a new row, when its own row
// is gone. When even that fails, the log line is its record.
func (a *App) recordAnew(slot *paySlot, target, asset string, atoms int64, state, ref, what string) {
	res, err := a.db.Exec("INSERT INTO arca_sends (user_id, purpose, target, asset, atoms, state, ref, ip, created_at) VALUES (?,?,?,?,?,?,?,?,?)",
		slot.userID, slot.purpose, target, asset, atoms, state, ref, slot.ip, now())
	if err != nil {
		log.Printf("arca: payment %d not recorded anywhere (%v): account %d, %s, %s: reconcile it by hand", slot.id, err, slot.userID, slot.purpose, what)
		return
	}
	id, _ := res.LastInsertId()
	log.Printf("arca: payment %d recorded anew as row %d (%s)", slot.id, id, state)
}

func isArcaTask(kind string) bool {
	switch kind {
	case "arca-pay-code", "arca-pay-btc", "arca-roundtrip", "arca-invoice", "arca-swap", "arca-name":
		return true
	}
	return false
}

func arcaPurpose(kind string) string {
	switch kind {
	case "arca-pay-btc":
		return "btc"
	case "arca-pay-code":
		return "code"
	}
	return strings.TrimPrefix(kind, "arca-")
}

// payCode pays a receive code of the account's wallet a random amount of an
// asset Emissio holds, and makes the code the account returns it to.
func (a *App) payCode(w http.ResponseWriter, r *http.Request, user *User, back, purpose, code, done string, slot *paySlot) {
	_, _, req, _ := codeParts(code)
	if req == "" {
		a.redirect(w, r, back, "", "Paste the receive code your web wallet shows under Receive.")
		return
	}
	op, genesis := a.arcaChain()
	req, err := expandRequest(req, op, genesis)
	if err != nil {
		a.redirect(w, r, back, "", "That receive code cannot be paid here: "+err.Error()+".")
		return
	}
	d, err := decodeRequest(req)
	if err != nil {
		a.redirect(w, r, back, "", "Paste the receive code your web wallet shows under Receive.")
		return
	}
	if op := a.arcaOperator(); op != "" && !strings.EqualFold(op, d.Operator) {
		a.redirect(w, r, back, "", "That receive code is for another server than Emissio's.")
		return
	}
	// Emissio never pays itself: a code of its own, paid, would credit the
	// account with Emissio's own coin.
	var mine int64
	a.db.QueryRow("SELECT COUNT(*) FROM arca_codes WHERE lower(owner) = ?", strings.ToLower(d.Owner)).Scan(&mine)
	if mine > 0 {
		a.redirect(w, r, back, "", "That is one of Emissio's own receive codes. Paste the receive code your web wallet shows under Receive.")
		return
	}
	if d.Until != 0 && d.Until < time.Now().Unix()+3600 {
		a.redirect(w, r, back, "", "That receive code has lapsed: show a new one in your wallet and paste it.")
		return
	}
	asset, atoms, err := a.arcaPick()
	if err != nil {
		log.Printf("arca: pick for user %d: %v", user.ID, err)
		a.redirect(w, r, back, "", "Emissio could not pay just now. Try again later.")
		return
	}
	var sent struct {
		Transfer struct {
			TransferID string `json:"transfer_id"`
		} `json:"transfer"`
	}
	if err := a.beginPayment(slot); err != nil {
		log.Printf("arca: payment %d not begun: %v", slot.id, err)
		a.redirect(w, r, back, "", "Emissio could not pay just now. Try again later.")
		return
	}
	if err := a.arca(3*time.Minute, &sent, "send", req, "--amount", strconv.FormatInt(atoms, 10), "--asset", asset, "--route", "leaf"); err != nil {
		log.Printf("arca: send to user %d (payment %d, which stays begun, to reconcile by hand): %v", user.ID, slot.id, err)
		a.redirect(w, r, back, "", "Emissio could not pay that code: "+err.Error())
		return
	}
	a.fillSlot(slot, d.Owner, asset, atoms, "sent", sent.Transfer.TransferID)
	if _, err := a.newCode(user.ID, purpose, "", 0); err != nil {
		log.Printf("arca: return code for user %d: %v", user.ID, err)
	}
	a.redirect(w, r, back, done, "")
}

// wordsOnly is a wallet's refusal without the record it may carry after
// its sentence (`...: {"amount":...}`), which says nothing to a person.
func wordsOnly(msg string) string {
	if i := strings.Index(msg, ": {"); i > 0 {
		return msg[:i] + "."
	}
	return msg
}

// addressLike is whether text looks like a bare address of either chain,
// transparent or confidential, rather than a receive code or an invoice.
func addressLike(text string) bool {
	t := strings.ToLower(text)
	for _, p := range []string{"tb1", "bc1", "bcrt1", "tsqb1", "sqb1", "tex1", "lq1", "el1"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// nameRe is what a payment name may be (the web wallet's lnaddr.js).
var nameRe = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// payName pays the account's wallet at its payment name: the name service
// gives the receive code the wallet makes while it is open, and Emissio pays
// it as it pays a pasted code. Only the wallet holding the name sees what
// arrived, which is what the account sends back.
func (a *App) payName(w http.ResponseWriter, r *http.Request, user *User, back, purpose string, slot *paySlot) {
	if a.cfg.LnaddrURL == "" {
		a.redirect(w, r, back, "", "Payment names are not taking part on this site right now.")
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("code")))
	if i := strings.LastIndex(name, "@"); i >= 0 {
		if dom := name[i+1:]; a.cfg.LnaddrDomain != "" && dom != a.cfg.LnaddrDomain {
			a.redirect(w, r, back, "", "Emissio pays names ending in @"+a.cfg.LnaddrDomain+".")
			return
		}
		name = name[:i]
	}
	if !nameRe.MatchString(name) {
		a.redirect(w, r, back, "", "A name has 3 to 32 characters: lowercase letters, digits, - and _.")
		return
	}
	code, err := a.resolveName(name)
	if err != nil {
		a.redirect(w, r, back, "", err.Error())
		return
	}
	a.payCode(w, r, user, back, purpose, code, "Emissio has paid you at "+name+"@"+a.cfg.LnaddrDomain+". Send back what arrived, to Emissio's code below.", slot)
}

// resolveName asks the name service for the receive code of the wallet
// holding a name. A wallet that is not open gives none Emissio can pay.
func (a *App) resolveName(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.LnaddrURL+"/lnaddr/"+name, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("lnaddr: %s: %v", name, err)
		return "", errors.New("The payment name service did not answer. Try again later.")
	}
	defer resp.Body.Close()
	var out struct {
		Code   string `json:"code"`
		Fresh  bool   `json:"fresh"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", errors.New("The payment name service did not answer. Try again later.")
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || (resp.StatusCode != http.StatusOK && out.Reason != ""):
		if out.Reason == "" {
			out.Reason = "No wallet goes by that name."
		}
		return "", errors.New(out.Reason)
	case resp.StatusCode != http.StatusOK:
		return "", errors.New("The payment name service did not answer. Try again later.")
	case !out.Fresh || out.Code == "":
		return "", errors.New("Your web wallet is not open: open it, keep it open, and ask again.")
	}
	return out.Code, nil
}

// payInvoice pays an invoice the account's wallet made, in BTC or a
// Sequentia asset, up to a small amount (invoicelimit.go).
func (a *App) payInvoice(w http.ResponseWriter, r *http.Request, user *User, back, purpose string, slot *paySlot) {
	inv := strings.TrimSpace(r.FormValue("code"))
	_, _, _, li := codeParts(inv)
	switch {
	case li != "":
		inv = li
	case strings.HasPrefix(strings.ToLower(inv), "bitcoin:"):
		a.redirect(w, r, back, "", "That receive code carries no invoice yet. In the web wallet, after you type the amount, wait a few seconds for the code to be drawn again with its invoice (it then holds LIGHTNING=), and copy it then. If it never gets one, try a smaller amount or another asset.")
		return
	case addressLike(inv):
		a.redirect(w, r, back, "", "That is an address, not a receive code with an amount. This task pays an invoice, which an address, transparent or confidential, does not carry: in the web wallet, open Receive, type an amount, and copy the whole code.")
		return
	}
	amt, err := invoiceAmount(inv)
	if err != nil {
		a.redirect(w, r, back, "", "Paste an invoice for an amount: in the web wallet, Receive, an amount, then copy the code.")
		return
	}
	asset, err := invoiceAsset(inv)
	if err != nil {
		a.redirect(w, r, back, "", "Paste an invoice for an amount: in the web wallet, Receive, an amount, then copy the code.")
		return
	}
	if why := a.invoiceRefusal(asset, amt); why != "" {
		a.redirect(w, r, back, "", why)
		return
	}
	var out struct {
		Payment *struct {
			State       string `json:"state"`
			Preimage    string `json:"preimage"`
			PaymentHash string `json:"payment_hash"`
		} `json:"payment"`
		Paying struct {
			PaymentHash string `json:"payment_hash"`
		} `json:"paying"`
	}
	if err := a.beginPayment(slot); err != nil {
		log.Printf("arca: payment %d not begun: %v", slot.id, err)
		a.redirect(w, r, back, "", "Emissio could not pay just now. Try again later.")
		return
	}
	if err := a.arca(3*time.Minute, &out, "lightning", "pay", inv); err != nil {
		log.Printf("arca: invoice for user %d (payment %d, which stays begun, to reconcile by hand): %v", user.ID, slot.id, err)
		if strings.Contains(err.Error(), "is paying this invoice already") {
			a.redirect(w, r, back, "", "Emissio is already paying that invoice, or has paid it: asking again pays nothing more. Keep your web wallet open, its tab in front or behind, and the payment completes there. To be paid again, make a new receive code.")
			return
		}
		a.redirect(w, r, back, "", "Emissio could not pay that invoice: "+wordsOnly(err.Error()))
		return
	}
	state, hash := "unknown", out.Paying.PaymentHash
	if out.Payment != nil {
		state = out.Payment.State
		if hash == "" {
			hash = out.Payment.PaymentHash
		}
	}
	// The payment hash is the ref the follower reads a payment still under
	// way by (followInvoices).
	a.fillSlot(slot, inv, asset, amt, state, hash)
	if state != "paid" {
		a.redirect(w, r, back, "Emissio's payment is on its way. Keep your web wallet open: it completes when the wallet takes it, and Emissio sees that within a few minutes. Then submit the task.", "")
		return
	}
	a.redirect(w, r, back, "Emissio paid your invoice. Submit the task.", "")
}

// arcaAddress is the on-chain address of Emissio's arca wallet, made once
// and kept: the part of its receive codes any wallet reads.
func (a *App) arcaAddress() string {
	var addr string
	if a.db.QueryRow("SELECT value FROM kv WHERE key = 'arca_address'").Scan(&addr) == nil && addr != "" {
		return addr
	}
	var raw string
	if a.arca(time.Minute, &raw, "address") != nil {
		return ""
	}
	var obj struct {
		Address string `json:"address"`
	}
	if json.Unmarshal([]byte(raw), &obj) == nil && obj.Address != "" {
		addr = obj.Address
	} else {
		json.Unmarshal([]byte(raw), &addr)
		if addr == "" {
			addr = strings.Trim(raw, "\" \n")
		}
	}
	if addr != "" {
		a.db.Exec("INSERT INTO kv (key, value) VALUES ('arca_address', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", addr)
	}
	return addr
}

// arcaOperator is the operator key the wallet pinned.
func (a *App) arcaOperator() string {
	op, _ := a.arcaChain()
	return op
}

// arcaChain is the operator key and the chain's genesis hash of Emissio's
// wallet, or empty strings when the wallet does not answer.
func (a *App) arcaChain() (operator, genesis string) {
	var info struct {
		Operator string `json:"operator"`
		Genesis  string `json:"genesis_hash"`
	}
	if a.arca(time.Minute, &info, "info") != nil {
		return "", ""
	}
	return info.Operator, info.Genesis
}

// arcaPick chooses a random asset Emissio holds enough of, and a random
// amount of it.
func (a *App) arcaPick() (string, int64, error) {
	var bal struct {
		Arca map[string]map[string]string `json:"arca"`
	}
	if err := a.arca(2*time.Minute, &bal, "balance", "--without-bitcoin"); err != nil {
		return "", 0, err
	}
	var cands []string
	for asset, st := range bal.Arca {
		v, _ := strconv.ParseInt(st["live"], 10, 64)
		if v >= minPaymentAtoms+paymentAtomsRange+proofAtoms {
			cands = append(cands, asset)
		}
	}
	if len(cands) == 0 {
		return "", 0, errors.New("no asset with enough live coins")
	}
	return cands[randInt(int64(len(cands)))], minPaymentAtoms + randInt(paymentAtomsRange), nil
}

// ---------- the checks ----------

func (a *App) arcaCheck(kind string, user *User) checkResult {
	if !a.arcaOn() {
		return review("Emissio's wallet is not set up here; manual review")
	}
	purpose := arcaPurpose(kind)
	switch kind {
	case "arca-invoice":
		var state string
		a.db.QueryRow("SELECT state FROM arca_sends WHERE user_id = ? AND purpose = ? AND state NOT IN ('reserved', 'begun') ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&state)
		switch state {
		case "":
			return fail("Emissio has not paid an invoice of yours yet: paste one above")
		case "paid":
			return pass("Emissio paid this account's invoice (OK)")
		}
		return wait("Emissio's payment of your invoice is %s, not yet paid: keep your wallet open", state)
	case "arca-roundtrip", "arca-name":
		var asset string
		var atoms, sentAt int64
		if a.db.QueryRow("SELECT asset, atoms, created_at FROM arca_sends WHERE user_id = ? AND purpose = ? AND state NOT IN ('reserved', 'begun') ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&asset, &atoms, &sentAt) != nil {
			return fail("Emissio has not paid your receive code yet: paste it above and ask Emissio to pay you")
		}
		var wrong *arcaCode
		for _, c := range a.codesOf(user.ID, purpose, sentAt) {
			switch {
			case c.PaidAt == 0:
			case c.PaidAsset != asset || c.PaidAtoms > atoms || atoms-c.PaidAtoms > returnShortAtoms:
				if wrong == nil {
					wrong = c
				}
			case c.PaidAtoms < atoms:
				return pass("received what Emissio paid and sent %s of %s back, %s short for the transfer's fees (OK)", amountText(c.PaidAtoms), short(asset), amountText(atoms-c.PaidAtoms))
			default:
				return pass("received what Emissio paid and sent exactly %s of %s back (OK)", amountText(atoms), short(asset))
			}
		}
		if wrong != nil {
			return fail("Emissio's code received %s of %s, not what Emissio sent you (a return may fall short by up to %s, what a wallet keeps back for its transfer's fees)",
				amountText(wrong.PaidAtoms), short(wrong.PaidAsset), amountText(returnShortAtoms))
		}
		return wait("nothing has reached Emissio's code yet; Emissio checks every few minutes")
	case "arca-pay-btc":
		codes := a.codesOf(user.ID, purpose, 0)
		if len(codes) == 0 {
			return fail("ask Emissio for your receive code above first")
		}
		for _, c := range codes {
			if c.BtcSats > 0 {
				return pass("%d sats reached this account's own Bitcoin ark address at Emissio (OK)", c.BtcSats)
			}
		}
		return wait("no BTC has reached your code's Bitcoin part yet; Emissio checks every few minutes")
	case "arca-pay-code", "arca-swap":
		codes := a.codesOf(user.ID, purpose, 0)
		if len(codes) == 0 {
			return fail("ask Emissio for your receive code above first")
		}
		var refusal *checkResult
		for _, c := range codes {
			if c.PaidAt == 0 {
				continue
			}
			if kind == "arca-swap" && c.PaidAsset == "" {
				// Paid, with its coin not found yet among the wallet's coins
				// (its record moved off a coin that pays another code): the
				// amount to judge is not known yet, so the check waits.
				continue
			}
			if kind == "arca-swap" {
				want, _ := strconv.ParseInt(proofTag(user.ClaimCode), 10, 64)
				var why checkResult
				switch {
				case c.PaidAtoms%proofAtoms != want:
					why = fail("received %s of %s, which does not end in your proof decimals .%s", amountText(c.PaidAtoms), short(c.PaidAsset), proofTag(user.ClaimCode))
				case c.PaidAsset == a.cfg.PolicyAsset:
					why = fail("paid in tSEQ: pay in the asset your swap got you")
				}
				if why.Verdict == vFail {
					if refusal == nil {
						refusal = &why
					}
					continue
				}
			}
			return pass("this account's own Emissio code received %s of %s (OK)", amountText(c.PaidAtoms), short(c.PaidAsset))
		}
		if refusal != nil {
			return *refusal
		}
		return wait("nothing has reached your Emissio code yet; Emissio checks every few minutes")
	}
	return review("reviewed by hand")
}

// arcaView is what a web wallet task's page shows.
type arcaView struct {
	Kind      string
	Code      string // Emissio's receive code for this account and task
	Spent     bool   // that code was paid already: it takes no other payment
	Sent      bool   // Emissio has paid the account (the amount is not shown)
	SentAt    int64
	Ready     bool   // the task's check passes now, so the submission form opens
	Next      string // what the check still misses, when not Ready
	Linked    bool
	Available bool
	Caps      string // the invoice task: the most Emissio pays, in each asset
	Renew     bool   // a new code can be had now
	RenewAt   int64  // or from when, while the code shown is unpaid
}

func (a *App) arcaViewFor(user *User, task *Task) *arcaView {
	if !isArcaTask(task.CheckKind) {
		return nil
	}
	v := &arcaView{Kind: task.CheckKind, Available: a.arcaOn()}
	if task.CheckKind == "arca-invoice" {
		v.Caps = capsText(a.shownCaps())
	}
	if user == nil {
		return v
	}
	v.Linked = walletLinkOf(a.db, user.ID) != nil
	purpose := arcaPurpose(task.CheckKind)
	if c := a.codeOf(user.ID, purpose); c != nil {
		v.Code = a.receiveCode(c)
		v.Spent = c.PaidAt != 0
		ok, from := a.renewable(user.ID, purpose, c)
		v.Renew, v.RenewAt = ok, from
	}
	a.db.QueryRow("SELECT created_at FROM arca_sends WHERE user_id = ? AND purpose = ? AND state NOT IN ('reserved', 'begun') ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&v.SentAt)
	v.Sent = v.SentAt > 0
	if v.Linked && v.Available {
		res := a.arcaCheck(task.CheckKind, user)
		v.Ready = res.Verdict == vPass
		if !v.Ready {
			v.Next = res.Note
		}
	}
	return v
}

// ---------- what a reviewer sees ----------

// walletNote says what Emissio's wallet holds for an account's web wallet
// task, from the wallet's own coins as the follower last read them: each of
// the account's codes for the task, the coin that paid it, any coin that
// names one of them and is not counted, and when the coins were read. A
// reviewer reads it beside a waiting, rejected or queued submission, so
// "nothing arrived" is never said from an old note. Empty for a task that
// takes no payment to a code.
func (a *App) walletNote(userID int64, kind string) string {
	switch kind {
	case "arca-pay-code", "arca-pay-btc", "arca-swap", "arca-roundtrip", "arca-name":
	default:
		return ""
	}
	when := func(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04 UTC") }
	codes := a.codesOf(userID, arcaPurpose(kind), 0)
	if len(codes) == 0 {
		return "Emissio's wallet: no code made for this account and task."
	}
	var parts []string
	ids := map[int64]bool{}
	for i := len(codes) - 1; i >= 0; i-- {
		c := codes[i]
		ids[c.ID] = true
		p := fmt.Sprintf("code #%d made %s", c.ID, when(c.CreatedAt))
		switch {
		case kind == "arca-pay-btc" && c.BtcSats > 0:
			p += fmt.Sprintf(": %d sats at its Bitcoin part", c.BtcSats)
		case c.PaidAt != 0:
			var leaf string
			a.db.QueryRow("SELECT paid_leaf FROM arca_codes WHERE id = ?", c.ID).Scan(&leaf)
			switch {
			case leaf == "" && c.PaidAsset == "":
				p += fmt.Sprintf(": paid %s, its coin not found yet among the wallet's coins", when(c.PaidAt))
			case leaf == "":
				p += fmt.Sprintf(": paid %s (%s of %s), its coin not found yet among the wallet's coins", when(c.PaidAt), amountText(c.PaidAtoms), short(c.PaidAsset))
			default:
				p += fmt.Sprintf(": paid %s by coin %s, %s of %s", when(c.PaidAt), short(leaf), amountText(c.PaidAtoms), short(c.PaidAsset))
			}
		default:
			p += ": unpaid"
		}
		parts = append(parts, p)
	}
	note := "Emissio's wallet: " + strings.Join(parts, "; ") + "."
	var extra []string
	rows, err := a.db.Query("SELECT leaf_id, code_id, asset, atoms FROM arca_coins WHERE code_id != 0 AND counted = 0")
	if err == nil {
		for rows.Next() {
			var leaf, asset string
			var id, atoms int64
			if rows.Scan(&leaf, &id, &asset, &atoms) == nil && ids[id] {
				extra = append(extra, fmt.Sprintf("coin %s, %s of %s, names code #%d, which another coin paid", short(leaf), amountText(atoms), short(asset), id))
			}
		}
		rows.Close()
	}
	if len(extra) > 0 {
		note += " Not counted: " + strings.Join(extra, "; ") + "."
	}
	if rows, err := a.db.Query("SELECT leaf_id, tries, error FROM arca_coins WHERE read_at = 0 AND tries > 0 ORDER BY seen_at LIMIT 10"); err == nil {
		var bad []string
		for rows.Next() {
			var leaf, why string
			var n int
			if rows.Scan(&leaf, &n, &why) == nil {
				bad = append(bad, fmt.Sprintf("coin %s (%d tries: %s)", leaf, n, why))
			}
		}
		rows.Close()
		if len(bad) > 0 {
			var all int
			a.db.QueryRow("SELECT COUNT(*) FROM arca_coins WHERE read_at = 0 AND tries > 0").Scan(&all)
			note += fmt.Sprintf(" %d coins' records could not be read yet and are asked for again: %s. Any of them may be this account's payment: do not say that nothing arrived.",
				all, strings.Join(bad, "; "))
		}
	}
	at, unread := a.reconciled()
	switch {
	case at == 0:
		note += " The wallet's coins have not been read yet: do not say that nothing arrived."
	case unread > 0:
		note += fmt.Sprintf(" The wallet's coins were read %s, with %d records still to read: do not say that nothing arrived until they are.", when(at), unread)
	default:
		note += fmt.Sprintf(" The wallet's coins were all read %s.", when(at))
	}
	return note
}

// withWalletNotes fills the wallet note of each web wallet submission.
func (a *App) withWalletNotes(subs []*Submission) {
	kinds := map[int64]string{}
	for _, s := range subs {
		k, ok := kinds[s.TaskID]
		if !ok {
			a.db.QueryRow("SELECT check_kind FROM tasks WHERE id = ?", s.TaskID).Scan(&k)
			kinds[s.TaskID] = k
		}
		s.Wallet = a.walletNote(s.UserID, k)
	}
}
