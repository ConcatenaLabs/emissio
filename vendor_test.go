package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"
)

// The connector layer in static/shared is seqdex-web's, vendored: each file
// is one header line naming the source path and commit, then the upstream
// file unchanged. These tests fail when a vendored body is edited here, or
// differs from that commit upstream, so the copy cannot drift silently. To
// update it, vendor the files again from a new seqdex-web commit and set the
// hashes below to the new bodies' sha256.
var vendored = map[string]string{
	"static/shared/connectors.js":         "7a4ddcc9f1e69197040d0af6fe58a0ddc1259aa4ab23082576967bce4768f8de",
	"static/shared/provider.js":           "81c351bae379aba6a639b4ccccc95a857234675692bd7149a73e4bfd5e4836d3",
	"static/shared/vendor/nostr-tools.js": "a87e35d03745faca61dff2ada2110e3757542a7dc412a41badf87615dd71757f",
	"static/shared/vendor/qrcode.mjs":     "ea91d7118a5395289170da848b7c6758b996163bfbccf312591ab65a4911b7c0",
}

var vendorHeader = regexp.MustCompile(`^// Vendored from ConcatenaLabs/seqdex-web (\S+) at ([0-9a-f]{40}), unchanged below this line`)

func vendoredBody(t *testing.T, path string) (src, commit string, body []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nl := bytes.IndexByte(b, '\n')
	m := vendorHeader.FindSubmatch(b[:max(nl, 0)])
	if nl < 0 || m == nil {
		t.Fatalf("%s: no vendoring header naming the source file and commit", path)
	}
	return string(m[1]), string(m[2]), b[nl+1:]
}

func TestVendoredConnectorsUnchanged(t *testing.T) {
	commits := map[string]bool{}
	for path, want := range vendored {
		_, commit, body := vendoredBody(t, path)
		commits[commit] = true
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s was edited here (sha256 %s, vendored %s): change it in seqdex-web and vendor it again", path, got, want)
		}
	}
	if len(commits) != 1 {
		t.Errorf("the vendored files come from %d seqdex-web commits; vendor them together from one", len(commits))
	}
}

// TestVendoredConnectorsMatchUpstream reads each file at the named commit
// from GitHub; without the network it is skipped.
func TestVendoredConnectorsMatchUpstream(t *testing.T) {
	client := &http.Client{Timeout: 20 * time.Second}
	for path := range vendored {
		src, commit, body := vendoredBody(t, path)
		resp, err := client.Get("https://raw.githubusercontent.com/ConcatenaLabs/seqdex-web/" + commit + "/" + src)
		if err != nil {
			t.Skipf("GitHub unreachable: %v", err)
		}
		up, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: upstream %s at %s answered %d", path, src, commit, resp.StatusCode)
		}
		if !bytes.Equal(up, body) {
			t.Errorf("%s differs from seqdex-web %s at %s", path, src, commit)
		}
	}
}
