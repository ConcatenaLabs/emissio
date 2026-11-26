package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bot's token in these tests: it must never reach a log line, an error
// or a note.
const fakeToken = "424242:TEST-token-never-logged"

// fakeBot is a local stand-in for the Bot API: it keeps updates until a
// getUpdates call confirms them (an offset past them), returns at most
// limit, oldest first, records each answer, and can be taken down.
type fakeBot struct {
	mu      sync.Mutex
	next    int64
	queue   []map[string]any
	replies []map[string]string
	gets    int
	down    bool
	failing int // sendMessage calls still to refuse
	srv     *httptest.Server
}

func newFakeBot(t *testing.T) *fakeBot {
	f := &fakeBot{next: 1000}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>502 Bad Gateway</html>"))
			return
		}
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+fakeToken+"/")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		r.ParseForm()
		switch method {
		case "getMe":
			w.Write([]byte(`{"ok":true,"result":{"username":"EmissioCheckBot"}}`))
		case "getUpdates":
			f.gets++
			if off, _ := strconv.ParseInt(r.Form.Get("offset"), 10, 64); off > 0 {
				var keep []map[string]any
				for _, u := range f.queue {
					if u["update_id"].(int64) >= off {
						keep = append(keep, u)
					}
				}
				f.queue = keep
			}
			limit, _ := strconv.Atoi(r.Form.Get("limit"))
			if limit <= 0 || limit > 100 {
				limit = 100
			}
			out := f.queue
			if len(out) > limit {
				out = out[:limit]
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
		case "sendMessage":
			if f.failing > 0 {
				f.failing--
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1"}`))
				return
			}
			f.replies = append(f.replies, map[string]string{"chat_id": r.Form.Get("chat_id"), "text": r.Form.Get("text")})
			w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"ok":false,"description":"Not Found"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// send queues a private message from a Telegram account, sent ago.
func (f *fakeBot) send(from int64, username, text string, ago time.Duration) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	sender := map[string]any{"id": from, "is_bot": false}
	if username != "" {
		sender["username"] = username
	}
	f.queue = append(f.queue, map[string]any{"update_id": f.next, "message": map[string]any{
		"message_id": f.next, "date": time.Now().Add(-ago).Unix(), "text": text,
		"from": sender, "chat": map[string]any{"id": from, "type": "private"}}})
	return f.next
}

// last is the newest answer to a chat.
func (f *fakeBot) last(chat int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.replies) - 1; i >= 0; i-- {
		if f.replies[i]["chat_id"] == strconv.FormatInt(chat, 10) {
			return f.replies[i]["text"]
		}
	}
	return ""
}

func (f *fakeBot) answers(chat int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.replies {
		if r["chat_id"] == strconv.FormatInt(chat, 10) {
			n++
		}
	}
	return n
}

func (f *fakeBot) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

// tgRig is Emissio with the bot set up against the fake, every log line
// kept to be searched for the token.
type tgRig struct {
	app  *App
	srv  *httptest.Server
	bot  *fakeBot
	logs *bytes.Buffer
}

func newTgRig(t *testing.T) *tgRig {
	app, srv, _ := newTestServer(t)
	bot := newFakeBot(t)
	app.cfg.TgBotToken, app.cfg.TgAPIBase = fakeToken, bot.srv.URL
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		if strings.Contains(logs.String(), "TEST-token") {
			t.Errorf("the bot's token reached the log:\n%s", logs.String())
		}
	})
	return &tgRig{app, srv, bot, &logs}
}

// account registers an account and gives its client and code.
func (g *tgRig) account(t *testing.T, email, ip string) (*http.Client, string, int64) {
	c := &http.Client{Jar: newJar()}
	from(t, c, ip, g.srv.URL+"/register", url.Values{"email": {email}, "password": {"a-long-password"}})
	var code string
	var id int64
	g.app.db.QueryRow("SELECT claim_code, id FROM users WHERE email = ?", email).Scan(&code, &id)
	return c, code, id
}

// poll reads the bot's messages until none is left unconfirmed.
func (g *tgRig) poll(t *testing.T) {
	for i := 0; i < 10; i++ {
		if err := g.app.pollTelegram(0); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if g.bot.pending() == 0 {
			g.app.pollTelegram(0) // confirms the last one
			return
		}
	}
	t.Fatal("the poller does not get through the bot's messages")
}

