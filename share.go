package main

// The invite kit: a ready-made post for X, Reddit and Telegram that carries
// the account's own code and referral link, so passing Emissio on takes one
// click and no copying of codes from the account page.
//
// The X post doubles as the X verification post: it contains the account
// code, which is what the X check looks for, so posting it and pasting its
// link under Verified accounts earns the verification as well.
//
// With EMISSIO_X_QUOTE_POST set to the project's official post, the kit also
// offers to quote it: the tester's text, with the same code and link, above
// the official post. A plain retweet would carry neither, so every quote is
// what spreads the original and brings in the tester's own referrals.

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

type shareView struct {
	Code     string
	Link     string
	XText    string
	XURL     string
	QuoteURL string // empty unless the official post is configured
	Reddit   string // the Reddit post's body
	RedTitle string
	RedURL   string
	TGURL    string
	// XVerified: the account already has a verified X account, so the X
	// post no longer earns the verification.
	XVerified bool
	Bonus     int64
	Threshold int64
}

// xURLWeight is what X counts for any link, whatever its length.
const xURLWeight = 23

// xLength is a post's length as X counts it: a link counts 23.
func xLength(text, link string) int {
	return utf8.RuneCountInString(strings.ReplaceAll(text, link, "")) + strings.Count(text, link)*xURLWeight
}

func shareTexts(code, link string) (x, quote, redTitle, reddit string) {
	tail := "Join with my link: " + link + "\nMy Emissio code: " + code
	x = "Every asset. On your Bitcoin wallet.\n\n" +
		"I'm testing Sequentia's wallet, node and ecosystem on testnet, and Emissio rewards every task with tokens. " +
		"Nothing to buy, no keys asked.\n\n" + tail
	// Above the official post, which already says what Sequentia is.
	quote = "I'm testing Sequentia, every asset on your Bitcoin wallet, and Emissio rewards every task with tokens. " +
		"Nothing to buy, no keys asked.\n\n" + tail
	redTitle = "I'm testing Sequentia, a Bitcoin wallet for every asset, and earning tokens for every task"
	reddit = "Sequentia puts every asset on your Bitcoin wallet: your Bitcoin address and keys hold BTC, stablecoins, " +
		"tokenized shares and tokens anyone can issue, and swap them against BTC. Its wallet, node and ecosystem are " +
		"open on testnet, and Emissio, the project's rewards program, credits Sequence tokens (SEQ) for every task: " +
		"making a transaction, swapping against BTC, issuing an asset, staking, running a node, reporting bugs.\n\n" +
		"Why this is not a scam or a chain letter:\n\n" +
		"- Nothing to buy and nothing to deposit. Testnet coins come from the faucet and have no value.\n" +
		"- Nobody asks for your seed phrase or private keys. Rewards are paid at mainnet launch to an address you generate yourself.\n" +
		"- Every task is checked against the public testnet chain, and the code of the network, the wallets and Emissio itself is open source: github.com/ConcatenaLabs\n" +
		fmt.Sprintf("- The referral bonus pays only once both people have earned %d SEQ each from real tasks, so fake accounts gain nothing.\n\n", referralThreshold) +
		"If you want to try it: " + link + "\n\n" +
		fmt.Sprintf("My Emissio code is %s. Join through the link and we each get %d SEQ once we have both earned %d.", code, referralBonus, referralThreshold)
	return x, quote, redTitle, reddit
}

// qesc escapes a query value with %20 for spaces, which every site reads
// the same way, where a + is left to the site's judgement.
func qesc(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// shareFor builds the kit for a signed-in user; nil when signed out.
func (a *App) shareFor(r *http.Request, user *User) *shareView {
	if user == nil {
		return nil
	}
	link := a.publicURL(r) + "/r/" + user.ClaimCode
	v := &shareView{Code: user.ClaimCode, Link: link, Bonus: referralBonus, Threshold: referralThreshold}
	var quote string
	v.XText, quote, v.RedTitle, v.Reddit = shareTexts(user.ClaimCode, link)
	v.XURL = "https://x.com/intent/post?text=" + qesc(v.XText)
	if a.cfg.XQuotePost != "" {
		// X attaches a post given as url as a quote of it.
		v.QuoteURL = "https://x.com/intent/post?text=" + qesc(quote) + "&url=" + qesc(a.cfg.XQuotePost)
	}
	v.RedURL = "https://www.reddit.com/submit?type=TEXT&title=" + qesc(v.RedTitle) + "&text=" + qesc(v.Reddit)
	v.TGURL = "https://t.me/share/url?url=" + qesc(link) + "&text=" + qesc(strings.TrimSpace(strings.Split(v.XText, "\n\n")[0]))
	if vs, err := verificationsOf(a.db, user.ID); err == nil {
		for _, s := range vs {
			if s.Platform == "x" && s.Status == "verified" {
				v.XVerified = true
			}
		}
	}
	return v
}

// xQuotePost is the configured official post as X links it, or empty when
// the setting is not a link to a post: a wrong value hides the button
// rather than sending testers to quote something else.
func xQuotePost(link string) string {
	if strings.TrimSpace(link) == "" {
		return ""
	}
	if _, canonical, ok := parseXPost(link); ok {
		return canonical
	}
	log.Printf("EMISSIO_X_QUOTE_POST is not a link to a post on X: %q; no quote button", link)
	return ""
}
