package main

// The automatic checks behind each task. A check reads the chain through
// the explorer (and, where a task is about a service, that service's own
// public data) and returns a verdict:
//
//   - pass: the evidence proves the task, bound to the account. The
//     submission is approved and credited at once.
//   - fail: the evidence does not prove it, for a reason the tester can fix
//     (no proof decimals, not yet confirmed, the wrong asset). The
//     submission is refused with that reason, unless the tester sends it to
//     a reviewer anyway.
//   - review: nothing automatic can decide (a service is unreachable, the
//     check needs a human). The submission waits in the review queue.
//
// Which check a task runs is the task's check kind, so an admin can add a
// task that reuses a check without a deploy.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type verdict int

const (
	vReview verdict = iota
	vPass
	vFail
)

type checkResult struct {
	Note    string
	Verdict verdict
}

func pass(format string, args ...any) checkResult {
	return checkResult{fmt.Sprintf(format, args...), vPass}
}
func fail(format string, args ...any) checkResult {
	return checkResult{fmt.Sprintf(format, args...), vFail}
}
func review(format string, args ...any) checkResult {
	return checkResult{fmt.Sprintf(format, args...), vReview}
}

// checkKinds names every check, for the admin's task editor.
var checkKinds = []struct{ Key, Desc string }{
	{"none", "No automatic check: every submission goes to a reviewer"},
	{"first-transaction", "A confirmed transaction with the proof decimals"},
	{"anchor-lookup", "Proof decimals, and the Bitcoin anchor hash in the notes matches the block's"},
	{"faucet-consolidate", "Proof decimals, spending coins from two faucet payments"},
	{"issue-asset", "Proof decimals on a new issuance"},
	{"reissue-asset", "Proof decimals on a reissuance of an asset this account issued"},
	{"burn-asset", "A burn of an asset this account issued, of an amount with the proof decimals"},
	{"any-asset-fee", "Proof decimals, and the fee paid in an asset other than tSEQ"},
	{"multi-asset-send", "Proof decimals, and outputs in two or more assets"},
	{"confidential-tx", "Proof decimals, and a blinded output"},
	{"emissio-return", "Returns Emissio's payment: spends it, pays the exact amount back, fee in that asset"},
	{"emissio-unblind", "Names the amount of Emissio's confidential payment, which only its receiver can unblind"},
	{"bridge-in", "Proof decimals, spending a Compages delivery for the deposit named in the notes"},
	{"bridge-both", "As bridge-in, from the source chain this account's first bridge did not use"},
	{"bridge-out", "Proof decimals sent to a Compages redemption address that has released"},
	{"install-release", "The release's checksum in the notes, and the node seen running that release"},
	{"build-from-source", "A peer announcing the account code as its client name"},
	{"run-node", "A week of sightings of a peer announcing the account code"},
	{"stake", "Proof decimals on a staking output of at least 40,000 tSEQ"},
	{"delegate-stake", "A delegation record of this account's stake key to a pool on the board"},
	{"redelegate", "Spends this account's delegation record into one for another pool"},
	{"unbond-stake", "Spends this account's staking output into an unbonding output of the same key"},
}

func knownCheck(k string) bool {
	for _, c := range checkKinds {
		if c.Key == k {
			return true
		}
	}
	return false
}

// evidenceCheck runs the task's check on a submission.
func (a *App) evidenceCheck(kind, txid, notes string, user *User) checkResult {
	switch kind {
	case "none", "":
		return review("reviewed by hand")
	case "build-from-source":
		return a.buildCheck(user.ClaimCode)
	case "run-node":
		return a.uptimeCheck(user.ClaimCode, time.Now())
	case "install-release":
		return a.releaseCheck(notes, user.ClaimCode)
	case "emissio-unblind":
		return a.unblindCheck(user, notes)
	}
	if txid == "" {
		return fail("no txid submitted")
	}
	tx, note := a.fetchTx(txid)
	if tx == nil {
		if strings.Contains(note, "NOT FOUND") {
			return fail("%s", note)
		}
		return review("%s", note)
	}
	if !tx.Status.Confirmed {
		return fail("the transaction is in the mempool and not yet confirmed: submit it once it is in a block")
	}
	where := fmt.Sprintf("confirmed at height %d", tx.Status.BlockHeight)
	var r checkResult
	switch kind {
	case "emissio-return":
		r = a.returnCheck(user, tx)
	case "delegate-stake":
		r = a.delegationCheck(user, tx)
	case "redelegate":
		r = a.redelegationCheck(user, tx)
	case "unbond-stake":
		r = a.unbondCheck(user, tx)
	case "burn-asset":
		r = a.burnCheck(user, tx)
	default:
		r = a.taggedCheck(kind, tx, notes, user)
	}
	r.Note += "; " + where
	return r
}

