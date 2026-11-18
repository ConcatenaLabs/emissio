package main

// Waiting submissions. A check that fails only because its evidence has not
// arrived yet (checks.go's wait verdict) does not send the submission to a
// reviewer: the submission waits, and this poller runs the same check again
// on every waiting submission. It is credited the moment the check passes,
// exactly as at submission; refused if the check now fails for a reason the
// tester must fix; and it lapses after waitLimit with the check's last note.
// A check that could not run at all (a service unreachable) keeps the
// submission waiting; if it still cannot run when the time is up, the
// submission goes to a reviewer, since nothing automatic could decide it.

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// waitPoll is how often waiting submissions are checked again.
const waitPoll = 3 * time.Minute

type waitingSub struct {
	ID, UserID, TaskID int64
	Txid, Notes, Kind  string
	CreatedAt          int64
}

// startWaitPoller checks waiting submissions again, every waitPoll.
func (a *App) startWaitPoller() {
	go func() {
		for {
			time.Sleep(waitPoll)
			a.recheckWaiting(time.Now())
		}
	}()
}

// recheckWaiting runs the check of every waiting submission once.
func (a *App) recheckWaiting(at time.Time) {
	rows, err := a.db.Query(`SELECT s.id, s.user_id, s.task_id, s.txid, s.notes, t.check_kind, s.created_at
		FROM submissions s JOIN tasks t ON t.id = s.task_id WHERE s.status = 'waiting' ORDER BY s.id`)
	if err != nil {
		log.Printf("waiting submissions: %v", err)
		return
	}
	var subs []waitingSub
	for rows.Next() {
		var w waitingSub
		if rows.Scan(&w.ID, &w.UserID, &w.TaskID, &w.Txid, &w.Notes, &w.Kind, &w.CreatedAt) == nil {
			subs = append(subs, w)
		}
	}
	rows.Close()
	for _, w := range subs {
		a.recheckOne(w, at)
	}
}

func (a *App) recheckOne(w waitingSub, at time.Time) {
	// The same locks as a submission, in the same order, so a new
	// submission of this claim and this check cannot interleave.
	defer a.claims.lock(fmt.Sprintf("task:%d:%d", w.UserID, w.TaskID))()
	if w.Txid != "" {
		defer a.claims.lock("txid:" + strings.ToLower(w.Txid))()
	}
	var status string
	if a.db.QueryRow("SELECT status FROM submissions WHERE id = ?", w.ID).Scan(&status) != nil || status != "waiting" {
		return
	}
	user, err := getUserByID(a.db, w.UserID)
	if err != nil {
		return
	}
	task, err := taskByID(a.db, w.TaskID)
	if err != nil {
		return
	}
	switch {
	case !task.Active:
		a.endWaiting(w.ID, "rejected", "", "The task closed before the evidence arrived.")
		return
	case task.Cap > 0 && task.Awarded >= task.Cap:
		a.endWaiting(w.ID, "rejected", "", "All rewards for this task were claimed before the evidence arrived.")
		return
	}
	res := a.evidenceCheck(w.Kind, w.Txid, w.Notes, user)
	lapsed := at.Sub(time.Unix(w.CreatedAt, 0)) >= waitLimit(w.Kind)
	switch {
	case res.Verdict == vPass:
		if !a.endWaiting(w.ID, "pending", res.Note, "") {
			return
		}
		if !user.Held {
			if err := a.creditChecked(w.ID, w.UserID, w.Txid); err != nil {
				log.Printf("waiting submission %d: %v", w.ID, err)
			}
		}
	case res.Verdict == vFail:
		a.endWaiting(w.ID, "rejected", res.Note, "Checked again: "+res.Note+".")
	case !lapsed:
		a.db.Exec("UPDATE submissions SET chain_note = ? WHERE id = ? AND status = 'waiting'", res.Note, w.ID)
	case res.Verdict == vReview:
		a.endWaiting(w.ID, "pending", res.Note, "")
	default:
		a.endWaiting(w.ID, "rejected", res.Note, fmt.Sprintf("Not there after %s: %s. Submit again once it is.", waitText(waitLimit(w.Kind)), res.Note))
	}
}

// endWaiting moves a waiting submission on, if it is still waiting. A note
// left empty keeps the stored one.
func (a *App) endWaiting(id int64, status, chainNote, reviewNote string) bool {
	q := "UPDATE submissions SET status = ?, review_note = ?, reviewed_at = ?"
	args := []any{status, reviewNote, now()}
	if status == "pending" {
		args[2] = int64(0)
	}
	if chainNote != "" {
		q += ", chain_note = ?"
		args = append(args, chainNote)
	}
	res, err := a.db.Exec(q+" WHERE id = ? AND status = 'waiting'", append(args, id)...)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// waitText says a waiting time in days or hours.
func waitText(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d hours", int(d.Hours()))
}
