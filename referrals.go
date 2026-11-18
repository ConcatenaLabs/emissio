package main

import (
	"database/sql"
	"fmt"
	"log"
)

// Referral program. Both sides of a referral receive referralBonus SEQ, but
// only once BOTH have earned referralThreshold SEQ from real contributions
// (tasks, competitions, security reports, adjustments). Referral and pre-sale
// credits never count toward the threshold, and a referrer is paid for at
// most referralCap referrals. This makes farming uneconomical: every sock
// puppet must first get 50 SEQ of evidence past a human reviewer.
const (
	referralBonus     int64 = 10
	referralThreshold int64 = 50
	referralCap             = 20
)

// qualifiedEarnings sums a user's positive ledger entries that count toward
// referral qualification.
func qualifiedEarnings(db *sql.DB, userID int64) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRow(`SELECT SUM(amount) FROM ledger
		WHERE user_id = ? AND amount > 0 AND kind NOT IN ('referral', 'referral-welcome', 'presale')`,
		userID).Scan(&v)
	return v.Int64, err
}

func qualifiedReferralCount(db *sql.DB, referrerID int64) (int64, error) {
	var n int64
	err := db.QueryRow("SELECT COUNT(*) FROM ledger WHERE user_id = ? AND kind = 'referral'", referrerID).Scan(&n)
	return n, err
}

// processReferrals is called after any credit lands for userID. It checks the
// user's own referral (as a referee) and every referral they made (as a
// referrer), crediting the ones that now qualify. Idempotent: the partial
// unique index on ledger(kind, ref_id) makes each referral pay at most once.
func (a *App) processReferrals(userID int64) {
	u, err := getUserByID(a.db, userID)
	if err != nil {
		log.Printf("referrals: load user %d: %v", userID, err)
		return
	}
	if u.ReferredBy > 0 {
		a.tryQualifyReferral(u.ReferredBy, u.ID)
	}
	rows, err := a.db.Query("SELECT id FROM users WHERE referred_by = ?", userID)
	if err != nil {
		log.Printf("referrals: list referees of %d: %v", userID, err)
		return
	}
	defer rows.Close()
	var referees []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return
		}
		referees = append(referees, id)
	}
	rows.Close()
	for _, id := range referees {
		a.tryQualifyReferral(userID, id)
	}
}

// referralSchema holds the referrals the automatic credit sent to a
// reviewer, and what the reviewer decided. A referral with a row here is
// never paid automatically again: it is paid when a reviewer approves it.
const referralSchema = `
CREATE TABLE IF NOT EXISTS referral_reviews (
	referee_id INTEGER PRIMARY KEY REFERENCES users(id),
	referrer_id INTEGER NOT NULL REFERENCES users(id),
	status TEXT NOT NULL DEFAULT 'pending',
	note TEXT NOT NULL DEFAULT '',
	review_note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NOT NULL DEFAULT 0
);
`

// tryQualifyReferral pays a referral once both sides qualify. The payment is
// an automatic credit, so it passes the same boundary as every other
// (autocredit.go): inside its transaction it reads again whether either
// side is held (then nothing is paid, as for any held account) and whether
// either shares a signal no reviewer has cleared (then the referral waits
// in the queue for a reviewer).
func (a *App) tryQualifyReferral(referrerID, refereeID int64) {
	var already, reviewed int64
	if err := a.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'referral-welcome' AND ref_id = ?", refereeID).
		Scan(&already); err != nil || already > 0 {
		return
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM referral_reviews WHERE referee_id = ?", refereeID).
		Scan(&reviewed); err != nil || reviewed > 0 {
		return
	}
	// Both sides must hold a verified social account, and neither may be
	// held by an admin, before a referral pays.
	for _, id := range []int64{referrerID, refereeID} {
		if n, err := verifiedCount(a.db, id); err != nil || n == 0 {
			return
		}
		var held bool
		if a.db.QueryRow("SELECT held FROM users WHERE id = ?", id).Scan(&held); held {
			return
		}
	}
	refereeEarn, err := qualifiedEarnings(a.db, refereeID)
	if err != nil || refereeEarn < referralThreshold {
		return
	}
	referrerEarn, err := qualifiedEarnings(a.db, referrerID)
	if err != nil || referrerEarn < referralThreshold {
		return
	}
	n, err := qualifiedReferralCount(a.db, referrerID)
	if err != nil || n >= referralCap {
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var held bool
	for _, id := range []int64{referrerID, refereeID} {
		var h bool
		if tx.QueryRow("SELECT held FROM users WHERE id = ?", id).Scan(&h) != nil {
			return
		}
		held = held || h
	}
	if held {
		return
	}
	var why string
	for _, side := range []struct {
		id   int64
		name string
	}{{refereeID, "the referee"}, {referrerID, "the referrer"}} {
		if sig := unclearedSignals(tx, side.id, nil); sig.Any() {
			if why != "" {
				why += "; "
			}
			why += side.name + " " + sig.Words()
		}
	}
	if why != "" {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO referral_reviews (referee_id, referrer_id, status, note, created_at)
			VALUES (?,?,'pending',?,?)`, refereeID, referrerID, "waiting for a reviewer: "+why, now()); err != nil {
			return
		}
		if tx.Commit() == nil {
			log.Printf("referral sent to a reviewer: referrer %d, referee %d: %s", referrerID, refereeID, why)
		}
		return
	}
	if err := payReferralTx(tx, referrerID, refereeID); err != nil {
		return
	}
	if err := tx.Commit(); err == nil {
		log.Printf("referral qualified: referrer %d, referee %d, %d SEQ each", referrerID, refereeID, referralBonus)
	}
}

// payReferralTx credits both sides of a referral inside a transaction.
func payReferralTx(tx *sql.Tx, referrerID, refereeID int64) error {
	if _, err := tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
		referrerID, referralBonus, "referral", refereeID, "Referral reward: a user you referred qualified", now()); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
		refereeID, referralBonus, "referral-welcome", refereeID, "Referral reward: you and your referrer both qualified", now())
	return err
}

// reviewReferral records a reviewer's decision on a referral the automatic
// credit held back. Approving pays both sides, once (the ledger's unique
// index), and within the referrer's cap.
func reviewReferral(db *sql.DB, refereeID int64, approve bool, note string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var referrerID int64
	var status string
	if err := tx.QueryRow("SELECT referrer_id, status FROM referral_reviews WHERE referee_id = ?", refereeID).Scan(&referrerID, &status); err != nil {
		return fmt.Errorf("no referral of account %d waits for review", refereeID)
	}
	if status != "pending" {
		return fmt.Errorf("the referral of account %d is already %s", refereeID, status)
	}
	newStatus := "rejected"
	if approve {
		var n int64
		if err := tx.QueryRow("SELECT COUNT(*) FROM ledger WHERE user_id = ? AND kind = 'referral'", referrerID).Scan(&n); err != nil {
			return err
		}
		if n >= referralCap {
			return fmt.Errorf("the referrer has been paid for %d referrals, the cap", referralCap)
		}
		if err := payReferralTx(tx, referrerID, refereeID); err != nil {
			return err
		}
		newStatus = "approved"
	}
	if _, err := tx.Exec("UPDATE referral_reviews SET status = ?, review_note = ?, reviewed_at = ? WHERE referee_id = ?",
		newStatus, note, now(), refereeID); err != nil {
		return err
	}
	return tx.Commit()
}
