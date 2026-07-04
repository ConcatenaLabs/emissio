package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type adminHome struct {
	Stats Stats
	Pool  int64
}

func (a *App) handleAdminHome(w http.ResponseWriter, r *http.Request) {
	stats, err := loadStats(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_home", "Admin", adminHome{Stats: stats, Pool: programPool})
}

func (a *App) handleAdminSubmissions(w http.ResponseWriter, r *http.Request) {
	subs, err := pendingSubmissions(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_submissions", "Review submissions", subs)
}

func (a *App) handleAdminReview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	action := r.FormValue("action")
	note := strings.TrimSpace(r.FormValue("note"))
	switch action {
	case "approve", "reject":
		if err := reviewSubmission(a.db, id, action == "approve", note); err != nil {
			a.redirect(w, r, "/admin/submissions", "", err.Error())
			return
		}
		a.redirect(w, r, "/admin/submissions", fmt.Sprintf("Submission %d %sd.", id, action), "")
	case "recheck":
		var txid string
		if err := a.db.QueryRow("SELECT txid FROM submissions WHERE id = ?", id).Scan(&txid); err != nil {
			http.NotFound(w, r)
			return
		}
		chainNote := "no txid submitted"
		if txid != "" {
			chainNote = a.chainCheck(txid)
		}
		if _, err := a.db.Exec("UPDATE submissions SET chain_note = ? WHERE id = ?", chainNote, id); err != nil {
			a.serverError(w, err)
			return
		}
		a.redirect(w, r, "/admin/submissions", fmt.Sprintf("Submission %d: %s", id, chainNote), "")
	default:
		a.redirect(w, r, "/admin/submissions", "", "Unknown action.")
	}
}

func (a *App) handleAdminTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := listTasks(a.db, true)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_tasks", "Manage tasks", tasks)
}

func (a *App) handleAdminTaskSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	slug := strings.TrimSpace(r.FormValue("slug"))
	title := strings.TrimSpace(r.FormValue("title"))
	category := strings.TrimSpace(r.FormValue("category"))
	body := strings.ReplaceAll(strings.TrimSpace(r.FormValue("body")), "\r\n", "\n")
	reward, _ := strconv.ParseInt(r.FormValue("reward"), 10, 64)
	cap, _ := strconv.ParseInt(r.FormValue("cap"), 10, 64)
	sort, _ := strconv.ParseInt(r.FormValue("sort"), 10, 64)
	needsTxid := r.FormValue("needs_txid") == "1"
	active := r.FormValue("active") == "1"
	if slug == "" || title == "" || reward <= 0 {
		a.redirect(w, r, "/admin/tasks", "", "A task needs a slug, a title, and a positive reward.")
		return
	}
	var err error
	if id > 0 {
		_, err = a.db.Exec(`UPDATE tasks SET slug=?, title=?, category=?, body=?, reward=?, cap=?, needs_txid=?, active=?, sort=? WHERE id=?`,
			slug, title, category, body, reward, cap, needsTxid, active, sort, id)
	} else {
		_, err = a.db.Exec(`INSERT INTO tasks (slug, title, category, body, reward, cap, needs_txid, active, sort) VALUES (?,?,?,?,?,?,?,?,?)`,
			slug, title, category, body, reward, cap, needsTxid, active, sort)
	}
	if err != nil {
		a.redirect(w, r, "/admin/tasks", "", "Save failed: "+err.Error())
		return
	}
	a.redirect(w, r, "/admin/tasks", "Task saved.", "")
}

func (a *App) handleAdminCompetitions(w http.ResponseWriter, r *http.Request) {
	comps, err := listCompetitions(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_competitions", "Manage competitions", comps)
}

func (a *App) handleAdminCompSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	slug := strings.TrimSpace(r.FormValue("slug"))
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.ReplaceAll(strings.TrimSpace(r.FormValue("body")), "\r\n", "\n")
	prizes := strings.TrimSpace(r.FormValue("prizes"))
	days, _ := strconv.ParseInt(r.FormValue("days"), 10, 64)
	if slug == "" || title == "" || len(parsePrizes(prizes)) == 0 {
		a.redirect(w, r, "/admin/competitions", "", "A competition needs a slug, a title, and prizes like 2000,750,250.")
		return
	}
	var err error
	if id > 0 {
		if days > 0 {
			_, err = a.db.Exec(`UPDATE competitions SET slug=?, title=?, body=?, prizes=?, closes_at=? WHERE id=?`,
				slug, title, body, prizes, now()+days*86400, id)
		} else {
			_, err = a.db.Exec(`UPDATE competitions SET slug=?, title=?, body=?, prizes=? WHERE id=?`,
				slug, title, body, prizes, id)
		}
	} else {
		if days <= 0 {
			days = 28
		}
		_, err = a.db.Exec(`INSERT INTO competitions (slug, title, body, prizes, closes_at, status) VALUES (?,?,?,?,?,'open')`,
			slug, title, body, prizes, now()+days*86400)
	}
	if err != nil {
		a.redirect(w, r, "/admin/competitions", "", "Save failed: "+err.Error())
		return
	}
	a.redirect(w, r, "/admin/competitions", "Competition saved.", "")
}

