package main

// The verification bot. One poller reads the bot's messages as they arrive
// (the Bot API's getUpdates, long-polled) and confirms each by asking for
// the next one: the offset of the last message handled is kept in the
// database, so a restart, or a poller that was down for a while, takes up
// where it stopped, and the Bot API's 24-hour store never fills with
// messages read already. Each message is answered in its chat. A message
// that carries an account code verifies that account's Telegram on the
// message alone: the sender's numeric id is the identity that vouches, and
// its age is estimated from the id (telegram.go). The credit goes through
// the boundary every automatic credit does (autocredit.go), so a held
// account, or one sharing a signal no reviewer has cleared, goes to a
// reviewer. The sender's id, username and text of each such message are
// kept for a day, which is what the account page's form reads when a tester
// uses it instead.
//
// What one Telegram account can do. It vouches for one Emissio account,
// ever: the verifications_telegram_id index on its numeric id, beside the
// one on its username, refuses a second verified row; one request of a
// Telegram id is checked and filed at a time, whichever account it names;
// and nothing is filed for a Telegram account with a live request for any
// account. Anyone may send any account's code, so a stranger can verify an
// account that is not theirs with their own aged Telegram account: their
// account gains nothing, the account gains a verification it did not ask
// for, and the stranger spends a Telegram account that can never vouch
// again. A message files nothing a reviewer would have to judge (an id with
// no age to vouch for, an account held or sharing an uncleared signal): the
// account's owner asks for that from the account page, so a stranger's
// message verifies an account at once or leaves no trace on it. Every
// credit passes the email gate. The answers tell a sender nothing of an
// account beyond what their own message did.
//
// The bot's token is in every Bot API address. No error, log line or note
// carries an address: tgError takes the token out of whatever the HTTP
// client says.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const tgSchema = `
CREATE TABLE IF NOT EXISTS tg_messages (
	update_id INTEGER PRIMARY KEY,
	from_id INTEGER NOT NULL,
	username TEXT NOT NULL DEFAULT '',
	chat_id INTEGER NOT NULL,
	text TEXT NOT NULL,
	code TEXT NOT NULL,
	answer TEXT NOT NULL DEFAULT '',
	answered INTEGER NOT NULL DEFAULT 0,
	sent_at INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_try_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tg_messages_code ON tg_messages(code, sent_at);
`

const (
	tgLongPoll = 50        // seconds a getUpdates call waits for a message
	tgKeep     = 24 * 3600 // seconds a message is kept, as long as the Bot API keeps it
	tgBatch    = 100       // messages per getUpdates call, the Bot API's most
)

// tgEvidence is what a Telegram verification records of the account that
// vouches: its numeric id, which no rename changes.
func tgEvidence(id int64) string { return "telegram-id:" + strconv.FormatInt(id, 10) }

// tgIDNote reads the numeric id a Telegram check note names.
var tgIDNote = regexp.MustCompile(`\(id (\d+)\)`)

// tgEvidenceFromNote is the evidence a verification made before the id was
// recorded takes from its check note: the first "(id DIGITS)" in it, or
// none. The migration and guardConflicts both read a note this way.
func tgEvidenceFromNote(note string) string {
	if m := tgIDNote.FindStringSubmatch(note); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return tgEvidence(n)
		}
	}
	return ""
}

// migrateTelegramIDs gives every Telegram verification made before the id
// was recorded the id its check note names, then guards the id: one
// verified Emissio account per Telegram account. A database where one id
// already vouches for two accounts cannot take the index, and Emissio does
// not start until a reviewer has rejected all but one (the error names the
// rows by id).
func migrateTelegramIDs(db *sql.DB) error {
	rows, err := db.Query("SELECT id, check_note FROM verifications WHERE platform = 'telegram' AND evidence = ''")
	if err == nil {
		type fix struct {
			id int64
			ev string
		}
		var fixes []fix
		for rows.Next() {
			var id int64
			var note string
			if rows.Scan(&id, &note) == nil {
				if ev := tgEvidenceFromNote(note); ev != "" {
					fixes = append(fixes, fix{id, ev})
				}
			}
		}
		rows.Close()
		for _, f := range fixes {
			db.Exec("UPDATE verifications SET evidence = ? WHERE id = ? AND evidence = ''", f.ev, f.id)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS verifications_telegram_id ON verifications(evidence)
		WHERE platform = 'telegram' AND status = 'verified' AND evidence != ''`); err != nil {
		found, _ := guardConflicts(db)
		return errGuard{"verifications rows " + rowList(found["verifications_telegram_id"]) +
			" are verified by one Telegram account each (verifications_telegram_id cannot be created): reject all but one of each group, then start again"}
	}
	return nil
}

