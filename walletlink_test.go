package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Signatures made by a Sequentia node's signmessage (elementsregtest, a
// legacy address of key linkPub), over "Emissio 0123456789" and
// "Emissio ffffffffff".
const (
	linkPub  = "02ea40c235a51e55512197cfe56eee151baba9477d27d789d4805f11e29d3b0162"
	linkSig  = "IChNI6p1s8MOGiOnrJP2T1XqLOIBFMNKnCwraO18vPpvHyURfdBA5No10lhDtzrgnqT33RTwUJ8pNvhWkT32DAE="
	linkSig2 = "HxGbZk2aa8shSnpKO9IjnNdfvF2BuYn9UBA2qSYT7HeEbaf0wMoGKKrJ6LQvmgo9z5WsRxpKTseHgrw2pEs7VVs="
)

func TestRecoverSigner(t *testing.T) {
	pub, err := recoverSigner("Emissio 0123456789", linkSig)
	if err != nil || hex.EncodeToString(pub) != linkPub {
		t.Fatalf("recovered %x, %v", pub, err)
	}
	if pub, _ := recoverSigner("Emissio 0123456789", linkSig2); hex.EncodeToString(pub) == linkPub {
		t.Fatal("a signature over another message recovered the same key")
	}
	if _, err := recoverSigner("x", "not base64!"); err == nil {
		t.Fatal("garbage accepted")
	}
	pb, _ := hex.DecodeString(linkPub)
	tb1, p2pkh := keyAddresses(pb)
	if !strings.HasPrefix(tb1, "tb1q") || len(tb1) != 42 || (p2pkh[0] != 'm' && p2pkh[0] != 'n') {
		t.Fatalf("addresses %s %s", tb1, p2pkh)
	}
	// The tb1 address round-trips through the payout's own decoder.
	if _, v, prog, err := segwitProgram(tb1); err != nil || v != 0 || hex.EncodeToString(prog) != hex.EncodeToString(hash160(pb)) {
		t.Fatalf("tb1 decode: %v %d %x", err, v, prog)
	}
}

func TestWalletLinkTask(t *testing.T) {
	app, srv, client := newTestServer(t)
	register(t, client, srv.URL, "w@x.io")
	app.db.Exec("UPDATE users SET claim_code = '0123456789' WHERE id = 1")
	pb, _ := hex.DecodeString(linkPub)
	tb1, p2pkh := keyAddresses(pb)
	user, _ := getUserByID(app.db, 1)
	cases := []struct {
		notes string
		v     verdict
		want  string
	}{
		{"no signature here " + tb1, vFail, "no signature in the notes"},
		{linkSig, vFail, "no address in the notes"},
		{"bitcoin:" + tb1 + "?sequentia=arca:00 " + linkSig2, vFail, `over the message "Emissio 0123456789"`},
		{"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx " + linkSig, vFail, "is not by the key of"},
		{"bitcoin:" + tb1 + "?ark=x&sequentia=arca:00\n" + linkSig, vPass, "now linked to this account (OK)"},
		{p2pkh + " " + linkSig, vPass, "now linked"},
	}
	for i, c := range cases {
		got := app.evidenceCheck("wallet-link", "", c.notes, user)
		if got.Verdict != c.v || !strings.Contains(got.Note, c.want) {
			t.Errorf("case %d: (%d) %q", i, got.Verdict, got.Note)
		}
	}
	if l := walletLinkOf(app.db, 1); l == nil || l.Address != tb1 {
		t.Fatalf("link: %+v", l)
	}
	if k, _ := keysOf(app.db, 1); k.RehearsalKey == "" {
		t.Fatal("the linked key was not offered to the rehearsal")
	}
	// The same wallet cannot link a second account.
	createUser(app.db, "w2@x.io", "x", "ffffffffff", "")
	u2, _ := getUserByID(app.db, 2)
	if got := app.evidenceCheck("wallet-link", "", tb1+" "+linkSig2, u2); got.Verdict != vFail || !strings.Contains(got.Note, "linked to another account") {
		t.Fatalf("second account: (%d) %q", got.Verdict, got.Note)
	}
}
