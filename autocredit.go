package main

// The automatic credit boundary. Every credit a check makes without a
// person (a task submission that passes at once, one the waiting poller
// sees pass, a verification that passes) goes through here, and here the
// two things a reviewer would weigh are read inside the approval's own
// transaction: whether an admin holds the account, and whether it shares a
// network address, a browser or a chain address with another account that
// no reviewer has cleared. Either one leaves the item pending, in the
// queue. The database has one connection (mustOpenDB), so nothing else can
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
CREATE TABLE IF NOT EXISTS cleared_links (
	user_id INTEGER NOT NULL REFERENCES users(id),
	other_id INTEGER NOT NULL REFERENCES users(id),
	admin_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, other_id)
);
`

// rowsQuerier is what both a database and a transaction answer.
type rowsQuerier interface {
	querier
	Query(query string, args ...any) (*sql.Rows, error)
}

// ids collects the first column of a query.
func ids(q rowsQuerier, query string, args ...any) map[int64]bool {
	out := map[int64]bool{}
	rows, err := q.Query(query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// unclearedSignals is what an automatic credit weighs: the accounts this one
// shares a network address, a browser or a chain address with, the chain
// addresses including those that funded the evidence being credited
// (funding), less the accounts a reviewer has cleared for this one.
func unclearedSignals(q rowsQuerier, userID int64, funding []string) signals {
	cleared := ids(q, "SELECT other_id FROM cleared_links WHERE user_id = ?", userID)
	count := func(set map[int64]bool) int64 {
		var n int64
		for id := range set {
			if id != userID && !cleared[id] {
				n++
			}
		}
		return n
	}
	ipsOf := ids(q, `SELECT DISTINCT b.user_id FROM user_ips a JOIN user_ips b ON a.ip = b.ip WHERE a.user_id = ?`, userID)
	devs := ids(q, `SELECT DISTINCT b.user_id FROM user_devices a JOIN user_devices b ON a.device = b.device WHERE a.user_id = ?`, userID)
	chain := ids(q, `SELECT DISTINCT b.user_id FROM chain_links a JOIN chain_links b ON a.address = b.address WHERE a.user_id = ?`, userID)
	for _, ad := range funding {
		for id := range ids(q, "SELECT DISTINCT user_id FROM chain_links WHERE address = ?", ad) {
			chain[id] = true
		}
	}
	return signals{SharedIPs: count(ipsOf), SharedDevices: count(devs), SharedFunding: count(chain)}
}

// fundingAddrs are the addresses that funded a transaction, less those
// everyone shares (the faucet's and Emissio's own), as recordFunding keeps
// them.
func (a *App) fundingAddrs(txid string) []string {
	if txid == "" {
		return nil
	}
	tx, _ := a.fetchTx(txid)
	if tx == nil {
		return nil
	}
	ret := ""
	a.db.QueryRow("SELECT value FROM kv WHERE key = 'return_address'").Scan(&ret)
	var out []string
	for _, in := range tx.Vin {
		if in.Prevout == nil || in.Prevout.Address == "" {
			continue
		}
		ad := strings.ToLower(strings.TrimSpace(in.Prevout.Address))
		if a.faucetAddr[ad] || ad == strings.ToLower(ret) {
			continue
		}
		out = append(out, ad)
	}
	return out
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

// handleAdminClearLinks records a reviewer's judgment that the accounts
// this one currently shares a signal with are not one person farming:
// automatic credits stop weighing those links, for both accounts. A link
// to an account not cleared here still sends the next credit to a reviewer.
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
	others := ids(a.db, `SELECT b.user_id FROM user_ips a JOIN user_ips b ON a.ip = b.ip WHERE a.user_id = ?
		UNION SELECT b.user_id FROM user_devices a JOIN user_devices b ON a.device = b.device WHERE a.user_id = ?
		UNION SELECT b.user_id FROM chain_links a JOIN chain_links b ON a.address = b.address WHERE a.user_id = ?`, id, id, id)
	n := 0
	for other := range others {
		if other == id {
			continue
		}
		for _, pair := range [][2]int64{{id, other}, {other, id}} {
			a.db.Exec("INSERT OR IGNORE INTO cleared_links (user_id, other_id, admin_id, created_at) VALUES (?,?,?,?)", pair[0], pair[1], adminID, now())
		}
		n++
	}
	a.logAdmin(r, "clear-links", id, 0, fmt.Sprintf("%d accounts: %s", n, note))
	a.redirect(w, r, back, fmt.Sprintf("Cleared the links to %d accounts: their shared signals no longer hold back this account's automatic credits.", n), "")
}