// tgError is an error from the Bot API with the bot's token taken out.
func (a *App) tgError(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if tok := a.cfg.TgBotToken; tok != "" {
		for _, t := range []string{tok, url.PathEscape(tok), url.QueryEscape(tok)} {
			s = strings.ReplaceAll(s, t, "[token]")
		}
	}
	return errors.New(s)
}

// tgCall calls one Bot API method with form parameters and decodes its
// result. wait is how long the call may take.
func (a *App) tgCall(method string, params url.Values, wait time.Duration, out any) error {
	client := &http.Client{Timeout: wait}
	resp, err := client.PostForm(a.tgAPI()+"/bot"+a.cfg.TgBotToken+"/"+method, params)
	if err != nil {
		return a.tgError(err)
	}
	defer resp.Body.Close()
	var body struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return fmt.Errorf("%s: HTTP %d, an answer that is not the Bot API's", method, resp.StatusCode)
	}
	if !body.OK {
		return a.tgError(fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, body.Description))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body.Result, out)
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Date int64  `json:"date"`
		Text string `json:"text"`
		From struct {
			ID       int64  `json:"id"`
			IsBot    bool   `json:"is_bot"`
			Username string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
}

// startTelegramBot runs the poller while a bot token is set.
func (a *App) startTelegramBot() {
	if a.cfg.TgBotToken == "" {
		return
	}
	go func() {
		for {
			if err := a.pollTelegram(tgLongPoll); err != nil {
				log.Printf("telegram: %v", err)
				time.Sleep(30 * time.Second)
			}
		}
	}()
}

// pollTelegram reads the messages after the last one handled, waiting up to
// wait seconds for one, and handles each in order. Asking from the offset
// past a message is what confirms it to the Bot API.
func (a *App) pollTelegram(wait int) error {
	a.tgMu.Lock()
	defer a.tgMu.Unlock()
	var last int64
	a.db.QueryRow("SELECT CAST(value AS INTEGER) FROM kv WHERE key = 'tg_offset'").Scan(&last)
	params := url.Values{"limit": {strconv.Itoa(tgBatch)}, "timeout": {strconv.Itoa(wait)}, "allowed_updates": {`["message"]`}}
	if last > 0 {
		params.Set("offset", strconv.FormatInt(last+1, 10))
	}
	var ups []tgUpdate
	if err := a.tgCall("getUpdates", params, time.Duration(wait+15)*time.Second, &ups); err != nil {
		a.setKV("tg_error", fmt.Sprintf("%d %s", now(), err))
		return err
	}
	a.setKV("tg_read", strconv.FormatInt(now(), 10))
	for _, u := range ups {
		if u.UpdateID <= last {
			continue
		}
		a.tgHandle(u)
		last = u.UpdateID
		a.db.Exec("INSERT INTO kv (key, value) VALUES ('tg_offset', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", strconv.FormatInt(last, 10))
	}
	a.tgRetryAnswers()
	a.db.Exec("DELETE FROM tg_messages WHERE sent_at < ?", now()-tgKeep)
	return nil
}

// tgRetryAnswers sends again the answers whose sending failed, for the
// messages kept, so a sender is never left with silence by a Bot API that
// refused one answer: those whose wait has passed, the one waiting longest
// first, a few each read. An answer the Bot API refuses for good (the chat
// blocked the bot, or is gone) is not asked again. Answers to messages that
// are not kept (no account code, or one no account has) are sent once: they
// carry nothing to act on, anyone can send any number of them, and keeping
// them would keep strangers' text.
func (a *App) tgRetryAnswers() {
	rows, err := a.db.Query(`SELECT update_id, chat_id, answer FROM tg_messages WHERE answered = 0 AND answer != '' AND sent_at > ?
		AND next_try_at <= ? ORDER BY next_try_at, update_id LIMIT 20`, now()-tgKeep, now())
	if err != nil {
		return
	}
	type due struct {
		id, chat int64
		text     string
	}
	var all []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.chat, &d.text) == nil {
			all = append(all, d)
		}
	}
	rows.Close()
	for _, d := range all {
		a.tgAnswer(d.id, d.chat, d.text)
	}
}

