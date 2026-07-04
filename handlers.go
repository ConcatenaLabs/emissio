package main

import (
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Page is the data envelope every template receives.
type Page struct {
	Title string
	User  *User
	CSRF  string
	Flash string
	Error string
	Data  any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, title string, data any) {
	user, token, err := a.currentUser(r)
	if err != nil {
		a.serverError(w, err)
		return
	}
	p := Page{
		Title: title,
		User:  user,
		CSRF:  csrfToken(token),
		Flash: r.URL.Query().Get("m"),
		Error: r.URL.Query().Get("e"),
		Data:  data,
	}
	t, ok := a.tpls[name]
	if !ok {
		a.serverError(w, nil)
		log.Printf("missing template %q", name)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	if err != nil {
		log.Printf("error: %v", err)
	}
	http.Error(w, "Something went wrong on our side. Try again in a moment.", http.StatusInternalServerError)
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	u := a.cfg.BasePath + path
	q := url.Values{}
	if msg != "" {
		q.Set("m", msg)
	}
	if errMsg != "" {
		q.Set("e", errMsg)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// requireUser resolves the signed-in user for a POST/protected handler,
// redirecting to login when anonymous. Returns nil after handling the
// response itself in that case.
func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (*User, string) {
	user, token, err := a.currentUser(r)
	if err != nil {
		a.serverError(w, err)
		return nil, ""
	}
	if user == nil {
		a.redirect(w, r, "/login", "", "Sign in first.")
		return nil, ""
	}
	return user, token
}

func (a *App) postGuard(w http.ResponseWriter, r *http.Request) (*User, bool) {
	user, token := a.requireUser(w, r)
	if user == nil {
		return nil, false
	}
	if err := r.ParseForm(); err != nil || !checkCSRF(r, token) {
		a.redirect(w, r, "/", "", "That form expired. Try again.")
		return nil, false
	}
	return user, true
}

func (a *App) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, token, err := a.currentUser(r)
		if err != nil {
			a.serverError(w, err)
			return
		}
		if user == nil || !user.IsAdmin {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil || !checkCSRF(r, token) {
				a.redirect(w, r, "/admin", "", "That form expired. Try again.")
				return
			}
		}
		next(w, r)
	}
}

// ---------- public pages ----------

type homeData struct {
	Stats     Stats
	Pool      int64
	Tasks     []*Task
	Comps     []*Competition
	TierMax   int64
	TaskTotal int64
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	stats, err := loadStats(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	tasks, err := listTasks(a.db, false)
	if err != nil {
		a.serverError(w, err)
		return
	}
	comps, err := listCompetitions(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	var taskTotal int64
	for _, t := range tasks {
		taskTotal += t.Reward
	}
	if len(comps) > 3 {
		comps = comps[:3]
	}
	a.render(w, r, "home", "Sequentia Emissio", homeData{
		Stats: stats, Pool: programPool, Tasks: tasks, Comps: comps,
		TierMax: 27000, TaskTotal: taskTotal,
	})
}

func (a *App) handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := listTasks(a.db, false)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "tasks", "Tasks", tasks)
}

type taskData struct {
	Task *Task
	Mine *Submission
	Full bool
}

func (a *App) handleTask(w http.ResponseWriter, r *http.Request) {
	task, err := taskBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := taskData{Task: task, Full: task.Cap > 0 && task.Awarded >= task.Cap}
	if user, _, _ := a.currentUser(r); user != nil {
		d.Mine, err = latestSubmission(a.db, user.ID, task.ID)
		if err != nil {
			a.serverError(w, err)
			return
		}
	}
	a.render(w, r, "task", task.Title, d)
}

func (a *App) handleTaskSubmit(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	task, err := taskBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := "/tasks/" + task.Slug
	if !task.Active {
		a.redirect(w, r, back, "", "This task is closed.")
		return
	}
	if task.Cap > 0 && task.Awarded >= task.Cap {
		a.redirect(w, r, back, "", "All rewards for this task have been claimed.")
		return
	}
	prev, err := latestSubmission(a.db, user.ID, task.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if prev != nil && prev.Status != "rejected" {
		a.redirect(w, r, back, "", "You already have a submission for this task.")
		return
	}
	txid := strings.TrimSpace(r.FormValue("txid"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	if len(notes) > 4000 {
		notes = notes[:4000]
	}
	if task.NeedsTxid && !txidRe.MatchString(txid) {
		a.redirect(w, r, back, "", "Enter the transaction id: 64 hex characters.")
		return
	}
	chainNote := ""
	if txid != "" {
		chainNote = a.chainCheck(txid)
	}
	_, err = a.db.Exec(`INSERT INTO submissions (user_id, task_id, txid, notes, chain_note, created_at)
		VALUES (?,?,?,?,?,?)`, user.ID, task.ID, strings.ToLower(txid), notes, chainNote, now())
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, back, "Submitted. Rewards are credited once a reviewer approves the evidence.", "")
}

func (a *App) handleCompetitions(w http.ResponseWriter, r *http.Request) {
	comps, err := listCompetitions(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "competitions", "Competitions", comps)
}

type compData struct {
	Comp    *Competition
	Mine    *Entry
	Winners []*Entry
	Open    bool
}

func (a *App) handleCompetition(w http.ResponseWriter, r *http.Request) {
	comp, err := compBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := compData{Comp: comp, Open: comp.Status == "open" && comp.ClosesAt > now()}
	if user, _, _ := a.currentUser(r); user != nil {
		d.Mine, err = entryOf(a.db, comp.ID, user.ID)
		if err != nil {
			a.serverError(w, err)
			return
		}
	}
	if comp.Status == "awarded" {
		entries, err := entriesOf(a.db, comp.ID)
		if err != nil {
			a.serverError(w, err)
			return
		}
		for _, e := range entries {
			if e.Place > 0 {
				d.Winners = append(d.Winners, e)
			}
		}
	}
	a.render(w, r, "competition", comp.Title, d)
}

func (a *App) handleCompetitionEnter(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	comp, err := compBySlug(a.db, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := "/competitions/" + comp.Slug
	if comp.Status != "open" || comp.ClosesAt <= now() {
		a.redirect(w, r, back, "", "This competition is closed to new entries.")
		return
	}
	entryURL := strings.TrimSpace(r.FormValue("url"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	if len(notes) > 4000 {
		notes = notes[:4000]
	}
	if u, err := url.Parse(entryURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		a.redirect(w, r, back, "", "Enter a public http(s) link to your work.")
		return
	}
	if err := upsertEntry(a.db, comp.ID, user.ID, entryURL, notes); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, back, "Entry saved. You can update it until the competition closes.", "")
}

type securityData struct {
	Tiers []struct {
		Name   string
		Reward int64
		Desc   string
	}
	Mine []*Report
}

func (a *App) handleSecurity(w http.ResponseWriter, r *http.Request) {
	d := securityData{Tiers: securityTiers}
	if user, _, _ := a.currentUser(r); user != nil {
		var err error
		d.Mine, err = reportsOf(a.db, user.ID)
		if err != nil {
			a.serverError(w, err)
			return
		}
	}
	a.render(w, r, "security", "Security rewards", d)
}

var severityRe = regexp.MustCompile(`^(low|medium|high|critical)$`)

func (a *App) handleSecurityReport(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	severity := r.FormValue("severity")
	bodyText := strings.TrimSpace(r.FormValue("body"))
	if title == "" || len(title) > 200 || !severityRe.MatchString(severity) || bodyText == "" {
		a.redirect(w, r, "/security", "", "Give the report a title, a severity, and a description.")
		return
	}
	if len(bodyText) > 20000 {
		bodyText = bodyText[:20000]
	}
	if _, err := createReport(a.db, user.ID, title, severity, bodyText); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/security", "Report received. We review reports privately and reply through this page.", "")
}

func (a *App) handleGuide(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "guide", "Generate a mainnet address", nil)
}

func (a *App) handleRules(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "rules", "Program rules", nil)
}

type accountData struct {
	Balance     int64
	Ledger      []LedgerEntry
	Submissions []*Submission
}

func (a *App) handleAccount(w http.ResponseWriter, r *http.Request) {
	user, _ := a.requireUser(w, r)
	if user == nil {
		return
	}
	bal, err := balanceOf(a.db, user.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	led, err := ledgerOf(a.db, user.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	subs, err := submissionsOf(a.db, user.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "account", "Your account", accountData{Balance: bal, Ledger: led, Submissions: subs})
}

func (a *App) handleAddress(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	addr := strings.TrimSpace(r.FormValue("address"))
	if addr == "" {
		if _, err := a.db.Exec("UPDATE users SET mainnet_address = '', address_updated_at = ? WHERE id = ?", now(), user.ID); err != nil {
			a.serverError(w, err)
			return
		}
		a.redirect(w, r, "/account", "Payout address removed.", "")
		return
	}
	if err := validateMainnetAddress(addr); err != nil {
		a.redirect(w, r, "/account", "", "Address not saved: "+err.Error()+".")
		return
	}
	if _, err := a.db.Exec("UPDATE users SET mainnet_address = ?, address_updated_at = ? WHERE id = ?",
		strings.ToLower(addr), now(), user.ID); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/account", "Payout address saved. Keep the keys for it safe until launch.", "")
}

// ---------- auth ----------

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func (a *App) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "register", "Create an account", nil)
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !a.authLimit.allow("reg|" + clientIP(r)) {
		a.redirect(w, r, "/register", "", "Too many attempts. Wait a minute and try again.")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.redirect(w, r, "/register", "", "Could not read the form.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	pass := r.FormValue("password")
	if !emailRe.MatchString(email) || len(email) > 254 {
		a.redirect(w, r, "/register", "", "Enter a valid email address.")
		return
	}
	if len(pass) < 10 {
		a.redirect(w, r, "/register", "", "Use a password of at least 10 characters.")
		return
	}
	id, err := createUser(a.db, email, hashPassword(pass), randomHex(5))
	if err != nil {
		a.redirect(w, r, "/login", "", "An account with that email already exists. Sign in instead.")
		return
	}
	if err := a.createSession(w, id); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/account", "Welcome to Emissio. Your claim code is on this page; you will use it in task evidence.", "")
}

func (a *App) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "login", "Sign in", nil)
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.authLimit.allow("login|" + clientIP(r)) {
		a.redirect(w, r, "/login", "", "Too many attempts. Wait a minute and try again.")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.redirect(w, r, "/login", "", "Could not read the form.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	user, err := getUserByEmail(a.db, email)
	if err != nil || !verifyPassword(user.PassHash, r.FormValue("password")) {
		a.redirect(w, r, "/login", "", "Email or password is wrong.")
		return
	}
	if err := a.createSession(w, user.ID); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/account", "", "")
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	_, token, _ := a.currentUser(r)
	r.ParseForm()
	if token != "" && checkCSRF(r, token) {
		a.destroySession(w, token)
	}
	a.redirect(w, r, "/", "", "")
}