// taggedCheck covers the tasks whose evidence is a transaction carrying the
// account's proof decimals plus one property of its own.
func (a *App) taggedCheck(kind string, tx *chainTx, notes string, user *User) checkResult {
	tag := proofTag(user.ClaimCode)
	idx, po := proofOutput(tx, tag)
	if po == nil {
		return fail("proof amount .%s NOT found in any transparent output", tag)
	}
	found := fmt.Sprintf("proof amount found (output %d, asset %s)", idx, short(po.Asset))
	policy := a.cfg.PolicyAsset
	switch kind {
	case "first-transaction":
		return pass("%s", found)
	case "anchor-lookup":
		ok, note := a.anchorCheck(tx, notes)
		if ok {
			return pass("%s; %s", found, note)
		}
		return fail("%s; %s", found, note)
	case "any-asset-fee":
		fee := feeAsset(tx)
		switch {
		case fee == "":
			return fail("%s; no fee output found", found)
		case fee == policy:
			return fail("%s; fee paid in tSEQ: it must be paid in another asset", found)
		}
		return pass("%s; fee paid in asset %s (OK)", found, short(fee))
	case "multi-asset-send":
		assets := map[string]bool{}
		for _, o := range tx.Vout {
			if o.Type != "fee" && o.Type != "op_return" && o.Asset != "" {
				assets[o.Asset] = true
			}
		}
		if len(assets) >= 2 {
			return pass("%s; %d assets in the outputs (OK)", found, len(assets))
		}
		return fail("%s; only one asset in the outputs: it must send two", found)
	case "confidential-tx":
		blinded := 0
		for _, o := range tx.Vout {
			if o.ValueCommitment != "" {
				blinded++
			}
		}
		if blinded > 0 {
			return pass("%s; %d blinded output(s) (OK)", found, blinded)
		}
		return fail("%s; no blinded output: one output must go to a confidential address", found)
	case "issue-asset", "reissue-asset":
		iss := issuanceOf(tx)
		switch {
		case iss == nil:
			return fail("%s; no issuance in this transaction", found)
		case kind == "issue-asset" && iss.IsReissuance:
			return fail("%s; this is a reissuance, not a new issuance", found)
		case kind == "issue-asset":
			return pass("%s; issues asset %s (OK)", found, short(iss.AssetID))
		case !iss.IsReissuance:
			return fail("%s; this is a new issuance, not a reissuance", found)
		}
		owns, note := a.ownsIssuance(user.ID, iss.AssetID)
		switch owns {
		case vPass:
			return pass("%s; reissues asset %s; %s", found, short(iss.AssetID), note)
		case vFail:
			return fail("%s; reissues asset %s; %s", found, short(iss.AssetID), note)
		}
		return review("%s; reissues asset %s; %s", found, short(iss.AssetID), note)
	case "stake":
		if po.Asset != policy {
			return fail("%s; the proof output is not tSEQ: a stake must be tSEQ", found)
		}
		if *po.Value < minStake {
			return fail("%s; the proof output is under 40,000 tSEQ", found)
		}
		key, ok := parseStakeScript(po.Script)
		if !ok {
			return fail("%s; the proof output is not a staking script", found)
		}
		return pass("%s; staking output of %s tSEQ for key %s (OK)", found, formatSEQ(*po.Value/proofAtoms), short(key))
	case "faucet-consolidate":
		return a.faucetCheck(tx, found)
	case "bridge-in", "bridge-both":
		return a.bridgeInCheck(kind, tx, notes, user, found)
	case "bridge-out":
		return a.bridgeOutCheck(tx, po, found)
	}
	return review("%s; reviewed by hand", found)
}

