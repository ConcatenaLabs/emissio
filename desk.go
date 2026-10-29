package main

// The admin desk: one queue for everything waiting on a person, account
// search with the signals that point at one person farming several
// accounts, holding an account, revoking a reward, reasoned adjustments,
// and CSV exports. Every admin action that changes an account or a ledger
// is logged with the admin who took it.

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const deskSchema = `
CREATE TABLE IF NOT EXISTS user_ips (
	user_id INTEGER NOT NULL REFERENCES users(id),
	ip TEXT NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	PRIMARY KEY (user_id, ip)
);
CREATE INDEX IF NOT EXISTS user_ips_ip ON user_ips(ip);
CREATE TABLE IF NOT EXISTS user_devices (
	user_id INTEGER NOT NULL REFERENCES users(id),
	device TEXT NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	PRIMARY KEY (user_id, device)
);
CREATE INDEX IF NOT EXISTS user_devices_device ON user_devices(device);
CREATE TABLE IF NOT EXISTS admin_log (
	id INTEGER PRIMARY KEY,
	admin_id INTEGER NOT NULL,
	action TEXT NOT NULL,
	user_id INTEGER NOT NULL DEFAULT 0,
	ref_id INTEGER NOT NULL DEFAULT 0,
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_revocation ON ledger(kind, ref_id) WHERE kind = 'revocation';
`

var claimLikeRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

const (
	deviceCookie      = "emissio_device"
	accountsPerIPDay  = 3 // new accounts from one network address in a day
	accountsPerDevice = 3 // accounts ever registered from one browser
	minAdjustmentNote = 8
)

// ---------- where accounts come from ----------

// device returns the browser's device id, setting one when it has none.
func (a *App) device(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(deviceCookie); err == nil && claimLikeRe.MatchString(c.Value) {
		return c.Value
	}
	id := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name: deviceCookie, Value: id, Path: a.cookiePath(),
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: 2 * 365 * 24 * 3600,
	})
	return id
}

// seen records the network address and the browser an account used.
func (a *App) seen(userID int64, ip, device string) {
	t := now()
	if ip != "" {
		a.db.Exec(`INSERT INTO user_ips (user_id, ip, first_seen, last_seen) VALUES (?,?,?,?)
			ON CONFLICT(user_id, ip) DO UPDATE SET last_seen = excluded.last_seen`, userID, ip, t, t)
	}
	if device != "" {
		a.db.Exec(`INSERT INTO user_devices (user_id, device, first_seen, last_seen) VALUES (?,?,?,?)
			ON CONFLICT(user_id, device) DO UPDATE SET last_seen = excluded.last_seen`, userID, device, t, t)
	}
}

// registrationAllowed bounds the accounts one network address makes in a
// day and one browser makes ever.
func (a *App) registrationAllowed(ip, device string) (bool, string) {
	var fromIP, fromDevice int64
	a.db.QueryRow("SELECT COUNT(*) FROM users WHERE reg_ip = ? AND reg_ip != '' AND created_at > ?", ip, now()-86400).Scan(&fromIP)
	if fromIP >= accountsPerIPDay {
		return false, "Several accounts were created from your network today. One account per person: sign in to yours, or try again tomorrow."
	}
	a.db.QueryRow(`SELECT COUNT(*) FROM user_devices d JOIN users u ON u.id = d.user_id
		WHERE d.device = ? AND d.first_seen = u.created_at`, device).Scan(&fromDevice)
	if fromDevice >= accountsPerDevice {
		return false, "This browser has already created several accounts. One account per person: sign in to yours."
	}
	return true, ""
}

// ---------- signals ----------

type signals struct {
	SharedIPs     int64 // other accounts seen at one of this account's addresses
	SharedDevices int64 // other accounts seen in one of this account's browsers
	SharedFunding int64 // other accounts whose evidence touched one of this account's addresses
}

func (s signals) Any() bool { return s.SharedIPs+s.SharedDevices+s.SharedFunding > 0 }

func signalsOf(db *sql.DB, userID int64) signals {
	var s signals
	db.QueryRow(`SELECT COUNT(DISTINCT b.user_id) FROM user_ips a JOIN user_ips b ON a.ip = b.ip
		WHERE a.user_id = ? AND b.user_id != ?`, userID, userID).Scan(&s.SharedIPs)
	db.QueryRow(`SELECT COUNT(DISTINCT b.user_id) FROM user_devices a JOIN user_devices b ON a.device = b.device
		WHERE a.user_id = ? AND b.user_id != ?`, userID, userID).Scan(&s.SharedDevices)
	db.QueryRow(`SELECT COUNT(DISTINCT b.user_id) FROM chain_links a JOIN chain_links b ON a.address = b.address
		WHERE a.user_id = ? AND b.user_id != ?`, userID, userID).Scan(&s.SharedFunding)
	return s
}

