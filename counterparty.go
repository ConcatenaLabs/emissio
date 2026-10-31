package main

// Tasks where Emissio is the counterparty. Emissio's own testnet wallet,
// named in the configuration and held by the public node, pays a tester an
// amount nobody can predict: a random fraction of a random asset the wallet
// holds, its fee paid in that same asset. The tester then proves the
// payment reached a wallet they control, by returning it (spending the very
// output Emissio paid, the exact amount back, the fee in that asset) or,
// for a confidential payment, by naming the amount, which only the holder
// of the receiving address's blinding key can see.
//
// The wallet is named, never picked: a name that reads as a reserve,
// treasury, bridge or service wallet is refused, so a misconfiguration can
// never spend from one of those.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const counterpartySchema = `
CREATE TABLE IF NOT EXISTS emissio_payments (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id),
	kind TEXT NOT NULL,
	address TEXT NOT NULL,
	asset TEXT NOT NULL,
	atoms INTEGER NOT NULL,
	txid TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	ip TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS emissio_payments_user ON emissio_payments(user_id, kind);
CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

const (
	paymentsPerTask   = 3 // payments one account can ask for, per task
	paymentGap        = 24 * time.Hour
	paymentsPerIPDay  = 3
	unblindAttempts   = 5
	minPaymentAtoms   = 10_000_000 // 0.1 of a unit
	paymentAtomsRange = 90_000_000 // up to 1.0
)

var forbiddenWallet = regexp.MustCompile(`(?i)reserve|treasury|bridge|faucet|sbtc|service|founder|custody|pool|stake`)

// walletName is the configured wallet, or an error saying why it may not be used.
func (a *App) walletName() (string, error) {
	w := a.cfg.Wallet
	if w == "" || a.cfg.NodeCLI == "" || a.cfg.NodeDatadir == "" {
		return "", errors.New("Emissio's wallet is not set up on this instance")
	}
	if forbiddenWallet.MatchString(w) {
		return "", fmt.Errorf("refusing wallet %q: it reads as a reserve, treasury, bridge or service wallet", w)
	}
	return w, nil
}

func isCounterparty(kind string) bool { return kind == "emissio-return" || kind == "emissio-unblind" }

// cli runs one node RPC through sequentia-cli, against the wallet when asked.
func (a *App) cli(wallet bool, args ...string) ([]byte, error) {
	full := []string{"-datadir=" + a.cfg.NodeDatadir}
	if wallet {
		w, err := a.walletName()
		if err != nil {
			return nil, err
		}
		full = append(full, "-rpcwallet="+w)
	}
	full = append(full, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, a.cfg.NodeCLI, full...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return bytes.TrimSpace(out), nil
}

// walletCall runs a wallet RPC, loading the wallet once if the node
// restarted without it.
func (a *App) walletCall(args ...string) ([]byte, error) {
	out, err := a.cli(true, args...)
	if err != nil && (strings.Contains(err.Error(), "not loaded") || strings.Contains(err.Error(), "does not exist")) {
		w, _ := a.walletName()
		if _, lerr := a.cli(false, "loadwallet", w); lerr == nil {
			out, err = a.cli(true, args...)
		}
	}
	return out, err
}

// returnAddress is where testers send Emissio's payments back: one
// transparent address of the wallet, made once and kept.
func (a *App) returnAddress() (string, error) {
	var addr string
	if a.db.QueryRow("SELECT value FROM kv WHERE key = 'return_address'").Scan(&addr) == nil && addr != "" {
		return addr, nil
	}
	out, err := a.walletCall("getnewaddress", "emissio-returns", "bech32")
	if err != nil {
		return "", err
	}
	addr = string(out)
	_, err = a.db.Exec("INSERT INTO kv (key, value) VALUES ('return_address', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", addr)
	return addr, err
}

// atomsOf parses a decimal amount with up to eight places exactly.
func atomsOf(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 8 {
		return 0, false
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return 0, false
	}
	frac += strings.Repeat("0", 8-len(frac))
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, false
	}
	return w*proofAtoms + f, true
}

func amountText(atoms int64) string {
	return fmt.Sprintf("%d.%08d", atoms/proofAtoms, atoms%proofAtoms)
}

// spendable sums the wallet's confirmed coins per asset.
func (a *App) spendable() (map[string]int64, error) {
	out, err := a.walletCall("listunspent", "1")
	if err != nil {
		return nil, err
	}
	var coins []struct {
		Asset     string      `json:"asset"`
		Amount    json.Number `json:"amount"`
		Spendable bool        `json:"spendable"`
	}
	d := json.NewDecoder(bytes.NewReader(out))
	d.UseNumber()
	if err := d.Decode(&coins); err != nil {
		return nil, err
	}
	sums := map[string]int64{}
	for _, c := range coins {
		if v, ok := atomsOf(c.Amount.String()); ok && c.Spendable && c.Asset != "" {
			sums[c.Asset] += v
		}
	}
	return sums, nil
}

func randInt(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		panic(err)
	}
	return v.Int64()
}

