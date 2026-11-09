package main

// How much Emissio pays one invoice, in each asset it pays.
//
// A receive code with an amount carries an invoice in the asset it asks for:
// a Bitcoin invoice for BTC, or a Sequentia invoice naming its asset in the
// tagged field `a` (type 29, the asset id in display order, as the web
// wallet's bolt11.js reads it). Emissio pays a BTC invoice of up to
// maxInvoiceSats, and an invoice in a Sequentia asset of up to about
// maxInvoiceUSD of it at the price feed's price, rounded down to a round
// figure (10 USDX, 5 EURX, 0.002 GOLD), so the limit reads as an amount of
// that asset and not as a count of its smallest units.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxInvoiceSats = 10_000 // the most Emissio pays a BTC invoice
	maxInvoiceUSD  = 10.0   // about the most it pays an invoice in a Sequentia asset
	capsFresh      = 10 * time.Minute
)

// invoiceCap is the most Emissio pays one invoice in one asset.
type invoiceCap struct {
	Asset  string // asset id, or "btc"
	Ticker string
	Atoms  int64
	Text   string // the limit as a person reads it: "10 USDX"
}

type invoiceCaps struct {
	at   time.Time
	list []invoiceCap // BTC first, then by ticker
}

// sequentiaNetworks are the invoice prefixes of Sequentia networks; every
// other prefix is a Bitcoin network's.
var sequentiaNetworks = map[string]bool{"sqt": true, "tsqt": true, "sqrt": true}

// invoiceAsset reads which asset an invoice is paid in: "btc" for a Bitcoin
// invoice, the asset id for a Sequentia one.
func invoiceAsset(inv string) (string, error) {
	m := bolt11Re.FindStringSubmatch(strings.TrimSpace(inv))
	if m == nil {
		return "", errors.New("not an invoice that names an amount")
	}
	if !sequentiaNetworks[strings.ToLower(m[1])] {
		return "btc", nil
	}
	s := strings.ToLower(strings.TrimSpace(inv))
	data := s[strings.LastIndexByte(s, '1')+1:]
	vals := make([]int, 0, len(data))
	for _, ch := range data {
		v := strings.IndexRune(bech32Charset, ch)
		if v < 0 {
			return "", errors.New("not an invoice")
		}
		vals = append(vals, v)
	}
	// timestamp (7 groups), tagged fields (type, 2-group length, data),
	// signature (104), checksum (6)
	end := len(vals) - 104 - 6
	for i := 7; i+3 <= end; {
		typ, n := vals[i], vals[i+1]<<5|vals[i+2]
		i += 3
		if i+n > end {
			break
		}
		if typ == 29 && n == 52 {
			return groupsHex(vals[i:i+n], 32), nil
		}
		i += n
	}
	return "", errors.New("a Sequentia invoice that names no asset")
}

// groupsHex packs five-bit groups into n bytes, as hex.
func groupsHex(groups []int, n int) string {
	var acc, bits int
	out := make([]byte, 0, n)
	for _, g := range groups {
		acc = (acc<<5 | g) & 0xffff
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
			if len(out) == n {
				break
			}
		}
	}
	return fmt.Sprintf("%x", out)
}

// roundDown is the largest of 1, 2 or 5 times a power of ten not above v,
// as its digit and exponent: 8.97 is 5e0, 0.00245 is 2e-3.
func roundDown(v float64) (digit int64, exp int) {
	exp = int(math.Floor(math.Log10(v)))
	m := v / math.Pow(10, float64(exp))
	if m >= 9.999999 { // log10 rounding just under a power of ten
		exp++
		m = 1
	}
	switch {
	case m >= 5:
		return 5, exp
	case m >= 2:
		return 2, exp
	}
	return 1, exp
}

// decimalText writes digit × 10^exp without trailing zeros: 10, 0.002.
func decimalText(digit int64, exp int) string {
	if exp >= 0 {
		return strconv.FormatInt(digit*int64(math.Pow10(exp)), 10)
	}
	return "0." + strings.Repeat("0", -exp-1) + strconv.FormatInt(digit, 10)
}

// assetCap is the limit for one Sequentia asset at a price in USD.
func assetCap(asset, ticker string, precision int, price float64) (invoiceCap, bool) {
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return invoiceCap{}, false
	}
	digit, exp := roundDown(maxInvoiceUSD / price)
	if exp+precision < 0 || exp+precision > 15 {
		return invoiceCap{}, false // below one atom, or absurdly many
	}
	return invoiceCap{Asset: asset, Ticker: ticker, Atoms: digit * int64(math.Pow10(exp+precision)),
		Text: decimalText(digit, exp) + " " + ticker}, true
}

