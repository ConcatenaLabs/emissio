package main

// The tester's dashboard, at the top of the account page: progress through
// each public surface, the tasks still open, what was earned from what, and
// every report filed with its status.

import "database/sql"

type surfaceProgress struct {
	Key, Name   string
	Done, Total int
	Pending     int
	Percent     int
	Bar         int // Percent rounded down to tens, for the bar's width class
}

type openTask struct {
	Task   *Task
	Status string // "", "pending" or "rejected"
}

type earning struct {
	Label  string
	Amount int64
}

type dashboard struct {
	Surfaces []surfaceProgress
	Open     []openTask
	Earnings []earning
	Bugs     []*Bug
	Reports  []*Report
	Done     int
	Total    int
}

var kindLabels = map[string]string{
	"submission": "Tasks", "bug": "Bug reports", "report": "Security reports", "entry": "Competitions",
	"verification": "Verified accounts", "referral": "Referrals", "referral-welcome": "Referrals",
	"adjustment": "Adjustments", "revocation": "Revoked task rewards", "payout": "Paid out", "presale": "Pre-sale",
}

func loadDashboard(db *sql.DB, userID int64) (*dashboard, error) {
	tasks, err := listTasks(db, false)
	if err != nil {
		return nil, err
	}
	status := map[int64]string{}
	rows, err := db.Query(`SELECT task_id, status FROM submissions WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var s string
		rows.Scan(&id, &s)
		if status[id] != "approved" {
			status[id] = s
		}
	}
	rows.Close()
	d := &dashboard{}
	idx := map[string]int{}
	for _, s := range surfaces {
		idx[s.Key] = len(d.Surfaces)
		d.Surfaces = append(d.Surfaces, surfaceProgress{Key: s.Key, Name: s.Name})
	}
	for _, t := range tasks {
		i, ok := idx[t.Surface]
		st := status[t.ID]
		if ok {
			d.Surfaces[i].Total++
			switch st {
			case "approved":
				d.Surfaces[i].Done++
			case "pending":
				d.Surfaces[i].Pending++
			}
		}
		d.Total++
		if st == "approved" {
			d.Done++
			continue
		}
		if t.Cap > 0 && t.Awarded >= t.Cap {
			continue
		}
		d.Open = append(d.Open, openTask{Task: t, Status: st})
	}
	for i := range d.Surfaces {
		if d.Surfaces[i].Total > 0 {
			d.Surfaces[i].Percent = d.Surfaces[i].Done * 100 / d.Surfaces[i].Total
			d.Surfaces[i].Bar = d.Surfaces[i].Percent / 10 * 10
		}
	}
	erows, err := db.Query("SELECT kind, SUM(amount) FROM ledger WHERE user_id = ? GROUP BY kind", userID)
	if err != nil {
		return nil, err
	}
	sums := map[string]int64{}
	for erows.Next() {
		var k string
		var v int64
		erows.Scan(&k, &v)
		label := kindLabels[k]
		if label == "" {
			label = k
		}
		sums[label] += v
	}
	erows.Close()
	for _, l := range []string{"Tasks", "Bug reports", "Security reports", "Competitions", "Verified accounts", "Referrals", "Adjustments", "Revoked task rewards", "Paid out", "Pre-sale"} {
		if v, ok := sums[l]; ok && v != 0 {
			d.Earnings = append(d.Earnings, earning{l, v})
		}
	}
	if d.Bugs, err = queryBugs(db, "WHERE b.user_id = ? ORDER BY b.id DESC LIMIT 100", userID); err != nil {
		return nil, err
	}
	if d.Reports, err = reportsOf(db, userID); err != nil {
		return nil, err
	}
	return d, nil
}
