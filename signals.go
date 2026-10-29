package main

// Clustering signals for the admin desk: addresses an account's evidence
// touched. A farm of accounts tends to be funded from one wallet, so the
// funding addresses of each account's approved transactions are recorded,
// and an address shared by several accounts is shown beside each of them.
// The faucet's own addresses and Emissio's are left out: everyone shares
// those.

import (
	"database/sql"
	"strings"
)

const signalSchema = `
CREATE TABLE IF NOT EXISTS chain_links (
	user_id INTEGER NOT NULL REFERENCES users(id),
	address TEXT NOT NULL,
	role TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE(user_id, address, role)
);
CREATE INDEX IF NOT EXISTS chain_links_address ON chain_links(address);
`

func (a *App) recordChainLinks(userID int64, role string, addrs []string) {
	ret := ""
	a.db.QueryRow("SELECT value FROM kv WHERE key = 'return_address'").Scan(&ret)
	for _, ad := range addrs {
		ad = strings.ToLower(strings.TrimSpace(ad))
		if ad == "" || a.faucetAddr[ad] || ad == strings.ToLower(ret) {
			continue
		}
		a.db.Exec("INSERT OR IGNORE INTO chain_links (user_id, address, role, created_at) VALUES (?,?,?,?)", userID, ad, role, now())
	}
}

// recordFunding stores the addresses that funded an approved transaction.
func (a *App) recordFunding(userID int64, txid string) {
	if txid == "" {
		return
	}
	tx, _ := a.fetchTx(txid)
	if tx == nil {
		return
	}
	var addrs []string
	for _, in := range tx.Vin {
		if in.Prevout != nil && in.Prevout.Address != "" {
			addrs = append(addrs, in.Prevout.Address)
		}
	}
	a.recordChainLinks(userID, "funded-by", addrs)
}

// sharedAddresses lists, for one account, each recorded address that other
// accounts share too, with how many.
type sharedAddr struct {
	Address string
	Role    string
	Others  int64
}

func sharedAddresses(db *sql.DB, userID int64) ([]sharedAddr, error) {
	rows, err := db.Query(`SELECT c.address, c.role,
		(SELECT COUNT(DISTINCT c2.user_id) FROM chain_links c2 WHERE c2.address = c.address AND c2.user_id != c.user_id)
		FROM chain_links c WHERE c.user_id = ? ORDER BY 3 DESC, c.address`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sharedAddr
	for rows.Next() {
		var s sharedAddr
		if err := rows.Scan(&s.Address, &s.Role, &s.Others); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
