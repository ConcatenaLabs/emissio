package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const policy = "c8eccacf0953e1931cd31e434d8319101cc36e6c38b0e2104d8687552fae3e40"

func TestProofTag(t *testing.T) {
	if got := proofTag("0000000000"); got != "00000000" {
		t.Errorf("zero code: %q", got)
	}
	tag := proofTag("0123456789")
	if len(tag) != 8 || tag != "00000000" && tag == proofTag("0123456788") {
		t.Errorf("tag %q", tag)
	}
	// 0x0123456789 = 4886718345 -> % 1e8 = 86718345
	if tag != "86718345" {
		t.Errorf("proofTag(0123456789) = %q, want 86718345", tag)
	}
}

// A fake explorer serving canned transactions, assets and blocks.
func fakeExplorer(t *testing.T, txs map[string]string, assets map[string]string, blocks map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		var ok bool
		switch {
		case strings.HasPrefix(r.URL.Path, "/tx/"):
			body, ok = txs[strings.TrimPrefix(r.URL.Path, "/tx/")]
		case strings.HasPrefix(r.URL.Path, "/asset/"):
			body, ok = assets[strings.TrimPrefix(r.URL.Path, "/asset/")]
		case strings.HasPrefix(r.URL.Path, "/block/"):
			body, ok = blocks[strings.TrimPrefix(r.URL.Path, "/block/")]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
}

func vout(value int64, asset, typ string) map[string]any {
	return map[string]any{"value": value, "asset": asset, "scriptpubkey_type": typ}
}

// voutS is an output with a script and an address.
func voutS(value int64, asset, typ, script, addr string) map[string]any {
	return map[string]any{"value": value, "asset": asset, "scriptpubkey_type": typ, "scriptpubkey": script, "scriptpubkey_address": addr}
}

func txJSON(t *testing.T, vin []map[string]any, vout []map[string]any, confirmed bool) string {
	m := map[string]any{"txid": "aa", "vin": vin, "vout": vout,
		"status": map[string]any{"confirmed": confirmed, "block_height": 42, "block_hash": "bh"}}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestPeerCodes(t *testing.T) {
	ps := []peer{
		{Addr: "1.2.3.4:1", Subver: "/Sequentia Core:24.7.10(emissio-0123456789)/"},
		{Addr: "1.2.3.5:1", Subver: "/Emissio-ABCDEFabcd:24.7.10/"},
		{Addr: "1.2.3.6:1", Subver: "/Sequentia Core:24.7.10/"},
	}
	codes := peerCodes(ps)
	if len(codes) != 2 || codes["0123456789"] == "" || codes["abcdefabcd"] == "" {
		t.Errorf("peerCodes = %v", codes)
	}
	if !builtClientName(ps[1].Subver, "abcdefABCD") || builtClientName(ps[0].Subver, "0123456789") {
		t.Errorf("builtClientName wrong")
	}
}

func TestUptimeVerdict(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("CREATE TABLE peer_sightings (code TEXT NOT NULL, seen_at INTEGER NOT NULL)")
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if got := uptimeVerdict(db, "abc", now); !strings.HasPrefix(got, "node never seen") {
		t.Errorf("empty: %q", got)
	}
	// A full week at the slot cadence, with one slot in ten missing.
	start := now.Add(-7 * 24 * time.Hour)
	for i := 0; i < 7*24*6; i++ {
		if i%10 == 3 {
			continue
		}
		db.Exec("INSERT INTO peer_sightings VALUES ('abc', ?)", start.Add(time.Duration(i)*uptimeSlot).Unix())
	}
	got := uptimeVerdict(db, "abc", now)
	if !strings.HasSuffix(got, "(OK)") || !strings.Contains(got, "90%") {
		t.Errorf("full week: %q", got)
	}
	// Two days only.
	db.Exec("DELETE FROM peer_sightings")
	for i := 0; i < 2*24*6; i++ {
		db.Exec("INSERT INTO peer_sightings VALUES ('abc', ?)", start.Add(time.Duration(i)*uptimeSlot).Unix())
	}
	if got := uptimeVerdict(db, "abc", now); !strings.HasSuffix(got, "(does NOT yet qualify)") {
		t.Errorf("two days: %q", got)
	}
}

func TestNormalizeURL(t *testing.T) {
	a := normalizeURL("HTTPS://Example.COM/Work/Piece.png/#top")
	b := normalizeURL("https://example.com/Work/Piece.png")
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
	if normalizeURL("https://example.com/a") == normalizeURL("https://example.com/A") {
		t.Errorf("path case must matter")
	}
}

func TestEntryReportChecks(t *testing.T) {
	code := "0123456789"
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page-with-code":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body>My entry, code 0123456789</body></html>"))
		case "/page-without":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body>nothing</body></html>"))
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	app, _, _ := newTestServer(t)

	entries := map[string]string{
		site.URL + "/page-with-code":         "account code found on the linked page (OK)",
		site.URL + "/page-without":           "account code NOT found on the linked page; the judges check that the work itself carries the account code 0123456789",
		site.URL + "/image.png":              "link is a file (image/png); the judges check that the work itself carries the account code 0123456789",
		site.URL + "/gallery/0123456789.png": "account code is in the link (OK)",
		site.URL + "/missing":                "link returned HTTP 404; the judges check that the work carries the account code 0123456789",
	}
	for u, want := range entries {
		if got := app.entryCheck(u, code); got != want {
			t.Errorf("entryCheck(%s)\n got %q\nwant %q", u, got, want)
		}
	}

	uid, _ := createUser(app.db, "r1@example.com", "x", code, "")
	other, _ := createUser(app.db, "r2@example.com", "x", "ffffffffff", "")
	if got := app.reportCheck(uid, "0123456789\nThe node crashes when...", code); got != "first line carries the account code (OK)" {
		t.Errorf("plain: %q", got)
	}
	if got := app.reportCheck(uid, "The node crashes when...", code); got != "account code NOT on the first line" {
		t.Errorf("no code: %q", got)
	}
	if got := app.reportCheck(uid, "-----BEGIN PGP MESSAGE-----\nabc\n-----END PGP MESSAGE-----", code); !strings.HasPrefix(got, "encrypted: after decrypting") {
		t.Errorf("encrypted: %q", got)
	}
	// The same body from another account is flagged.
	if _, err := createReport(app.db, other, "t", "low", "ffffffffff\nsame text", "x"); err != nil {
		t.Fatal(err)
	}
	if got := app.reportCheck(uid, "ffffffffff\nsame text", code); !strings.Contains(got, "IDENTICAL to report #") {
		t.Errorf("duplicate: %q", got)
	}

}