// tgCodeRe is what may be an account code in a message: ten hex digits.
var tgCodeRe = regexp.MustCompile(`\b[0-9A-Fa-f]{10}\b`)

// tgHandle answers one message and, when it carries an account code,
// verifies that account's Telegram. A message handled already (asked for
// again after a restart between handling it and keeping the offset) is not
// handled twice; its answer is sent at least once (tgAnswer).
func (a *App) tgHandle(u tgUpdate) {
	m := u.Message
	if m == nil || m.From.IsBot || m.Chat.Type != "private" {
		return
	}
	var answer string
	var answered, kept bool
	if a.db.QueryRow("SELECT answer, answered FROM tg_messages WHERE update_id = ?", u.UpdateID).Scan(&answer, &answered) == nil {
		if answer != "" {
			if !answered {
				a.tgAnswer(u.UpdateID, m.Chat.ID, answer)
			}
			return
		}
		kept = true // kept, and stopped before its verification: run it now
	}
	cands := tgCodeRe.FindAllString(m.Text, 8)
	var user *User
	for _, c := range cands {
		if v, err := getUserByClaimCode(a.db, strings.ToLower(c)); err == nil {
			user = v
			break
		}
	}
	switch {
	case len(cands) == 0:
		a.tgSend(m.Chat.ID, tgWords("nocode", nil))
		return
	case user == nil:
		a.tgSend(m.Chat.ID, tgWords("unknown", &tgOutcome{Code: cands[0]}))
		return
	}
	sent := m.Date
	if sent == 0 || sent > now() {
		sent = now()
	}
	if now()-sent > tgKeep {
		// Older than the Bot API keeps: not evidence any more.
		a.tgSend(m.Chat.ID, tgWords("stale", nil))
		return
	}
	if !kept {
		if _, err := a.db.Exec(`INSERT INTO tg_messages (update_id, from_id, username, chat_id, text, code, sent_at) VALUES (?,?,?,?,?,?,?)`,
			u.UpdateID, m.From.ID, m.From.Username, m.Chat.ID, m.Text, user.ClaimCode, sent); err != nil {
			log.Printf("telegram: message %d not kept: %v", u.UpdateID, err)
			return
		}
	}
	o := a.tgVerify(user, tgSender{ID: m.From.ID, Username: m.From.Username, Sent: sent}, false)
	text := tgWords(o.Kind, &o)
	a.db.Exec("UPDATE tg_messages SET answer = ? WHERE update_id = ?", text, u.UpdateID)
	log.Printf("telegram: message %d: %s", u.UpdateID, o.Kind)
	a.tgAnswer(u.UpdateID, m.Chat.ID, text)
}

// tgAnswer sends a kept message's answer and marks it answered.
// Delivery is at least once: the attempt is recorded before the answer is
// sent and the answer marked sent after, so a stop or a failed write in
// between sends it again on a later read, and never leaves it unsent. A
// second copy of an answer is harmless; a lost one is the silence a sender
// reads as "wait".
func (a *App) tgAnswer(updateID, chat int64, text string) {
	if text == "" {
		return
	}
	var attempts int
	a.db.QueryRow("SELECT attempts FROM tg_messages WHERE update_id = ?", updateID).Scan(&attempts)
	attempts++
	if _, err := a.db.Exec("UPDATE tg_messages SET attempts = ?, next_try_at = ? WHERE update_id = ?", attempts, now()+tgAnswerWait(attempts), updateID); err != nil {
		log.Printf("telegram: answer to message %d not attempted: %v", updateID, err)
		return
	}
	err := a.tgSendErr(chat, text)
	switch {
	case err == nil:
		if _, err := a.db.Exec("UPDATE tg_messages SET answered = 1 WHERE update_id = ?", updateID); err != nil {
			log.Printf("telegram: answer to message %d sent but not recorded (%v): it may be sent again", updateID, err)
		}
	case tgRefusedForGood(err):
		if _, werr := a.db.Exec("UPDATE tg_messages SET answered = 2 WHERE update_id = ?", updateID); werr != nil {
			log.Printf("telegram: answer to message %d refused for good, not recorded: %v", updateID, werr)
		}
		log.Printf("telegram: answer to message %d given up: %v", updateID, err)
	case attempts&(attempts-1) == 0: // the 1st, 2nd, 4th... failure
		log.Printf("telegram: answer to message %d not sent (%d tries, asked again later): %v", updateID, attempts, err)
	}
}

