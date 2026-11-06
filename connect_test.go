package main

import (
	"io/fs"
	"strings"
	"testing"
)

// The content security policy allows no inline style, so a style attribute
// in a template is silently dropped by the browser: none may be written.
func TestNoInlineStyles(t *testing.T) {
	fs.WalkDir(tplFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(tplFS, p)
		if strings.Contains(string(b), "style=\"") {
			t.Errorf("%s has an inline style attribute: give it a class in static/style.css", p)
		}
		return nil
	})
}

// The connectors setting: known ids in order, and a policy that lets the
// page reach a phone's relays or the local daemon only when they are offered.
func TestConnectorSetting(t *testing.T) {
	if got := parseConnectors(" WebWallet, phone,webwallet ,"); strings.Join(got, ",") != "webwallet,phone" {
		t.Fatalf("parsed %v", got)
	}
	csp := contentSecurityPolicy([]string{"webwallet"})
	if !strings.HasSuffix(csp, "connect-src 'self'") {
		t.Fatalf("web wallet alone: %s", csp)
	}
	csp = contentSecurityPolicy([]string{"webwallet", "phone", "local"})
	for _, want := range []string{"wss://relay.primal.net", "wss://relay.damus.io", "http://127.0.0.1:7419"} {
		if !strings.Contains(csp, want) {
			t.Errorf("all offered: no %s in %s", want, csp)
		}
	}
	// The relays and the port are shared/connectors.js's.
	_, _, body := vendoredBody(t, "static/shared/connectors.js")
	for _, want := range append(append([]string{}, pairRelays...), connectLocal[0]) {
		if !strings.Contains(string(body), "'"+want+"'") {
			t.Errorf("connectors.js no longer names %s", want)
		}
	}
}
