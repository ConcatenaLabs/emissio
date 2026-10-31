package main

// Linking a wallet to an account by a signed message. The web wallet's Sign
// tab (and `signmessage` on a node) signs a message with the key of one of
// the wallet's receive addresses, in the classic "Bitcoin Signed Message"
// format: a compact signature from which the signing key is recovered. The
// account signs "Emissio <account code>"; the key recovered is the wallet's,
// so its addresses (the tb1 address of the receive code, and the older
// P2PKH form the Sign tab shows next to it) are bound to the account. A
// wallet links to one account only, ever.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // HASH160 of a key
)

const walletLinkSchema = `
CREATE TABLE IF NOT EXISTS wallet_links (
	user_id INTEGER PRIMARY KEY REFERENCES users(id),
	address TEXT UNIQUE NOT NULL,
	pubkey TEXT UNIQUE NOT NULL,
	created_at INTEGER NOT NULL
);
`

const messageMagic = "Bitcoin Signed Message:\n"

func varIntBytes(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	}
	return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
}

func signedMessageHash(msg string) []byte {
	var b []byte
	b = append(b, varIntBytes(len(messageMagic))...)
	b = append(b, messageMagic...)
	b = append(b, varIntBytes(len(msg))...)
	b = append(b, msg...)
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	return h[:]
}

// recoverSigner returns the compressed key that made a compact signature
// over msg, or an error if the signature is malformed or names no key.
func recoverSigner(msg, sigB64 string) ([]byte, error) {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != 65 {
		return nil, errors.New("not a signature (65 bytes, base64)")
	}
	header := int(sig[0])
	if header < 27 || header > 34 {
		return nil, errors.New("not a signature this site reads")
	}
	recid := (header - 27) & 3
	r := new(big.Int).SetBytes(sig[1:33])
	s := new(big.Int).SetBytes(sig[33:])
	if r.Sign() == 0 || s.Sign() == 0 || r.Cmp(secpN) >= 0 || s.Cmp(secpN) >= 0 {
		return nil, errors.New("not a valid signature")
	}
	x := new(big.Int).Set(r)
	if recid&2 != 0 {
		x.Add(x, secpN)
	}
	if x.Cmp(secpP) >= 0 {
		return nil, errors.New("not a valid signature")
	}
	xb := make([]byte, 33)
	xb[0] = 2 + byte(recid&1)
	x.FillBytes(xb[1:])
	R, err := decompress(xb)
	if err != nil {
		return nil, errors.New("not a valid signature")
	}
	e := new(big.Int).SetBytes(signedMessageHash(msg))
	rInv := new(big.Int).ModInverse(r, secpN)
	// Q = r^-1 (sR - eG)
	sR := ptMul(s, R)
	negE := new(big.Int).Sub(secpN, new(big.Int).Mod(e, secpN))
	eG := ptMul(negE, point{secpGx, secpGy})
	Q := ptMul(rInv, ptAdd(sR, eG))
	if Q.x == nil {
		return nil, errors.New("not a valid signature")
	}
	return compress(Q), nil
}

func hash160(b []byte) []byte {
	h := sha256.Sum256(b)
	rm := ripemd160.New()
	rm.Write(h[:])
	return rm.Sum(nil)
}

// keyAddresses are the addresses of a key on the testnet: its tb1 (P2WPKH)
// address, which a receive code carries, and its P2PKH form.
func keyAddresses(pub []byte) (tb1, p2pkh string) {
	h := hash160(pub)
	data := []int{0}
	conv, _ := convertBits(bytesToInts(h), 8, 5, true)
	for _, c := range conv {
		data = append(data, int(c))
	}
	tb1 = bech32Encode("tb", data, 1)
	return tb1, base58checkEncode(0x6f, h)
}

