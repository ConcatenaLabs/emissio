package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // HASH160 of a P2WPKH address's key
)

// What a person pastes to name the testnet wallet the rehearsal pays, read
// into the one key that wallet looks for payments under:
//
//   - the wallet's account key, as the web wallet shows it under Receive >
//     Show xpub (a tpub, with or without its [fingerprint/84h/1h/0h] origin,
//     or the receive-line descriptor wpkh(…/0/*)): the key of the wallet's
//     first receive address, m/84'/1'/0'/0/0, which every wallet on the kit
//     looks for payments under. Only that key is kept, never the account key;
//   - an address of the wallet that has already sent a transaction: an
//     address starting tb1q names only a hash of its key, and the key itself
//     is public from the first time the address spends, in that spend;
//   - a taproot address, whose program is its key;
//   - a public key, x-only (64 hex digits) or compressed (66).

var (
	secpN, _  = new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)
	secpGx, _ = new(big.Int).SetString("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", 16)
	secpGy, _ = new(big.Int).SetString("483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8", 16)
)

type point struct{ x, y *big.Int } // nil x: the point at infinity

func ptAdd(a, b point) point {
	if a.x == nil {
		return b
	}
	if b.x == nil {
		return a
	}
	p := secpP
	var l *big.Int
	if a.x.Cmp(b.x) == 0 {
		if new(big.Int).Add(a.y, b.y).Mod(new(big.Int).Add(a.y, b.y), p).Sign() == 0 {
			return point{}
		}
		num := new(big.Int).Mul(big.NewInt(3), new(big.Int).Mul(a.x, a.x))
		den := new(big.Int).ModInverse(new(big.Int).Mul(big.NewInt(2), a.y), p)
		l = num.Mul(num, den).Mod(num, p)
	} else {
		num := new(big.Int).Sub(b.y, a.y)
		den := new(big.Int).ModInverse(new(big.Int).Mod(new(big.Int).Sub(b.x, a.x), p), p)
		l = num.Mul(num, den).Mod(num, p)
	}
	x := new(big.Int).Sub(new(big.Int).Mul(l, l), a.x)
	x.Sub(x, b.x).Mod(x, p)
	y := new(big.Int).Sub(a.x, x)
	y.Mul(y, l).Sub(y, a.y).Mod(y, p)
	return point{x, y}
}

func ptMul(k *big.Int, g point) point {
	r := point{}
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = ptAdd(r, r)
		if k.Bit(i) == 1 {
			r = ptAdd(r, g)
		}
	}
	return r
}

// decompress reads a compressed public key.
func decompress(b []byte) (point, error) {
	if len(b) != 33 || (b[0] != 2 && b[0] != 3) {
		return point{}, errors.New("not a compressed public key")
	}
	x := new(big.Int).SetBytes(b[1:])
	if x.Cmp(secpP) >= 0 {
		return point{}, errors.New("not a valid public key")
	}
	y2 := new(big.Int).Exp(x, big.NewInt(3), secpP)
	y2.Add(y2, big.NewInt(7)).Mod(y2, secpP)
	y := new(big.Int).ModSqrt(y2, secpP)
	if y == nil {
		return point{}, errors.New("not a valid public key")
	}
	if y.Bit(0) != uint(b[0]&1) {
		y.Sub(secpP, y)
	}
	return point{x, y}, nil
}

func compress(p point) []byte {
	out := make([]byte, 33)
	out[0] = 2 + byte(p.y.Bit(0))
	p.x.FillBytes(out[1:])
	return out
}

