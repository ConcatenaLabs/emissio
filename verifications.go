package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Social-account verification: a non-KYC farming deterrent. Users prove they
// own a Telegram, X or Reddit account that is at least verifMinAgeYears old
// by putting their public account code where only the owner can: the bio
// for Telegram and Reddit, a post for X. The handle is what the unique index
// keys on, so a social account vouches for exactly one Emissio account,
// ever, and a farmer needs a distinct aged account per profile. For X the
// handle is derived from the post link rather than typed, so the account
// that is locked is the one that published the code.
const (
	verificationBonus int64 = 15
	verifMinAgeYears        = 2
)

var verifPlatforms = []struct {
	Key    string
	Name   string
	Hint   string
	URLFmt string
}{
	{"telegram", "Telegram", "Add your account code to your Telegram bio (Settings, Bio), then submit your public @username. The bio is checked automatically; account age is assessed by the reviewer.", "https://t.me/%s"},
	{"x", "X", "Publish a post from your X account containing your account code, then submit the link to that post. Ownership and account age are checked automatically from the post; you can delete it once verified.", "https://x.com/%s"},
	{"reddit", "Reddit", "", "https://www.reddit.com/user/%s"},
}

var handleRe = regexp.MustCompile(`^@?[A-Za-z0-9_.\-]{2,32}$`)

// xPostRe matches a link to a post on X: the handle and the numeric post id
// are the two captures. x.com and twitter.com are the same site.
var xPostRe = regexp.MustCompile(`^(?:https?://)?(?:www\.|mobile\.)?(?:x|twitter)\.com/([A-Za-z0-9_]{1,15})/status/([0-9]{5,25})(?:[/?#].*)?$`)

// parseXPost returns the handle and canonical URL of a post link, or ok=false.
func parseXPost(link string) (handle, canonical string, ok bool) {
	m := xPostRe.FindStringSubmatch(strings.TrimSpace(link))
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), "https://x.com/" + m[1] + "/status/" + m[2], true
}

// xPostID returns the numeric id of a canonical post URL.
func xPostID(canonical string) string {
	return canonical[strings.LastIndex(canonical, "/")+1:]
}

type Verification struct {
	ID         int64
	UserID     int64
	Platform   string
	Handle     string
	Evidence   string
	Status     string
	CheckNote  string
	ReviewNote string
	CreatedAt  int64
	ReviewedAt int64
	UserEmail  string
	ClaimCode  string
}

// verifiedCount returns how many platforms the user has verified. At least
// one is required for the launch payout and for referral rewards.
func verifiedCount(db *sql.DB, userID int64) (int64, error) {
	var n int64
	err := db.QueryRow("SELECT COUNT(*) FROM verifications WHERE user_id = ? AND status = 'verified'", userID).Scan(&n)
	return n, err
}

func verificationsOf(db *sql.DB, userID int64) ([]*Verification, error) {
	return queryVerifications(db, "WHERE v.user_id = ?", userID)
}

func pendingVerifications(db *sql.DB) ([]*Verification, error) {
	return queryVerifications(db, "WHERE v.status = 'pending'")
}