func feeAsset(tx *chainTx) string {
	for _, o := range tx.Vout {
		if o.Type == "fee" {
			return o.Asset
		}
	}
	return ""
}

type issuance struct {
	AssetID      string `json:"asset_id"`
	IsReissuance bool   `json:"is_reissuance"`
	AssetAmount  int64  `json:"assetamount"`
}

func issuanceOf(tx *chainTx) *issuance {
	for i := range tx.Vin {
		if tx.Vin[i].Issuance != nil {
			return tx.Vin[i].Issuance
		}
	}
	return nil
}

// priorTxids are the txids this account submitted for a task, newest
// first, approved ones before pending ones.
func (a *App) priorTxids(userID int64, slug string) []string {
	rows, err := a.db.Query(`SELECT s.txid FROM submissions s JOIN tasks t ON t.id = s.task_id
		WHERE s.user_id = ? AND t.slug = ? AND s.status != 'rejected' AND s.txid != ''
		ORDER BY s.status = 'approved' DESC, s.id DESC`, userID, slug)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		out = append(out, t)
	}
	return out
}

// ownsIssuance says whether the asset's issuance transaction is one this
// user submitted for the issue-asset task.
func (a *App) ownsIssuance(userID int64, assetID string) (verdict, string) {
	var info struct {
		IssuanceTxin struct {
			Txid string `json:"txid"`
		} `json:"issuance_txin"`
	}
	if err := a.fetchJSON("/asset/"+assetID, &info); err != nil || info.IssuanceTxin.Txid == "" {
		return vReview, "issuer unknown; manual review"
	}
	for _, t := range a.priorTxids(userID, "issue-asset") {
		if t == strings.ToLower(info.IssuanceTxin.Txid) {
			return vPass, "issued by this account (OK)"
		}
	}
	return vFail, "issued by a transaction this account did not submit for the issuance task"
}

// issuedAssets are the assets this account's issue-asset submissions issued.
func (a *App) issuedAssets(userID int64) map[string]bool {
	out := map[string]bool{}
	for _, t := range a.priorTxids(userID, "issue-asset") {
		if tx, _ := a.fetchTx(t); tx != nil {
			if iss := issuanceOf(tx); iss != nil && !iss.IsReissuance {
				out[iss.AssetID] = true
			}
		}
	}
	return out
}

// burnCheck: an unspendable output of an asset this account issued, of an
// amount ending in the proof decimals.
func (a *App) burnCheck(user *User, tx *chainTx) checkResult {
	tag := proofTag(user.ClaimCode)
	want, _ := strconv.ParseInt(tag, 10, 64)
	mine := a.issuedAssets(user.ID)
	var burned *chainVout
	for i := range tx.Vout {
		o := &tx.Vout[i]
		if o.Type == "op_return" && o.Value != nil && *o.Value > 0 && *o.Value%proofAtoms == want {
			burned = o
		}
	}
	switch {
	case burned == nil:
		return fail("no burned output of an amount ending in .%s", tag)
	case len(mine) == 0:
		return fail("burns %s of asset %s, but this account has no issuance on file: complete the issuance task first", atomsText(*burned.Value), short(burned.Asset))
	case !mine[burned.Asset]:
		return fail("burns asset %s, which this account did not issue", short(burned.Asset))
	}
	return pass("burns %s of asset %s, issued by this account (OK)", atomsText(*burned.Value), short(burned.Asset))
}

// ---- staking scripts, from their bytes ----

// pushAt reads one data push at b[i:], returning its bytes and the next
// offset. Small-integer opcodes count as pushes of their number.
func pushAt(b []byte, i int) ([]byte, int, bool) {
	if i >= len(b) {
		return nil, i, false
	}
	op := b[i]
	switch {
	case op == 0x00:
		return []byte{}, i + 1, true
	case op >= 0x51 && op <= 0x60, op == 0x4f:
		return []byte{op}, i + 1, true
	case op >= 0x01 && op <= 0x4b:
		n := int(op)
		if i+1+n > len(b) {
			return nil, i, false
		}
		return b[i+1 : i+1+n], i + 1 + n, true
	case op == 0x4c:
		if i+1 >= len(b) {
			return nil, i, false
		}
		n := int(b[i+1])
		if i+2+n > len(b) {
			return nil, i, false
		}
		return b[i+2 : i+2+n], i + 2 + n, true
	}
	return nil, i, false
}

