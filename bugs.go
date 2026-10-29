package main

// Bug reports. A tester files one from the bug page (surface, steps,
// expected and actual behaviour, an optional screenshot); a wallet files one
// through the JSON intake when its user reports a problem from inside it.
// Both land in the same table and the same review queue. A report earns
// nothing until an admin accepts it, at a severity whose reward is fixed
// below; a later report of the same bug is merged into the first as a
// duplicate and earns nothing.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const bugSchema = `
CREATE TABLE IF NOT EXISTS bugs (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL DEFAULT 0,
	source TEXT NOT NULL DEFAULT 'form',
	surface TEXT NOT NULL,
	title TEXT NOT NULL,
	steps TEXT NOT NULL DEFAULT '',
	expected TEXT NOT NULL DEFAULT '',
	actual TEXT NOT NULL DEFAULT '',
	meta TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'new',
	severity TEXT NOT NULL DEFAULT '',
	award INTEGER NOT NULL DEFAULT 0,
	dup_of INTEGER NOT NULL DEFAULT 0,
	review_note TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	body_hash TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS bugs_user ON bugs(user_id);
CREATE INDEX IF NOT EXISTS bugs_status ON bugs(status);
CREATE INDEX IF NOT EXISTS bugs_ip ON bugs(ip, created_at);
CREATE TABLE IF NOT EXISTS bug_files (
	bug_id INTEGER PRIMARY KEY REFERENCES bugs(id),
	content_type TEXT NOT NULL,
	data BLOB NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_bug ON ledger(kind, ref_id) WHERE kind = 'bug';
`

// bugSurfaces are what the form offers: the public testnet's surfaces.
var bugSurfaces = []struct{ Key, Name string }{
	{"explorer", "Block explorer"},
	{"faucet", "Faucet"},
	{"compages", "Compages bridge"},
	{"core", "Sequentia Core (download, node, desktop wallet)"},
	{"pools", "Staking pool board"},
	{"emissio", "Emissio (this site)"},
	{"other", "Something else on sequentiatestnet.com"},
}

// wallets that file reports through the intake. Their names appear only in
// the admin desk.
var intakeWallets = map[string]string{
	"web-wallet":    "Web wallet",
	"ambra-browser": "Ambra browser",
	"ambra-mobile":  "Ambra mobile",
}

// bugTiers are the rewards an accepted bug report earns, by severity.
var bugTiers = []struct {
	Key, Name string
	Reward    int64
	Desc      string
}{
	{"minor", "Minor", 10, "Wrong text, a confusing screen, a broken link, a cosmetic defect you can show."},
	{"major", "Major", 25, "A feature that does not work as documented, a wrong number shown to the user, an error with no way forward."},
	{"severe", "Severe", 50, "Lost or stuck testnet funds, a crash or hang you can reproduce, a flow that cannot be completed at all."},
}

func bugTier(key string) (int64, bool) {
	for _, t := range bugTiers {
		if t.Key == key {
			return t.Reward, true
		}
	}
	return 0, false
}

func surfaceName(key string) string {
	for _, s := range bugSurfaces {
		if s.Key == key {
			return s.Name
		}
	}
	if n, ok := intakeWallets[key]; ok {
		return n
	}
	return key
}

type Bug struct {
	ID         int64
	UserID     int64
	Source     string
	Surface    string
	Title      string
	Steps      string
	Expected   string
	Actual     string
	Meta       string
	Status     string
	Severity   string
	Award      int64
	DupOf      int64
	ReviewNote string
	IP         string
	CreatedAt  int64
	ReviewedAt int64
	UserEmail  string // joined; empty for an unattributed report
	HasFile    bool
	Dups       []int64 // reports merged into this one (detail page)
	SameBody   int64   // another report with an identical body, if any
}

const bugCols = `b.id, b.user_id, b.source, b.surface, b.title, b.steps, b.expected, b.actual, b.meta, b.status,
	b.severity, b.award, b.dup_of, b.review_note, b.ip, b.created_at, b.reviewed_at, COALESCE(u.email, ''),
	EXISTS(SELECT 1 FROM bug_files f WHERE f.bug_id = b.id)`

func queryBugs(db *sql.DB, where string, args ...any) ([]*Bug, error) {
	rows, err := db.Query(`SELECT `+bugCols+` FROM bugs b LEFT JOIN users u ON u.id = b.user_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Bug
	for rows.Next() {
		var b Bug
		if err := rows.Scan(&b.ID, &b.UserID, &b.Source, &b.Surface, &b.Title, &b.Steps, &b.Expected, &b.Actual, &b.Meta,
			&b.Status, &b.Severity, &b.Award, &b.DupOf, &b.ReviewNote, &b.IP, &b.CreatedAt, &b.ReviewedAt, &b.UserEmail, &b.HasFile); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

func bugByID(db *sql.DB, id int64) (*Bug, error) {
	bs, err := queryBugs(db, "WHERE b.id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(bs) == 0 {
		return nil, sql.ErrNoRows
	}
	b := bs[0]
	rows, err := db.Query("SELECT id FROM bugs WHERE dup_of = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d int64
		rows.Scan(&d)
		b.Dups = append(b.Dups, d)
	}
	var hash string
	db.QueryRow("SELECT body_hash FROM bugs WHERE id = ?", id).Scan(&hash)
	db.QueryRow("SELECT id FROM bugs WHERE body_hash = ? AND id != ? ORDER BY id LIMIT 1", hash, id).Scan(&b.SameBody)
	return b, nil
}

func bugHash(steps, expected, actual string) string {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	sum := sha256.Sum256([]byte(norm(steps) + "\x00" + norm(expected) + "\x00" + norm(actual)))
	return fmt.Sprintf("%x", sum[:])
}

type newBug struct {
	UserID                        int64
	Source, Surface, Title        string
	Steps, Expected, Actual, Meta string
	IP                            string
	FileType                      string
	File                          []byte
}

func createBug(db *sql.DB, b newBug) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO bugs (user_id, source, surface, title, steps, expected, actual, meta, ip, body_hash, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		b.UserID, b.Source, b.Surface, b.Title, b.Steps, b.Expected, b.Actual, b.Meta, b.IP, bugHash(b.Steps, b.Expected, b.Actual), now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if len(b.File) > 0 {
		if _, err := tx.Exec("INSERT INTO bug_files (bug_id, content_type, data) VALUES (?,?,?)", id, b.FileType, b.File); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// reviewBug records an admin's outcome. Accepting credits the severity's
// reward to the reporter inside the same transaction (once, by the ledger's
// unique index); a duplicate points at the report it repeats.
func reviewBug(db *sql.DB, id int64, status, severity string, dupOf int64, note string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID int64
	var current, title string
	if err := tx.QueryRow("SELECT user_id, status, title FROM bugs WHERE id = ?", id).Scan(&userID, &current, &title); err != nil {
		return err
	}
	if current != "new" {
		return fmt.Errorf("report %d is already %s", id, current)
	}
	var award int64
	switch status {
	case "accepted":
		r, ok := bugTier(severity)
		if !ok {
			return errors.New("pick a severity for an accepted report")
		}
		if userID > 0 {
			award = r
			if _, err := tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
				userID, award, "bug", id, "Bug report: "+title, now()); err != nil {
				return err
			}
		}
	case "duplicate":
		if dupOf <= 0 || dupOf == id {
			return errors.New("name the report this one duplicates")
		}
		var target int64
		if err := tx.QueryRow("SELECT dup_of FROM bugs WHERE id = ?", dupOf).Scan(&target); err != nil {
			return fmt.Errorf("no report %d", dupOf)
		}
		if target > 0 {
			dupOf = target // merge into the original, never into another duplicate
		}
		severity = ""
	case "rejected":
		severity = ""
	default:
		return errors.New("pick an outcome")
	}
	if status != "duplicate" {
		dupOf = 0
	}
	_, err = tx.Exec("UPDATE bugs SET status = ?, severity = ?, award = ?, dup_of = ?, review_note = ?, reviewed_at = ? WHERE id = ?",
		status, severity, award, dupOf, note, now(), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- the bug page ----------

type bugsData struct {
	Surfaces []struct{ Key, Name string }
	Tiers    []struct {
		Key, Name string
		Reward    int64
		Desc      string
	}
	Mine []*Bug
}

func (a *App) handleBugs(w http.ResponseWriter, r *http.Request) {
	d := bugsData{Surfaces: bugSurfaces, Tiers: bugTiers}
	if user, _, _ := a.currentUser(r); user != nil {
		var err error
		d.Mine, err = queryBugs(a.db, "WHERE b.user_id = ? ORDER BY b.id DESC LIMIT 200", user.ID)
		if err != nil {
			a.serverError(w, err)
			return
		}
	}
	a.render(w, r, "bugs", "Report a bug", d)
}

const (
	maxScreenshot = 2 << 20
	bugsPerDay    = 10
)

// sniffImage accepts the image types a browser renders, judged by the
// bytes, never by the name or the type the client claims.
func sniffImage(b []byte) (string, bool) {
	ct := http.DetectContentType(b)
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct, true
	}
	return "", false
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func (a *App) handleBugSubmit(w http.ResponseWriter, r *http.Request) {
	user, token := a.requireUser(w, r)
	if user == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxScreenshot+64<<10)
	if err := r.ParseMultipartForm(maxScreenshot + 64<<10); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		a.redirect(w, r, "/bugs", "", "That upload is too large: a screenshot can be up to 2 MB.")
		return
	}
	if !checkCSRF(r, token) {
		a.redirect(w, r, "/bugs", "", "That form expired. Try again.")
		return
	}
	if !a.gate(w, r, user, "/bugs") {
		return
	}
	surface := r.FormValue("surface")
	known := false
	for _, s := range bugSurfaces {
		known = known || s.Key == surface
	}
	title := clip(r.FormValue("title"), 160)
	steps, expected, actual := clip(r.FormValue("steps"), 6000), clip(r.FormValue("expected"), 3000), clip(r.FormValue("actual"), 3000)
	if !known || title == "" || steps == "" || actual == "" {
		a.redirect(w, r, "/bugs", "", "Pick where the bug is, give it a title, and describe the steps and what happened.")
		return
	}
	var today int64
	a.db.QueryRow("SELECT COUNT(*) FROM bugs WHERE user_id = ? AND created_at > ?", user.ID, now()-86400).Scan(&today)
	if today >= bugsPerDay {
		a.redirect(w, r, "/bugs", "", fmt.Sprintf("You have filed %d reports today, the daily limit. File the rest tomorrow.", bugsPerDay))
		return
	}
	nb := newBug{UserID: user.ID, Source: "form", Surface: surface, Title: title, Steps: steps, Expected: expected, Actual: actual, IP: clientIP(r)}
	if f, _, err := r.FormFile("screenshot"); err == nil {
		data, _ := io.ReadAll(io.LimitReader(f, maxScreenshot+1))
		f.Close()
		if len(data) > maxScreenshot {
			a.redirect(w, r, "/bugs", "", "That screenshot is over 2 MB. Crop it or save it as JPEG.")
			return
		}
		if len(data) > 0 {
			ct, ok := sniffImage(data)
			if !ok {
				a.redirect(w, r, "/bugs", "", "The screenshot must be a PNG, JPEG, GIF or WebP image.")
				return
			}
			nb.FileType, nb.File = ct, data
		}
	}
	if _, err := createBug(a.db, nb); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/bugs", "Report received. It earns its reward once a reviewer confirms the bug; you can follow it below.", "")
}

// handleBugFile serves a report's screenshot to its reporter and to admins.
func (a *App) handleBugFile(w http.ResponseWriter, r *http.Request) {
	user, _, _ := a.currentUser(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var owner int64
	var ct string
	var data []byte
	err := a.db.QueryRow("SELECT b.user_id, f.content_type, f.data FROM bug_files f JOIN bugs b ON b.id = f.bug_id WHERE f.bug_id = ?", id).
		Scan(&owner, &ct, &data)
	if err != nil || user == nil || (!user.IsAdmin && user.ID != owner) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}

// ---------- the intake: problem reports sent from inside a wallet ----------
//
// POST /api/problem-reports, JSON, anonymous: no cookies, no CSRF, any
// origin. The contract is fixed with the wallets that call it:
//
//	{"v":1, "wallet":"web-wallet"|"ambra-browser"|"ambra-mobile", "version":"…",
//	 "mode":"default"|"developer", "description":"…", "account_code":"…",
//	 "errors":[{"at":"…","message":"…"}], "context":{"platform":"…","user_agent":"…","network":"…"},
//	 "attached":{"addresses":["…"]}}
//
// 201 {"id":N,"account":"linked"|"unknown"|"none"}; 400, 409, 413, 415 and 429
// carry {"error":"a plain sentence"}. Unknown fields are ignored.

const maxIntakeBody = 32 << 10

type intakeReport struct {
	V           int    `json:"v"`
	Wallet      string `json:"wallet"`
	Version     string `json:"version"`
	Mode        string `json:"mode"`
	Description string `json:"description"`
	AccountCode string `json:"account_code"`
	Errors      []struct {
		At      string `json:"at"`
		Message string `json:"message"`
	} `json:"errors"`
	Context  map[string]string `json:"context"`
	Attached struct {
		Addresses []string `json:"addresses"`
	} `json:"attached"`
}

// slidingLimiter counts events per key over several windows at once.
type slidingLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
	rules  []limitRule
}

type limitRule struct {
	per time.Duration
	max int
}

func newSlidingLimiter(rules ...limitRule) *slidingLimiter {
	return &slidingLimiter{events: map[string][]time.Time{}, rules: rules}
}

// allow records an event for key when every rule still has room, and
// otherwise says how long until the tightest rule frees a slot.
func (l *slidingLimiter) allow(key string, at time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	longest := time.Duration(0)
	for _, r := range l.rules {
		if r.per > longest {
			longest = r.per
		}
	}
	ev := l.events[key]
	keep := ev[:0]
	for _, t := range ev {
		if at.Sub(t) < longest {
			keep = append(keep, t)
		}
	}
	ev = keep
	var wait time.Duration
	for _, r := range l.rules {
		n := 0
		var oldest time.Time
		for _, t := range ev {
			if at.Sub(t) < r.per {
				if n == 0 {
					oldest = t
				}
				n++
			}
		}
		if n >= r.max {
			if d := r.per - at.Sub(oldest); d > wait {
				wait = d
			}
		}
	}
	if wait > 0 {
		l.events[key] = ev
		return false, wait
	}
	if len(l.events) > 100000 {
		l.events = map[string][]time.Time{}
	}
	l.events[key] = append(ev, at)
	return true, 0
}

var claimCodeRe = regexp.MustCompile(`^[0-9a-f]{10}$`)

func (a *App) intakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *App) handleIntakeOptions(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleIntake(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) { a.intakeJSON(w, status, map[string]string{"error": msg}) }
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "application/json" {
		fail(http.StatusUnsupportedMediaType, "Send the report as JSON.")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxIntakeBody+1))
	if err != nil {
		fail(http.StatusBadRequest, "The report could not be read.")
		return
	}
	if len(raw) > maxIntakeBody {
		fail(http.StatusRequestEntityTooLarge, "This report is too large.")
		return
	}
	var in intakeReport
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&in); err != nil || in.V != 1 {
		fail(http.StatusBadRequest, "The report is not in a format this site reads.")
		return
	}
	if _, ok := intakeWallets[in.Wallet]; !ok {
		fail(http.StatusBadRequest, "The report does not say which wallet sent it.")
		return
	}
	desc := clip(in.Description, 4000)
	if desc == "" {
		fail(http.StatusBadRequest, "Describe what happened.")
		return
	}
	ip := clientIP(r)
	at := time.Now()
	code := strings.ToLower(strings.TrimSpace(in.AccountCode))
	// A repeat of this address's last report within the hour is answered
	// as received, and neither stored nor counted against the limits.
	hash := bugHash(desc, "", "")
	var dup int64
	a.db.QueryRow("SELECT id FROM bugs WHERE source = 'wallet' AND ip = ? AND body_hash = ? AND created_at > ? ORDER BY id DESC LIMIT 1",
		ip, hash, at.Unix()-3600).Scan(&dup)
	if dup > 0 {
		fail(http.StatusConflict, "This report was already received.")
		return
	}
	if ok, wait := a.intakeIPLimit.allow(ip, at); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(http.StatusTooManyRequests, "Too many reports from here. Try again later.")
		return
	}
	account := "none"
	var userID int64
	if code != "" {
		account = "unknown"
		if claimCodeRe.MatchString(code) && a.db.QueryRow("SELECT id FROM users WHERE claim_code = ?", code).Scan(&userID) == nil {
			if ok, wait := a.intakeCodeLimit.allow(code, at); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				fail(http.StatusTooManyRequests, "Too many reports from here. Try again later.")
				return
			}
			account = "linked"
		} else {
			userID = 0
		}
	}
	// What is kept besides the description, each part bounded.
	meta := map[string]any{"wallet": in.Wallet, "version": clip(in.Version, 40), "mode": clip(in.Mode, 20)}
	if code != "" && account == "unknown" {
		meta["account_code"] = clip(code, 40)
	}
	errs := in.Errors
	if len(errs) > 20 {
		errs = errs[len(errs)-20:]
	}
	var el []map[string]string
	for _, e := range errs {
		el = append(el, map[string]string{"at": clip(e.At, 40), "message": clip(e.Message, 500)})
	}
	if len(el) > 0 {
		meta["errors"] = el
	}
	ctx := map[string]string{}
	for _, k := range []string{"platform", "user_agent", "network"} {
		if v := clip(in.Context[k], 300); v != "" {
			ctx[k] = v
		}
	}
	if len(ctx) > 0 {
		meta["context"] = ctx
	}
	addrs := in.Attached.Addresses
	if len(addrs) > 20 {
		addrs = addrs[:20]
	}
	var al []string
	for _, s := range addrs {
		if s = clip(s, 120); s != "" {
			al = append(al, s)
		}
	}
	if len(al) > 0 {
		meta["addresses"] = al
	}
	mb, _ := json.Marshal(meta)
	title := strings.SplitN(desc, "\n", 2)[0]
	if len(title) > 80 {
		title = title[:80] + "…"
	}
	id, err := createBug(a.db, newBug{UserID: userID, Source: "wallet", Surface: in.Wallet, Title: title, Steps: desc, Meta: string(mb), IP: ip})
	if err != nil {
		a.intakeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The report could not be stored. Try again in a moment."})
		return
	}
	a.intakeJSON(w, http.StatusCreated, map[string]any{"id": id, "account": account})
}

