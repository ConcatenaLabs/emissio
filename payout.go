package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // HASH160 of a P2WPKH address's key; nothing else uses it
)

// The payout: every payable account's allocation paid in one payment, each a
// share of its own that the account's wallet collects by itself. A payout run
// pays from the treasury's wallet, whose key never touches this host:
//
//  1. The admin opens a run: every account with a positive balance, at least
//     one verified platform and a payout key is in it, at its balance times
//     EMISSIO_PAYOUT_ATOMS_PER_SEQ atoms of EMISSIO_PAYOUT_ASSET. Anyone else
//     is left out with the reason.
//  2. The treasury's wallet pays the run's list and the admin records the
//     payment's reference here.
//  3. Emissio records it only when the payment service's public status names
//     exactly the list, then follows it: paid, collected, returned.
//
// The payment service (EMISSIO_PAYOUT_URL) is an operator that pays many
// wallets from one round and pays back to the payer whatever is not collected
// by the deadline. No page names it: an account reads "paid to your wallet".
//
// EMISSIO_PAYOUT_NETWORK says which payout this is. "testnet" is the
// rehearsal: it pays tSEQ to each account's rehearsal key and moves no
// balance, because a balance here is a promise of mainnet SEQ and tSEQ is not
// that. "mainnet" pays SEQ to each account's payout key, the key of its
// registered address, and a collected allocation is a ledger debit; one that
// comes back stays owed and goes into the next run.

const payoutSchema = `
CREATE TABLE IF NOT EXISTS payout_keys (
	user_id INTEGER PRIMARY KEY REFERENCES users(id),
	payout_key TEXT NOT NULL DEFAULT '',
	rehearsal_key TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS payout_runs (
	id INTEGER PRIMARY KEY,
	network TEXT NOT NULL,
	asset TEXT NOT NULL,
	atoms_per_seq INTEGER NOT NULL,
	state TEXT NOT NULL,
	reference TEXT NOT NULL DEFAULT '',
	total_atoms INTEGER NOT NULL DEFAULT 0,
	accounts INTEGER NOT NULL DEFAULT 0,
	left_out INTEGER NOT NULL DEFAULT 0,
	fee_bound INTEGER NOT NULL DEFAULT 0,
	round_txid TEXT NOT NULL DEFAULT '',
	deadline INTEGER NOT NULL DEFAULT 0,
	collected_atoms INTEGER NOT NULL DEFAULT 0,
	returned_atoms INTEGER NOT NULL DEFAULT 0,
	return_txids TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS payout_runs_reference ON payout_runs(reference) WHERE reference != '';
CREATE TABLE IF NOT EXISTS payout_rows (
	id INTEGER PRIMARY KEY,
	run_id INTEGER NOT NULL REFERENCES payout_runs(id),
	user_id INTEGER NOT NULL REFERENCES users(id),
	named_key TEXT NOT NULL DEFAULT '',
	seq_amount INTEGER NOT NULL,
	atoms INTEGER NOT NULL,
	state TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	claim_index INTEGER NOT NULL DEFAULT -1,
	returned_in TEXT NOT NULL DEFAULT '',
	UNIQUE(run_id, user_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS payout_rows_key ON payout_rows(run_id, named_key) WHERE named_key != '';
-- An account is in at most one live payout at a time, so an allocation that
-- came back can be paid again but never twice.
CREATE UNIQUE INDEX IF NOT EXISTS payout_rows_open ON payout_rows(user_id) WHERE state IN ('planned', 'pending', 'paid');
CREATE UNIQUE INDEX IF NOT EXISTS ledger_payout ON ledger(kind, ref_id) WHERE kind = 'payout';
`

// ---------- keys ----------

var secpP, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)

