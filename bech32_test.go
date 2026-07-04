package main

import (
	"strings"
	"testing"
)

func TestValidateMainnetAddress(t *testing.T) {
	valid := []string{
		// BIP-173 reference P2WPKH
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4",
	}
	for _, a := range valid {
		if err := validateMainnetAddress(a); err != nil {
			t.Errorf("expected valid %q, got %v", a, err)
		}
	}

	// Round-trip a synthetic taproot (v1, 32-byte program) address.
	prog := make([]int, 0, 52)
	prog = append(prog, 1) // witness v1
	raw := make([]int, 0)
	for i := 0; i < 32; i++ {
		raw = append(raw, i%256)
	}
	conv := make([]int, 0)
	acc, bits := 0, 0
	for _, b := range raw {
		acc = acc<<8 | b
		bits += 8
		for bits >= 5 {
			bits -= 5
			conv = append(conv, (acc>>bits)&31)
		}
	}
	if bits > 0 {
		conv = append(conv, (acc<<(5-bits))&31)
	}
	prog = append(prog, conv...)
	taproot := bech32Encode("bc", prog, bech32mConst)
	if err := validateMainnetAddress(taproot); err != nil {
		t.Errorf("expected synthetic taproot %q valid, got %v", taproot, err)
	}
	// Same data with the wrong checksum constant must fail.
	badChecksum := bech32Encode("bc", prog, bech32Const)
	if err := validateMainnetAddress(badChecksum); err == nil {
		t.Errorf("expected v1-with-bech32 %q to be rejected", badChecksum)
	}

	invalid := map[string]string{
		"":    "empty",
		"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx":    "testnet",
		"sqb1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq":  "confidential",
		"lq1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq":  "liquid",
		"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2":            "legacy",
		"3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy":            "legacy p2sh",
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5":    "bad checksum",
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3tb":    "bad checksum b",
		"notanaddress": "garbage",
	}
	for a, why := range invalid {
		if err := validateMainnetAddress(a); err == nil {
			t.Errorf("expected %s address %q to be rejected", why, a)
		}
	}
}

func TestFormatSEQ(t *testing.T) {
	cases := map[int64]string{0: "0", 10: "10", 1000: "1,000", 1000000: "1,000,000", -2500: "-2,500"}
	for in, want := range cases {
		if got := formatSEQ(in); got != want {
			t.Errorf("formatSEQ(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatUSD(t *testing.T) {
	if got := formatUSD(1000000); got != "375,000" {
		t.Errorf("formatUSD(1000000) = %q", got)
	}
	if got := formatUSD(215); got != "80.63" {
		t.Errorf("formatUSD(215) = %q", got)
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse battery staple")
	if !verifyPassword(h, "correct horse battery staple") {
		t.Error("valid password rejected")
	}
	if verifyPassword(h, "wrong password") {
		t.Error("wrong password accepted")
	}
	if !strings.HasPrefix(h, "argon2id$") {
		t.Errorf("unexpected hash format %q", h)
	}
}
