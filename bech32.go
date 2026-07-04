package main

// Minimal bech32/bech32m decoder used to validate mainnet Sequentia
// receiving addresses. Sequentia's transparent mainnet addresses use the
// same encoding and human-readable part as Bitcoin ("bc1..."), so this is
// a standard BIP-173/BIP-350 segwit address check.

import (
	"errors"
	"fmt"
	"strings"
)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

const (
	bech32Const  = 1
	bech32mConst = 0x2bc830a3
)

func bech32Polymod(values []int) int {
	gen := []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []int {
	out := make([]int, 0, len(hrp)*2+1)
	for _, c := range hrp {
		out = append(out, int(c)>>5)
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, int(c)&31)
	}
	return out
}

// bech32Decode returns the HRP, the 5-bit data values (without checksum) and
// the checksum constant that verified (bech32 or bech32m).
func bech32Decode(addr string) (string, []int, int, error) {
	if len(addr) < 8 || len(addr) > 90 {
		return "", nil, 0, errors.New("invalid length")
	}
	lower := strings.ToLower(addr)
	if addr != lower && addr != strings.ToUpper(addr) {
		return "", nil, 0, errors.New("mixed case")
	}
	addr = lower
	sep := strings.LastIndexByte(addr, '1')
	if sep < 1 || sep+7 > len(addr) {
		return "", nil, 0, errors.New("missing separator")
	}
	hrp := addr[:sep]
	for _, c := range hrp {
		if c < 33 || c > 126 {
			return "", nil, 0, errors.New("invalid hrp character")
		}
	}
	data := make([]int, 0, len(addr)-sep-1)
	for _, c := range addr[sep+1:] {
		v := strings.IndexRune(bech32Charset, c)
		if v < 0 {
			return "", nil, 0, fmt.Errorf("invalid character %q", c)
		}
		data = append(data, v)
	}
	pm := bech32Polymod(append(bech32HRPExpand(hrp), data...))
	if pm != bech32Const && pm != bech32mConst {
		return "", nil, 0, errors.New("bad checksum")
	}
	return hrp, data[:len(data)-6], pm, nil
}

func convertBits(data []int, from, to uint, pad bool) ([]byte, error) {
	acc, bits := 0, uint(0)
	maxv := (1 << to) - 1
	var out []byte
	for _, v := range data {
		if v < 0 || v>>from != 0 {
			return nil, errors.New("invalid data range")
		}
		acc = acc<<from | v
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, errors.New("invalid padding")
	}
	return out, nil
}

// validateMainnetAddress checks a Sequentia mainnet transparent receiving
// address. It returns a human-readable error suitable for showing to users.
func validateMainnetAddress(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("enter an address")
	}
	low := strings.ToLower(addr)
	switch {
	case strings.HasPrefix(low, "tb1") || strings.HasPrefix(low, "tsqb1"):
		return errors.New("that is a testnet address. Your allocation is paid on mainnet, so Emissio needs a mainnet address, which starts with bc1")
	case strings.HasPrefix(low, "sqb1"):
		return errors.New("that is a confidential (blinded) address. For the launch allocation, submit a transparent address, which starts with bc1")
	case strings.HasPrefix(low, "lq1") || strings.HasPrefix(low, "ex1") || strings.HasPrefix(low, "ert1") || strings.HasPrefix(low, "el1"):
		return errors.New("that looks like a Liquid or Elements address, not a Sequentia mainnet address. Sequentia mainnet addresses start with bc1")
	case strings.HasPrefix(low, "1") || strings.HasPrefix(low, "3"):
		return errors.New("legacy address formats are not supported here. Submit a native segwit address, which starts with bc1")
	}
	hrp, data, checksum, err := bech32Decode(addr)
	if err != nil {
		return fmt.Errorf("not a valid bech32 address (%v). A mainnet address starts with bc1", err)
	}
	if hrp != "bc" {
		return fmt.Errorf("wrong network prefix %q. A Sequentia mainnet address starts with bc1", hrp)
	}
	if len(data) < 1 {
		return errors.New("address has no witness program")
	}
	version := data[0]
	prog, err := convertBits(data[1:], 5, 8, false)
	if err != nil {
		return errors.New("not a valid segwit address")
	}
	switch {
	case version == 0:
		if checksum != bech32Const {
			return errors.New("version 0 addresses must use bech32, not bech32m")
		}
		if len(prog) != 20 && len(prog) != 32 {
			return errors.New("not a valid segwit v0 address")
		}
	case version == 1:
		if checksum != bech32mConst {
			return errors.New("version 1 (taproot) addresses must use bech32m")
		}
		if len(prog) != 32 {
			return errors.New("not a valid taproot address")
		}
	default:
		return errors.New("unsupported witness version. Submit a bc1q (segwit v0) or bc1p (taproot) address")
	}
	return nil
}

// bech32Encode is used by tests to build known-good addresses.
func bech32Encode(hrp string, data []int, checksumConst int) string {
	values := append(bech32HRPExpand(hrp), data...)
	pm := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ checksumConst
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range data {
		b.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(pm>>uint(5*(5-i)))&31])
	}
	return b.String()
}
