package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	email TEXT UNIQUE NOT NULL,
	pass_hash TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	claim_code TEXT UNIQUE NOT NULL,
	is_admin INTEGER NOT NULL DEFAULT 0,
	mainnet_address TEXT NOT NULL DEFAULT '',
	address_updated_at INTEGER NOT NULL DEFAULT 0,
	referred_by INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
	id INTEGER PRIMARY KEY,
	slug TEXT UNIQUE NOT NULL,
	title TEXT NOT NULL,
	category TEXT NOT NULL DEFAULT 'testnet',
	body TEXT NOT NULL DEFAULT '',
	reward INTEGER NOT NULL,
	cap INTEGER NOT NULL DEFAULT 0,
	needs_txid INTEGER NOT NULL DEFAULT 1,
	active INTEGER NOT NULL DEFAULT 1,
	sort INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS submissions (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	task_id INTEGER NOT NULL REFERENCES tasks(id),
	txid TEXT NOT NULL DEFAULT '',
	notes TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	chain_note TEXT NOT NULL DEFAULT '',
	review_note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS submissions_user ON submissions(user_id);
CREATE INDEX IF NOT EXISTS submissions_status ON submissions(status);
CREATE TABLE IF NOT EXISTS competitions (
	id INTEGER PRIMARY KEY,
	slug TEXT UNIQUE NOT NULL,
	title TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	prizes TEXT NOT NULL DEFAULT '',
	closes_at INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'open'
);
CREATE TABLE IF NOT EXISTS entries (
	id INTEGER PRIMARY KEY,
	comp_id INTEGER NOT NULL REFERENCES competitions(id),
	user_id INTEGER NOT NULL REFERENCES users(id),
	url TEXT NOT NULL DEFAULT '',
	notes TEXT NOT NULL DEFAULT '',
	place INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(comp_id, user_id)
);
CREATE TABLE IF NOT EXISTS reports (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	title TEXT NOT NULL,
	severity TEXT NOT NULL DEFAULT 'low',
	body TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'new',
	award INTEGER NOT NULL DEFAULT 0,
	review_note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ledger (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	amount INTEGER NOT NULL,
	kind TEXT NOT NULL,
	ref_id INTEGER NOT NULL DEFAULT 0,
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ledger_user ON ledger(user_id);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(kind, ref_id)
	WHERE kind IN ('submission', 'entry', 'report');
CREATE UNIQUE INDEX IF NOT EXISTS ledger_referral ON ledger(kind, ref_id)
	WHERE kind IN ('referral', 'referral-welcome');
CREATE TABLE IF NOT EXISTS verifications (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	platform TEXT NOT NULL,
	handle TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	check_note TEXT NOT NULL DEFAULT '',
	review_note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS verifications_user ON verifications(user_id);
-- A social account vouches for exactly one Emissio account, ever.
CREATE UNIQUE INDEX IF NOT EXISTS verifications_unique ON verifications(platform, handle)
	WHERE status = 'verified';
CREATE UNIQUE INDEX IF NOT EXISTS ledger_verification ON ledger(kind, ref_id)
	WHERE kind = 'verification';
`

func mustOpenDB(path string) *sql.DB {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		panic(err)
	}
	// Migration for databases created before referrals existed. On fresh
	// databases the column is already in CREATE TABLE and this errors, which
	// is fine.
	db.Exec("ALTER TABLE users ADD COLUMN referred_by INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE users ADD COLUMN reg_ip TEXT NOT NULL DEFAULT ''")
	return db
}

func now() int64 { return time.Now().Unix() }

type User struct {
	ID               int64
	Email            string
	PassHash         string
	DisplayName      string
	ClaimCode        string
	IsAdmin          bool
	MainnetAddress   string
	AddressUpdatedAt int64
	ReferredBy       int64
	RegIP            string
	CreatedAt        int64
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PassHash, &u.DisplayName, &u.ClaimCode,
		&u.IsAdmin, &u.MainnetAddress, &u.AddressUpdatedAt, &u.ReferredBy, &u.RegIP, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

const userCols = "id, email, pass_hash, display_name, claim_code, is_admin, mainnet_address, address_updated_at, referred_by, reg_ip, created_at"

func getUserByEmail(db *sql.DB, email string) (*User, error) {
	return scanUser(db.QueryRow("SELECT "+userCols+" FROM users WHERE email = ?", email))
}

func getUserByID(db *sql.DB, id int64) (*User, error) {
	return scanUser(db.QueryRow("SELECT "+userCols+" FROM users WHERE id = ?", id))
}

func createUser(db *sql.DB, email, passHash, claimCode, regIP string) (int64, error) {
	res, err := db.Exec("INSERT INTO users (email, pass_hash, claim_code, reg_ip, created_at) VALUES (?,?,?,?,?)",
		email, passHash, claimCode, regIP, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func balanceOf(db *sql.DB, userID int64) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = ?", userID).Scan(&v)
	return v.Int64, err
}

type LedgerEntry struct {
	ID        int64
	Amount    int64
	Kind      string
	Note      string
	CreatedAt int64
}

func ledgerOf(db *sql.DB, userID int64) ([]LedgerEntry, error) {
	rows, err := db.Query("SELECT id, amount, kind, note, created_at FROM ledger WHERE user_id = ? ORDER BY id DESC LIMIT 200", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.Amount, &e.Kind, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type Task struct {
	ID        int64
	Slug      string
	Title     string
	Category  string
	Body      string
	Reward    int64
	Cap       int64
	NeedsTxid bool
	Active    bool
	Sort      int64
	Awarded   int64 // approved submissions so far
}

const taskCols = "t.id, t.slug, t.title, t.category, t.body, t.reward, t.cap, t.needs_txid, t.active, t.sort"

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Slug, &t.Title, &t.Category, &t.Body, &t.Reward,
		&t.Cap, &t.NeedsTxid, &t.Active, &t.Sort, &t.Awarded)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func listTasks(db *sql.DB, includeInactive bool) ([]*Task, error) {
	where := "WHERE t.active = 1"
	if includeInactive {
		where = ""
	}
	rows, err := db.Query(`SELECT ` + taskCols + `,
		(SELECT COUNT(*) FROM submissions s WHERE s.task_id = t.id AND s.status = 'approved')
		FROM tasks t ` + where + ` ORDER BY t.sort, t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func taskBySlug(db *sql.DB, slug string) (*Task, error) {
	return scanTask(db.QueryRow(`SELECT `+taskCols+`,
		(SELECT COUNT(*) FROM submissions s WHERE s.task_id = t.id AND s.status = 'approved')
		FROM tasks t WHERE t.slug = ?`, slug))
}

func taskByID(db *sql.DB, id int64) (*Task, error) {
	return scanTask(db.QueryRow(`SELECT `+taskCols+`,
		(SELECT COUNT(*) FROM submissions s WHERE s.task_id = t.id AND s.status = 'approved')
		FROM tasks t WHERE t.id = ?`, id))
}

type Submission struct {
	ID         int64
	UserID     int64
	TaskID     int64
	Txid       string
	Notes      string
	Status     string
	ChainNote  string
	ReviewNote string
	CreatedAt  int64
	ReviewedAt int64
	// joined
	TaskTitle  string
	TaskSlug   string
	TaskReward int64
	UserEmail  string
	ClaimCode  string
	VerifCount int64 // submitter's verified platforms (pending queue only)
}

// latestSubmission returns the user's most recent submission for a task, or nil.
func latestSubmission(db *sql.DB, userID, taskID int64) (*Submission, error) {
	var s Submission
	err := db.QueryRow(`SELECT id, user_id, task_id, txid, notes, status, chain_note, review_note, created_at, reviewed_at
		FROM submissions WHERE user_id = ? AND task_id = ? ORDER BY id DESC LIMIT 1`, userID, taskID).
		Scan(&s.ID, &s.UserID, &s.TaskID, &s.Txid, &s.Notes, &s.Status, &s.ChainNote, &s.ReviewNote, &s.CreatedAt, &s.ReviewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func submissionsOf(db *sql.DB, userID int64) ([]*Submission, error) {
	rows, err := db.Query(`SELECT s.id, s.user_id, s.task_id, s.txid, s.notes, s.status, s.chain_note, s.review_note,
		s.created_at, s.reviewed_at, t.title, t.slug, t.reward
		FROM submissions s JOIN tasks t ON t.id = s.task_id
		WHERE s.user_id = ? ORDER BY s.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Submission
	for rows.Next() {
		var s Submission
		if err := rows.Scan(&s.ID, &s.UserID, &s.TaskID, &s.Txid, &s.Notes, &s.Status, &s.ChainNote, &s.ReviewNote,
			&s.CreatedAt, &s.ReviewedAt, &s.TaskTitle, &s.TaskSlug, &s.TaskReward); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

func pendingSubmissions(db *sql.DB) ([]*Submission, error) {
	rows, err := db.Query(`SELECT s.id, s.user_id, s.task_id, s.txid, s.notes, s.status, s.chain_note, s.review_note,
		s.created_at, s.reviewed_at, t.title, t.slug, t.reward, u.email, u.claim_code,
		(SELECT COUNT(*) FROM verifications v WHERE v.user_id = s.user_id AND v.status = 'verified')
		FROM submissions s JOIN tasks t ON t.id = s.task_id JOIN users u ON u.id = s.user_id
		WHERE s.status = 'pending' ORDER BY s.id LIMIT 200`)
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

// reviewSubmission approves or rejects a pending submission. Approval credits
// the ledger inside the same transaction; the partial unique index on
// ledger(kind, ref_id) makes double-credits impossible.
func reviewSubmission(db *sql.DB, subID int64, approve bool, note string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, taskID int64
	var status string
	err = tx.QueryRow("SELECT user_id, task_id, status FROM submissions WHERE id = ?", subID).
		Scan(&userID, &taskID, &status)
	if err != nil {
		return err
	}
	if status != "pending" {
		return fmt.Errorf("submission %d is already %s", subID, status)
	}
	newStatus := "rejected"
	if approve {
		newStatus = "approved"
		var reward int64
		var title string
		if err := tx.QueryRow("SELECT reward, title FROM tasks WHERE id = ?", taskID).Scan(&reward, &title); err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
			userID, reward, "submission", subID, "Task: "+title, now())
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE submissions SET status = ?, review_note = ?, reviewed_at = ? WHERE id = ?",
		newStatus, note, now(), subID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type Competition struct {
	ID       int64
	Slug     string
	Title    string
	Body     string
	Prizes   string // comma-separated SEQ amounts for 1st, 2nd, ...
	ClosesAt int64
	Status   string
	Entries  int64
}

const compCols = "c.id, c.slug, c.title, c.body, c.prizes, c.closes_at, c.status"

func scanComp(row interface{ Scan(...any) error }) (*Competition, error) {
	var c Competition
	err := row.Scan(&c.ID, &c.Slug, &c.Title, &c.Body, &c.Prizes, &c.ClosesAt, &c.Status, &c.Entries)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func listCompetitions(db *sql.DB) ([]*Competition, error) {
	rows, err := db.Query(`SELECT ` + compCols + `,
		(SELECT COUNT(*) FROM entries e WHERE e.comp_id = c.id)
		FROM competitions c ORDER BY c.status = 'open' DESC, c.closes_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Competition
	for rows.Next() {
		c, err := scanComp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func compBySlug(db *sql.DB, slug string) (*Competition, error) {
	return scanComp(db.QueryRow(`SELECT `+compCols+`,
		(SELECT COUNT(*) FROM entries e WHERE e.comp_id = c.id)
		FROM competitions c WHERE c.slug = ?`, slug))
}

type Entry struct {
	ID        int64
	CompID    int64
	UserID    int64
	URL       string
	Notes     string
	Place     int64
	CreatedAt int64
	UpdatedAt int64
	UserEmail string
}

func entryOf(db *sql.DB, compID, userID int64) (*Entry, error) {
	var e Entry
	err := db.QueryRow(`SELECT id, comp_id, user_id, url, notes, place, created_at, updated_at
		FROM entries WHERE comp_id = ? AND user_id = ?`, compID, userID).
		Scan(&e.ID, &e.CompID, &e.UserID, &e.URL, &e.Notes, &e.Place, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func upsertEntry(db *sql.DB, compID, userID int64, url, notes string) error {
	_, err := db.Exec(`INSERT INTO entries (comp_id, user_id, url, notes, created_at, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(comp_id, user_id) DO UPDATE SET url = excluded.url, notes = excluded.notes, updated_at = excluded.updated_at`,
		compID, userID, url, notes, now(), now())
	return err
}

func entriesOf(db *sql.DB, compID int64) ([]*Entry, error) {
	rows, err := db.Query(`SELECT e.id, e.comp_id, e.user_id, e.url, e.notes, e.place, e.created_at, e.updated_at, u.email
		FROM entries e JOIN users u ON u.id = e.user_id WHERE e.comp_id = ? ORDER BY e.id`, compID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.CompID, &e.UserID, &e.URL, &e.Notes, &e.Place, &e.CreatedAt, &e.UpdatedAt, &e.UserEmail); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// awardEntry assigns a place to an entry and credits the prize.
func awardEntry(db *sql.DB, entryID int64, place int64, prize int64, compTitle string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, existing int64
	if err := tx.QueryRow("SELECT user_id, place FROM entries WHERE id = ?", entryID).Scan(&userID, &existing); err != nil {
		return err
	}
	if existing != 0 {
		return fmt.Errorf("entry %d already placed", entryID)
	}
	if _, err := tx.Exec("UPDATE entries SET place = ? WHERE id = ?", place, entryID); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
		userID, prize, "entry", entryID, fmt.Sprintf("Competition: %s (place %d)", compTitle, place), now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

type Report struct {
	ID         int64
	UserID     int64
	Title      string
	Severity   string
	Body       string
	Status     string
	Award      int64
	ReviewNote string
	CreatedAt  int64
	ReviewedAt int64
	UserEmail  string
}

func createReport(db *sql.DB, userID int64, title, severity, body string) (int64, error) {
	res, err := db.Exec("INSERT INTO reports (user_id, title, severity, body, created_at) VALUES (?,?,?,?,?)",
		userID, title, severity, body, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func reportsOf(db *sql.DB, userID int64) ([]*Report, error) {
	return queryReports(db, "WHERE r.user_id = ?", userID)
}

func allReports(db *sql.DB) ([]*Report, error) {
	return queryReports(db, "")
}

func queryReports(db *sql.DB, where string, args ...any) ([]*Report, error) {
	rows, err := db.Query(`SELECT r.id, r.user_id, r.title, r.severity, r.body, r.status, r.award, r.review_note,
		r.created_at, r.reviewed_at, u.email
		FROM reports r JOIN users u ON u.id = r.user_id `+where+` ORDER BY r.id DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Report
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.UserID, &r.Title, &r.Severity, &r.Body, &r.Status, &r.Award, &r.ReviewNote,
			&r.CreatedAt, &r.ReviewedAt, &r.UserEmail); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func reportByID(db *sql.DB, id int64) (*Report, error) {
	rs, err := queryReports(db, "WHERE r.id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, sql.ErrNoRows
	}
	return rs[0], nil
}

// reviewReport sets a report's outcome. status "accepted" with award > 0
// credits the ledger in the same transaction.
func reviewReport(db *sql.DB, reportID int64, status string, award int64, note string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID int64
	var current, title string
	if err := tx.QueryRow("SELECT user_id, status, title FROM reports WHERE id = ?", reportID).Scan(&userID, &current, &title); err != nil {
		return err
	}
	if current != "new" {
		return fmt.Errorf("report %d is already %s", reportID, current)
	}
	if status == "accepted" && award > 0 {
		_, err = tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
			userID, award, "report", reportID, "Security report: "+title, now())
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE reports SET status = ?, award = ?, review_note = ?, reviewed_at = ? WHERE id = ?",
		status, award, note, now(), reportID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type Stats struct {
	Users         int64
	Distributed   int64
	Pending       int64
	OpenReports   int64
	PendingVerifs int64
}

func loadStats(db *sql.DB) (Stats, error) {
	var s Stats
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&s.Users); err != nil {
		return s, err
	}
	var d sql.NullInt64
	if err := db.QueryRow("SELECT SUM(amount) FROM ledger WHERE amount > 0").Scan(&d); err != nil {
		return s, err
	}
	s.Distributed = d.Int64
	if err := db.QueryRow("SELECT COUNT(*) FROM submissions WHERE status = 'pending'").Scan(&s.Pending); err != nil {
		return s, err
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM reports WHERE status = 'new'").Scan(&s.OpenReports); err != nil {
		return s, err
	}
	err := db.QueryRow("SELECT COUNT(*) FROM verifications WHERE status = 'pending'").Scan(&s.PendingVerifs)
	return s, err
}

type UserRow struct {
	User
	Balance    int64
	IPPeers    int64 // accounts registered from the same IP, including this one
	VerifCount int64 // verified platforms; payout needs at least one
}

func listUsers(db *sql.DB, limit int) ([]*UserRow, error) {
	rows, err := db.Query(`SELECT `+userCols+`,
		COALESCE((SELECT SUM(amount) FROM ledger l WHERE l.user_id = users.id), 0),
		CASE WHEN users.reg_ip = '' THEN 1 ELSE
			(SELECT COUNT(*) FROM users u2 WHERE u2.reg_ip = users.reg_ip) END
		FROM users ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserRow
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Email, &u.PassHash, &u.DisplayName, &u.ClaimCode,
			&u.IsAdmin, &u.MainnetAddress, &u.AddressUpdatedAt, &u.ReferredBy, &u.RegIP, &u.CreatedAt, &u.Balance, &u.IPPeers); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// allocationRows returns every user with a positive balance for the mainnet
// genesis allocation export, with their verified-platform count: payout
// requires at least one verified platform, so the payout tooling filters on
// that column.
func allocationRows(db *sql.DB) ([]*UserRow, error) {
	rows, err := db.Query(`SELECT ` + userCols + `,
		COALESCE((SELECT SUM(amount) FROM ledger l WHERE l.user_id = users.id), 0) AS bal,
		(SELECT COUNT(*) FROM verifications v WHERE v.user_id = users.id AND v.status = 'verified')
		FROM users WHERE bal > 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserRow
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Email, &u.PassHash, &u.DisplayName, &u.ClaimCode,
			&u.IsAdmin, &u.MainnetAddress, &u.AddressUpdatedAt, &u.ReferredBy, &u.RegIP, &u.CreatedAt,
			&u.Balance, &u.VerifCount); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}