// onCurve reports whether x is the x coordinate of a secp256k1 point.
func onCurve(x []byte) bool {
	xi := new(big.Int).SetBytes(x)
	if xi.Sign() == 0 || xi.Cmp(secpP) >= 0 {
		return false
	}
	y2 := new(big.Int).Exp(xi, big.NewInt(3), secpP)
	y2.Add(y2, big.NewInt(7)).Mod(y2, secpP)
	return new(big.Int).ModSqrt(y2, secpP) != nil
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var hex66 = regexp.MustCompile(`^0[23][0-9a-f]{64}$`)

// segwitProgram decodes a bech32 or bech32m address: its prefix, version
// and program.
func segwitProgram(addr string) (string, int, []byte, error) {
	hrp, data, checksum, err := bech32Decode(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, nil, err
	}
	if len(data) < 1 {
		return "", 0, nil, errors.New("no witness program")
	}
	prog, err := convertBits(data[1:], 5, 8, false)
	if err != nil {
		return "", 0, nil, err
	}
	v := data[0]
	if (v == 0 && checksum != bech32Const) || (v != 0 && checksum != bech32mConst) {
		return "", 0, nil, errors.New("wrong checksum kind for its version")
	}
	return hrp, v, prog, nil
}

// namedKey reads the key a wallet is paid under, given as an x-only key (64
// hex digits), a compressed public key (66), or a taproot address with one of
// `hrps`, whose program is its key. It answers the x-only key.
func namedKey(s string, hrps []string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case hex64.MatchString(s):
		b, _ := hex.DecodeString(s)
		if !onCurve(b) {
			return "", errors.New("that is not a valid public key")
		}
		return s, nil
	case hex66.MatchString(s):
		b, _ := hex.DecodeString(s[2:])
		if !onCurve(b) {
			return "", errors.New("that is not a valid public key")
		}
		return s[2:], nil
	}
	hrp, v, prog, err := segwitProgram(s)
	if err != nil {
		return "", errors.New("give a public key (64 or 66 hexadecimal characters) or a taproot address")
	}
	ok := false
	for _, h := range hrps {
		ok = ok || hrp == h
	}
	if !ok {
		return "", fmt.Errorf("an address starting %s1 is for another network", hrp)
	}
	if v != 1 || len(prog) != 32 {
		return "", errors.New("that address names only its key's hash: give its public key instead, or a taproot address")
	}
	if !onCurve(prog) {
		return "", errors.New("that taproot address has no valid key")
	}
	return hex.EncodeToString(prog), nil
}

// launchKey is the key the launch payout names for a registered mainnet
// address: a taproot address's program, or for a P2WPKH address the public
// key `given`, checked against the address's hash. "" and why when there is
// none.
func launchKey(addr, given string) (string, string) {
	if addr == "" {
		return "", "no mainnet payout address"
	}
	_, v, prog, err := segwitProgram(addr)
	if err != nil {
		return "", "the payout address does not decode"
	}
	switch {
	case v == 1 && len(prog) == 32:
		if !onCurve(prog) {
			return "", "the taproot payout address has no valid key"
		}
		return hex.EncodeToString(prog), ""
	case v == 0 && len(prog) == 32:
		return "", "the payout address is a script address, which cannot be paid this way: register a single-key address"
	case v == 0 && len(prog) == 20:
		given = strings.ToLower(strings.TrimSpace(given))
		if given == "" {
			return "", "the payout address names only its key's hash: add its public key on the account page"
		}
		if !hex66.MatchString(given) {
			return "", "the public key given is not a compressed public key"
		}
		b, _ := hex.DecodeString(given)
		sh := sha256.Sum256(b)
		rh := ripemd160.New()
		rh.Write(sh[:])
		if !bytes.Equal(rh.Sum(nil), prog) {
			return "", "the public key given is not the key of the payout address"
		}
		if !onCurve(b[1:]) {
			return "", "the public key given is not valid"
		}
		return given[2:], ""
	}
	return "", "the payout address cannot be paid this way"
}

