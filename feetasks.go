package main

// The fee-list tasks: the tester lists USDX on their own node at a value
// this account is given (by hand, or through the price server), then pays a
// fee in USDX at a fee rate the task names. A node turns a fee rate into an
// amount of the fee asset at its own rate:
//
//	fee atoms = ceil(fee_rate × vsize × 1e8 / rate)
//
// (measured on a regtest node, exact for every rate and fee rate tried), so
// the fee of the mined transaction shows the rate the sending node held.
// The check sees that fee and nothing of the node: a rate set by hand, a
// fee output written by hand and the price server's push all look alike.

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// feeTaskRate is the fee rate both tasks name: reference atoms per vbyte.
	feeTaskRate int64 = 2
	// rateScale is the node's rate for one whole unit worth one reference unit.
	rateScale int64 = 100_000_000
)

// feeTaskCents is the account's value of one USDX, in hundredths of a
// reference unit, for a fee-list task. The two tasks' ranges do not
// overlap, so one transaction can pass only one of them: the whitelist task
// gives 0.30 to 0.45, the price-server task 0.55 to 0.70. Both are below
// the network's 1, so a producer always values the fee at least as highly
// as the tester's node does.
func feeTaskCents(kind, tag string) int64 {
	n, _ := strconv.ParseInt(tag, 10, 64)
	if kind == "price-server-fee" {
		return 55 + (n/16)%16
	}
	return 30 + n%16
}

// feeTaskRateFor is the node rate that value is: cents × 1e6 (USDX has 8
// decimals, so no denomination factor).
func feeTaskRateFor(kind, tag string) int64 { return feeTaskCents(kind, tag) * 1_000_000 }

func priceText(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

// feeAt is what a node holding rate pays at feeTaskRate for vsize vbytes.
func feeAt(rate, vsize int64) int64 {
	return (feeTaskRate*vsize*rateScale + rate - 1) / rate
}

// feeFollows reports whether a fee of f atoms comes from rate for this
// transaction. The size a wallet prices is the signed size it estimates,
// which can differ from the final one by a byte or so; the explorer gives
// both the weight and the discounted vsize, and either may be the one the
// node priced.
func feeFollows(f, rate int64, sizes []int64) bool {
	for _, v := range sizes {
		for d := int64(-1); d <= 2; d++ {
			if v+d > 0 && feeAt(rate, v+d) == f {
				return true
			}
		}
	}
	return false
}

func txSizes(tx *chainTx) []int64 {
	var out []int64
	if tx.Weight > 0 {
		out = append(out, (tx.Weight+3)/4)
	}
	if tx.DiscountVsize > 0 && (len(out) == 0 || tx.DiscountVsize != out[0]) {
		out = append(out, tx.DiscountVsize)
	}
	return out
}

func feeAmount(tx *chainTx) (asset string, atoms int64, ok bool) {
	for _, o := range tx.Vout {
		if o.Type == "fee" && o.Value != nil {
			return o.Asset, *o.Value, true
		}
	}
	return "", 0, false
}

// feeListCheck: the fee is in USDX and follows from this account's rate.
func (a *App) feeListCheck(kind string, tx *chainTx, tag, found string) checkResult {
	usdx := strings.ToLower(a.cfg.USDXAsset)
	asset, f, ok := feeAmount(tx)
	if !ok {
		return fail("%s; no fee output with a visible amount found", found)
	}
	if strings.ToLower(asset) != usdx {
		return fail("%s; the fee is paid in asset %s: this task's fee is paid in USDX (fee_asset_label=USDX)", found, short(asset))
	}
	sizes := txSizes(tx)
	if len(sizes) == 0 {
		return review("%s; the explorer gives no size for this transaction; manual review", found)
	}
	rate := feeTaskRateFor(kind, tag)
	cents := feeTaskCents(kind, tag)
	if feeFollows(f, rate, sizes) {
		return pass("%s; a fee of %d USDX atoms for %d vbytes: your node valued USDX at %s, rate %d (OK)", found, f, sizes[0], priceText(cents), rate)
	}
	other := "whitelist-fee"
	otherTask := "List an asset for fees on your node"
	if kind == "whitelist-fee" {
		other, otherTask = "price-server-fee", "Run the price server"
	}
	switch {
	case feeFollows(f, rateScale, sizes):
		return fail("%s; your node valued USDX at 1, the network's rate (100000000): list it at %d first, as the task says, and send with fee_rate=%d", found, rate, feeTaskRate)
	case feeFollows(f, feeTaskRateFor(other, tag), sizes):
		return fail("%s; this fee follows from the price of the task \"%s\" (%s): for this task your node must value USDX at %s, rate %d", found, otherTask, priceText(feeTaskCents(other, tag)), priceText(cents), rate)
	}
	implied := feeTaskRate * sizes[0] * rateScale / f
	return fail("%s; a fee of %d USDX atoms for %d vbytes is, at fee_rate=%d, a rate of about %d: this task asks for rate %d (USDX at %s) and fee_rate=%d. A different fee rate gives a different fee: check both", found, f, sizes[0], feeTaskRate, implied, rate, priceText(cents), feeTaskRate)
}