func (g *tgRig) verif(userID int64) (status, evidence, note string) {
	g.app.db.QueryRow("SELECT status, evidence, check_note FROM verifications WHERE user_id = ? ORDER BY id DESC LIMIT 1", userID).Scan(&status, &evidence, &note)
	return
}

// An old Telegram id below the newest anchor: over two years by estimate.
const oldTG = 54845238

// A message to the bot verifies the account at once, with no form, and the
// bot says so; the account page then shows it.
func TestBotVerifiesOnTheMessage(t *testing.T) {
	g := newTgRig(t)
	c, code, id := g.account(t, "tg1@example.com", "10.3.0.1")
	g.bot.send(oldTG, "old_tg", "hello, my code is "+code, time.Minute)
	g.poll(t)
	if st, ev, _ := g.verif(id); st != "verified" || ev != "telegram-id:54845238" {
		t.Fatalf("after the message: %s %s", st, ev)
	}
	var bonus int64
	g.app.db.QueryRow("SELECT SUM(amount) FROM ledger WHERE user_id = ? AND kind = 'verification'", id).Scan(&bonus)
	if bonus != verificationBonus {
		t.Fatalf("bonus %d", bonus)
	}
	if a := g.bot.last(oldTG); !strings.HasPrefix(a, "Verified.") || !strings.Contains(a, code) {
		t.Fatalf("answer: %q", a)
	}
	page := get(t, c, g.srv.URL+"/account")
	if !strings.Contains(page, "@old_tg") || !strings.Contains(page, ">verified<") || !strings.Contains(page, "the bot answers in the chat") {
		t.Fatal("the account page does not show the verification")
	}
	// Confirmed: polling again neither verifies nor answers twice.
	g.poll(t)
	if g.bot.answers(oldTG) != 1 {
		t.Fatalf("%d answers", g.bot.answers(oldTG))
	}
	// The same message sent again: already verified.
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if a := g.bot.last(oldTG); !strings.Contains(a, "already verified") {
		t.Fatalf("again: %q", a)
	}
}

// A message with no code, or a code no account has, is answered with what
// to send, and nothing is filed or kept.
func TestBotUnknownCode(t *testing.T) {
	g := newTgRig(t)
	g.bot.send(111, "someone", "/start", 0)
	g.bot.send(112, "other", "is it deadbeef01?", 0)
	g.poll(t)
	if a := g.bot.last(111); !strings.HasPrefix(a, "Send your Emissio account code") {
		t.Fatalf("no code: %q", a)
	}
	if a := g.bot.last(112); !strings.Contains(a, "No Emissio account has the code deadbeef01") {
		t.Fatalf("unknown code: %q", a)
	}
	var n int
	g.app.db.QueryRow("SELECT (SELECT COUNT(*) FROM verifications) + (SELECT COUNT(*) FROM tg_messages)").Scan(&n)
	if n != 0 {
		t.Fatal("a message without a known code was filed or kept")
	}
}

// One Telegram account vouches for one Emissio account: a second account's
// code from it verifies nothing, under a new username too, and the schema
// refuses a second verified row of its id.
func TestBotOneTelegramOneAccount(t *testing.T) {
	g := newTgRig(t)
	_, codeA, a := g.account(t, "a@example.com", "10.3.0.3")
	_, codeB, b := g.account(t, "b@example.com", "10.3.0.4")
	g.bot.send(oldTG, "old_tg", codeA, 0)
	g.bot.send(oldTG, "old_tg", codeB, 0)
	g.bot.send(oldTG, "renamed_tg", codeB, 0)
	g.poll(t)
	if st, _, _ := g.verif(a); st != "verified" {
		t.Fatalf("first: %s", st)
	}
	if st, _, _ := g.verif(b); st != "" {
		t.Fatalf("the second account got %s", st)
	}
	if a := g.bot.last(oldTG); !strings.Contains(a, "already vouches for another Emissio account") {
		t.Fatalf("answer: %q", a)
	}
	if _, err := g.app.db.Exec(`INSERT INTO verifications (user_id, platform, handle, evidence, status, created_at) VALUES (?, 'telegram', 'other_name', 'telegram-id:54845238', 'verified', 0)`, b); err == nil {
		t.Fatal("the schema took a second verified row for one Telegram id")
	}
	// A pending request of one Telegram account blocks its others too.
	_, codeC, c := g.account(t, "c@example.com", "10.3.0.5")
	_, codeD, d := g.account(t, "d@example.com", "10.3.0.6")
	g.app.db.Exec("UPDATE users SET held = 1 WHERE id = ?", c)
	uc, _ := getUserByID(g.app.db, c)
	if o := g.app.tgVerify(uc, tgSender{ID: 60000000, Username: "mid_tg", Sent: now()}, true); o.Kind != "pending" {
		t.Fatalf("the owner's form on a held account: %s", o.Kind)
	}
	_ = codeC
	g.bot.send(60000000, "mid_tg", codeD, 0)
	g.poll(t)
	if st, _, _ := g.verif(d); st != "" {
		t.Fatalf("a Telegram account with a pending request filed another: %s", st)
	}
}