// tgAnswerWait is how long to wait after an answer failed n times: a
// minute, doubling up to an hour.
func tgAnswerWait(n int) int64 {
	return min(int64(60)<<min(max(n-1, 0), 20), 3600)
}

// tgRefusedForGood is whether the Bot API refuses an answer in a way that
// will not change: the chat blocked the bot, its user is gone, or there is
// no such chat.
func tgRefusedForGood(err error) bool {
	e := err.Error()
	return strings.Contains(e, "HTTP 403") || strings.Contains(e, "chat not found") || strings.Contains(e, "user is deactivated")
}

func (a *App) tgSendErr(chat int64, text string) error {
	return a.tgCall("sendMessage", url.Values{"chat_id": {strconv.FormatInt(chat, 10)}, "text": {text}}, 20*time.Second, nil)
}

// tgSend sends an answer once, for a message that is not kept.
func (a *App) tgSend(chat int64, text string) bool {
	if err := a.tgSendErr(chat, text); err != nil {
		log.Printf("telegram: answer not sent: %v", err)
		return false
	}
	return true
}

// tgSender is the Telegram account a message came from.
type tgSender struct {
	ID       int64
	Username string
	Sent     int64 // when the message was sent
}

// handle is what a verification names the account by: its username, or
// its numeric id when it has none.
func (s tgSender) handle() string {
	if s.Username != "" {
		return strings.ToLower(s.Username)
	}
	return strconv.FormatInt(s.ID, 10)
}

// tgOutcome is what came of a message, or of the form, for one account.
type tgOutcome struct {
	Kind    string // verified, held, pending, form, review, young, already, waiting, blocked, taken, error
	Code    string
	Handle  string
	Created time.Time
}