func btcCap() invoiceCap {
	return invoiceCap{Asset: "btc", Ticker: "BTC", Atoms: maxInvoiceSats,
		Text: decimalText(roundDown(float64(maxInvoiceSats)/1e8)) + " BTC"}
}

// invoiceCaps is the limit in every asset Emissio pays: BTC, and each
// Sequentia asset it holds whose ticker the registry names and whose price
// the feed gives. Worked out at most every few minutes; on a failure the
// last list stands, or BTC alone.
func (a *App) invoiceCaps() []invoiceCap {
	a.capsMu.Lock()
	defer a.capsMu.Unlock()
	if a.caps.list != nil && time.Since(a.caps.at) < capsFresh {
		return a.caps.list
	}
	list, err := a.workOutCaps()
	if err != nil {
		log.Printf("invoice limits: %v", err)
		if a.caps.list != nil {
			return a.caps.list
		}
	}
	a.caps = invoiceCaps{at: time.Now(), list: list}
	return list
}

// shownCaps is the last list worked out, without waiting for a new one: what
// a task page shows. BTC alone until the first list is in.
func (a *App) shownCaps() []invoiceCap {
	if !a.capsMu.TryLock() {
		return []invoiceCap{btcCap()}
	}
	defer a.capsMu.Unlock()
	if a.caps.list == nil {
		return []invoiceCap{btcCap()}
	}
	return a.caps.list
}

func (a *App) workOutCaps() ([]invoiceCap, error) {
	list := []invoiceCap{btcCap()}
	if a.cfg.PricesURL == "" || a.cfg.RegistryURL == "" || !a.arcaOn() {
		return list, nil
	}
	var bal struct {
		Arca map[string]map[string]string `json:"arca"`
	}
	if err := a.arca(2*time.Minute, &bal, "balance", "--without-bitcoin"); err != nil {
		return list, err
	}
	var prices map[string]struct {
		Price float64 `json:"price"`
	}
	if err := getJSON(a.cfg.PricesURL, &prices); err != nil {
		return list, err
	}
	var more []invoiceCap
	for asset := range bal.Arca {
		var reg struct {
			Contract struct {
				Ticker    string `json:"ticker"`
				Precision *int   `json:"precision"`
			} `json:"contract"`
		}
		if getJSON(a.cfg.RegistryURL+"/"+asset, &reg) != nil || reg.Contract.Ticker == "" {
			continue
		}
		precision := 0
		if reg.Contract.Precision != nil {
			precision = *reg.Contract.Precision
		}
		t := reg.Contract.Ticker
		p, ok := prices[t]
		if !ok && len(t) > 1 && t[0] == 't' {
			p, ok = prices[t[1:]] // the feed names a testnet asset without its t
		}
		if !ok {
			continue
		}
		if c, ok := assetCap(asset, t, precision, p.Price); ok {
			more = append(more, c)
		}
	}
	sort.Slice(more, func(i, j int) bool { return more[i].Ticker < more[j].Ticker })
	return append(list, more...), nil
}

func getJSON(url string, out any) error {
	c := http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// capsText lists the limits: "0.0001 BTC (10,000 sats), 5 EURX, 10 USDX".
func capsText(list []invoiceCap) string {
	parts := make([]string, len(list))
	for i, c := range list {
		parts[i] = c.Text
		if c.Asset == "btc" {
			parts[i] += " (" + groupThousands(c.Atoms) + " sats)"
		}
	}
	return strings.Join(parts, ", ")
}

func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// invoiceRefusal says why Emissio does not pay an invoice for amt in asset,
// or "" when it pays it.
func (a *App) invoiceRefusal(asset string, amt int64) string {
	list := a.invoiceCaps()
	for _, c := range list {
		if c.Asset != asset {
			continue
		}
		if amt <= c.Atoms {
			return ""
		}
		return fmt.Sprintf("That invoice asks more than Emissio pays. Emissio pays an invoice in %s of up to %s. Make a new receive code for at most that much. In every asset, Emissio pays up to: %s.",
			c.Ticker, c.Text, capsText(list))
	}
	return "Emissio does not pay invoices in that asset. It pays invoices of up to: " + capsText(list) + "."
}