// More than a hundred messages in a day: the poller confirms as it reads,
// so the newest is reached.
func TestBotPastAHundredMessages(t *testing.T) {
	g := newTgRig(t)
	_, code, id := g.account(t, "late@example.com", "10.3.0.7")
	for i := 0; i < 150; i++ {
		g.bot.send(int64(500+i), "", "spam "+strconv.Itoa(i), time.Hour)
	}
	g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	if st, _, _ := g.verif(id); st != "verified" {
		t.Fatalf("the 151st message: %s", st)
	}
	if g.bot.gets < 2 {
		t.Fatalf("%d reads", g.bot.gets)
	}
}

// The poller down for an hour, or the Bot API not answering: nothing is
// lost, and no error carries the token.
func TestBotDownForAnHour(t *testing.T) {
	g := newTgRig(t)
	_, code, id := g.account(t, "patient@example.com", "10.3.0.8")
	g.bot.mu.Lock()
	g.bot.down = true
	g.bot.mu.Unlock()
	g.bot.send(oldTG, "old_tg", code, time.Hour)
	if err := g.app.pollTelegram(0); err == nil || strings.Contains(err.Error(), "TEST-token") {
		t.Fatalf("down: %v", err)
	} else {
		log.Printf("telegram: %v", err)
	}
	// Unreachable: the HTTP client's error names the address it called.
	base := g.app.cfg.TgAPIBase
	g.app.cfg.TgAPIBase = "http://127.0.0.1:1"
	if err := g.app.pollTelegram(0); err == nil || strings.Contains(err.Error(), "TEST-token") || !strings.Contains(err.Error(), "[token]") {
		t.Fatalf("unreachable: %v", err)
	} else {
		log.Printf("telegram: %v", err)
	}
	// A wrong token: the Bot API's refusal, without the token.
	g.app.cfg.TgAPIBase = base
	g.bot.mu.Lock()
	g.bot.down = false
	g.bot.mu.Unlock()
	g.app.cfg.TgBotToken = "999:TEST-token-wrong"
	if err := g.app.pollTelegram(0); err == nil || !strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "TEST-token") {
		t.Fatalf("wrong token: %v", err)
	}
	g.app.cfg.TgBotToken = fakeToken
	if st, _, _ := g.verif(id); st != "" {
		t.Fatal("verified while the Bot API was down")
	}
	g.poll(t)
	if st, _, _ := g.verif(id); st != "verified" {
		t.Fatalf("after an hour down: %s", st)
	}
}

// A message handled, then asked for again (a restart before the offset was
// kept): not verified twice, nor answered twice.
func TestBotRedelivery(t *testing.T) {
	g := newTgRig(t)
	_, code, id := g.account(t, "twice@example.com", "10.3.0.9")
	up := g.bot.send(oldTG, "old_tg", code, 0)
	g.poll(t)
	g.app.db.Exec("UPDATE kv SET value = ? WHERE key = 'tg_offset'", strconv.FormatInt(up-1, 10))
	g.bot.mu.Lock()
	g.bot.next = up - 1
	g.bot.mu.Unlock()
	g.bot.send(oldTG, "old_tg", code, 0) // the same update id
	g.poll(t)
	var n int
	g.app.db.QueryRow("SELECT COUNT(*) FROM verifications WHERE user_id = ?", id).Scan(&n)
	if n != 1 || g.bot.answers(oldTG) != 1 {
		t.Fatalf("%d verifications, %d answers", n, g.bot.answers(oldTG))
	}
}

