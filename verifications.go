package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Social-account verification: a non-KYC farming deterrent. Users prove they
// own a Telegram, X or Reddit account that is at least verifMinAgeYears old
// by putting their public account code in the profile's bio, then submitting
// the handle. A social account can vouch for exactly one Emissio account,
// ever (unique index), so a farmer needs a distinct aged account per profile.
const (
	verificationBonus int64 = 15
	verifMinAgeYears        = 2
)

var verifPlatforms = []struct {
	Key   string
	Name  string
	Hint  string
	URLFmt string
}{
	{"telegram", "Telegram", "Add your account code to your Telegram bio (Settings, Bio), then submit your public @username. The bio is checked automatically; account age is assessed by the reviewer.", "https://t.me/%s"},
	{"x", "X", "Add your account code to your X bio, then submit your handle. A reviewer checks the bio and that the profile's join date is at least two years ago.", "https://x.com/%s"},
	{"reddit", "Reddit", "Add your account code to your Reddit profile's public description (Profile, Edit), then submit your username. Ownership and account age are checked automatically.", "https://www.reddit.com/user/%s"},
}

var handleRe = regexp.MustCompile(`^@?[A-Za-z0-9_.\-]{2,32}$`)

type Verification struct {
	ID         int64
	UserID     int64
	Platform   string
	Handle     string
	Status     string
	CheckNote  string
	ReviewNote string
	CreatedAt  int64
	ReviewedAt int64
	UserEmail  string
	ClaimCode  string
}

func verificationsOf(db *sql.DB, userID int64) ([]*Verification, error) {
	return queryVerifications(db, "WHERE v.user_id = ?", userID)
}

func pendingVerifications(db *sql.DB) ([]*Verification, error) {
	return queryVerifications(db, "WHERE v.status = 'pending'")
}

func queryVerifications(db *sql.DB, where string, args ...any) ([]*Verification, error) {
	rows, err := db.Query(`SELECT v.id, v.user_id, v.platform, v.handle, v.status, v.check_note, v.review_note,
		v.created_at, v.reviewed_at, u.email, u.claim_code
		FROM verifications v JOIN users u ON u.id = v.user_id `+where+` ORDER BY v.id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Verification
	for rows.Next() {
		var v Verification
		if err := rows.Scan(&v.ID, &v.UserID, &v.Platform, &v.Handle, &v.Status, &v.CheckNote, &v.ReviewNote,
			&v.CreatedAt, &v.ReviewedAt, &v.UserEmail, &v.ClaimCode); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// reviewVerification approves or rejects a pending verification. Approval
// credits the bonus in the same transaction; the unique indexes guarantee one
// credit per verification and one Emissio account per social account.
func reviewVerification(db *sql.DB, verifID int64, approve bool, note string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID int64
	var status, platform string
	if err := tx.QueryRow("SELECT user_id, status, platform FROM verifications WHERE id = ?", verifID).
		Scan(&userID, &status, &platform); err != nil {
		return err
	}
	if status != "pending" {
		return fmt.Errorf("verification %d is already %s", verifID, status)
	}
	newStatus := "rejected"
	if approve {
		newStatus = "verified"
		_, err = tx.Exec("INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)",
			userID, verificationBonus, "verification", verifID, "Verified "+platformName(platform)+" account", now())
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE verifications SET status = ?, review_note = ?, reviewed_at = ? WHERE id = ?",
		newStatus, note, now(), verifID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func platformName(key string) string {
	for _, p := range verifPlatforms {
		if p.Key == key {
			return p.Name
		}
	}
	return key
}

func platformProfileURL(key, handle string) string {
	for _, p := range verifPlatforms {
		if p.Key == key {
			return fmt.Sprintf(p.URLFmt, handle)
		}
	}
	return ""
}

// ---------- automatic checks (advisory; the reviewer decides) ----------

func (a *App) verifCheck(platform, handle, claimCode string) string {
	switch platform {
	case "reddit":
		return checkReddit(a.cfg.RedditBase, handle, claimCode)
	case "telegram":
		if a.cfg.TgBotToken != "" {
			return a.checkTelegramBot(handle, claimCode)
		}
		return checkTelegram(a.cfg.TelegramBase, handle, claimCode)
	default:
		return "no automatic check for X; open the profile, confirm the bio contains the account code and the join date is at least two years ago"
	}
}

// telegramHint describes the flow that is actually active.
func (a *App) telegramHint() string {
	if a.cfg.TgBotToken != "" {
		name := a.cfg.TgBotName
		if name == "" {
			name = "our verification bot"
		}
		return "Send your account code as a Telegram message to @" + name + ", then submit your public @username. Ownership and account age (estimated from your Telegram ID) are checked automatically."
	}
	return "Add your account code to your Telegram bio (Settings, Bio), then submit your public @username. The bio is checked automatically; account age is assessed by the reviewer."
}

var verifClient = &http.Client{Timeout: 8 * time.Second}

// checkReddit verifies ownership (account code in the profile's public
// description) and age (created_utc) in one about.json fetch.
func checkReddit(base, handle, claimCode string) string {
	req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/user/"+handle+"/about.json", nil)
	if err != nil {
		return "check failed; manual review"
	}
	req.Header.Set("User-Agent", "emissio-verifier/1.0 (sequentiatestnet.com)")
	resp, err := verifClient.Do(req)
	if err != nil {
		return "reddit unreachable; manual review"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "reddit user NOT FOUND"
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("reddit returned HTTP %d; manual review", resp.StatusCode)
	}
	var about struct {
		Data struct {
			CreatedUTC float64 `json:"created_utc"`
			Subreddit  struct {
				PublicDescription string `json:"public_description"`
			} `json:"subreddit"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&about); err != nil {
		return "reddit response unreadable; manual review"
	}
	created := time.Unix(int64(about.Data.CreatedUTC), 0)
	ageOK := time.Since(created) >= time.Duration(verifMinAgeYears) * 365 * 24 * time.Hour
	owned := strings.Contains(about.Data.Subreddit.PublicDescription, claimCode)
	note := fmt.Sprintf("account created %s (age %s)", created.Format("Jan 2006"), map[bool]string{true: "OK", false: "UNDER " + fmt.Sprint(verifMinAgeYears) + " YEARS"}[ageOK])
	if owned {
		return "code found in profile description; " + note
	}
	return "code NOT found in profile description; " + note
}

// checkTelegram verifies ownership via the public t.me profile page, which
// renders the bio server-side. Telegram does not expose account age.
func checkTelegram(base, handle, claimCode string) string {
	req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/"+handle, nil)
	if err != nil {
		return "check failed; manual review"
	}
	req.Header.Set("User-Agent", "emissio-verifier/1.0 (sequentiatestnet.com)")
	resp, err := verifClient.Do(req)
	if err != nil {
		return "t.me unreachable; manual review"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("t.me returned HTTP %d; manual review", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "t.me response unreadable; manual review"
	}
	if strings.Contains(string(body), claimCode) {
		return "code found in the public profile page; age at reviewer discretion (Telegram does not publish it)"
	}
	return "code NOT found on the public profile page (bio not set, not public, or username wrong)"
}
