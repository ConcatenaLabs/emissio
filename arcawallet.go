package main

// Emissio's own web-wallet-grade wallet: the `arca` command line on a
// directory of its own (EMISSIO_ARCA, EMISSIO_ARCA_DATADIR), as the faucet
// runs its leaf wallet. With it Emissio is the counterparty of the web
// wallet tasks:
//
//   - each account gets its own receive code from Emissio,
//     bitcoin:<address>?ark=<a Bitcoin ark address of its own>&sequentia=<a
//     receive request of its own>, so whatever arrives at either part is
//     known to come from that account's wallet;
//   - Emissio pays an account's receive code an amount it does not show,
//     and the account sends back what arrived, which only a wallet
//     that received it can know;
//   - Emissio pays an invoice the account's wallet made;
//   - Emissio pays the wallet at its payment name, through the name service
//     (EMISSIO_LNADDR), and the account sends back what arrived.
//
// Every arca call is serialised: the wallet's store is not safe for two
// processes at once, and its Bitcoin side takes an exclusive lock. A
// follower syncs on the wallet's own schedule, reads each coin the mailbox
// accepted and attributes it to the request whose owner and nonce its record
// carries.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const arcaSchema = `
CREATE TABLE IF NOT EXISTS arca_codes (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	purpose TEXT NOT NULL,
	request TEXT NOT NULL,
	owner TEXT NOT NULL,
	owner_nonce TEXT NOT NULL,
	ark_address TEXT NOT NULL DEFAULT '',
	want_asset TEXT NOT NULL DEFAULT '',
	want_atoms INTEGER NOT NULL DEFAULT 0,
	paid_leaf TEXT NOT NULL DEFAULT '',
	paid_asset TEXT NOT NULL DEFAULT '',
	paid_atoms INTEGER NOT NULL DEFAULT 0,
	paid_at INTEGER NOT NULL DEFAULT 0,
	btc_sats INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS arca_codes_user ON arca_codes(user_id, purpose);
CREATE TABLE IF NOT EXISTS arca_sends (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	purpose TEXT NOT NULL,
	target TEXT NOT NULL,
	asset TEXT NOT NULL,
	atoms INTEGER NOT NULL,
	state TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS arca_sends_user ON arca_sends(user_id, purpose);
`

const (
	arcaSendsDay = 3 // payments per network address per day
)

func (a *App) arcaOn() bool { return a.cfg.ArcaBin != "" && a.cfg.ArcaDatadir != "" }

// arca runs one arca command under the wallet's lock and decodes its JSON.
// A refusal comes back as an error carrying the wallet's own message.
func (a *App) arca(timeout time.Duration, out any, args ...string) error {
	a.arcaMu.Lock()
	defer a.arcaMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.cfg.ArcaBin, append([]string{"--datadir", a.cfg.ArcaDatadir}, args...)...)
	raw, err := cmd.Output()
	var refusal struct {
		Error *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &refusal) == nil && refusal.Error != nil {
		return fmt.Errorf("%s", refusal.Error.Message)
	}
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n")
			msg = lines[len(lines)-1]
		}
		return fmt.Errorf("arca %s: %v %s", args[0], err, msg)
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = strings.TrimSpace(string(raw))
		return nil
	}
	return json.Unmarshal(raw, out)
}

// requestDetails is what a receive request names.
type requestDetails struct {
	Kind       string `json:"kind"`
	Operator   string `json:"operator"`
	Owner      string `json:"owner"`
	OwnerNonce string `json:"owner_nonce"`
	Until      int64  `json:"until"`
	Asset      string `json:"asset"`
	Value      string `json:"value"`
}

// decodeRequest reads an arca:<hex of JSON> receive request.
func decodeRequest(req string) (*requestDetails, error) {
	h, ok := strings.CutPrefix(strings.TrimSpace(req), "arca:")
	if !ok {
		return nil, errors.New("not a receive request")
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, errors.New("not a receive request")
	}
	var d requestDetails
	if json.Unmarshal(b, &d) != nil || d.Kind != "request" || len(d.Owner) != 64 || len(d.OwnerNonce) != 64 {
		return nil, errors.New("not a receive request")
	}
	return &d, nil
}

// codeParts splits a receive code into its address, ark, sequentia and
// lightning parts (receivecode.go). A bare receive request is its sequentia
// part, and anything else its address.
func codeParts(code string) (addr, ark, req, invoice string) {
	code = strings.TrimSpace(code)
	if c, ok := readReceiveCode(code); ok {
		return c.Address, c.Ark, c.Sequentia, c.Lightning
	}
	if strings.HasPrefix(code, "arca:") || strings.HasPrefix(strings.ToLower(code), "sqr1") {
		return "", "", code, ""
	}
	return code, "", "", ""
}

// newCode makes an account's receive code for one purpose: a fresh receive
// request and, when the Bitcoin side is set up, a Bitcoin ark address of
// its own. A request may name the asset and amount it asks for.
func (a *App) newCode(userID int64, purpose, asset string, atoms int64) (string, error) {
	args := []string{"receive"}
	if asset != "" {
		args = append(args, "--asset", asset, "--amount", strconv.FormatInt(atoms, 10))
	}
	var r struct {
		Request string         `json:"request"`
		Details requestDetails `json:"details"`
	}
	if err := a.arca(2*time.Minute, &r, args...); err != nil {
		return "", err
	}
	var ark string
	if purpose == "btc" {
		if err := a.arca(2*time.Minute, &ark, "bitcoin", "--quiet", "address"); err != nil {
			return "", err
		}
	}
	_, err := a.db.Exec(`INSERT INTO arca_codes (user_id, purpose, request, owner, owner_nonce, ark_address, want_asset, want_atoms, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, userID, purpose, r.Request, r.Details.Owner, r.Details.OwnerNonce, ark, asset, atoms, now())
	return r.Request, err
}

type arcaCode struct {
	ID                int64
	Purpose, Request  string
	Owner, OwnerNonce string
	Ark               string
	WantAsset         string
	WantAtoms         int64
	PaidAsset         string
	PaidAtoms, PaidAt int64
	BtcSats           int64
	CreatedAt         int64
}

func (a *App) codeOf(userID int64, purpose string) *arcaCode {
	var c arcaCode
	err := a.db.QueryRow(`SELECT id, purpose, request, owner, owner_nonce, ark_address, want_asset, want_atoms, paid_asset, paid_atoms, paid_at, btc_sats, created_at
		FROM arca_codes WHERE user_id = ? AND purpose = ? ORDER BY id DESC LIMIT 1`, userID, purpose).
		Scan(&c.ID, &c.Purpose, &c.Request, &c.Owner, &c.OwnerNonce, &c.Ark, &c.WantAsset, &c.WantAtoms, &c.PaidAsset, &c.PaidAtoms, &c.PaidAt, &c.BtcSats, &c.CreatedAt)
	if err != nil {
		return nil
	}
	return &c
}

// receiveCode is the code shown to the account: Emissio's on-chain address
// first, as any Bitcoin wallet reads it, then the ark and Sequentia parts.
func (a *App) receiveCode(c *arcaCode) string {
	addr := a.arcaAddress()
	q := url.Values{}
	if c.Ark != "" {
		q.Set("ark", c.Ark)
	}
	q.Set("sequentia", c.Request)
	return "bitcoin:" + addr + "?" + q.Encode()
}

// ---------- the follower ----------

// followArca syncs the wallet and attributes what arrived.
func (a *App) followArca() time.Duration {
	var s struct {
		Mailbox struct {
			Accepted []struct {
				LeafID string `json:"leaf_id"`
				Asset  string `json:"asset"`
				Value  string `json:"value"`
			} `json:"accepted"`
		} `json:"mailbox"`
		Schedule struct {
			Now        int64  `json:"now"`
			NextSyncAt *int64 `json:"next_sync_at"`
		} `json:"schedule"`
	}
	wait := time.Minute
	if err := a.arca(10*time.Minute, &s, "sync"); err != nil {
		log.Printf("arca: sync: %v", err)
		return wait
	}
	for _, c := range s.Mailbox.Accepted {
		a.attribute(c.LeafID, c.Asset, c.Value)
	}
	a.followArk()
	a.followInvoices()
	if s.Schedule.NextSyncAt != nil {
		if d := time.Duration(*s.Schedule.NextSyncAt-s.Schedule.Now) * time.Second; d > wait {
			wait = d
		}
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute // a tester waits on the credit
	}
	return wait
}

// attribute finds the request a coin paid: its record carries the
// request's owner key and nonce side by side.
func (a *App) attribute(leaf, asset, value string) {
	var rec struct {
		Record string `json:"record"`
	}
	if err := a.arca(time.Minute, &rec, "record", leaf); err != nil {
		log.Printf("arca: record %s: %v", leaf, err)
		return
	}
	rows, err := a.db.Query("SELECT id, owner, owner_nonce FROM arca_codes WHERE paid_at = 0")
	if err != nil {
		return
	}
	type open struct {
		id   int64
		mark string
	}
	var opens []open
	for rows.Next() {
		var o open
		var owner, nonce string
		rows.Scan(&o.id, &owner, &nonce)
		o.mark = strings.ToLower(owner + nonce)
		opens = append(opens, o)
	}
	rows.Close()
	r := strings.ToLower(rec.Record)
	atoms, _ := strconv.ParseInt(value, 10, 64)
	for _, o := range opens {
		if strings.Contains(r, o.mark) {
			a.db.Exec("UPDATE arca_codes SET paid_leaf = ?, paid_asset = ?, paid_atoms = ?, paid_at = ? WHERE id = ? AND paid_at = 0",
				leaf, asset, atoms, now(), o.id)
			return
		}
	}
	log.Printf("arca: coin %s (%s atoms of %s) paid no open request", leaf, value, short(asset))
}

// followArk reads what arrived at each account's Bitcoin ark address.
func (a *App) followArk() {
	rows, err := a.db.Query("SELECT id, ark_address FROM arca_codes WHERE ark_address != '' AND btc_sats = 0")
	if err != nil {
		return
	}
	type open struct {
		id   int64
		addr string
	}
	var opens []open
	for rows.Next() {
		var o open
		rows.Scan(&o.id, &o.addr)
		opens = append(opens, o)
	}
	rows.Close()
	if len(opens) == 0 {
		return
	}
	// history syncs Bark; the lookups then read what it recorded.
	var hist []json.RawMessage
	if err := a.arca(5*time.Minute, &hist, "bitcoin", "--quiet", "history"); err != nil {
		log.Printf("arca: bitcoin history: %v", err)
		return
	}
	for _, o := range opens {
		var moves []struct {
			Status     string `json:"status"`
			ReceivedOn []struct {
				Destination struct {
					Value string `json:"value"`
				} `json:"destination"`
				AmountSat int64 `json:"amount_sat"`
			} `json:"received_on"`
		}
		if err := a.arca(time.Minute, &moves, "bitcoin", "--quiet", "address", "lookup", "--address", o.addr); err != nil {
			continue
		}
		var sats int64
		for _, m := range moves {
			if m.Status != "successful" {
				continue
			}
			for _, r := range m.ReceivedOn {
				if r.Destination.Value == o.addr {
					sats += r.AmountSat
				}
			}
		}
		if sats > 0 {
			a.db.Exec("UPDATE arca_codes SET btc_sats = ? WHERE id = ?", sats, o.id)
		}
	}
}

// followInvoices brings the state of each invoice payment in a Sequentia
// asset still under way up to date: the wallet's own record of it, which
// `sync` moves on. A payment the receiving wallet has not taken yet answers
// `paying` when it is made, and the invoice task's check reads the state
// recorded here.
func (a *App) followInvoices() {
	rows, err := a.db.Query(`SELECT id, ref FROM arca_sends WHERE purpose = 'invoice' AND asset != 'btc' AND ref != ''
		AND state NOT IN ('paid', 'returned', 'failed') AND created_at > ?`, now()-7*24*3600)
	if err != nil {
		return
	}
	type open struct {
		id   int64
		hash string
	}
	var opens []open
	for rows.Next() {
		var o open
		rows.Scan(&o.id, &o.hash)
		opens = append(opens, o)
	}
	rows.Close()
	if len(opens) == 0 {
		return
	}
	var pays map[string]struct {
		State string `json:"state"`
	}
	if err := a.arca(time.Minute, &pays, "lightning", "payments"); err != nil {
		log.Printf("arca: lightning payments: %v", err)
		return
	}
	for _, o := range opens {
		if p, ok := pays[o.hash]; ok && p.State != "" {
			a.db.Exec("UPDATE arca_sends SET state = ? WHERE id = ?", p.State, o.id)
		}
	}
}

func (a *App) startArcaFollower() {
	if !a.arcaOn() {
		return
	}
	go func() {
		for {
			time.Sleep(a.followArca())
		}
	}()
	go func() { // the invoice limits a task page shows (invoicelimit.go)
		for {
			a.invoiceCaps()
			time.Sleep(capsFresh)
		}
	}()
}

// ---------- invoices ----------

var bolt11Re = regexp.MustCompile(`(?i)^ln([a-z]+?)(\d+)([munp]?)1[02-9ac-hj-np-z]+$`)

// invoiceAmount reads the amount an invoice names, in sats (atoms for an
// asset invoice), from its human-readable part. An invoice naming no
// amount is refused: Emissio pays only a known amount.
func invoiceAmount(inv string) (int64, error) {
	m := bolt11Re.FindStringSubmatch(strings.TrimSpace(inv))
	if m == nil {
		return 0, errors.New("not an invoice that names an amount")
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("not an invoice that names an amount")
	}
	// amount in whole coins times the multiplier, in units of 1e-8
	switch strings.ToLower(m[3]) {
	case "":
		return n * 100_000_000, nil
	case "m":
		return n * 100_000, nil
	case "u":
		return n * 100, nil
	case "n":
		if n%10 != 0 {
			return 0, errors.New("an invoice for a fraction of a sat")
		}
		return n / 10, nil
	case "p":
		if n%10_000 != 0 {
			return 0, errors.New("an invoice for a fraction of a sat")
		}
		return n / 10_000, nil
	}
	return 0, errors.New("not an invoice that names an amount")
}

// ---------- the request handler ----------

// handleArcaRequest serves the web wallet tasks' buttons: make the
// account's receive code, pay the account's receive code, or pay its
// invoice.
func (a *App) handleArcaRequest(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	task, err := taskBySlug(a.db, r.PathValue("slug"))
	if err != nil || !isArcaTask(task.CheckKind) {
		http.NotFound(w, r)
		return
	}
	back := "/tasks/" + task.Slug
	if !a.gate(w, r, user, back) {
		return
	}
	if !task.Active || (task.Cap > 0 && task.Awarded >= task.Cap) {
		a.redirect(w, r, back, "", "This task is closed.")
		return
	}
	if !a.arcaOn() {
		a.redirect(w, r, back, "", "Emissio's wallet is not taking part on this site right now.")
		return
	}
	if walletLinkOf(a.db, user.ID) == nil {
		a.redirect(w, r, back, "", "Link your web wallet first (the Link your web wallet task).")
		return
	}
	var done int64
	a.db.QueryRow("SELECT COUNT(*) FROM submissions WHERE user_id = ? AND task_id = ? AND status = 'approved'", user.ID, task.ID).Scan(&done)
	if done > 0 {
		a.redirect(w, r, back, "", "You have completed this task.")
		return
	}
	purpose := arcaPurpose(task.CheckKind)
	switch task.CheckKind {
	case "arca-pay-code", "arca-pay-btc", "arca-swap":
		if c := a.codeOf(user.ID, purpose); c != nil && c.PaidAt == 0 && c.BtcSats == 0 && now()-c.CreatedAt < 20*86400 {
			a.redirect(w, r, back, "", "Your receive code from Emissio is below; it works for 20 days.")
			return
		}
		if _, err := a.newCode(user.ID, purpose, "", 0); err != nil {
			log.Printf("arca: code for user %d: %v", user.ID, err)
			a.redirect(w, r, back, "", "Emissio could not make a receive code just now. Try again later.")
			return
		}
		a.redirect(w, r, back, "Your receive code from Emissio is below.", "")
	case "arca-roundtrip", "arca-invoice", "arca-name":
		var mine, ipToday int64
		a.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE user_id = ? AND purpose = ?", user.ID, purpose).Scan(&mine)
		a.db.QueryRow("SELECT COUNT(*) FROM arca_sends WHERE ip = ? AND created_at > ?", clientIP(r), now()-86400).Scan(&ipToday)
		if mine >= paymentsPerTask {
			a.redirect(w, r, back, "", fmt.Sprintf("Emissio has paid you %d times for this task, the most it pays one account.", paymentsPerTask))
			return
		}
		if ipToday >= arcaSendsDay {
			a.redirect(w, r, back, "", "Emissio has paid enough accounts from your network today. Try again tomorrow.")
			return
		}
		switch task.CheckKind {
		case "arca-roundtrip":
			a.payCode(w, r, user, back, purpose, r.FormValue("code"), "Emissio has paid your receive code. Send back what arrived, to Emissio's code below.")
		case "arca-name":
			a.payName(w, r, user, back, purpose)
		default:
			a.payInvoice(w, r, user, back, purpose)
		}
	}
}

func isArcaTask(kind string) bool {
	switch kind {
	case "arca-pay-code", "arca-pay-btc", "arca-roundtrip", "arca-invoice", "arca-swap", "arca-name":
		return true
	}
	return false
}

func arcaPurpose(kind string) string {
	switch kind {
	case "arca-pay-btc":
		return "btc"
	case "arca-pay-code":
		return "code"
	}
	return strings.TrimPrefix(kind, "arca-")
}

// payCode pays a receive code of the account's wallet a random amount of an
// asset Emissio holds, and makes the code the account returns it to.
func (a *App) payCode(w http.ResponseWriter, r *http.Request, user *User, back, purpose, code, done string) {
	_, _, req, _ := codeParts(code)
	if req == "" {
		a.redirect(w, r, back, "", "Paste the receive code your web wallet shows under Receive.")
		return
	}
	op, genesis := a.arcaChain()
	req, err := expandRequest(req, op, genesis)
	if err != nil {
		a.redirect(w, r, back, "", "That receive code cannot be paid here: "+err.Error()+".")
		return
	}
	d, err := decodeRequest(req)
	if err != nil {
		a.redirect(w, r, back, "", "Paste the receive code your web wallet shows under Receive.")
		return
	}
	if op := a.arcaOperator(); op != "" && !strings.EqualFold(op, d.Operator) {
		a.redirect(w, r, back, "", "That receive code is for another server than Emissio's.")
		return
	}
	if d.Until != 0 && d.Until < time.Now().Unix()+3600 {
		a.redirect(w, r, back, "", "That receive code has lapsed: show a new one in your wallet and paste it.")
		return
	}
	asset, atoms, err := a.arcaPick()
	if err != nil {
		log.Printf("arca: pick for user %d: %v", user.ID, err)
		a.redirect(w, r, back, "", "Emissio could not pay just now. Try again later.")
		return
	}
	var sent struct {
		Transfer struct {
			TransferID string `json:"transfer_id"`
		} `json:"transfer"`
	}
	if err := a.arca(3*time.Minute, &sent, "send", req, "--amount", strconv.FormatInt(atoms, 10), "--asset", asset, "--route", "leaf"); err != nil {
		log.Printf("arca: send to user %d: %v", user.ID, err)
		a.redirect(w, r, back, "", "Emissio could not pay that code: "+err.Error())
		return
	}
	a.db.Exec("INSERT INTO arca_sends (user_id, purpose, target, asset, atoms, state, ref, ip, created_at) VALUES (?,?,?,?,?,?,?,?,?)",
		user.ID, purpose, d.Owner, asset, atoms, "sent", sent.Transfer.TransferID, clientIP(r), now())
	if _, err := a.newCode(user.ID, purpose, "", 0); err != nil {
		log.Printf("arca: return code for user %d: %v", user.ID, err)
	}
	a.redirect(w, r, back, done, "")
}

// wordsOnly is a wallet's refusal without the record it may carry after
// its sentence (`...: {"amount":...}`), which says nothing to a person.
func wordsOnly(msg string) string {
	if i := strings.Index(msg, ": {"); i > 0 {
		return msg[:i] + "."
	}
	return msg
}

// addressLike is whether text looks like a bare address of either chain,
// transparent or confidential, rather than a receive code or an invoice.
func addressLike(text string) bool {
	t := strings.ToLower(text)
	for _, p := range []string{"tb1", "bc1", "bcrt1", "tsqb1", "sqb1", "tex1", "lq1", "el1"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// nameRe is what a payment name may be (the web wallet's lnaddr.js).
var nameRe = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// payName pays the account's wallet at its payment name: the name service
// gives the receive code the wallet makes while it is open, and Emissio pays
// it as it pays a pasted code. Only the wallet holding the name sees what
// arrived, which is what the account sends back.
func (a *App) payName(w http.ResponseWriter, r *http.Request, user *User, back, purpose string) {
	if a.cfg.LnaddrURL == "" {
		a.redirect(w, r, back, "", "Payment names are not taking part on this site right now.")
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("code")))
	if i := strings.LastIndex(name, "@"); i >= 0 {
		if dom := name[i+1:]; a.cfg.LnaddrDomain != "" && dom != a.cfg.LnaddrDomain {
			a.redirect(w, r, back, "", "Emissio pays names ending in @"+a.cfg.LnaddrDomain+".")
			return
		}
		name = name[:i]
	}
	if !nameRe.MatchString(name) {
		a.redirect(w, r, back, "", "A name has 3 to 32 characters: lowercase letters, digits, - and _.")
		return
	}
	code, err := a.resolveName(name)
	if err != nil {
		a.redirect(w, r, back, "", err.Error())
		return
	}
	a.payCode(w, r, user, back, purpose, code, "Emissio has paid you at "+name+"@"+a.cfg.LnaddrDomain+". Send back what arrived, to Emissio's code below.")
}

// resolveName asks the name service for the receive code of the wallet
// holding a name. A wallet that is not open gives none Emissio can pay.
func (a *App) resolveName(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.LnaddrURL+"/lnaddr/"+name, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("lnaddr: %s: %v", name, err)
		return "", errors.New("The payment name service did not answer. Try again later.")
	}
	defer resp.Body.Close()
	var out struct {
		Code   string `json:"code"`
		Fresh  bool   `json:"fresh"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", errors.New("The payment name service did not answer. Try again later.")
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || (resp.StatusCode != http.StatusOK && out.Reason != ""):
		if out.Reason == "" {
			out.Reason = "No wallet goes by that name."
		}
		return "", errors.New(out.Reason)
	case resp.StatusCode != http.StatusOK:
		return "", errors.New("The payment name service did not answer. Try again later.")
	case !out.Fresh || out.Code == "":
		return "", errors.New("Your web wallet is not open: open it, keep it open, and ask again.")
	}
	return out.Code, nil
}

// payInvoice pays an invoice the account's wallet made, in BTC or a
// Sequentia asset, up to a small amount (invoicelimit.go).
func (a *App) payInvoice(w http.ResponseWriter, r *http.Request, user *User, back, purpose string) {
	inv := strings.TrimSpace(r.FormValue("code"))
	_, _, _, li := codeParts(inv)
	switch {
	case li != "":
		inv = li
	case strings.HasPrefix(strings.ToLower(inv), "bitcoin:"):
		a.redirect(w, r, back, "", "That receive code carries no invoice yet. In the web wallet, after you type the amount, wait a few seconds for the code to be drawn again with its invoice (it then holds LIGHTNING=), and copy it then. If it never gets one, try a smaller amount or another asset.")
		return
	case addressLike(inv):
		a.redirect(w, r, back, "", "That is an address, not a receive code with an amount. This task pays an invoice, which an address, transparent or confidential, does not carry: in the web wallet, open Receive, type an amount, and copy the whole code.")
		return
	}
	amt, err := invoiceAmount(inv)
	if err != nil {
		a.redirect(w, r, back, "", "Paste an invoice for an amount: in the web wallet, Receive, an amount, then copy the code.")
		return
	}
	asset, err := invoiceAsset(inv)
	if err != nil {
		a.redirect(w, r, back, "", "Paste an invoice for an amount: in the web wallet, Receive, an amount, then copy the code.")
		return
	}
	if why := a.invoiceRefusal(asset, amt); why != "" {
		a.redirect(w, r, back, "", why)
		return
	}
	var out struct {
		Payment *struct {
			State       string `json:"state"`
			Preimage    string `json:"preimage"`
			PaymentHash string `json:"payment_hash"`
		} `json:"payment"`
		Paying struct {
			PaymentHash string `json:"payment_hash"`
		} `json:"paying"`
	}
	if err := a.arca(3*time.Minute, &out, "lightning", "pay", inv); err != nil {
		log.Printf("arca: invoice for user %d: %v", user.ID, err)
		if strings.Contains(err.Error(), "is paying this invoice already") {
			a.redirect(w, r, back, "", "Emissio is already paying that invoice, or has paid it: asking again pays nothing more. Keep your web wallet open, its tab in front or behind, and the payment completes there. To be paid again, make a new receive code.")
			return
		}
		a.redirect(w, r, back, "", "Emissio could not pay that invoice: "+wordsOnly(err.Error()))
		return
	}
	state, hash := "unknown", out.Paying.PaymentHash
	if out.Payment != nil {
		state = out.Payment.State
		if hash == "" {
			hash = out.Payment.PaymentHash
		}
	}
	// The payment hash is the ref the follower reads a payment still under
	// way by (followInvoices).
	a.db.Exec("INSERT INTO arca_sends (user_id, purpose, target, asset, atoms, state, ref, ip, created_at) VALUES (?,?,?,?,?,?,?,?,?)",
		user.ID, purpose, inv, asset, amt, state, hash, clientIP(r), now())
	if state != "paid" {
		a.redirect(w, r, back, "Emissio's payment is on its way. Keep your web wallet open: it completes when the wallet takes it, and Emissio sees that within a few minutes. Then submit the task.", "")
		return
	}
	a.redirect(w, r, back, "Emissio paid your invoice. Submit the task.", "")
}

// arcaAddress is the on-chain address of Emissio's arca wallet, made once
// and kept: the part of its receive codes any wallet reads.
func (a *App) arcaAddress() string {
	var addr string
	if a.db.QueryRow("SELECT value FROM kv WHERE key = 'arca_address'").Scan(&addr) == nil && addr != "" {
		return addr
	}
	var raw string
	if a.arca(time.Minute, &raw, "address") != nil {
		return ""
	}
	var obj struct {
		Address string `json:"address"`
	}
	if json.Unmarshal([]byte(raw), &obj) == nil && obj.Address != "" {
		addr = obj.Address
	} else {
		json.Unmarshal([]byte(raw), &addr)
		if addr == "" {
			addr = strings.Trim(raw, "\" \n")
		}
	}
	if addr != "" {
		a.db.Exec("INSERT INTO kv (key, value) VALUES ('arca_address', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", addr)
	}
	return addr
}

// arcaOperator is the operator key the wallet pinned.
func (a *App) arcaOperator() string {
	op, _ := a.arcaChain()
	return op
}

// arcaChain is the operator key and the chain's genesis hash of Emissio's
// wallet, or empty strings when the wallet does not answer.
func (a *App) arcaChain() (operator, genesis string) {
	var info struct {
		Operator string `json:"operator"`
		Genesis  string `json:"genesis_hash"`
	}
	if a.arca(time.Minute, &info, "info") != nil {
		return "", ""
	}
	return info.Operator, info.Genesis
}

// arcaPick chooses a random asset Emissio holds enough of, and a random
// amount of it.
func (a *App) arcaPick() (string, int64, error) {
	var bal struct {
		Arca map[string]map[string]string `json:"arca"`
	}
	if err := a.arca(2*time.Minute, &bal, "balance", "--without-bitcoin"); err != nil {
		return "", 0, err
	}
	var cands []string
	for asset, st := range bal.Arca {
		v, _ := strconv.ParseInt(st["live"], 10, 64)
		if v >= minPaymentAtoms+paymentAtomsRange+proofAtoms {
			cands = append(cands, asset)
		}
	}
	if len(cands) == 0 {
		return "", 0, errors.New("no asset with enough live coins")
	}
	return cands[randInt(int64(len(cands)))], minPaymentAtoms + randInt(paymentAtomsRange), nil
}

// ---------- the checks ----------

func (a *App) arcaCheck(kind string, user *User) checkResult {
	if !a.arcaOn() {
		return review("Emissio's wallet is not set up here; manual review")
	}
	purpose := arcaPurpose(kind)
	switch kind {
	case "arca-invoice":
		var state string
		a.db.QueryRow("SELECT state FROM arca_sends WHERE user_id = ? AND purpose = ? ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&state)
		switch state {
		case "":
			return fail("Emissio has not paid an invoice of yours yet: paste one above")
		case "paid":
			return pass("Emissio paid this account's invoice (OK)")
		}
		return fail("Emissio's payment of your invoice is %s, not paid: keep your wallet open and submit again", state)
	case "arca-roundtrip", "arca-name":
		var asset string
		var atoms int64
		if a.db.QueryRow("SELECT asset, atoms FROM arca_sends WHERE user_id = ? AND purpose = ? ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&asset, &atoms) != nil {
			return fail("Emissio has not paid your receive code yet: paste it above and ask Emissio to pay you")
		}
		c := a.codeOf(user.ID, purpose)
		switch {
		case c == nil || c.PaidAt == 0:
			return fail("nothing has reached Emissio's code yet; Emissio checks every few minutes")
		case c.PaidAsset != asset || c.PaidAtoms > atoms || atoms-c.PaidAtoms > returnShortAtoms:
			return fail("Emissio's code received %s of %s, not what Emissio sent you (a return may fall short by up to %s, what a wallet keeps back for its transfer's fees)",
				amountText(c.PaidAtoms), short(c.PaidAsset), amountText(returnShortAtoms))
		}
		if c.PaidAtoms < atoms {
			return pass("received what Emissio paid and sent %s of %s back, %s short for the transfer's fees (OK)", amountText(c.PaidAtoms), short(asset), amountText(atoms-c.PaidAtoms))
		}
		return pass("received what Emissio paid and sent exactly %s of %s back (OK)", amountText(atoms), short(asset))
	case "arca-pay-btc":
		c := a.codeOf(user.ID, purpose)
		switch {
		case c == nil:
			return fail("ask Emissio for your receive code above first")
		case c.BtcSats == 0:
			return fail("no BTC has reached your code's Bitcoin part yet; Emissio checks every few minutes")
		}
		return pass("%d sats reached this account's own Bitcoin ark address at Emissio (OK)", c.BtcSats)
	case "arca-pay-code", "arca-swap":
		c := a.codeOf(user.ID, purpose)
		switch {
		case c == nil:
			return fail("ask Emissio for your receive code above first")
		case c.PaidAt == 0:
			return fail("nothing has reached your Emissio code yet; Emissio checks every few minutes")
		}
		if kind == "arca-swap" {
			want, _ := strconv.ParseInt(proofTag(user.ClaimCode), 10, 64)
			if c.PaidAtoms%proofAtoms != want {
				return fail("received %s of %s, which does not end in your proof decimals .%s", amountText(c.PaidAtoms), short(c.PaidAsset), proofTag(user.ClaimCode))
			}
			if c.PaidAsset == a.cfg.PolicyAsset {
				return fail("paid in tSEQ: pay in the asset your swap got you")
			}
		}
		return pass("this account's own Emissio code received %s of %s (OK)", amountText(c.PaidAtoms), short(c.PaidAsset))
	}
	return review("reviewed by hand")
}

// arcaView is what a web wallet task's page shows.
type arcaView struct {
	Kind      string
	Code      string // Emissio's receive code for this account and task
	Spent     bool   // that code was paid already: it takes no other payment
	Sent      bool   // Emissio has paid the account (the amount is not shown)
	SentAt    int64
	Ready     bool   // the task's check passes now, so the submission form opens
	Next      string // what the check still misses, when not Ready
	Linked    bool
	Available bool
	Caps      string // the invoice task: the most Emissio pays, in each asset
}

func (a *App) arcaViewFor(user *User, task *Task) *arcaView {
	if !isArcaTask(task.CheckKind) {
		return nil
	}
	v := &arcaView{Kind: task.CheckKind, Available: a.arcaOn()}
	if task.CheckKind == "arca-invoice" {
		v.Caps = capsText(a.shownCaps())
	}
	if user == nil {
		return v
	}
	v.Linked = walletLinkOf(a.db, user.ID) != nil
	purpose := arcaPurpose(task.CheckKind)
	if c := a.codeOf(user.ID, purpose); c != nil {
		v.Code = a.receiveCode(c)
		v.Spent = c.PaidAt != 0
	}
	a.db.QueryRow("SELECT created_at FROM arca_sends WHERE user_id = ? AND purpose = ? ORDER BY id DESC LIMIT 1", user.ID, purpose).Scan(&v.SentAt)
	v.Sent = v.SentAt > 0
	if v.Linked && v.Available {
		res := a.arcaCheck(task.CheckKind, user)
		v.Ready = res.Verdict == vPass
		if !v.Ready {
			v.Next = res.Note
		}
	}
	return v
}