// pay sends the tester a random amount of a random asset the wallet can
// afford, the fee in the same asset. Any asset the wallet holds stands
// equal; an asset the node will not take a fee in is skipped for the next.
func (a *App) pay(address string) (asset string, atoms int64, txid string, err error) {
	sums, err := a.spendable()
	if err != nil {
		return "", 0, "", err
	}
	allowed := map[string]bool{}
	for _, s := range a.cfg.WalletAssets {
		if s = strings.TrimSpace(strings.ToLower(s)); s != "" {
			allowed[s] = true
		}
	}
	var candidates []string
	for as, v := range sums {
		if v >= minPaymentAtoms+paymentAtomsRange+proofAtoms && (len(allowed) == 0 || allowed[as]) {
			candidates = append(candidates, as)
		}
	}
	if len(candidates) == 0 {
		return "", 0, "", errors.New("the wallet holds no asset with enough to pay")
	}
	// Shuffle, so no asset is the default.
	for i := len(candidates) - 1; i > 0; i-- {
		j := randInt(int64(i + 1))
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}
	atoms = minPaymentAtoms + randInt(paymentAtomsRange)
	var lastErr error
	for _, as := range candidates {
		out, err := a.walletCall("-named", "sendtoaddress", "address="+address, "amount="+amountText(atoms),
			"assetlabel="+as, "fee_asset_label="+as, "fee_rate="+a.cfg.WalletFeeRate)
		if err != nil {
			lastErr = err
			continue
		}
		return as, atoms, strings.TrimSpace(string(out)), nil
	}
	return "", 0, "", lastErr
}

type payment struct {
	ID        int64
	Kind      string
	Address   string
	Asset     string
	Atoms     int64
	Txid      string
	Attempts  int64
	CreatedAt int64
}