const (
	opDrop     = 0x75
	opCSV      = 0xb2
	opCLTV     = 0xb1
	opCheckSig = 0xac
)

// parseStakeScript reads a staking output the way the node does
// (ParseStakeScriptFull): <csv> OP_CSV OP_DROP, optionally <bls48> OP_DROP
// <pop96> OP_DROP, optionally <locktime> OP_CLTV OP_DROP, then <pubkey>
// OP_CHECKSIG. It returns the key.
func parseStakeScript(script string) (string, bool) {
	b, err := hex.DecodeString(script)
	if err != nil {
		return "", false
	}
	csv, i, ok := pushAt(b, 0)
	if !ok || len(csv) == 0 || len(csv) > 4 || i+2 > len(b) || b[i] != opCSV || b[i+1] != opDrop {
		return "", false
	}
	i += 2
	if p, j, ok := pushAt(b, i); ok && len(p) == 48 {
		pop, k, ok := pushAt(b, j+1)
		if j >= len(b) || b[j] != opDrop || !ok || len(pop) != 96 || k >= len(b) || b[k] != opDrop {
			return "", false
		}
		i = k + 1
	}
	if lt, j, ok := pushAt(b, i); ok && len(lt) <= 5 && j < len(b) && b[j] == opCLTV {
		if j+1 >= len(b) || b[j+1] != opDrop {
			return "", false
		}
		i = j + 2
	}
	key, j, ok := pushAt(b, i)
	if !ok || (len(key) != 33 && len(key) != 65) || j != len(b)-1 || b[j] != opCheckSig {
		return "", false
	}
	return hex.EncodeToString(key), true
}

var (
	delegTag  = []byte("SEQDEL")
	unbondTag = []byte("SEQUNBOND")
)

// parseDelegation reads <"SEQDEL"> OP_DROP <signer> OP_DROP <controller> OP_CHECKSIG.
func parseDelegation(script string) (signer, controller string, ok bool) {
	b, err := hex.DecodeString(script)
	if err != nil {
		return "", "", false
	}
	tag, i, ok := pushAt(b, 0)
	if !ok || string(tag) != string(delegTag) || i >= len(b) || b[i] != opDrop {
		return "", "", false
	}
	s, j, ok := pushAt(b, i+1)
	if !ok || len(s) != 33 || j >= len(b) || b[j] != opDrop {
		return "", "", false
	}
	c, k, ok := pushAt(b, j+1)
	if !ok || len(c) != 33 || k != len(b)-1 || b[k] != opCheckSig {
		return "", "", false
	}
	return hex.EncodeToString(s), hex.EncodeToString(c), true
}

// parseUnbond reads <"SEQUNBOND"> OP_DROP <pubkey> OP_CHECKSIG.
func parseUnbond(script string) (string, bool) {
	b, err := hex.DecodeString(script)
	if err != nil {
		return "", false
	}
	tag, i, ok := pushAt(b, 0)
	if !ok || string(tag) != string(unbondTag) || i >= len(b) || b[i] != opDrop {
		return "", false
	}
	k, j, ok := pushAt(b, i+1)
	if !ok || len(k) != 33 || j != len(b)-1 || b[j] != opCheckSig {
		return "", false
	}
	return hex.EncodeToString(k), true
}

type stakeOut struct {
	Txid string
	Vout int
	Key  string
}

// stakesOf finds this account's staking outputs, read again from the
// chain: those its stake and delegation submissions created with its proof
// decimals.
func (a *App) stakesOf(user *User) []stakeOut {
	want, _ := strconv.ParseInt(proofTag(user.ClaimCode), 10, 64)
	var out []stakeOut
	for _, slug := range []string{"stake", "delegate-stake"} {
		for _, t := range a.priorTxids(user.ID, slug) {
			tx, _ := a.fetchTx(t)
			if tx == nil {
				continue
			}
			for i, o := range tx.Vout {
				if k, ok := parseStakeScript(o.Script); ok && o.Value != nil && *o.Value%proofAtoms == want {
					out = append(out, stakeOut{t, i, k})
				}
			}
		}
	}
	return out
}