type adminCompData struct {
	Comp    *Competition
	Entries []*Entry
	Prizes  []int64
}

func (a *App) handleAdminCompetition(w http.ResponseWriter, r *http.Request) {
	comp, err := compBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	entries, err := entriesOf(a.db, comp.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_competition", "Judge: "+comp.Title,
		adminCompData{Comp: comp, Entries: entries, Prizes: parsePrizes(comp.Prizes)})
}

func (a *App) handleAdminAward(w http.ResponseWriter, r *http.Request) {
	comp, err := compBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := "/admin/competitions/" + comp.Slug
	entryID, _ := strconv.ParseInt(r.FormValue("entry"), 10, 64)
	place, _ := strconv.ParseInt(r.FormValue("place"), 10, 64)
	prizes := parsePrizes(comp.Prizes)
	if place < 1 || place > int64(len(prizes)) {
		a.redirect(w, r, back, "", "That place has no prize defined.")
		return
	}
	if err := awardEntry(a.db, entryID, place, prizes[place-1], comp.Title); err != nil {
		a.redirect(w, r, back, "", err.Error())
		return
	}
	a.redirect(w, r, back, fmt.Sprintf("Awarded place %d (%s SEQ).", place, formatSEQ(prizes[place-1])), "")
}

func (a *App) handleAdminCompStatus(w http.ResponseWriter, r *http.Request) {
	comp, err := compBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	status := r.FormValue("status")
	if status != "open" && status != "judging" && status != "awarded" {
		a.redirect(w, r, "/admin/competitions/"+comp.Slug, "", "Unknown status.")
		return
	}
	if _, err := a.db.Exec("UPDATE competitions SET status = ? WHERE id = ?", status, comp.ID); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/admin/competitions/"+comp.Slug, "Status set to "+status+".", "")
}

func (a *App) handleAdminReports(w http.ResponseWriter, r *http.Request) {
	reports, err := allReports(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_reports", "Security reports", reports)
}

type adminReportData struct {
	Report *Report
	Tiers  []struct {
		Name   string
		Reward int64
		Desc   string
	}
}

func (a *App) handleAdminReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	report, err := reportByID(a.db, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "admin_report", "Report: "+report.Title, adminReportData{Report: report, Tiers: securityTiers})
}

func (a *App) handleAdminReportReview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	status := r.FormValue("status")
	award, _ := strconv.ParseInt(r.FormValue("award"), 10, 64)
	note := strings.TrimSpace(r.FormValue("note"))
	if status != "accepted" && status != "rejected" && status != "duplicate" {
		a.redirect(w, r, fmt.Sprintf("/admin/reports/%d", id), "", "Pick an outcome.")
		return
	}
	if status != "accepted" {
		award = 0
	}
	if award < 0 || award > 27000 {
		a.redirect(w, r, fmt.Sprintf("/admin/reports/%d", id), "", "Award must be between 0 and 27,000 SEQ.")
		return
	}
	if err := reviewReport(a.db, id, status, award, note); err != nil {
		a.redirect(w, r, fmt.Sprintf("/admin/reports/%d", id), "", err.Error())
		return
	}
	a.redirect(w, r, "/admin/reports", fmt.Sprintf("Report %d marked %s.", id, status), "")
}

func (a *App) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := listUsers(a.db, 500)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_users", "Users", users)
}

func (a *App) handleAdminAdjust(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.FormValue("user"), 10, 64)
	amount, _ := strconv.ParseInt(r.FormValue("amount"), 10, 64)
	note := strings.TrimSpace(r.FormValue("note"))
	if userID <= 0 || amount == 0 || note == "" {
		a.redirect(w, r, "/admin/users", "", "An adjustment needs a user id, a non-zero amount, and a note.")
		return
	}
	if _, err := getUserByID(a.db, userID); err != nil {
		a.redirect(w, r, "/admin/users", "", "No such user.")
		return
	}
	_, err := a.db.Exec("INSERT INTO ledger (user_id, amount, kind, note, created_at) VALUES (?,?,?,?,?)",
		userID, amount, "adjustment", note, now())
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/admin/users", fmt.Sprintf("Adjusted user %d by %s SEQ.", userID, formatSEQ(amount)), "")
}

func (a *App) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	rows, err := allocationRows(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="emissio-allocations.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"user_id", "email", "mainnet_address", "balance_seq"})
	for _, u := range rows {
		cw.Write([]string{
			strconv.FormatInt(u.ID, 10), u.Email, u.MainnetAddress, strconv.FormatInt(u.Balance, 10),
		})
	}
	cw.Flush()
}
