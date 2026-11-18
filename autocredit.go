package main

// The automatic credit boundary. Every credit a check makes without a
// person (a task submission that passes at once, one the waiting poller
// sees pass, a verification that passes) goes through here, and here the
// two things a reviewer would weigh are read inside the approval's own
// transaction: whether an admin holds the account, and whether it shares a
// network address, a browser or a chain address with another account that
// no reviewer has cleared. Either one leaves the item pending, in the
// queue. A clearance is of one signal, between one pair of accounts: this
// network address, this browser, this chain address. Any other value they
// come to share is weighed again. The database has one connection
// (mustOpenDB), so nothing else can
// write between that read and the approval: a hold or a shared signal that
// landed while the check was running is honoured.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const clearedSchema = `
CREATE TABLE IF NOT EXISTS cleared_signals (
	user_id INTEGER NOT NULL REFERENCES users(id),
	other_id INTEGER NOT NULL REFERENCES users(id),
	kind TEXT NOT NULL,
	value TEXT NOT NULL,
	admin_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, other_id, kind, value)
);
`

// The kinds of shared signal, as cleared_signals names them.
const (
	sigIP     = "ip"
	sigDevice = "device"
	sigChain  = "chain"
)

// sharedSignal is one value an account shares with another.
type sharedSignal struct {
	Other       int64
	Kind, Value string
}

// rowsQuerier is what both a database and a transaction answer.
type rowsQuerier interface {
	querier
	Query(query string, args ...any) (*sql.Rows, error)
}

// sharedSignals lists every value this account shares with another
// account: each network address, browser and chain address, the chain
// addresses including those that funded the evidence being credited
// (funding).
func sharedSignals(q rowsQuerier, userID int64, funding []string) []sharedSignal {
	var out []sharedSignal
	seen := map[sharedSignal]bool{}
	collect := func(kind, query string, args ...any) {
		rows, err := q.Query(query, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			sg := sharedSignal{Kind: kind}
			if rows.Scan(&sg.Other, &sg.Value) == nil && sg.Other != userID && !seen[sg] {
				seen[sg] = true
				out = append(out, sg)
			}
		}
	}
	collect(sigIP, `SELECT b.user_id, a.ip FROM user_ips a JOIN user_ips b ON a.ip = b.ip WHERE a.user_id = ? AND b.user_id != ?`, userID, userID)
	collect(sigDevice, `SELECT b.user_id, a.device FROM user_devices a JOIN user_devices b ON a.device = b.device WHERE a.user_id = ? AND b.user_id != ?`, userID, userID)
	collect(sigChain, `SELECT b.user_id, a.address FROM chain_links a JOIN chain_links b ON a.address = b.address WHERE a.user_id = ? AND b.user_id != ?`, userID, userID)
	for _, ad := range funding {
		collect(sigChain, "SELECT user_id, address FROM chain_links WHERE address = ? AND user_id != ?", ad, userID)
	}
	return out
}

// unclearedSignals is what an automatic credit weighs: the accounts this one
// shares a value with that no reviewer has cleared for this pair, counted
// by kind.
func unclearedSignals(q rowsQuerier, userID int64, funding []string) signals {
	cleared := map[sharedSignal]bool{}
	if rows, err := q.Query("SELECT other_id, kind, value FROM cleared_signals WHERE user_id = ?", userID); err == nil {
		for rows.Next() {
			var sg sharedSignal
			if rows.Scan(&sg.Other, &sg.Kind, &sg.Value) == nil {
				cleared[sg] = true
			}
		}
		rows.Close()
	}
	by := map[string]map[int64]bool{sigIP: {}, sigDevice: {}, sigChain: {}}
	for _, sg := range sharedSignals(q, userID, funding) {
		if !cleared[sg] {
			by[sg.Kind][sg.Other] = true
		}
	}
	return signals{SharedIPs: int64(len(by[sigIP])), SharedDevices: int64(len(by[sigDevice])), SharedFunding: int64(len(by[sigChain]))}
}

// fundingOf gives the addresses that funded the transaction a check read,
// less those everyone shares (the faucet's and Emissio's own), as
// recordFunding keeps them. They come from the very transaction the check
// judged, never from a second read. ok is false when the evidence is a
// transaction the check did not hand over: the funding is then unknown, and
// the credit must not go ahead on what is known so far.
func (a *App) fundingOf(kind, txid string, res checkResult) (addrs []string, ok bool) {
	if txid == "" || !readsTx(kind) {
		return nil, true
	}
	if res.Tx == nil {
		return nil, false
	}
	ret := ""
	a.db.QueryRow("SELECT value FROM kv WHERE key = 'return_address'").Scan(&ret)
	for _, in := range res.Tx.Vin {
		if in.Prevout == nil || in.Prevout.Address == "" {
			continue
		}
		ad := strings.ToLower(strings.TrimSpace(in.Prevout.Address))
		if a.faucetAddr[ad] || ad == strings.ToLower(ret) {
			continue
		}
		addrs = append(addrs, ad)
	}
	return addrs, true
}

// whyNotAuto says, inside the approval's transaction, why an automatic
// credit must wait for a reviewer: the account is held, or shares a signal
// no reviewer has cleared. Empty: nothing stands in the way.
func whyNotAuto(tx *sql.Tx, userID int64, funding []string) string {
	var held bool
	if err := tx.QueryRow("SELECT held FROM users WHERE id = ?", userID).Scan(&held); err != nil {
		return "the account could not be read"
	}
	if held {
		return "the account is held"
	}
	if sig := unclearedSignals(tx, userID, funding); sig.Any() {
		return "this account " + sig.Words()
	}
	return ""
}

// autoApproveSubmission credits a pending submission whose check passed,
// unless whyNotAuto finds a reason, which it then adds to the check's note
// for the reviewer and returns. Either way the addresses that funded the
// evidence become the account's chain links, so a reviewer sees the account
// it shares them with and can clear that link.
func (a *App) autoApproveSubmission(subID, userID int64, funding []string) (string, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if why := whyNotAuto(tx, userID, funding); why != "" {
		if _, err := tx.Exec("UPDATE submissions SET chain_note = chain_note || ? WHERE id = ? AND status = 'pending'",
			"; waiting for a reviewer: "+why, subID); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		a.recordChainLinks(userID, "funded-by", funding)
		return why, nil
	}
	if err := reviewSubmissionTx(tx, subID, true, "Checked automatically."); err != nil {
		return "", err
	}
	if _, err := tx.Exec("UPDATE submissions SET auto = 1 WHERE id = ?", subID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	a.recordChainLinks(userID, "funded-by", funding)
	a.processReferrals(userID)
	return "", nil
}

// autoApproveVerification is the same for a verification whose check
// passed.
func (a *App) autoApproveVerification(verifID, userID int64) (string, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if why := whyNotAuto(tx, userID, nil); why != "" {
		if _, err := tx.Exec("UPDATE verifications SET check_note = check_note || ? WHERE id = ? AND status = 'pending'",
			"; waiting for a reviewer: "+why, verifID); err != nil {
			return "", err
		}
		return why, tx.Commit()
	}
	if err := reviewVerificationTx(tx, verifID, true, "Checked automatically."); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	a.processReferrals(userID)
	return "", nil
}

// handleAdminClearLinks records a reviewer's judgment that what this
// account shares with others now is not one person farming: each network
// address, browser and chain address it shares with each account, for both
// accounts of each pair. Automatic credits stop weighing exactly those; a
// value the pair comes to share later, or any value shared with another
// account, sends the next credit to a reviewer again.
func (a *App) handleAdminClearLinks(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/users/%d", id)
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) < minAdjustmentNote {
		a.redirect(w, r, back, "", "Say why these accounts are not one person farming.")
		return
	}
	admin, _, _ := a.currentUser(r)
	var adminID int64
	if admin != nil {
		adminID = admin.ID
	}
	shared := sharedSignals(a.db, id, nil)
	others := map[int64]bool{}
	for _, sg := range shared {
		for _, pair := range [][2]int64{{id, sg.Other}, {sg.Other, id}} {
			a.db.Exec("INSERT OR IGNORE INTO cleared_signals (user_id, other_id, kind, value, admin_id, created_at) VALUES (?,?,?,?,?,?)",
				pair[0], pair[1], sg.Kind, sg.Value, adminID, now())
		}
		others[sg.Other] = true
	}
	a.logAdmin(r, "clear-links", id, 0, fmt.Sprintf("%d signals with %d accounts: %s", len(shared), len(others), note))
	a.redirect(w, r, back, fmt.Sprintf("Cleared %d shared signals with %d accounts: those no longer hold back this account's automatic credits; anything they come to share next does.", len(shared), len(others)), "")
}
