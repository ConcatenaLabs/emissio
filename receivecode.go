package main

// A wallet's receive code, read wherever Emissio asks for an address or a
// code:
//
//	BITCOIN:<ADDRESS>?ark=<ARK ADDRESS>&sequentia=<RECEIVE REQUEST>
//
// The format is the web wallet's (sequentia-web-wallet `pay-code.js`, which
// writes it and documents every part): a BIP321 payment request whose path is
// the wallet's own address, the same tb1 address on both chains; `ark` is its
// address on the Bitcoin ark and `sequentia` its single-use Sequentia receive
// request, written short (`SQR1…`). Codes are in upper case for the QR code;
// every bech32 part comes back here in lower case. The same reader is in the
// faucet and Compages: change them together.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

type receiveCode struct {
	Address, Ark, Sequentia, Lightning string
}

// lowerBech returns a bech32 string (all one case) in lower case, and
// anything else as it is.
func lowerBech(v string) string {
	if v == strings.ToUpper(v) || v == strings.ToLower(v) {
		return strings.ToLower(v)
	}
	return v
}

// readReceiveCode splits a receive code into its parts; ok is false when
// text is not a code (a plain address, for instance).
func readReceiveCode(text string) (c receiveCode, ok bool) {
	s := strings.TrimSpace(text)
	if len(s) < 8 || !strings.EqualFold(s[:8], "bitcoin:") {
		return c, false
	}
	rest := s[8:]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	path, query, _ := strings.Cut(rest, "?")
	if p, err := url.PathUnescape(path); err == nil {
		c.Address = lowerBech(p)
	}
	for _, part := range strings.Split(query, "&") {
		k, v, found := strings.Cut(part, "=")
		if !found || k == "" {
			continue
		}
		v, err := url.QueryUnescape(v)
		if err != nil {
			continue
		}
		switch strings.ToLower(k) {
		case "ark":
			if c.Ark == "" {
				c.Ark = lowerBech(v)
			}
		case "sequentia":
			if c.Sequentia == "" {
				c.Sequentia = lowerBech(v)
			}
		case "lightning":
			if c.Lightning == "" {
				c.Lightning = lowerBech(v)
			}
		}
	}
	return c, true
}

// addressOf is the address a receive code names, or text itself (trimmed)
// when it is not a code.
func addressOf(text string) string {
	if c, ok := readReceiveCode(text); ok {
		return c.Address
	}
	return strings.TrimSpace(text)
}

// bech32mBytes decodes a bech32m string of any length (a short receive
// request is longer than an address) into its human part and its bytes.
func bech32mBytes(s string) (string, []byte, error) {
	l := strings.ToLower(s)
	if s != l && s != strings.ToUpper(s) {
		return "", nil, errors.New("mixed case")
	}
	sep := strings.LastIndexByte(l, '1')
	if sep < 1 || sep+7 > len(l) || len(l) > 2000 {
		return "", nil, errors.New("not bech32")
	}
	hrp := l[:sep]
	data := make([]int, 0, len(l)-sep-1)
	for _, c := range l[sep+1:] {
		d := strings.IndexRune(bech32Charset, c)
		if d < 0 {
			return "", nil, errors.New("not bech32")
		}
		data = append(data, d)
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != bech32mConst {
		return "", nil, errors.New("bad checksum")
	}
	b, err := convertBits(data[:len(data)-6], 5, 8, false)
	return hrp, b, err
}

// expandRequest turns a receive request as a code carries it, short
// (`sqr1…`) or in the leaf library's spelling (`arca:<hex of its JSON>`),
// into the library's spelling for a payer on `operator` and `genesis` (64
// hex characters each). The short form keeps only the first four bytes of
// each, which are checked against the payer's own: a request for another
// operator or chain is refused.
func expandRequest(text, operator, genesis string) (string, error) {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "arca:") {
		return s, nil
	}
	hrp, b, err := bech32mBytes(s)
	if err != nil || hrp != "sqr" {
		return "", errors.New("not a receive request")
	}
	if len(b) < 112 || b[0] != 1 || b[1]&^31 != 0 {
		return "", errors.New("not a receive request this site reads")
	}
	if len(operator) != 64 || len(genesis) != 64 {
		return "", errors.New("Emissio's own operator is not known yet")
	}
	h := func(i, n int) string { return hex.EncodeToString(b[i : i+n]) }
	if !strings.EqualFold(operator[:8], h(2, 4)) {
		return "", errors.New("that receive code is for another server than Emissio's")
	}
	if !strings.EqualFold(genesis[:8], h(6, 4)) {
		return "", errors.New("that receive code is for another chain")
	}
	flags := b[1]
	r := map[string]any{
		"kind": "request", "operator": strings.ToLower(operator), "genesis_hash": strings.ToLower(genesis),
		"owner": h(10, 32), "owner_nonce": h(42, 32), "mailbox": h(74, 32),
		"exit_delay_units": int(b[106])<<8 | int(b[107]),
		"until":            int64(b[108])<<24 | int64(b[109])<<16 | int64(b[110])<<8 | int64(b[111]),
	}
	i := 112
	need := func(n int) (int, bool) {
		if i+n > len(b) {
			return 0, false
		}
		s := i
		i += n
		return s, true
	}
	short := errors.New("the receive request is cut short")
	if flags&2 != 0 {
		at, ok := need(20)
		if !ok {
			return "", short
		}
		r["landing"] = h(at, 20)
	}
	if flags&4 != 0 {
		at, ok := need(32)
		if !ok {
			return "", short
		}
		r["asset"] = h(at, 32)
	}
	if flags&8 != 0 {
		at, ok := need(8)
		if !ok {
			return "", short
		}
		var v uint64
		for k := 0; k < 8; k++ {
			v = v<<8 | uint64(b[at+k])
		}
		r["value"] = strconv.FormatUint(v, 10)
	}
	if flags&1 != 0 {
		r["template"] = "vtxo-2"
	}
	if flags&16 != 0 {
		at, ok := need(1)
		if !ok || b[at] == 0 {
			return "", short
		}
		ch := map[string]string{}
		for k := 0; k < int(b[at]); k++ {
			c, ok := need(64)
			if !ok {
				return "", short
			}
			ch[h(c, 32)] = h(c+32, 32)
		}
		r["channels"] = ch
	}
	if i != len(b) {
		return "", errors.New("the receive request has bytes left over")
	}
	j, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return "arca:" + hex.EncodeToString(j), nil
}