func bytesToInts(b []byte) []int {
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

func base58checkEncode(version byte, payload []byte) string {
	b := append([]byte{version}, payload...)
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	b = append(b, h[:4]...)
	n := new(big.Int).SetBytes(b)
	var out []byte
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, big.NewInt(58), mod)
		out = append(out, b58alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func linkMessage(code string) string { return "Emissio " + code }

// receiveAddress takes the address from what the tester pasted: an
// address, or a receive code (receivecode.go).
func receiveAddress(s string) string { return addressOf(s) }

// linkCheck verifies a linking signature and says which wallet it links.
func linkCheck(code, addrIn, sig string) (tb1, pubHex string, err error) {
	pub, err := recoverSigner(linkMessage(code), sig)
	if err != nil {
		return "", "", fmt.Errorf("%v: sign the message \"%s\" exactly", err, linkMessage(code))
	}
	tb1, p2pkh := keyAddresses(pub)
	addr := receiveAddress(addrIn)
	if addr != "" && !strings.EqualFold(addr, tb1) && addr != p2pkh {
		return "", "", fmt.Errorf("the signature is not by the key of %s over the message \"%s\": sign exactly that message, with the address of your receive code", short(addr), linkMessage(code))
	}
	return tb1, hex.EncodeToString(pub), nil
}

type walletLink struct {
	Address, Pubkey string
	CreatedAt       int64
}

func walletLinkOf(db *sql.DB, userID int64) *walletLink {
	var l walletLink
	if db.QueryRow("SELECT address, pubkey, created_at FROM wallet_links WHERE user_id = ?", userID).Scan(&l.Address, &l.Pubkey, &l.CreatedAt) != nil {
		return nil
	}
	return &l
}

// linkWallet stores the link, refusing a wallet another account linked,
// and offers its key to the payout rehearsal when the account gave none.
func (a *App) linkWallet(userID int64, tb1, pub string) error {
	var other int64
	a.db.QueryRow("SELECT user_id FROM wallet_links WHERE (address = ? OR pubkey = ?) AND user_id != ?", tb1, pub, userID).Scan(&other)
	if other != 0 {
		return errors.New("that wallet is linked to another account: a wallet links to one account only")
	}
	if _, err := a.db.Exec(`INSERT INTO wallet_links (user_id, address, pubkey, created_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET address = excluded.address, pubkey = excluded.pubkey, created_at = excluded.created_at`,
		userID, tb1, pub, now()); err != nil {
		return err
	}
	a.recordChainLinks(userID, "linked-wallet", []string{tb1})
	return nil
}

var (
	sigRe     = regexp.MustCompile(`[A-Za-z0-9+/]{87}=`)
	addrTokRe = regexp.MustCompile(`(?i)(bitcoin:)?(tb1[0-9a-z]{20,80}|[mn][1-9A-HJ-NP-Za-km-z]{25,34})`)
)

// walletLinkCheck reads the signature and the address from the notes, and
// links the wallet when they pass.
func (a *App) walletLinkCheck(user *User, notes string) checkResult {
	sig := sigRe.FindString(notes)
	if sig == "" {
		return fail("no signature in the notes: paste the signature the Sign tab shows")
	}
	addr := ""
	if m := addrTokRe.FindStringSubmatch(sigRe.ReplaceAllString(notes, " ")); m != nil {
		addr = m[2]
	}
	if addr == "" {
		return fail("no address in the notes: paste your receive code or the address you signed with")
	}
	tb1, pub, err := linkCheck(user.ClaimCode, addr, sig)
	if err != nil {
		return fail("%v", err)
	}
	if err := a.linkWallet(user.ID, tb1, pub); err != nil {
		return fail("%v", err)
	}
	// The rehearsal pays the key of a receive address of the wallet; the
	// linked one serves when the account gave none.
	if k, _ := keysOf(a.db, user.ID); k.RehearsalKey == "" {
		if key, err := namedKey(pub, a.cfg.PayoutHRPs); err == nil {
			setKey(a.db, user.ID, "rehearsal_key", key)
		}
	}
	return pass("signed by the key of %s, now linked to this account (OK)", tb1)
}