type payoutKeys struct {
	PayoutKey    string // as the account gave it: the compressed key of a P2WPKH payout address
	RehearsalKey string // x-only
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func keysOf(db querier, userID int64) (payoutKeys, error) {
	var k payoutKeys
	err := db.QueryRow(`SELECT payout_key, rehearsal_key FROM payout_keys WHERE user_id = ?`, userID).Scan(&k.PayoutKey, &k.RehearsalKey)
	if err == sql.ErrNoRows {
		return k, nil
	}
	return k, err
}

func setKey(db *sql.DB, userID int64, col, val string) error {
	_, err := db.Exec(`INSERT INTO payout_keys (user_id, `+col+`, updated_at) VALUES (?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET `+col+` = excluded.`+col+`, updated_at = excluded.updated_at`, userID, val, now())
	return err
}

// handlePayoutKey saves the public key of a P2WPKH payout address.
func (a *App) handlePayoutKey(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	given := strings.ToLower(strings.TrimSpace(r.FormValue("key")))
	if given != "" {
		if _, why := launchKey(user.MainnetAddress, given); why != "" {
			a.redirect(w, r, "/account", "", "Key not saved: "+why+".")
			return
		}
	}
	if err := setKey(a.db, user.ID, "payout_key", given); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/account", "Public key saved: it is the key of your payout address.", "")
}

// handleRehearsalKey saves the key of the account's testnet wallet that the
// testnet rehearsal pays.
func (a *App) handleRehearsalKey(w http.ResponseWriter, r *http.Request) {
	user, ok := a.postGuard(w, r)
	if !ok {
		return
	}
	given := strings.TrimSpace(r.FormValue("key"))
	key := ""
	if given != "" {
		k, err := namedKey(given, a.cfg.PayoutHRPs)
		if err != nil {
			a.redirect(w, r, "/account", "", "Key not saved: "+err.Error()+".")
			return
		}
		key = k
	}
	if err := setKey(a.db, user.ID, "rehearsal_key", key); err != nil {
		a.serverError(w, err)
		return
	}
	a.redirect(w, r, "/account", "Testnet wallet key saved.", "")
}

// ---------- the payment service ----------

type serviceTerms struct {
	Served   bool
	MinAtoms int64
	FeeBound func(total int64, n int) int64
}

func (a *App) payoutCall(path string, body any, out any) error {
	if a.cfg.PayoutURL == "" {
		return errors.New("no payment service is configured (EMISSIO_PAYOUT_URL)")
	}
	method, rdr := "GET", io.Reader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		method, rdr = "POST", bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(a.cfg.PayoutURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("the payment service is unreachable: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("the payment service answers %d: %s", resp.StatusCode, serviceMessage(raw))
	}
	return json.Unmarshal(raw, out)
}

// serviceMessage is the reason in a refusal from the payment service, whose
// body is {"error": {"code", "message"}}, or the body itself.
func serviceMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &e)
	switch {
	case e.Error.Message != "":
		return e.Error.Message
	case e.Message != "":
		return e.Message
	}
	return strings.TrimSpace(string(raw))
}

func (a *App) terms(asset string) (serviceTerms, error) {
	var info struct {
		Assets []struct {
			Asset   string          `json:"asset"`
			MinLeaf string          `json:"min_leaf"`
			Fees    json.RawMessage `json:"fees"`
		} `json:"assets"`
		Fees json.RawMessage `json:"fees"`
	}
	if err := a.payoutCall("/v1/info", nil, &info); err != nil {
		return serviceTerms{}, err
	}
	var fees struct {
		RefreshPPM   int64  `json:"refresh_ppm"`
		RefreshBase  string `json:"refresh_base"`
		OffboardBase string `json:"offboard_base"`
	}
	json.Unmarshal(info.Fees, &fees)
	t := serviceTerms{}
	for _, as := range info.Assets {
		if !strings.EqualFold(as.Asset, asset) {
			continue
		}
		t.Served = true
		t.MinAtoms, _ = strconv.ParseInt(as.MinLeaf, 10, 64)
		if len(as.Fees) > 0 {
			json.Unmarshal(as.Fees, &fees)
		}
	}
	base, _ := strconv.ParseInt(fees.RefreshBase, 10, 64)
	off, _ := strconv.ParseInt(fees.OffboardBase, 10, 64)
	ppm := fees.RefreshPPM
	// The most the fee can be: the fixed parts exactly, the refresh fee at its
	// full rate on the coin given up. The treasury's wallet shows the exact
	// figure before it pays.
	t.FeeBound = func(total int64, n int) int64 {
		fixed := base*int64(n) + off
		var refresh int64
		for i := 0; i < 3; i++ {
			coin := new(big.Int).SetInt64(total + fixed + refresh)
			coin.Mul(coin, big.NewInt(ppm))
			coin.Add(coin, big.NewInt(999_999))
			coin.Div(coin, big.NewInt(1_000_000))
			refresh = coin.Int64() + base
		}
		return fixed + refresh
	}
	return t, nil
}