func (a *App) latestPayment(userID int64, kind string) *payment {
	var p payment
	err := a.db.QueryRow(`SELECT id, kind, address, asset, atoms, txid, attempts, created_at FROM emissio_payments
		WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, userID, kind).
		Scan(&p.ID, &p.Kind, &p.Address, &p.Asset, &p.Atoms, &p.Txid, &p.Attempts, &p.CreatedAt)
	if err != nil {
		return nil
	}
	return &p
}

// handlePaymentRequest asks Emissio's wallet to pay the tester for a
// counterparty task.
func (a *App) handlePaymentRequest(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	task, err := taskBySlug(a.db, r.PathValue("slug"))
	if err != nil || !isCounterparty(task.CheckKind) {
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
	if _, err := a.walletName(); err != nil {
		a.redirect(w, r, back, "", "Emissio cannot pay on this instance right now.")
		return
	}
	var done, first int64
	a.db.QueryRow("SELECT COUNT(*) FROM submissions WHERE user_id = ? AND task_id = ? AND status = 'approved'", user.ID, task.ID).Scan(&done)
	if done > 0 {
		a.redirect(w, r, back, "", "You have completed this task.")
		return
	}
	// Only an account that has already proven a wallet of its own is paid.
	a.db.QueryRow(`SELECT COUNT(*) FROM submissions s JOIN tasks t ON t.id = s.task_id
		WHERE s.user_id = ? AND t.slug = 'first-transaction' AND s.status = 'approved'`, user.ID).Scan(&first)
	if first == 0 {
		a.redirect(w, r, back, "", "Complete the first-transaction task first: Emissio pays accounts that have shown a wallet of their own.")
		return
	}
	var mine, ipToday, today int64
	a.db.QueryRow("SELECT COUNT(*) FROM emissio_payments WHERE user_id = ? AND kind = ?", user.ID, task.CheckKind).Scan(&mine)
	a.db.QueryRow("SELECT COUNT(*) FROM emissio_payments WHERE ip = ? AND created_at > ?", clientIP(r), now()-86400).Scan(&ipToday)
	a.db.QueryRow("SELECT COUNT(*) FROM emissio_payments WHERE created_at > ?", now()-86400).Scan(&today)
	if p := a.latestPayment(user.ID, task.CheckKind); p != nil && now()-p.CreatedAt < int64(paymentGap.Seconds()) {
		a.redirect(w, r, back, "", "Emissio paid you less than a day ago; that payment is shown below. You can ask for a new one a day after the last.")
		return
	}
	switch {
	case mine >= paymentsPerTask:
		a.redirect(w, r, back, "", fmt.Sprintf("Emissio has paid you %d times for this task, the most it pays one account.", paymentsPerTask))
		return
	case ipToday >= paymentsPerIPDay:
		a.redirect(w, r, back, "", "Emissio has paid enough accounts from your network today. Try again tomorrow.")
		return
	case today >= a.cfg.WalletDailyCap:
		a.redirect(w, r, back, "", "Emissio has made all of today's payments. Try again tomorrow.")
		return
	}
	addr := addressOf(r.FormValue("address"))
	if len(addr) > 120 || !regexp.MustCompile(`^[0-9A-Za-z]+$`).MatchString(addr) {
		a.redirect(w, r, back, "", "Enter one address of your wallet.")
		return
	}
	out, err := a.cli(false, "validateaddress", addr)
	var va struct {
		IsValid         bool   `json:"isvalid"`
		ConfidentialKey string `json:"confidential_key"`
		IsMine          bool   `json:"ismine"`
	}
	if err != nil || json.Unmarshal(out, &va) != nil || !va.IsValid {
		a.redirect(w, r, back, "", "That is not a Sequentia testnet address.")
		return
	}
	confidential := va.ConfidentialKey != ""
	if task.CheckKind == "emissio-unblind" && !confidential {
		a.redirect(w, r, back, "", "This task needs a confidential address, one starting tsqb.")
		return
	}
	if task.CheckKind == "emissio-return" && confidential {
		a.redirect(w, r, back, "", "This task needs a transparent address, one starting tb1.")
		return
	}
	if ret, _ := a.returnAddress(); strings.EqualFold(ret, addr) {
		a.redirect(w, r, back, "", "That is Emissio's own address. Enter one of your wallet's.")
		return
	}
	asset, atoms, txid, err := a.pay(addr)
	if err != nil {
		log.Printf("counterparty: paying user %d: %v", user.ID, err)
		a.redirect(w, r, back, "", "Emissio could not pay just now. Try again later.")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO emissio_payments (user_id, kind, address, asset, atoms, txid, ip, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, user.ID, task.CheckKind, addr, asset, atoms, txid, clientIP(r), now()); err != nil {
		a.serverError(w, err)
		return
	}
	a.recordChainLinks(user.ID, "paid-to", []string{addr})
	a.redirect(w, r, back, "Emissio has paid you. The details are below.", "")
}

// returnCheck: the transaction spends Emissio's payment, sends exactly that
// amount of that asset back, and pays its fee in that asset.
func (a *App) returnCheck(user *User, tx *chainTx) checkResult {
	p := a.latestPayment(user.ID, "emissio-return")
	if p == nil {
		return fail("Emissio has not paid you yet: ask for a payment on the task page first")
	}
	ret, err := a.returnAddress()
	if err != nil {
		return review("Emissio's return address unavailable; manual review")
	}
	spends := false
	for _, in := range tx.Vin {
		if strings.EqualFold(in.Txid, p.Txid) && in.Prevout != nil && strings.EqualFold(in.Prevout.Address, p.Address) {
			spends = true
		}
	}
	if !spends {
		return fail("this transaction does not spend Emissio's payment %s to %s", short(p.Txid), short(p.Address))
	}
	back := false
	for _, o := range tx.Vout {
		if strings.EqualFold(o.Address, ret) && o.Asset == p.Asset && o.Value != nil && *o.Value == p.Atoms {
			back = true
		}
	}
	if !back {
		return fail("no output pays exactly %s of asset %s back to %s", amountText(p.Atoms), short(p.Asset), ret)
	}
	if fee := feeAsset(tx); fee != p.Asset {
		return fail("returns the payment, but the fee is paid in %s: pay it in the asset you return, %s", short(fee), short(p.Asset))
	}
	return pass("spends Emissio's payment and returns exactly %s of asset %s, fee in the same asset (OK)", amountText(p.Atoms), short(p.Asset))
}

var amountRe = regexp.MustCompile(`[0-9]+(\.[0-9]{1,8})?`)

// unblindCheck: the notes name the amount of Emissio's confidential
// payment, as a number or inside an explorer link that unblinds it.
func (a *App) unblindCheck(user *User, notes string) checkResult {
	p := a.latestPayment(user.ID, "emissio-unblind")
	if p == nil {
		return fail("Emissio has not paid you yet: ask for a payment on the task page first")
	}
	if p.Attempts >= unblindAttempts {
		return fail("too many wrong amounts for this payment: ask Emissio for a new one a day after the last")
	}
	for _, m := range amountRe.FindAllString(notes, 64) {
		v, ok := atomsOf(m)
		if ok && (v == p.Atoms || (!strings.Contains(m, ".") && m == strconv.FormatInt(p.Atoms, 10))) {
			return pass("names the exact amount of Emissio's confidential payment %s (OK)", short(p.Txid))
		}
	}
	a.db.Exec("UPDATE emissio_payments SET attempts = attempts + 1 WHERE id = ?", p.ID)
	return fail("the notes do not name the amount Emissio paid in %s", short(p.Txid))
}

// paymentView is what the task page shows of a counterparty payment.
type paymentView struct {
	Payment       *payment
	Amount        string
	ReturnAddress string
	Confidential  bool
	Available     bool
}

func (a *App) paymentViewFor(user *User, task *Task) *paymentView {
	if !isCounterparty(task.CheckKind) {
		return nil
	}
	v := &paymentView{Confidential: task.CheckKind == "emissio-unblind"}
	_, err := a.walletName()
	v.Available = err == nil
	if user != nil {
		v.Payment = a.latestPayment(user.ID, task.CheckKind)
		if v.Payment != nil && !v.Confidential {
			v.Amount = amountText(v.Payment.Atoms)
		}
	}
	if v.Available && !v.Confidential {
		v.ReturnAddress, _ = a.returnAddress()
	}
	return v
}