const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58check(s string) ([]byte, error) {
	n := new(big.Int)
	for _, c := range s {
		i := strings.IndexRune(b58alphabet, c)
		if i < 0 {
			return nil, fmt.Errorf("not base58: %q", c)
		}
		n.Mul(n, big.NewInt(58)).Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	for _, c := range s {
		if c != '1' {
			break
		}
		b = append([]byte{0}, b...)
	}
	if len(b) < 5 {
		return nil, errors.New("too short")
	}
	body, sum := b[:len(b)-4], b[len(b)-4:]
	h := sha256.Sum256(body)
	h = sha256.Sum256(h[:])
	if !bytes.Equal(h[:4], sum) {
		return nil, errors.New("bad checksum")
	}
	return body, nil
}

// tpubVersion is a testnet extended public key's version bytes.
const tpubVersion = 0x043587cf

// The first account key in what was pasted: the bare tpub, the web wallet's
// "[fingerprint/84h/1h/0h]tpub…", or a descriptor line (or the pair of them)
// carrying it.
var accountKeyRe = regexp.MustCompile(`(?:\[([0-9a-fA-F]{8})((?:/[0-9]+['h])*)\])?(tpub[1-9A-HJ-NP-Za-km-z]{100,112})`)

// firstReceiveKey is the key of an account key's first receive address,
// child 0 of child 0. With an origin, it must be m/84'/1'/0', the account
// every wallet on the kit derives its testnet addresses from.
func firstReceiveKey(s string) (string, error) {
	m := accountKeyRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", errors.New("not an extended public key")
	}
	if m[2] != "" && strings.ReplaceAll(m[2], "h", "'") != "/84'/1'/0'" {
		return "", fmt.Errorf("that account key is at %s, not at the web wallet's 84h/1h/0h", m[2])
	}
	raw, err := base58check(m[3])
	if err != nil || len(raw) != 78 {
		return "", errors.New("that extended public key does not decode")
	}
	if binary.BigEndian.Uint32(raw[:4]) != tpubVersion {
		return "", errors.New("that is not a testnet extended public key (tpub)")
	}
	if m[2] == "" && raw[4] != 3 {
		return "", errors.New("that extended public key is not an account key (depth 3)")
	}
	chain, key := raw[13:45], raw[45:78]
	for _, i := range []uint32{0, 0} {
		k, err := decompress(key)
		if err != nil {
			return "", err
		}
		mac := hmac.New(sha512.New, chain)
		mac.Write(key)
		binary.Write(mac, binary.BigEndian, i)
		I := mac.Sum(nil)
		il := new(big.Int).SetBytes(I[:32])
		if il.Cmp(secpN) >= 0 {
			return "", errors.New("that account key derives no key at this index")
		}
		child := ptAdd(ptMul(il, point{secpGx, secpGy}), k)
		if child.x == nil {
			return "", errors.New("that account key derives no key at this index")
		}
		key, chain = compress(child), I[32:]
	}
	return hex.EncodeToString(key[1:]), nil
}

var esploraClient = &http.Client{Timeout: 10 * time.Second}

// spentKey reads the key of a P2WPKH address from a transaction that spent
// from it, through the explorer.
func (a *App) spentKey(addr string, prog []byte) (string, error) {
	if a.cfg.EsploraURL == "" {
		return "", errors.New("that address names only a hash of its key, and this site cannot look it up: paste the account key instead")
	}
	resp, err := esploraClient.Get(strings.TrimRight(a.cfg.EsploraURL, "/") + "/address/" + addr + "/txs")
	if err != nil {
		return "", errors.New("the explorer cannot be reached; try again in a minute")
	}
	defer resp.Body.Close()
	var txs []struct {
		Vin []struct {
			Witness []string `json:"witness"`
			Prevout *struct {
				Address string `json:"scriptpubkey_address"`
			} `json:"prevout"`
		} `json:"vin"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&txs) != nil {
		return "", errors.New("the explorer does not know that address")
	}
	for _, tx := range txs {
		for _, in := range tx.Vin {
			if in.Prevout == nil || !strings.EqualFold(in.Prevout.Address, addr) || len(in.Witness) != 2 {
				continue
			}
			pub, err := hex.DecodeString(in.Witness[1])
			if err != nil || len(pub) != 33 {
				continue
			}
			sh := sha256.Sum256(pub)
			rh := ripemd160.New()
			rh.Write(sh[:])
			if bytes.Equal(rh.Sum(nil), prog) && onCurve(pub[1:]) {
				return hex.EncodeToString(pub[1:]), nil
			}
		}
	}
	return "", errors.New("that address has not sent anything yet, so its key is not public: paste the account key instead (Receive, then Show xpub)")
}

// walletKey reads what a person pasted into the key the rehearsal pays.
func (a *App) walletKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("paste the account key from your wallet")
	}
	if strings.Contains(s, "tpub") {
		return firstReceiveKey(s)
	}
	low := strings.ToLower(s)
	if hrp, v, prog, err := segwitProgram(low); err == nil && v == 0 && len(prog) == 20 {
		ok := false
		for _, h := range a.cfg.PayoutHRPs {
			ok = ok || hrp == h
		}
		if !ok {
			return "", fmt.Errorf("an address starting %s1 is for another network: use your Sequentia testnet wallet", hrp)
		}
		return a.spentKey(low, prog)
	}
	return namedKey(s, a.cfg.PayoutHRPs)
}