// ---------- runs ----------

type PayoutRun struct {
	ID             int64
	Network        string
	Asset          string
	AtomsPerSEQ    int64
	State          string
	Reference      string
	TotalAtoms     int64
	Accounts       int
	LeftOut        int
	FeeBound       int64
	RoundTxid      string
	Deadline       int64
	CollectedAtoms int64
	ReturnedAtoms  int64
	ReturnTxids    string
	Note           string
	CreatedAt      int64
	UpdatedAt      int64
}

type PayoutRow struct {
	ID         int64
	RunID      int64
	UserID     int64
	Email      string
	Key        string
	SEQ        int64
	Atoms      int64
	State      string
	Reason     string
	ClaimIndex int
	ReturnedIn string
}

const runCols = `id, network, asset, atoms_per_seq, state, reference, total_atoms, accounts, left_out, fee_bound,
	round_txid, deadline, collected_atoms, returned_atoms, return_txids, note, created_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (*PayoutRun, error) {
	var p PayoutRun
	err := row.Scan(&p.ID, &p.Network, &p.Asset, &p.AtomsPerSEQ, &p.State, &p.Reference, &p.TotalAtoms, &p.Accounts,
		&p.LeftOut, &p.FeeBound, &p.RoundTxid, &p.Deadline, &p.CollectedAtoms, &p.ReturnedAtoms, &p.ReturnTxids, &p.Note,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func getRun(db *sql.DB, id int64) (*PayoutRun, error) {
	return scanRun(db.QueryRow(`SELECT `+runCols+` FROM payout_runs WHERE id = ?`, id))
}

func runsWhere(db *sql.DB, where string, args ...any) ([]*PayoutRun, error) {
	rows, err := db.Query(`SELECT `+runCols+` FROM payout_runs WHERE `+where+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PayoutRun
	for rows.Next() {
		p, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func rowsOf(db *sql.DB, runID int64) ([]*PayoutRow, error) {
	rows, err := db.Query(`SELECT r.id, r.run_id, r.user_id, u.email, r.named_key, r.seq_amount, r.atoms, r.state, r.reason,
		r.claim_index, r.returned_in FROM payout_rows r JOIN users u ON u.id = r.user_id WHERE r.run_id = ? ORDER BY r.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PayoutRow
	for rows.Next() {
		var p PayoutRow
		if err := rows.Scan(&p.ID, &p.RunID, &p.UserID, &p.Email, &p.Key, &p.SEQ, &p.Atoms, &p.State, &p.Reason, &p.ClaimIndex,
			&p.ReturnedIn); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// rehearsal reports whether this deployment's payout is the testnet one.
func (a *App) rehearsal() bool { return a.cfg.PayoutNetwork != "mainnet" }

// openRun plans a run: every account with a positive balance, in order.
func (a *App) openRun(note string) (*PayoutRun, error) {
	t, err := a.terms(a.cfg.PayoutAsset)
	if err != nil {
		return nil, err
	}
	if !t.Served {
		return nil, fmt.Errorf("the payment service does not pay in asset %s", a.cfg.PayoutAsset)
	}
	accounts, err := allocationRows(a.db)
	if err != nil {
		return nil, err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO payout_runs (network, asset, atoms_per_seq, state, note, created_at, updated_at)
		VALUES (?,?,?,'planned',?,?,?)`, a.cfg.PayoutNetwork, a.cfg.PayoutAsset, a.cfg.PayoutAtomsPerSEQ, note, now(), now())
	if err != nil {
		return nil, err
	}
	runID, _ := res.LastInsertId()
	// Each account's key, and the first account (by id) that gives each key: a
	// wallet key is paid for one account only, in this run and in any other.
	keyOf := map[int64]string{}
	whyOf := map[int64]string{}
	first := map[string]int64{}
	for _, u := range accounts {
		keys, err := keysOf(tx, u.ID)
		if err != nil {
			return nil, err
		}
		if a.rehearsal() {
			keyOf[u.ID] = keys.RehearsalKey
			if keys.RehearsalKey == "" {
				whyOf[u.ID] = "no testnet wallet key given"
			}
		} else {
			keyOf[u.ID], whyOf[u.ID] = launchKey(u.MainnetAddress, keys.PayoutKey)
		}
		if k := keyOf[u.ID]; k != "" && first[k] == 0 {
			first[k] = u.ID
		}
	}
	var total int64
	var n, out int
	for _, u := range accounts {
		atoms := u.Balance * a.cfg.PayoutAtomsPerSEQ
		key, why := keyOf[u.ID], whyOf[u.ID]
		var open int64
		tx.QueryRow(`SELECT run_id FROM payout_rows WHERE user_id = ? AND state IN ('planned','pending','paid')`, u.ID).Scan(&open)
		switch {
		case u.VerifCount == 0:
			why = "no verified platform yet"
		case open != 0:
			why = fmt.Sprintf("already in payout run %d", open)
		case why == "" && first[key] != u.ID:
			why = fmt.Sprintf("gives the same wallet key as account %d: a wallet key is paid for one account only", first[key])
		}
		if why == "" && atoms < t.MinAtoms {
			why, key = fmt.Sprintf("%d atoms is below the smallest payment the service makes (%d)", atoms, t.MinAtoms), ""
		}
		state := "planned"
		if why != "" {
			state, key = "left_out", ""
			out++
		} else {
			total += atoms
			n++
		}
		if _, err := tx.Exec(`INSERT INTO payout_rows (run_id, user_id, named_key, seq_amount, atoms, state, reason)
			VALUES (?,?,?,?,?,?,?)`, runID, u.ID, key, u.Balance, atoms, state, why); err != nil {
			return nil, err
		}
	}
	if n == 0 {
		return nil, errors.New("no account can be paid in this run")
	}
	if _, err := tx.Exec(`UPDATE payout_runs SET total_atoms = ?, accounts = ?, left_out = ?, fee_bound = ? WHERE id = ?`,
		total, n, out, t.FeeBound(total, n), runID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return getRun(a.db, runID)
}

type paymentStatus struct {
	State    string `json:"state"`
	Asset    string `json:"asset"`
	Round    string `json:"round"`
	Deadline int64  `json:"deadline"`
	Claims   []struct {
		Index      int     `json:"index"`
		Key        string  `json:"key"`
		Value      string  `json:"value"`
		State      string  `json:"state"`
		ReturnedIn *string `json:"returned_in"`
	} `json:"claims"`
}

// matchRun checks a payment against a run's planned rows: the asset, every
// planned key exactly once with exactly its amount, nobody else.
func matchRun(p *PayoutRun, rows []*PayoutRow, st *paymentStatus) (map[string]int, error) {
	if !strings.EqualFold(st.Asset, p.Asset) {
		return nil, fmt.Errorf("the payment is in asset %s, and this run pays in %s", st.Asset, p.Asset)
	}
	byKey := map[string]*PayoutRow{}
	for _, r := range rows {
		if r.State != "left_out" {
			byKey[r.Key] = r
		}
	}
	idx := map[string]int{}
	for _, c := range st.Claims {
		k := strings.ToLower(c.Key)
		if _, twice := idx[k]; twice {
			return nil, fmt.Errorf("the payment names wallet key %s twice", k)
		}
		r := byKey[k]
		if r == nil {
			return nil, fmt.Errorf("the payment pays wallet key %s, which is not one of this run's", k)
		}
		v, err := strconv.ParseInt(c.Value, 10, 64)
		if err != nil || v != r.Atoms {
			return nil, fmt.Errorf("the payment pays account %d %s atoms, and the list says %d", r.UserID, c.Value, r.Atoms)
		}
		idx[k] = c.Index
	}
	for k, r := range byKey {
		if _, ok := idx[k]; !ok {
			return nil, fmt.Errorf("the payment leaves out account %d (wallet key %s)", r.UserID, k)
		}
	}
	return idx, nil
}

var ref64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// recordReference records a run's payment once it names exactly the list.
func (a *App) recordReference(runID int64, ref string) error {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if !ref64.MatchString(ref) {
		return errors.New("a payment reference is 64 hexadecimal characters, as the paying wallet shows it")
	}
	p, err := getRun(a.db, runID)
	if err != nil {
		return err
	}
	if p.State != "planned" {
		return fmt.Errorf("this run already has a payment (%s)", p.Reference)
	}
	var st paymentStatus
	if err := a.payoutCall("/v1/distribution_status", map[string]any{"distribution_id": ref}, &st); err != nil {
		return err
	}
	switch st.State {
	case "pending", "issued", "released":
	default:
		return fmt.Errorf("that payment is %s: it pays nobody", st.State)
	}
	rows, err := rowsOf(a.db, runID)
	if err != nil {
		return err
	}
	idx, err := matchRun(p, rows, &st)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE payout_runs SET state = 'paying', reference = ?, updated_at = ? WHERE id = ? AND state = 'planned'`,
		ref, now(), runID); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return errors.New("that payment is already recorded against another run")
		}
		return err
	}
	for _, r := range rows {
		if i, ok := idx[r.Key]; ok && r.State == "planned" {
			if _, err := tx.Exec(`UPDATE payout_rows SET state = 'pending', claim_index = ? WHERE id = ?`, i, r.ID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.followRun(runID)
	return nil
}

// followRun reads a run's payment and records what changed.
func (a *App) followRun(runID int64) {
	p, err := getRun(a.db, runID)
	if err != nil || (p.State != "paying" && p.State != "paid") {
		return
	}
	var st paymentStatus
	if err := a.payoutCall("/v1/distribution_status", map[string]any{"distribution_id": p.Reference}, &st); err != nil {
		log.Printf("payout run %d: %v", runID, err)
		return
	}
	rows, err := rowsOf(a.db, runID)
	if err != nil {
		return
	}
	byIndex := map[int]*PayoutRow{}
	for _, r := range rows {
		if r.ClaimIndex >= 0 {
			byIndex[r.ClaimIndex] = r
		}
	}
	switch st.State {
	case "void", "expired":
		// It never went out; the list stands and can be paid again.
		a.db.Exec(`UPDATE payout_runs SET state = 'planned', reference = '', updated_at = ? WHERE id = ?`, now(), runID)
		a.db.Exec(`UPDATE payout_rows SET state = 'planned', claim_index = -1 WHERE run_id = ? AND state = 'pending'`, runID)
		return
	case "released":
	default:
		return
	}
	if p.State == "paying" {
		a.db.Exec(`UPDATE payout_runs SET state = 'paid', round_txid = ?, deadline = ?, updated_at = ? WHERE id = ?`,
			st.Round, st.Deadline, now(), runID)
		a.db.Exec(`UPDATE payout_rows SET state = 'paid' WHERE run_id = ? AND state = 'pending'`, runID)
	}
	var collected, returned int64
	txids := map[string]bool{}
	open := 0
	for _, c := range st.Claims {
		r := byIndex[c.Index]
		if r == nil {
			continue
		}
		switch c.State {
		case "claimed":
			collected += r.Atoms
			if r.State != "collected" {
				a.collect(p, r)
			}
		case "returned":
			returned += r.Atoms
			in := ""
			if c.ReturnedIn != nil {
				in = *c.ReturnedIn
				txids[in] = true
			}
			if r.State != "returned" {
				a.db.Exec(`UPDATE payout_rows SET state = 'returned', returned_in = ? WHERE id = ?`, in, r.ID)
			}
		default:
			open++
		}
	}
	var list []string
	for t := range txids {
		list = append(list, t)
	}
	sort.Strings(list)
	state := "paid"
	if open == 0 && len(st.Claims) > 0 {
		state = "complete"
	}
	a.db.Exec(`UPDATE payout_runs SET state = ?, collected_atoms = ?, returned_atoms = ?, return_txids = ?, updated_at = ? WHERE id = ?`,
		state, collected, returned, strings.Join(list, ","), now(), runID)
}

// collect records one allocation collected. On mainnet it is paid, so the
// account's balance is debited by it, once; a rehearsal moves no balance.
func (a *App) collect(p *PayoutRun, r *PayoutRow) {
	tx, err := a.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE payout_rows SET state = 'collected' WHERE id = ?`, r.ID); err != nil {
		return
	}
	if p.Network == "mainnet" {
		if _, err := tx.Exec(`INSERT INTO ledger (user_id, amount, kind, ref_id, note, created_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT DO NOTHING`, r.UserID, -r.SEQ, "payout", r.ID,
			fmt.Sprintf("Launch payout run %d, collected by your wallet", p.ID), now()); err != nil {
			log.Printf("payout ledger: %v", err)
			return
		}
	}
	tx.Commit()
}

// startPayoutFollower follows every run with a payment until it completes.
func (a *App) startPayoutFollower() {
	if a.cfg.PayoutURL == "" {
		return
	}
	go func() {
		for {
			time.Sleep(a.cfg.PayoutPoll)
			runs, err := runsWhere(a.db, `state IN ('paying', 'paid')`)
			if err != nil {
				continue
			}
			for _, p := range runs {
				a.followRun(p.ID)
			}
		}
	}()
}

// ---------- admin pages ----------

type payoutsPage struct {
	Runs      []*PayoutRun
	Rehearsal bool
	Asset     string
	PerSEQ    int64
	Available bool
}

func (a *App) handleAdminPayouts(w http.ResponseWriter, r *http.Request) {
	runs, err := runsWhere(a.db, `1`)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "admin_payouts", "Payouts", payoutsPage{Runs: runs, Rehearsal: a.rehearsal(), Asset: a.cfg.PayoutAsset,
		PerSEQ: a.cfg.PayoutAtomsPerSEQ, Available: a.cfg.PayoutURL != ""})
}

func (a *App) handleAdminPayoutNew(w http.ResponseWriter, r *http.Request) {
	p, err := a.openRun(strings.TrimSpace(r.FormValue("note")))
	if err != nil {
		a.redirect(w, r, "/admin/payouts", "", "No run opened: "+err.Error()+".")
		return
	}
	a.redirect(w, r, fmt.Sprintf("/admin/payouts/%d", p.ID), fmt.Sprintf("Payout run %d is ready to pay.", p.ID), "")
}

type payoutPage struct {
	Run       *PayoutRun
	Rows      []*PayoutRow
	LeftOut   []*PayoutRow
	Paid      []*PayoutRow
	Unit      string
	Summary   string
	Rehearsal bool
	Counts    map[string]int
}

func (a *App) runID(w http.ResponseWriter, r *http.Request) (*PayoutRun, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	p, err := getRun(a.db, id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return p, true
}

// unitOf names a run's asset to a person.
func unitOf(p *PayoutRun) string {
	if p.Network == "mainnet" {
		return "SEQ"
	}
	return "tSEQ"
}

// atomsText writes atoms as a decimal amount of an 8-decimal asset.
func atomsText(v int64) string {
	s := fmt.Sprintf("%d.%08d", v/100_000_000, v%100_000_000)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func runSummary(p *PayoutRun) string {
	u := unitOf(p)
	amt := func(v int64) string { return atomsText(v) + " " + u }
	switch p.State {
	case "planned":
		return fmt.Sprintf("Pay %d accounts a total of %s. The fee is at most %s.", p.Accounts, amt(p.TotalAtoms), amt(p.FeeBound))
	case "paying":
		return fmt.Sprintf("The payment of %s to %d accounts is on its way.", amt(p.TotalAtoms), p.Accounts)
	case "paid":
		return fmt.Sprintf("Paid %s to %d accounts' wallets; %s collected so far. Wallets collect it when they next open, before %s.",
			amt(p.TotalAtoms), p.Accounts, amt(p.CollectedAtoms), time.Unix(p.Deadline, 0).UTC().Format("2 Jan 2006 15:04 UTC"))
	case "complete":
		s := fmt.Sprintf("Paid %s to %d accounts; %s collected.", amt(p.TotalAtoms), p.Accounts, amt(p.CollectedAtoms))
		if p.ReturnedAtoms > 0 {
			s += fmt.Sprintf(" %s was not collected by the deadline and came back to the paying wallet.", amt(p.ReturnedAtoms))
		}
		return s
	}
	return p.State
}

func (a *App) handleAdminPayout(w http.ResponseWriter, r *http.Request) {
	p, ok := a.runID(w, r)
	if !ok {
		return
	}
	rows, err := rowsOf(a.db, p.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	pg := payoutPage{Run: p, Rows: rows, Unit: unitOf(p), Summary: runSummary(p), Rehearsal: p.Network != "mainnet",
		Counts: map[string]int{}}
	for _, row := range rows {
		pg.Counts[row.State]++
		if row.State == "left_out" {
			pg.LeftOut = append(pg.LeftOut, row)
		} else {
			pg.Paid = append(pg.Paid, row)
		}
	}
	a.render(w, r, "admin_payout", fmt.Sprintf("Payout run %d", p.ID), pg)
}

func (a *App) handleAdminPayoutList(w http.ResponseWriter, r *http.Request) {
	p, ok := a.runID(w, r)
	if !ok {
		return
	}
	rows, err := rowsOf(a.db, p.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="emissio-payout-%d.txt"`, p.ID))
	fmt.Fprintf(w, "# Emissio payout run %d (%s): %d accounts, %d atoms of asset %s in all\n", p.ID, p.Network, p.Accounts, p.TotalAtoms, p.Asset)
	for _, row := range rows {
		if row.State != "left_out" {
			fmt.Fprintf(w, "%s %d\n", row.Key, row.Atoms)
		}
	}
}

func (a *App) handleAdminPayoutReference(w http.ResponseWriter, r *http.Request) {
	p, ok := a.runID(w, r)
	if !ok {
		return
	}
	path := fmt.Sprintf("/admin/payouts/%d", p.ID)
	if err := a.recordReference(p.ID, r.FormValue("reference")); err != nil {
		a.redirect(w, r, path, "", "Not recorded: "+err.Error()+".")
		return
	}
	a.redirect(w, r, path, "Payment recorded: it pays exactly this run's list.", "")
}

// ---------- the account page ----------

type accountPayout struct {
	Rehearsal  bool
	Unit       string
	Line       string
	PayoutKey  string
	LaunchKey  string
	LaunchWhy  string
	NeedsKey   bool
	Rehearsing bool
	RehKey     string
}

// accountPayoutOf is what the account page says about the payout: the key the
// launch payout will name, and the account's allocation in the latest run.
func (a *App) accountPayoutOf(u *User) accountPayout {
	k, _ := keysOf(a.db, u.ID)
	ap := accountPayout{PayoutKey: k.PayoutKey, RehKey: k.RehearsalKey, Rehearsing: a.cfg.PayoutURL != "" && a.rehearsal()}
	ap.LaunchKey, ap.LaunchWhy = launchKey(u.MainnetAddress, k.PayoutKey)
	if u.MainnetAddress != "" {
		if _, v, prog, err := segwitProgram(u.MainnetAddress); err == nil && v == 0 && len(prog) == 20 {
			ap.NeedsKey = true
		}
	}
	var row PayoutRow
	var runID, deadline int64
	var network string
	err := a.db.QueryRow(`SELECT r.run_id, r.atoms, r.state, p.deadline, p.network FROM payout_rows r JOIN payout_runs p ON p.id = r.run_id
		WHERE r.user_id = ? AND r.state != 'left_out' AND r.state != 'planned' ORDER BY r.run_id DESC LIMIT 1`, u.ID).
		Scan(&runID, &row.Atoms, &row.State, &deadline, &network)
	if err != nil {
		return ap
	}
	ap.Rehearsal = network != "mainnet"
	ap.Unit = "SEQ"
	what := "Your launch payout"
	if ap.Rehearsal {
		ap.Unit, what = "tSEQ", "Your testnet rehearsal payout"
	}
	amt := atomsText(row.Atoms) + " " + ap.Unit
	when := time.Unix(deadline, 0).UTC().Format("2 Jan 2006")
	switch row.State {
	case "pending":
		ap.Line = fmt.Sprintf("%s of %s is on its way to your wallet.", what, amt)
	case "paid":
		ap.Line = fmt.Sprintf("%s of %s was paid to your wallet. Your wallet collects it when it next opens, before %s.", what, amt, when)
	case "collected":
		ap.Line = fmt.Sprintf("%s of %s is in your wallet.", what, amt)
	case "returned":
		if ap.Rehearsal {
			ap.Line = fmt.Sprintf("%s of %s was not collected by %s and went back to the paying wallet.", what, amt, when)
		} else {
			ap.Line = fmt.Sprintf("%s of %s was not collected by %s and went back; it stays owed to you and is paid in a later run.", what, amt, when)
		}
	}
	return ap
}