type linkedAccount struct {
	ID    int64
	Email string
	Via   string // what they share
	Held  bool
}

// linkedAccounts lists every other account that shares an address, a
// browser or a chain address with this one, and what it shares.
func linkedAccounts(db *sql.DB, userID int64) ([]linkedAccount, error) {
	rows, err := db.Query(`
		SELECT u.id, u.email, u.held, 'network address ' || b.ip FROM user_ips a JOIN user_ips b ON a.ip = b.ip JOIN users u ON u.id = b.user_id
			WHERE a.user_id = ? AND b.user_id != ?
		UNION
		SELECT u.id, u.email, u.held, 'browser' FROM user_devices a JOIN user_devices b ON a.device = b.device JOIN users u ON u.id = b.user_id
			WHERE a.user_id = ? AND b.user_id != ?
		UNION
		SELECT u.id, u.email, u.held, 'chain address ' || b.address FROM chain_links a JOIN chain_links b ON a.address = b.address JOIN users u ON u.id = b.user_id
			WHERE a.user_id = ? AND b.user_id != ?
		UNION
		SELECT u.id, u.email, u.held, 'payout address' FROM users me JOIN users u ON u.mainnet_address = me.mainnet_address
			WHERE me.id = ? AND u.id != ? AND me.mainnet_address != ''
		ORDER BY 1 LIMIT 300`, userID, userID, userID, userID, userID, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linkedAccount
	for rows.Next() {
		var l linkedAccount
		if err := rows.Scan(&l.ID, &l.Email, &l.Held, &l.Via); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (a *App) logAdmin(r *http.Request, action string, userID, refID int64, note string) {
	admin, _, _ := a.currentUser(r)
	var id int64
	if admin != nil {
		id = admin.ID
	}
	a.db.Exec("INSERT INTO admin_log (admin_id, action, user_id, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
		id, action, userID, refID, note, now())
}

// ---------- the queue ----------

type queueItem struct {
	Kind    string // task, bug, security, verification, entry
	ID      int64
	Title   string
	Detail  string
	Link    string
	UserID  int64
	Email   string
	Held    bool
	Signals signals
	At      int64
	Sub     *Submission // task evidence, reviewed in place
}

type queueData struct {
	Items  []queueItem
	Filter string
	Counts map[string]int
	Recent []*Submission // automatic approvals, newest first
}

func (a *App) handleAdminQueue(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("kind")
	d := queueData{Filter: filter, Counts: map[string]int{}}
	add := func(it queueItem) {
		d.Counts[it.Kind]++
		if filter == "" || filter == it.Kind {
			it.Signals = signalsOf(a.db, it.UserID)
			d.Items = append(d.Items, it)
		}
	}
	subs, err := pendingSubmissions(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	held := map[int64]bool{}
	hrows, _ := a.db.Query("SELECT id FROM users WHERE held = 1")
	for hrows != nil && hrows.Next() {
		var id int64
		hrows.Scan(&id)
		held[id] = true
	}
	if hrows != nil {
		hrows.Close()
	}
	for _, s := range subs {
		add(queueItem{Kind: "task", ID: s.ID, Title: s.TaskTitle, Detail: s.ChainNote, UserID: s.UserID, Email: s.UserEmail, Held: held[s.UserID], At: s.CreatedAt, Sub: s})
	}
	bugs, _ := queryBugs(a.db, "WHERE b.status = 'new' ORDER BY b.id")
	for _, b := range bugs {
		src := surfaceName(b.Surface)
		if b.Source == "wallet" {
			src += ", sent from the wallet"
		}
		add(queueItem{Kind: "bug", ID: b.ID, Title: b.Title, Detail: src, Link: fmt.Sprintf("/admin/bugs/%d", b.ID), UserID: b.UserID, Email: b.UserEmail, Held: held[b.UserID], At: b.CreatedAt})
	}
	reps, _ := queryReports(a.db, "WHERE r.status = 'new'")
	for _, rp := range reps {
		add(queueItem{Kind: "security", ID: rp.ID, Title: rp.Title, Detail: "claimed " + rp.Severity + "; " + rp.CheckNote, Link: fmt.Sprintf("/admin/reports/%d", rp.ID), UserID: rp.UserID, Email: rp.UserEmail, Held: held[rp.UserID], At: rp.CreatedAt})
	}
	verifs, _ := pendingVerifications(a.db)
	for _, v := range verifs {
		add(queueItem{Kind: "verification", ID: v.ID, Title: platformName(v.Platform) + " " + v.Handle, Detail: v.CheckNote, Link: "/admin/verifications", UserID: v.UserID, Email: v.UserEmail, Held: held[v.UserID], At: v.CreatedAt})
	}
	erows, err := a.db.Query(`SELECT e.id, e.user_id, u.email, c.title, c.slug, e.url, e.check_note, e.updated_at FROM entries e
		JOIN competitions c ON c.id = e.comp_id JOIN users u ON u.id = e.user_id
		WHERE e.place = 0 AND c.status != 'awarded' AND c.closes_at <= ? ORDER BY e.id`, now())
	if err == nil {
		for erows.Next() {
			var it queueItem
			var title, slug, link string
			erows.Scan(&it.ID, &it.UserID, &it.Email, &title, &slug, &link, &it.Detail, &it.At)
			it.Kind, it.Title, it.Link, it.Held = "entry", title+": "+link, "/admin/competitions/"+slug, held[it.UserID]
			add(it)
		}
		erows.Close()
	}
	rec, err := querySubmissions(a.db, "WHERE s.auto = 1 AND s.status = 'approved' ORDER BY s.reviewed_at DESC LIMIT 50")
	if err != nil {
		a.serverError(w, err)
		return
	}
	d.Recent = rec
	a.render(w, r, "admin_queue", "Review queue", d)
}

// querySubmissions reads submissions with their task and account.
func querySubmissions(db *sql.DB, where string, args ...any) ([]*Submission, error) {
	rows, err := db.Query(`SELECT s.id, s.user_id, s.task_id, s.txid, s.notes, s.status, s.chain_note, s.review_note,
		s.created_at, s.reviewed_at, t.title, t.slug, t.reward, u.email, u.claim_code, 0
		FROM submissions s JOIN tasks t ON t.id = s.task_id JOIN users u ON u.id = s.user_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Submission
	for rows.Next() {
		var s Submission
		if err := rows.Scan(&s.ID, &s.UserID, &s.TaskID, &s.Txid, &s.Notes, &s.Status, &s.ChainNote, &s.ReviewNote,
			&s.CreatedAt, &s.ReviewedAt, &s.TaskTitle, &s.TaskSlug, &s.TaskReward, &s.UserEmail, &s.ClaimCode, &s.VerifCount); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// ---------- accounts ----------

type userSearchData struct {
	Q     string
	Users []*UserRow
	Sig   map[int64]signals
}

// searchUsers finds accounts by email, account code, network address, payout
// address, an address their evidence touched, or a txid they submitted.
func searchUsers(db *sql.DB, q string, limit int) ([]*UserRow, error) {
	if q == "" {
		return listUsers(db, limit)
	}
	like := "%" + strings.ToLower(q) + "%"
	rows, err := db.Query(`SELECT `+userCols+`,
		COALESCE((SELECT SUM(amount) FROM ledger l WHERE l.user_id = users.id), 0),
		CASE WHEN users.reg_ip = '' THEN 1 ELSE (SELECT COUNT(*) FROM users u2 WHERE u2.reg_ip = users.reg_ip) END
		FROM users WHERE lower(email) LIKE ? OR claim_code = ? OR reg_ip = ? OR lower(mainnet_address) = ?
			OR id IN (SELECT user_id FROM user_ips WHERE ip = ?)
			OR id IN (SELECT user_id FROM chain_links WHERE address = ?)
			OR id IN (SELECT user_id FROM submissions WHERE txid = ?)
			OR CAST(id AS TEXT) = ?
		ORDER BY id DESC LIMIT ?`, like, strings.ToLower(q), q, strings.ToLower(q), q, strings.ToLower(q), strings.ToLower(q), q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserRow
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Email, &u.PassHash, &u.DisplayName, &u.ClaimCode,
			&u.IsAdmin, &u.MainnetAddress, &u.AddressUpdatedAt, &u.ReferredBy, &u.RegIP, &u.CreatedAt, &u.EmailVerifiedAt, &u.Held, &u.Balance, &u.IPPeers); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func (a *App) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	users, err := searchUsers(a.db, q, 500)
	if err != nil {
		a.serverError(w, err)
		return
	}
	sig := map[int64]signals{}
	for _, u := range users {
		sig[u.ID] = signalsOf(a.db, u.ID)
	}
	a.render(w, r, "admin_users", "Users", userSearchData{Q: q, Users: users, Sig: sig})
}

type adminUserData struct {
	U       *User
	Balance int64
	Ledger  []LedgerEntry
	Subs    []*Submission
	Bugs    []*Bug
	Verifs  []*Verification
	Linked  []linkedAccount
	Shared  []sharedAddr
	IPs     []string
	Devices int
	Signals signals
	Log     []adminLogRow
}

type adminLogRow struct {
	Action, Note, Admin string
	At                  int64
}

func (a *App) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := getUserByID(a.db, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := adminUserData{U: u, Signals: signalsOf(a.db, id)}
	d.Balance, _ = balanceOf(a.db, id)
	d.Ledger, _ = ledgerOf(a.db, id)
	d.Subs, _ = querySubmissions(a.db, "WHERE s.user_id = ? ORDER BY s.id DESC", id)
	d.Bugs, _ = queryBugs(a.db, "WHERE b.user_id = ? ORDER BY b.id DESC LIMIT 100", id)
	d.Verifs, _ = verificationsOf(a.db, id)
	d.Linked, _ = linkedAccounts(a.db, id)
	d.Shared, _ = sharedAddresses(a.db, id)
	rows, _ := a.db.Query("SELECT ip FROM user_ips WHERE user_id = ? ORDER BY last_seen DESC LIMIT 50", id)
	for rows != nil && rows.Next() {
		var ip string
		rows.Scan(&ip)
		d.IPs = append(d.IPs, ip)
	}
	if rows != nil {
		rows.Close()
	}
	var n int64
	a.db.QueryRow("SELECT COUNT(*) FROM user_devices WHERE user_id = ?", id).Scan(&n)
	d.Devices = int(n)
	lrows, _ := a.db.Query(`SELECT l.action, l.note, COALESCE(u.email, ''), l.created_at FROM admin_log l LEFT JOIN users u ON u.id = l.admin_id
		WHERE l.user_id = ? ORDER BY l.id DESC LIMIT 100`, id)
	for lrows != nil && lrows.Next() {
		var x adminLogRow
		lrows.Scan(&x.Action, &x.Note, &x.Admin, &x.At)
		d.Log = append(d.Log, x)
	}
	if lrows != nil {
		lrows.Close()
	}
	a.render(w, r, "admin_user", "Account "+u.Email, d)
}

func (a *App) handleAdminHold(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	hold := r.FormValue("hold") == "1"
	note := strings.TrimSpace(r.FormValue("note"))
	back := fmt.Sprintf("/admin/users/%d", id)
	if hold && note == "" {
		a.redirect(w, r, back, "", "Say why the account is held.")
		return
	}
	if _, err := a.db.Exec("UPDATE users SET held = ? WHERE id = ?", hold, id); err != nil {
		a.serverError(w, err)
		return
	}
	action := "release"
	if hold {
		action = "hold"
	}
	a.logAdmin(r, action, id, 0, note)
	if hold {
		a.redirect(w, r, back, "Account held: nothing it submits is approved automatically, and it is left out of payout runs.", "")
		return
	}
	a.redirect(w, r, back, "Account released.", "")
}

// revokeSubmission takes back an approved submission's reward with a
// visible ledger row, once.
func revokeSubmission(db *sql.DB, subID int64, reason string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var userID, credit int64
	var status, title string
	if err := tx.QueryRow(`SELECT s.user_id, s.status, t.title, COALESCE((SELECT amount FROM ledger l WHERE l.kind = 'submission' AND l.ref_id = s.id), 0)
		FROM submissions s JOIN tasks t ON t.id = s.task_id WHERE s.id = ?`, subID).Scan(&userID, &status, &title, &credit); err != nil {
		return 0, err
	}
	if status != "approved" {
		return 0, fmt.Errorf("submission %d is %s, not approved", subID, status)
	}
	if credit > 0 {
		if _, err := tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
			userID, -credit, "revocation", subID, "Revoked: "+title+" ("+reason+")", now()); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec("UPDATE submissions SET status = 'revoked', review_note = ?, reviewed_at = ? WHERE id = ?", reason, now(), subID); err != nil {
		return 0, err
	}
	return userID, tx.Commit()
}

func (a *App) handleAdminRevoke(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	reason := strings.TrimSpace(r.FormValue("note"))
	back := r.FormValue("back")
	if back != "/admin/queue" {
		back = "/admin/users"
	}
	if len(reason) < minAdjustmentNote {
		a.redirect(w, r, back, "", "Give the reason for revoking; the account sees it in its ledger.")
		return
	}
	userID, err := revokeSubmission(a.db, id, reason)
	if err != nil {
		a.redirect(w, r, back, "", err.Error())
		return
	}
	a.logAdmin(r, "revoke", userID, id, reason)
	if back == "/admin/users" {
		back = fmt.Sprintf("/admin/users/%d", userID)
	}
	a.redirect(w, r, back, fmt.Sprintf("Submission %d revoked.", id), "")
}

// ---------- exports ----------

func (a *App) csvOut(w http.ResponseWriter, name string, header []string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	cw := csv.NewWriter(w)
	cw.Write(header)
	for _, r := range rows {
		for i, v := range r {
			// A cell a spreadsheet would run as a formula is quoted as text.
			if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
				r[i] = "'" + v
			}
		}
		cw.Write(r)
	}
	cw.Flush()
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }
func tstamp(v int64) string {
	if v == 0 {
		return ""
	}
	return time.Unix(v, 0).UTC().Format(time.RFC3339)
}

func (a *App) handleAdminCSV(w http.ResponseWriter, r *http.Request) {
	var header []string
	var out [][]string
	name := r.PathValue("name")
	switch name {
	case "users.csv":
		users, err := searchUsers(a.db, "", 1000000)
		if err != nil {
			a.serverError(w, err)
			return
		}
		header = []string{"user_id", "email", "account_code", "created", "email_confirmed", "held", "reg_ip", "balance_seq", "verified_platforms", "shared_ip_accounts", "shared_browser_accounts", "shared_chain_accounts", "referred_by"}
		for _, u := range users {
			s := signalsOf(a.db, u.ID)
			v, _ := verifiedCount(a.db, u.ID)
			out = append(out, []string{i64(u.ID), u.Email, u.ClaimCode, tstamp(u.CreatedAt), tstamp(u.EmailVerifiedAt), strconv.FormatBool(u.Held), u.RegIP,
				i64(u.Balance), i64(v), i64(s.SharedIPs), i64(s.SharedDevices), i64(s.SharedFunding), i64(u.ReferredBy)})
		}
	case "submissions.csv":
		subs, err := querySubmissions(a.db, "ORDER BY s.id")
		if err != nil {
			a.serverError(w, err)
			return
		}
		header = []string{"submission_id", "user_id", "email", "task", "txid", "status", "check_note", "review_note", "submitted", "reviewed"}
		for _, s := range subs {
			out = append(out, []string{i64(s.ID), i64(s.UserID), s.UserEmail, s.TaskSlug, s.Txid, s.Status, s.ChainNote, s.ReviewNote, tstamp(s.CreatedAt), tstamp(s.ReviewedAt)})
		}
	case "ledger.csv":
		rows, err := a.db.Query("SELECT l.id, l.user_id, u.email, l.amount, l.kind, l.ref_id, l.note, l.created_at FROM ledger l JOIN users u ON u.id = l.user_id ORDER BY l.id")
		if err != nil {
			a.serverError(w, err)
			return
		}
		header = []string{"entry_id", "user_id", "email", "amount_seq", "kind", "ref_id", "note", "created"}
		for rows.Next() {
			var id, uid, amt, ref, at int64
			var email, kind, note string
			rows.Scan(&id, &uid, &email, &amt, &kind, &ref, &note, &at)
			out = append(out, []string{i64(id), i64(uid), email, i64(amt), kind, i64(ref), note, tstamp(at)})
		}
		rows.Close()
	case "bugs.csv":
		bugs, err := queryBugs(a.db, "ORDER BY b.id")
		if err != nil {
			a.serverError(w, err)
			return
		}
		header = []string{"report_id", "user_id", "email", "source", "surface", "title", "status", "severity", "award_seq", "duplicate_of", "filed", "reviewed"}
		for _, b := range bugs {
			out = append(out, []string{i64(b.ID), i64(b.UserID), b.UserEmail, b.Source, b.Surface, b.Title, b.Status, b.Severity, i64(b.Award), i64(b.DupOf), tstamp(b.CreatedAt), tstamp(b.ReviewedAt)})
		}
	default:
		http.NotFound(w, r)
		return
	}
	a.csvOut(w, "emissio-"+name, header, out)
}