// tgVerify files and, where the check passes, credits the Telegram
// verification of user by sender. consent is whether the account's owner
// asked for it (the form). Without it nothing is filed that a reviewer must
// judge: an id that gives no age to vouch for, or an account the automatic
// boundary holds back (held, or sharing a signal no reviewer has cleared);
// the sender is told the owner can ask from the account page. So a
// stranger's message either verifies an account at once or leaves no trace
// on it. The credit passes the email gate as every credit does.
func (a *App) tgVerify(user *User, s tgSender, consent bool) tgOutcome {
	o := tgOutcome{Code: user.ClaimCode, Handle: s.handle()}
	defer a.claims.lock(fmt.Sprintf("verify:%d:telegram", user.ID))()
	// One request of a Telegram account at a time, whichever Emissio
	// account it names, so the form and the bot cannot both pass the check
	// below; verifications_telegram_id is the second guard.
	defer a.claims.lock(fmt.Sprintf("telegram-id:%d", s.ID))()
	if u, err := getUserByID(a.db, user.ID); err == nil {
		user = u
	}
	if !a.mayEarn(user) {
		o.Kind = "blocked"
		return o
	}
	var status, evidence string
	err := a.db.QueryRow("SELECT status, evidence FROM verifications WHERE user_id = ? AND platform = 'telegram' AND status != 'rejected' ORDER BY id DESC LIMIT 1", user.ID).Scan(&status, &evidence)
	switch {
	case err == nil && evidence != tgEvidence(s.ID):
		// Another Telegram account's: nothing of it is said to this sender.
		o.Kind = "blocked"
		return o
	case err == nil && status == "verified":
		o.Kind = "already"
		return o
	case err == nil:
		o.Kind = "waiting"
		return o
	case !errors.Is(err, sql.ErrNoRows):
		o.Kind = "error"
		return o
	}
	// A Telegram account with a live request, for any account, files no
	// other: by its id, and by its username for a request kept without one.
	var taken int64
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM verifications WHERE platform = 'telegram' AND status != 'rejected' AND user_id != ?
		AND (evidence = ? OR (? != '' AND handle = ?))`, user.ID, tgEvidence(s.ID), s.Username, strings.ToLower(s.Username)).Scan(&taken); err != nil {
		o.Kind = "error"
		return o
	}
	if taken > 0 {
		o.Kind = "taken"
		return o
	}
	tgAfterCheck()
	at := time.Unix(s.Sent, 0)
	created, weak := tgEstimateCreation(s.ID)
	o.Created = created
	ageOK := oldEnough(created, at)
	note := fmt.Sprintf("message to the bot from %s (id %d); account created ~%s, about %.1f years old (%s)",
		tgName(s), s.ID, created.Format("Jan 2006"), at.Sub(created).Hours()/24/365, ageWord(ageOK))
	switch {
	case !ageOK:
		o.Kind = "young"
		return o
	case weak && !consent:
		o.Kind = "form"
		return o
	case weak:
		note += "; id beyond the newest anchor, so the account is younger than this estimate: manual review"
	}
	// The row and the approval are one transaction, so a message whose
	// credit the boundary holds back leaves nothing behind.
	tx, err := a.db.Begin()
	if err != nil {
		o.Kind = "error"
		return o
	}
	defer tx.Rollback()
	ins, err := tx.Exec(`INSERT INTO verifications (user_id, platform, handle, evidence, check_note, created_at) VALUES (?, 'telegram', ?, ?, ?, ?)`,
		user.ID, s.handle(), tgEvidence(s.ID), note, now())
	if err != nil {
		o.Kind = "error"
		if strings.Contains(err.Error(), "UNIQUE") {
			o.Kind = "blocked"
		}
		return o
	}
	if weak {
		if tx.Commit() != nil {
			o.Kind = "error"
			return o
		}
		o.Kind = "review"
		return o
	}
	id, _ := ins.LastInsertId()
	why, err := autoApproveVerificationTx(tx, id, user.ID)
	switch {
	case err != nil:
		o.Kind = "error"
		return o
	case why != "" && !consent:
		o.Kind = "held" // rolled back: the owner asks for the review
		return o
	}
	if err := tx.Commit(); err != nil {
		o.Kind = "error"
		if strings.Contains(err.Error(), "UNIQUE") {
			o.Kind = "blocked"
		}
		return o
	}
	if why != "" {
		o.Kind = "pending"
		return o
	}
	a.processReferrals(user.ID)
	o.Kind = "verified"
	return o
}

// tgAfterCheck runs between tgVerify's check that a Telegram account has no
// live request and the request it files: a test widens the window there.
var tgAfterCheck = func() {}

// name is how an answer names the sender's Telegram account for the form:
// its @username, or its numeric id when it has none.
func (o *tgOutcome) name() string {
	if _, err := strconv.ParseInt(o.Handle, 10, 64); err == nil {
		return o.Handle
	}
	return "@" + o.Handle
}

// tgName is how a note names a Telegram account.
func tgName(s tgSender) string {
	if s.Username != "" {
		return "@" + s.Username
	}
	return "an account with no username"
}

// tgWords is the bot's answer for an outcome, in plain words. The sender
// need not be the account's owner, so it says nothing of the account beyond
// what this sender's own message did: not why a person must look (a hold, a
// shared signal, the account's age), and not whether another Telegram
// account verified it, is waiting, or its email is unconfirmed. Whether a
// code exists is said, as codes are public (the referral link carries one).
func tgWords(kind string, o *tgOutcome) string {
	switch kind {
	case "verified", "held":
		// One answer for both: the sender need not be the account's owner,
		// and a different one would tell them the account is held or
		// shares a signal.
		return fmt.Sprintf("Received. If the Emissio account with code %s can take this Telegram account, it is linked now, with %s SEQ in its ledger. If a person must look first, only the account's owner can ask for that: sign in to Emissio and, under Verified accounts, Telegram, enter %s and press Request verification, within a day. The account page shows which.", o.Code, formatSEQ(verificationBonus), o.name())
	case "pending":
		return "Received. A reviewer confirms this verification before it counts; the Emissio account page shows when that is done."
	case "form":
		return fmt.Sprintf("Received. The age of this Telegram account cannot be read from its id, so a person must look at it, and only the account's owner can ask for that: sign in to Emissio and, under Verified accounts, Telegram, enter %s and press Request verification, within a day.", o.name())
	case "young":
		return fmt.Sprintf("Received, but this Telegram account looks under %d years old (made about %s), and Emissio links only accounts at least %d years old. Nothing was linked.", verifMinAgeYears, o.Created.Format("January 2006"), verifMinAgeYears)
	case "already":
		return "That Emissio account is already verified with this Telegram account. There is nothing more to do."
	case "waiting":
		return "That Emissio account's verification with this Telegram account is waiting for a reviewer. There is nothing more to do."
	case "blocked":
		return "Nothing was linked: the Emissio account with that code cannot take a Telegram verification from this message. If the account is yours, sign in: your account page says why."
	case "taken":
		return "This Telegram account already vouches for another Emissio account, and a Telegram account can vouch for one only. Nothing was linked."
	case "unknown":
		return fmt.Sprintf("No Emissio account has the code %s. Send the account code shown at the top of your Emissio account page, from the Telegram account you want to verify.", o.Code)
	case "stale":
		return "That message is more than a day old. Send your Emissio account code again."
	case "nocode":
		return "Send your Emissio account code, shown at the top of your Emissio account page, as a message here, from the Telegram account you want to verify. It is linked at once."
	}
	return "Emissio could not take this just now. Send your account code again in a few minutes."
}

// tgMessageFor finds the newest message of the last day that carries the
// account's code and came from the Telegram account a tester named, by
// username or numeric id.
func (a *App) tgMessageFor(claimCode, handle string) (tgSender, bool) {
	var s tgSender
	id, _ := strconv.ParseInt(handle, 10, 64)
	err := a.db.QueryRow(`SELECT from_id, username, sent_at FROM tg_messages WHERE code = ? AND sent_at > ?
		AND ((username != '' AND lower(username) = ?) OR from_id = ?) ORDER BY update_id DESC LIMIT 1`,
		claimCode, now()-tgKeep, strings.ToLower(handle), id).Scan(&s.ID, &s.Username, &s.Sent)
	return s, err == nil
}

// verifyTelegramForm is the account page's way in: the tester names the
// Telegram account that sent the bot their code, and the message the poller
// kept is checked as the bot checks it, with the owner's consent to a
// review where the id gives no age.
func (a *App) verifyTelegramForm(w http.ResponseWriter, r *http.Request, user *User, handle string) {
	s, ok := a.tgMessageFor(user.ClaimCode, handle)
	if !ok {
		a.redirect(w, r, "/account", "", "Not verified: no message with your account code from "+handle+" reached the bot in the last 24 hours. Send your account code as a Telegram message to the bot from that account; it is linked at once, with nothing more to press.")
		return
	}
	a.seen(user.ID, clientIP(r), a.device(w, r))
	o := a.tgVerify(user, s, true)
	switch o.Kind {
	case "verified":
		a.redirect(w, r, "/account", fmt.Sprintf("Verified: your Telegram account is linked, and %s SEQ is in your ledger.", formatSEQ(verificationBonus)), "")
	case "pending", "review":
		a.redirect(w, r, "/account", "Verification request received. A reviewer confirms it, and the reward lands in your ledger.", "")
	case "already", "waiting", "blocked":
		a.redirect(w, r, "/account", "", "You already have a Telegram verification on file.")
	case "taken":
		a.redirect(w, r, "/account", "", "That Telegram account already vouches for another Emissio account.")
	case "young":
		a.redirect(w, r, "/account", "", fmt.Sprintf("Not verified: that Telegram account looks under %d years old (made about %s).", verifMinAgeYears, o.Created.Format("January 2006")))
	default:
		a.redirect(w, r, "/account", "", "Emissio could not take this just now. Try again in a few minutes.")
	}
}

func (a *App) setKV(key, value string) {
	a.db.Exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
}

// tgHealth says, for the desk, whether the poller reads the bot's messages:
// when it last did, and the last error since, if any (with no token: every
// error has been through tgError).
func (a *App) tgHealth() (line string, bad bool) {
	if a.cfg.TgBotToken == "" {
		return "", false
	}
	var readS, errS string
	a.db.QueryRow("SELECT value FROM kv WHERE key = 'tg_read'").Scan(&readS)
	a.db.QueryRow("SELECT value FROM kv WHERE key = 'tg_error'").Scan(&errS)
	read, _ := strconv.ParseInt(readS, 10, 64)
	var errAt int64
	var errText string
	if i := strings.IndexByte(errS, ' '); i > 0 {
		errAt, _ = strconv.ParseInt(errS[:i], 10, 64)
		errText = errS[i+1:]
	}
	when := func(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04 UTC") }
	switch {
	case errAt > read && read == 0:
		return "The Telegram bot has not read its messages since Emissio started: " + errText, true
	case errAt > read:
		return "The Telegram bot has not read its messages since " + when(read) + ": " + errText, true
	case read == 0:
		return "The Telegram bot has not read its messages yet.", true
	case now()-read > 5*60:
		return "The Telegram bot has not read its messages since " + when(read) + ".", true
	}
	return "The Telegram bot read its messages at " + when(read) + ".", false
}
