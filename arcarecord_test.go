package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// realRecord is a coin record a real arca wallet made on a private chain,
// with the owner nonce that wallet keeps for the coin in its own store.
type realRecord struct {
	Name, Kind, Nonce, Record string
	Owner                     string // the owner the library's own reader names
	Library                   string // the library versions that read it
}

var (
	realOnce sync.Once
	realRecs []realRecord
)

// realRecords reads testdata/arca-records.json: records copied from arca
// wallets of a private-chain test run, both format versions, batch leaves,
// a board, fills, and transfers whose inputs are a board, a fill or another
// transfer.
func realRecords(t *testing.T) []realRecord {
	realOnce.Do(func() {
		b, err := os.ReadFile("testdata/arca-records.json")
		if err == nil {
			json.Unmarshal(b, &realRecs)
		}
	})
	if len(realRecs) == 0 {
		t.Fatal("no real records")
	}
	return realRecs
}

func realOf(t *testing.T, kind string, size int) []byte {
	for _, r := range realRecords(t) {
		b, _ := hex.DecodeString(r.Record)
		if r.Kind == kind && (size == 0 || len(b) == size) {
			return b
		}
	}
	t.Fatalf("no real %s record of %d bytes", kind, size)
	return nil
}

// realNamed is the real record the fixture names: "paid", a transfer's coin;
// "spends-paid", a coin of the transfer that spent it; "change-of-paid", the
// other coin of the transfer that made "paid"; "spends-fill", a version 2
// transfer spending the fill "fill-paid", which spent the batch leaf
// "in-spends-fill".
func realNamed(t *testing.T, name string) []byte {
	for _, r := range realRecords(t) {
		if r.Name == name {
			b, _ := hex.DecodeString(r.Record)
			return b
		}
	}
	t.Fatalf("no real record %q", name)
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// synthRecord is the record of a transfer, in arca's version 1 form, whose
// own leaf is owner and nonce: it spends input (a record without its
// version byte; a real batch leaf when nil) and pays one output to script.
func synthRecord(t *testing.T, input []byte, owner, nonce string, script []byte) string {
	if input == nil {
		input = realOf(t, "batch", 362)[1:]
	}
	var w bytes.Buffer
	w.Write([]byte{coinRecordVersion, 1, 1})
	w.Write(input)
	w.Write(make([]byte, 8+4*64)) // checkpoint value, four signatures
	w.WriteByte(1)                // one output
	w.Write(bytes.Repeat([]byte{0xab}, 32))
	binary.Write(&w, binary.LittleEndian, uint64(1000))
	w.WriteByte(byte(len(script))) // a compact size under 0xfd
	w.Write(script)
	w.WriteByte(0) // index
	o, _ := hex.DecodeString(owner)
	n, _ := hex.DecodeString(nonce)
	w.Write(o)
	w.Write(n)
	w.Write(make([]byte, 32+2)) // creator nonce, exit delay
	return hex.EncodeToString(w.Bytes())
}

// Every vector is read as the arca library reads it: the owner and nonce
// the library's own CoinRecord::owner names (at 1bc38878d2, the version on
// the server, and at master, as the fixture records), for boards and
// accumulator slots too. A fill's leaf credited to its maker, or its rest,
// names its nonce alone here (the library takes its key from the order's
// terms). The fixture holds records real wallets made and records built for
// the tags those lack, each kept only if the library's reader accepts it.
func TestOwnLeafOfRealRecords(t *testing.T) {
	seen := map[string]int{}
	for _, r := range realRecords(t) {
		raw, _ := hex.DecodeString(r.Record)
		l, named, err := ownLeaf(raw)
		switch {
		case err != nil:
			t.Errorf("%s (%s): %v", r.Name, r.Kind, err)
		case !named || l.Nonce != r.Nonce:
			t.Errorf("%s (%s): nonce %q, the library's %s", r.Name, r.Kind, l.Nonce, r.Nonce)
		case l.Owner != r.Owner && !(r.Kind == "fill" && l.Owner == ""):
			t.Errorf("%s (%s): owner %q, the library's %s", r.Name, r.Kind, l.Owner, r.Owner)
		}
		seen[r.Kind]++
	}
	t.Logf("read %v", seen)
	// The history is skipped by structure: the transfer whose input is
	// another transfer names its own leaf, not its input's.
	raw := realOf(t, "transfer", 1863)
	in := &recReader{b: raw, pos: 3}
	inner, tag, err := in.record(1)
	if err != nil || tag != 1 || inner == nil {
		t.Fatalf("the input transfer: %v %d", err, tag)
	}
	own, _, _ := ownLeaf(raw)
	if !strings.Contains(hex.EncodeToString(raw), inner.Owner+inner.Nonce) || own == *inner {
		t.Fatal("the fixture does not carry its input's leaf ahead of its own")
	}
	// Truncated, extended or of an unknown version: not read.
	for _, bad := range [][]byte{raw[:len(raw)-1], append(append([]byte{}, raw...), 0), append([]byte{3}, raw[1:]...)} {
		if _, _, err := ownLeaf(bad); err == nil {
			t.Error("a damaged record was read")
		}
	}
}

// A payer's bytes outside the leaf (an output's script) never name the
// coin's owner.
func TestOwnLeafIgnoresOutputs(t *testing.T) {
	owner, nonce, other := randHex(32), randHex(32), randHex(64)
	o, _ := hex.DecodeString(other)
	raw, _ := hex.DecodeString(synthRecord(t, nil, owner, nonce, o))
	l, ok, err := ownLeaf(raw)
	if err != nil || !ok || l.Owner != owner || l.Nonce != nonce {
		t.Fatalf("%v %v %+v", err, ok, l)
	}
}