func queryVerifications(db *sql.DB, where string, args ...any) ([]*Verification, error) {
	rows, err := db.Query(`SELECT v.id, v.user_id, v.platform, v.handle, v.evidence, v.status, v.check_note, v.review_note,
		v.created_at, v.reviewed_at, u.email, u.claim_code
		FROM verifications v JOIN users u ON u.id = v.user_id `+where+` ORDER BY v.id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Verification
	for rows.Next() {
		var v Verification
		if err := rows.Scan(&v.ID, &v.UserID, &v.Platform, &v.Handle, &v.Evidence, &v.Status, &v.CheckNote, &v.ReviewNote,
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

func (a *App) verifCheck(platform, handle, evidence, claimCode string) string {
	switch platform {
	case "reddit":
		if a.cfg.RedditID == "" || a.cfg.RedditSecret == "" {
			return "reddit blocks unauthenticated checks and no API credentials are configured; manual review"
		}
		return checkReddit(a.cfg.RedditBase, a.cfg.RedditOAuth, a.cfg.RedditID, a.cfg.RedditSecret, handle, claimCode)
	case "telegram":
		if a.cfg.TgBotToken != "" {
			return a.checkTelegramBot(handle, claimCode)
		}
		return checkTelegram(a.cfg.TelegramBase, handle, claimCode)
	case "x":
		return checkX(a.cfg.XBase, handle, xPostID(evidence), claimCode)
	default:
		return "no automatic check; manual review"
	}
}

// redditHint says whether the check is automatic, which needs API
// credentials because Reddit blocks unauthenticated requests.
func (a *App) redditHint() string {
	if a.cfg.RedditID != "" && a.cfg.RedditSecret != "" {
		return "Add your account code to your Reddit profile's public description (Profile, Edit), then submit your username. Ownership and account age are checked automatically."
	}
	return "Add your account code to your Reddit profile's public description (Profile, Edit), then submit your username. A reviewer checks the description and that the account is at least two years old."
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

// redditToken fetches an application-only access token. Reddit refuses
// unauthenticated requests, so every check goes through the API with the
// registered app's credentials.
func redditToken(base, id, secret string) (string, error) {
	req, err := http.NewRequest("POST", strings.TrimRight(base, "/")+"/api/v1/access_token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "emissio-verifier/1.0 (sequentiatestnet.com)")
	resp, err := verifClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("token unreadable")
	}
	return tok.AccessToken, nil
}

// checkReddit verifies ownership (account code in the profile's public
// description) and age (created_utc) in one about fetch through the API.
// base is where tokens are issued, api where the profile is read.
func checkReddit(base, api, id, secret, handle, claimCode string) string {
	token, err := redditToken(base, id, secret)
	if err != nil {
		return "reddit token failed (" + err.Error() + "); manual review"
	}
	req, err := http.NewRequest("GET", strings.TrimRight(api, "/")+"/user/"+handle+"/about", nil)
	if err != nil {
		return "check failed; manual review"
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
	ageOK := time.Since(created) >= time.Duration(verifMinAgeYears)*365*24*time.Hour
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

// xSnowflakeEpoch is the millisecond epoch of X's snowflake ids. Post ids
// have always used it; account ids have used it since 2016, and every
// account id below xSequentialMax predates that, which is older than any
// age bar this program sets.
const (
	xSnowflakeEpoch int64  = 1288834974657
	xSequentialMax  uint64 = 10_000_000_000
)

// xAccountCreated returns when the account with this id was created, and
// whether the id encodes a time at all.
func xAccountCreated(id uint64) (time.Time, bool) {
	if id < xSequentialMax {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(id>>22) + xSnowflakeEpoch), true
}

// xToken reproduces the token the embedded-post widget sends with each
// request: ((id / 1e15) * pi) in base 36, without the dot and the zeros.
func xToken(postID string) string {
	id, _ := strconv.ParseFloat(postID, 64)
	x := id / 1e15 * math.Pi
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	whole := uint64(x)
	frac := x - float64(whole)
	var b strings.Builder
	if whole == 0 {
		b.WriteByte('0')
	}
	var w []byte
	for whole > 0 {
		w = append([]byte{digits[whole%36]}, w...)
		whole /= 36
	}
	b.Write(w)
	for i := 0; i < 12; i++ {
		frac *= 36
		d := int(frac)
		b.WriteByte(digits[d])
		frac -= float64(d)
	}
	return strings.NewReplacer("0", "", ".", "").Replace(b.String())
}

// checkX verifies an X post in one fetch of the keyless endpoint that
// renders embedded posts: the author must be the handle from the link (X
// shows a post under any handle in the URL, so this is the check that
// stops someone claiming another person's account), the text must contain
// the account code, and the author's id gives the account age.
func checkX(base, handle, postID, claimCode string) string {
	req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/tweet-result?id="+postID+"&token="+xToken(postID)+"&lang=en", nil)
	if err != nil {
		return "check failed; manual review"
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; emissio-verifier/1.0; sequentiatestnet.com)")
	resp, err := verifClient.Do(req)
	if err != nil {
		return "x unreachable; manual review"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "post NOT FOUND (deleted, protected account, or wrong link)"
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("x returned HTTP %d; manual review", resp.StatusCode)
	}
	var post struct {
		Type string `json:"__typename"`
		Text string `json:"text"`
		User struct {
			ScreenName string `json:"screen_name"`
			ID         string `json:"id_str"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&post); err != nil {
		return "x response unreadable; manual review"
	}
	if post.Type != "Tweet" || post.User.ScreenName == "" {
		return "post unavailable (deleted, protected account, or wrong link)"
	}
	if !strings.EqualFold(post.User.ScreenName, handle) {
		return "post is by @" + post.User.ScreenName + ", NOT @" + handle + ": someone else's post"
	}
	var note string
	id, err := strconv.ParseUint(post.User.ID, 10, 64)
	if err != nil {
		note = "account age unknown; manual review"
	} else if created, ok := xAccountCreated(id); !ok {
		note = "account id predates 2016 (age OK)"
	} else {
		ageOK := time.Since(created) >= time.Duration(verifMinAgeYears)*365*24*time.Hour
		note = fmt.Sprintf("account created %s (age %s)", created.Format("Jan 2006"), map[bool]string{true: "OK", false: "UNDER " + fmt.Sprint(verifMinAgeYears) + " YEARS"}[ageOK])
	}
	if strings.Contains(post.Text, claimCode) {
		return "post by @" + post.User.ScreenName + " contains the code; " + note
	}
	return "code NOT found in the post; " + note
}