// An id beyond the newest anchor gives no age to vouch for: the message
// files nothing on its own; the owner asks for a reviewer from the form,
// which reads the message the poller kept.
func TestBotUnreadableAgeGoesToTheForm(t *testing.T) {
	g := newTgRig(t)
	c, code, id := g.account(t, "new@example.com", "10.3.0.10")
	csrf := csrfOf(t, c, g.srv.URL+"/account")
	verify := func(handle string) string {
		return from(t, c, "10.3.0.10", g.srv.URL+"/account/verify", url.Values{"csrf": {csrf}, "platform": {"telegram"}, "handle": {handle}})
	}
	if m := verify("new_tg"); !strings.Contains(m, "Not verified: no message with your account code from new_tg reached the bot") {
		t.Fatalf("no message: %q", m)
	}
	g.bot.send(9000000000, "new_tg", code, 0)
	g.poll(t)
	if st, _, _ := g.verif(id); st != "" {
		t.Fatalf("filed on the message alone: %s", st)
	}
	if a := g.bot.last(9000000000); !strings.Contains(a, "only the account's owner can ask") || !strings.Contains(a, "enter @new_tg") {
		t.Fatalf("answer: %q", a)
	}
	if m := verify("@New_TG"); !strings.Contains(m, "Verification request received") {
		t.Fatalf("form: %q", m)
	}
	if st, ev, note := g.verif(id); st != "pending" || ev != "telegram-id:9000000000" || !strings.Contains(note, "younger than this estimate") {
		t.Fatalf("form: %s %s %q", st, ev, note)
	}
	// With no username, the bot names the id to enter.
	_, code2, _ := g.account(t, "nouser@example.com", "10.3.0.11")
	g.bot.send(9000000001, "", code2, 0)
	g.poll(t)
	if a := g.bot.last(9000000001); !strings.Contains(a, "enter 9000000001 and") {
		t.Fatalf("no username: %q", a)
	}
}

// A Telegram account under two years old links nothing; a message older
// than a day is not evidence.
func TestBotYoungAndStale(t *testing.T) {
	g := newTgRig(t)
	_, code, id := g.account(t, "young@example.com", "10.3.0.12")
	user, _ := getUserByID(g.app.db, id)
	sent := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	if o := g.app.tgVerify(user, tgSender{ID: 5500000000, Username: "young_tg", Sent: sent}, false); o.Kind != "young" {
		t.Fatalf("young: %s", o.Kind)
	}
	if w := tgWords("young", &tgOutcome{Created: time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)}); !strings.Contains(w, "under 2 years old (made about July 2024)") {
		t.Fatalf("young words: %q", w)
	}
	g.bot.send(oldTG, "old_tg", code, 25*time.Hour)
	g.poll(t)
	if st, _, _ := g.verif(id); st != "" {
		t.Fatalf("a stale message verified: %s", st)
	}
	if a := g.bot.last(oldTG); !strings.Contains(a, "more than a day old") {
		t.Fatalf("stale: %q", a)
	}
}

// Verifications made before the id was recorded take it from their note,
// so a renamed Telegram account cannot vouch twice.
func TestTelegramIDBackfill(t *testing.T) {
	g := newTgRig(t)
	_, _, a := g.account(t, "early@example.com", "10.3.0.13")
	_, codeB, b := g.account(t, "later@example.com", "10.3.0.14")
	g.app.db.Exec(`INSERT INTO verifications (user_id, platform, handle, status, check_note, created_at) VALUES (?, 'telegram', 'first_name', 'verified', 'message verified from @first_name (id 54845238); account created ~Sep 2014', 0)`, a)
	if err := migrateTelegramIDs(g.app.db); err != nil {
		t.Fatal(err)
	}
	if _, ev, _ := g.verif(a); ev != "telegram-id:54845238" {
		t.Fatalf("backfill: %q", ev)
	}
	g.bot.send(oldTG, "second_name", codeB, 0)
	g.poll(t)
	if st, _, _ := g.verif(b); st != "" {
		t.Fatalf("a renamed Telegram account vouched again: %s", st)
	}
}