// bugMeta unpacks a wallet report's context for the admin view.
type bugMetaView struct {
	Wallet, Version, Mode, AccountCode string
	Errors                             []map[string]string
	Context                            map[string]string
	Addresses                          []string
}

func parseBugMeta(s string) *bugMetaView {
	if s == "" {
		return nil
	}
	var m struct {
		Wallet      string              `json:"wallet"`
		Version     string              `json:"version"`
		Mode        string              `json:"mode"`
		AccountCode string              `json:"account_code"`
		Errors      []map[string]string `json:"errors"`
		Context     map[string]string   `json:"context"`
		Addresses   []string            `json:"addresses"`
	}
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return &bugMetaView{Wallet: intakeWallets[m.Wallet], Version: m.Version, Mode: m.Mode, AccountCode: m.AccountCode,
		Errors: m.Errors, Context: m.Context, Addresses: m.Addresses}
}

// ---------- admin ----------

func (a *App) handleAdminBugs(w http.ResponseWriter, r *http.Request) {
	where := "ORDER BY b.status != 'new', b.id DESC LIMIT 300"
	bugs, err := queryBugs(a.db, where)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_bugs", "Bug reports", bugs)
}

type adminBugData struct {
	Bug   *Bug
	Meta  *bugMetaView
	Tiers []struct {
		Key, Name string
		Reward    int64
		Desc      string
	}
}

func (a *App) handleAdminBug(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := bugByID(a.db, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "admin_bug", "Bug: "+b.Title, adminBugData{Bug: b, Meta: parseBugMeta(b.Meta), Tiers: bugTiers})
}

func (a *App) handleAdminBugReview(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/bugs/%d", id)
	status := r.FormValue("status")
	dupOf, _ := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(r.FormValue("dup_of")), "#"), 10, 64)
	note := strings.TrimSpace(r.FormValue("note"))
	if err := reviewBug(a.db, id, status, r.FormValue("severity"), dupOf, note); err != nil {
		a.redirect(w, r, back, "", err.Error())
		return
	}
	if status == "accepted" {
		var userID int64
		if a.db.QueryRow("SELECT user_id FROM bugs WHERE id = ?", id).Scan(&userID) == nil && userID > 0 {
			a.processReferrals(userID)
		}
	}
	a.redirect(w, r, "/admin/bugs", fmt.Sprintf("Bug report %d marked %s.", id, status), "")
}
