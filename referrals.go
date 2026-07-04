package main

import (
	"database/sql"
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

func (a *App) tryQualifyReferral(referrerID, refereeID int64) {
	var already int64
	if err := a.db.QueryRow("SELECT COUNT(*) FROM ledger WHERE kind = 'referral-welcome' AND ref_id = ?", refereeID).
		Scan(&already); err != nil || already > 0 {
		return
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
	_, err = tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
		referrerID, referralBonus, "referral", refereeID, "Referral reward: a user you referred qualified", now())
	if err != nil {
		return
	}
	_, err = tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
		refereeID, referralBonus, "referral-welcome", refereeID, "Referral reward: you and your referrer both qualified", now())
	if err != nil {
		return
	}
	if err := tx.Commit(); err == nil {
		log.Printf("referral qualified: referrer %d, referee %d, %d SEQ each", referrerID, refereeID, referralBonus)
	}
}