// stakeOf is the account's first staking output.
func (a *App) stakeOf(user *User) (txid string, vout int, key string, ok bool) {
	if ss := a.stakesOf(user); len(ss) > 0 {
		return ss[0].Txid, ss[0].Vout, ss[0].Key, true
	}
	return "", 0, "", false
}

func (a *App) stakeKeys(user *User) map[string]bool {
	keys := map[string]bool{}
	for _, s := range a.stakesOf(user) {
		keys[s.Key] = true
	}
	return keys
}

// poolSigners reads the staking pool board's feed.
func (a *App) poolSigners() (map[string]bool, error) {
	if a.cfg.PoolsURL == "" {
		return nil, fmt.Errorf("no pool board configured")
	}
	resp, err := proofClient.Get(a.cfg.PoolsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var feed struct {
		Pools []struct {
			Signer string `json:"signer"`
		} `json:"pools"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&feed); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range feed.Pools {
		out[strings.ToLower(p.Signer)] = true
	}
	return out, nil
}

func (a *App) delegationCheck(user *User, tx *chainTx) checkResult {
	// The controller is this account's stake key, or the key of a new
	// stake bonded in the same transaction with the proof decimals.
	keys := a.stakeKeys(user)
	want, _ := strconv.ParseInt(proofTag(user.ClaimCode), 10, 64)
	for _, o := range tx.Vout {
		if k, ok := parseStakeScript(o.Script); ok && o.Value != nil && *o.Value%proofAtoms == want {
			keys[k] = true
		}
	}
	if len(keys) == 0 {
		return fail("this account has no staking output on file, and this transaction bonds none with the proof decimals")
	}
	var signer, key string
	for _, o := range tx.Vout {
		if s, c, ok := parseDelegation(o.Script); ok && keys[c] {
			signer, key = s, c
		}
	}
	if signer == "" {
		return fail("no delegation record whose controller is this account's stake key")
	}
	pools, err := a.poolSigners()
	if err != nil {
		return review("delegates stake key %s to %s; pool board unavailable, manual review", short(key), short(signer))
	}
	if !pools[signer] {
		return fail("delegates to %s, which is not a pool on the board", short(signer))
	}
	return pass("delegates this account's stake key %s to pool %s (OK)", short(key), short(signer))
}

func (a *App) redelegationCheck(user *User, tx *chainTx) checkResult {
	keys := a.stakeKeys(user)
	if len(keys) == 0 {
		return fail("this account has no staking output on file: complete the staking task first")
	}
	prior := a.priorTxids(user.ID, "delegate-stake")
	if len(prior) == 0 {
		return fail("this account has no delegation on file: complete the delegation task first")
	}
	var old string
	for _, in := range tx.Vin {
		if in.Prevout == nil {
			continue
		}
		if s, c, ok := parseDelegation(in.Prevout.Script); ok && keys[c] {
			old = s
		}
	}
	if old == "" {
		return fail("this transaction spends no delegation record of this account's stake key")
	}
	var signer string
	for _, o := range tx.Vout {
		if s, c, ok := parseDelegation(o.Script); ok && keys[c] {
			signer = s
		}
	}
	switch {
	case signer == "":
		return fail("spends the delegation to %s but creates no new record", short(old))
	case signer == old:
		return fail("the new record names the same pool, %s", short(old))
	}
	pools, err := a.poolSigners()
	if err != nil {
		return review("moves the delegation from %s to %s; pool board unavailable, manual review", short(old), short(signer))
	}
	if !pools[signer] {
		return fail("moves the delegation to %s, which is not a pool on the board", short(signer))
	}
	return pass("moves this account's delegation from %s to pool %s (OK)", short(old), short(signer))
}

func (a *App) unbondCheck(user *User, tx *chainTx) checkResult {
	stakes := a.stakesOf(user)
	if len(stakes) == 0 {
		return fail("this account has no staking output on file: complete the staking task first")
	}
	key := ""
	for _, in := range tx.Vin {
		for _, s := range stakes {
			if strings.EqualFold(in.Txid, s.Txid) && in.Vout == s.Vout {
				key = s.Key
			}
		}
	}
	if key == "" {
		return fail("this transaction spends none of this account's staking outputs")
	}
	for _, o := range tx.Vout {
		if k, ok := parseUnbond(o.Script); ok && k == key {
			return pass("spends this account's stake into an unbonding output of key %s (OK)", short(key))
		}
	}
	return fail("spends the stake, but creates no unbonding output of its key %s", short(key))
}

// ---- the faucet ----

// fromFaucet says whether a transaction was paid by the faucet: one of its
// inputs is one of the faucet's own addresses.
func (a *App) fromFaucet(txid string) bool {
	tx, _ := a.fetchTx(txid)
	if tx == nil {
		return false
	}
	for _, in := range tx.Vin {
		if in.Prevout != nil && a.faucetAddr[in.Prevout.Address] {
			return true
		}
	}
	return false
}

func (a *App) faucetCheck(tx *chainTx, found string) checkResult {
	if len(a.faucetAddr) == 0 {
		return review("%s; no faucet address configured, manual review", found)
	}
	payments := map[string]bool{}
	for i, in := range tx.Vin {
		if i >= 12 {
			break
		}
		if !payments[in.Txid] && a.fromFaucet(in.Txid) {
			payments[in.Txid] = true
		}
	}
	if len(payments) >= 2 {
		return pass("%s; spends coins from %d faucet payments (OK)", found, len(payments))
	}
	return fail("%s; spends coins from %d faucet payment(s): it must combine coins from two", found, len(payments))
}

// ---- Compages ----

var (
	ethHashRe   = regexp.MustCompile(`0x[0-9a-fA-F]{64}`)
	solAddrRe   = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{32,44}\b`)
	releasedRed = map[string]bool{"released": true, "done": true, "destroy_pending": true, "destroy_manual": true}
)

func (a *App) compagesGet(path string, out any) (int, error) {
	resp, err := proofClient.Get(strings.TrimRight(a.cfg.CompagesURL, "/") + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return 200, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// deliveries reads the Sequentia delivery transactions of the deposit the
// notes name: an Ethereum deposit hash, or a Solana deposit address.
func (a *App) deliveries(notes string) (chain string, txids map[string]bool, err error) {
	txids = map[string]bool{}
	type dep struct {
		Status  string `json:"status"`
		SeqTxid string `json:"seqTxid"`
	}
	if h := ethHashRe.FindString(notes); h != "" {
		var ds []dep
		if _, err := a.compagesGet("/deposit/tx/"+strings.ToLower(h), &ds); err != nil {
			return "ethereum", nil, err
		}
		for _, d := range ds {
			if d.SeqTxid != "" {
				txids[strings.ToLower(d.SeqTxid)] = true
			}
		}
		return "ethereum", txids, nil
	}
	for _, s := range solAddrRe.FindAllString(notes, 4) {
		var w struct {
			Deposits []dep `json:"deposits"`
		}
		if code, err := a.compagesGet("/sol/wrap/"+s, &w); err != nil {
			if code == 404 {
				continue
			}
			return "solana", nil, err
		}
		for _, d := range w.Deposits {
			if d.SeqTxid != "" {
				txids[strings.ToLower(d.SeqTxid)] = true
			}
		}
		return "solana", txids, nil
	}
	return "", txids, nil
}

func (a *App) bridgeInCheck(kind string, tx *chainTx, notes string, user *User, found string) checkResult {
	if a.cfg.CompagesURL == "" {
		return review("%s; no bridge configured, manual review", found)
	}
	chain, txids, err := a.deliveries(notes)
	if chain == "" {
		return fail("%s; the notes name no Ethereum deposit hash (0x...) or Solana deposit address", found)
	}
	if err != nil {
		return review("%s; bridge unavailable (%v), manual review", found, err)
	}
	if len(txids) == 0 {
		return fail("%s; the bridge has delivered nothing for that %s deposit yet", found, chain)
	}
	spent := false
	for _, in := range tx.Vin {
		if txids[strings.ToLower(in.Txid)] {
			spent = true
		}
	}
	if !spent {
		return fail("%s; this transaction does not spend the bridge's delivery for that %s deposit", found, chain)
	}
	if kind == "bridge-both" {
		for _, t := range a.priorTxids(user.ID, "bridge-in") {
			var notes string
			a.db.QueryRow("SELECT notes FROM submissions WHERE txid = ? AND user_id = ?", t, user.ID).Scan(&notes)
			if first, _, _ := a.deliveries(notes); first == chain {
				return fail("%s; your first bridge already came from %s: this one must come from the other chain", found, chain)
			}
		}
		if len(a.priorTxids(user.ID, "bridge-in")) == 0 {
			return fail("%s; complete the first bridge task before this one", found)
		}
	}
	return pass("%s; spends the bridge's delivery for a %s deposit (OK)", found, chain)
}

func (a *App) bridgeOutCheck(tx *chainTx, po *chainVout, found string) checkResult {
	if a.cfg.CompagesURL == "" {
		return review("%s; no bridge configured, manual review", found)
	}
	if po.Address == "" {
		return fail("%s; the proof output has no address", found)
	}
	type red struct {
		Txid   string `json:"txid"`
		Status string `json:"status"`
	}
	var st struct {
		Redemptions []red `json:"redemptions"`
	}
	code, err := a.compagesGet("/redeem/"+po.Address, &st)
	if code == 404 {
		code, err = a.compagesGet("/sol/redeem/"+po.Address, &st)
	}
	if code == 404 {
		return fail("%s; %s is not a Compages redemption address", found, short(po.Address))
	}
	if err != nil {
		return review("%s; bridge unavailable (%v), manual review", found, err)
	}
	for _, r := range st.Redemptions {
		if strings.EqualFold(r.Txid, tx.Txid) {
			if releasedRed[r.Status] {
				return pass("%s; redemption released on the source chain (OK)", found)
			}
			return fail("%s; the redemption is %s: submit once the bridge has released it", found, strings.ReplaceAll(r.Status, "_", " "))
		}
	}
	return fail("%s; the bridge has not seen this transaction at that redemption address yet", found)
}

// ---- the release from the download page ----

var (
	sumLineRe = regexp.MustCompile(`(?m)^([0-9a-f]{64})\s+\*?(\S+)$`)
	sumVerRe  = regexp.MustCompile(`sequentia-core-([0-9]+\.[0-9]+\.[0-9]+)`)
	subverRe  = regexp.MustCompile(`^/Sequentia Core:([0-9]+\.[0-9]+\.[0-9]+)`)
	hexHash64 = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)
)

// releaseCheck: the notes carry the checksum of a file the download page
// lists, and a node announcing the account code runs that release.
func (a *App) releaseCheck(notes, code string) checkResult {
	if a.cfg.SumsURL == "" {
		return review("no checksum list configured; manual review")
	}
	resp, err := proofClient.Get(a.cfg.SumsURL)
	if err != nil {
		return review("checksum list unreachable; manual review")
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	files := map[string]string{}
	for _, m := range sumLineRe.FindAllStringSubmatch(string(body), -1) {
		files[m[1]] = m[2]
	}
	var file string
	for _, h := range hexHash64.FindAllString(notes, -1) {
		if f, ok := files[strings.ToLower(h)]; ok {
			file = f
		}
	}
	if file == "" {
		return fail("no checksum in the notes matches a file on the download page")
	}
	ver := ""
	if m := sumVerRe.FindStringSubmatch(file); m != nil {
		ver = m[1]
	}
	ps, err := a.peers()
	if err != nil {
		return review("checksum of %s matches (OK); public node unreachable, manual review", file)
	}
	sv, seen := peerCodes(ps)[strings.ToLower(code)]
	if !seen {
		return fail("checksum of %s matches; no connected node announces emissio-%s: run it with -uacomment=emissio-%s and submit while it is connected", file, code, code)
	}
	m := subverRe.FindStringSubmatch(sv)
	if m == nil || (ver != "" && m[1] != ver) {
		return fail("checksum of %s matches, but the node announcing your code runs %s, not release %s", file, sv, ver)
	}
	return pass("checksum of %s matches, and the node announcing your code runs it: %s (OK)", file, sv)
}
