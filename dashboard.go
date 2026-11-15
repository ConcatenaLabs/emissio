package main

// The tester's dashboard, at the top of the account page: progress through
// each stage of the path, the tasks still open, what was earned from what, and
// every report filed with its status.

import "database/sql"

type stageProgress struct {
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
	Stages   []stageProgress
	Open     []openTask
	Earnings []earning
	Bugs     []*Bug
	Reports  []*Report
	Done     int
	Total    int
	Status   map[int64]string // task id: its latest submission's status, "approved" once any is
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
	// The path's order, so the next task is the next one on the path.
	var ordered []*Task
	for _, g := range taskGroupsFull(tasks) {
		ordered = append(ordered, g.Tasks...)
	}
	tasks = ordered
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
	d := &dashboard{Status: status}
	idx := map[string]int{}
	for _, g := range taskGroupsFull(tasks) {
		idx[g.Key] = len(d.Stages)
		d.Stages = append(d.Stages, stageProgress{Key: g.Key, Name: g.Name})
	}
	for _, t := range tasks {
		i, ok := idx[stageOf(t.Category)]
		st := status[t.ID]
		if ok {
			d.Stages[i].Total++
			switch st {
			case "approved":
				d.Stages[i].Done++
			case "pending":
				d.Stages[i].Pending++
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
	for i := range d.Stages {
		if d.Stages[i].Total > 0 {
			d.Stages[i].Percent = d.Stages[i].Done * 100 / d.Stages[i].Total
			d.Stages[i].Bar = d.Stages[i].Percent / 10 * 10
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
