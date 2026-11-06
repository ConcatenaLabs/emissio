package main

// Connecting a wallet on the link task. The page carries the connector layer
// SeqDEX uses (static/shared, vendored from seqdex-web; vendor_test.go keeps
// it identical), and offers the connectors EMISSIO_CONNECTORS names, in order.
// Whichever wallet answers, it only signs "Emissio <account code>"; the
// address and signature go through walletlink.go's check like a pasted pair.
//
// The page's content security policy follows the setting: a wallet on a
// phone is reached through the pairing relays, and Sequentia Core through
// the sequentia-connect daemon on the visitor's own computer, so each is
// allowed only while it is offered.

import (
	"log"
	"strings"
)

// knownConnectors are the connector ids shared/connectors.js defines.
var knownConnectors = map[string]bool{"webwallet": true, "extension": true, "phone": true, "local": true}

// pairRelays are shared/connectors.js's PAIR_RELAYS; connectLocal is the
// daemon's loopback address (LOCAL_DEFAULT_URL).
var (
	pairRelays   = []string{"wss://relay.primal.net", "wss://relay.damus.io"}
	connectLocal = []string{"http://127.0.0.1:7419", "http://localhost:7419"}
)

func parseConnectors(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range strings.Split(s, ",") {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		if !knownConnectors[c] {
			log.Fatalf("EMISSIO_CONNECTORS: %q is not a connector (webwallet, extension, phone, local)", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

func contentSecurityPolicy(connectors []string) string {
	connect := []string{"'self'"}
	for _, c := range connectors {
		switch c {
		case "phone":
			connect = append(connect, pairRelays...)
		case "local":
			connect = append(connect, connectLocal...)
		}
	}
	return "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; connect-src " + strings.Join(connect, " ")
}
