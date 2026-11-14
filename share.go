package main

// The invite kit: a ready-made post for X, Reddit and Telegram that carries
// the account's own code and referral link, so passing Emissio on takes one
// click and no copying of codes from the account page.
//
// The X post doubles as the X verification post: it contains the account
// code, which is what the X check looks for, so posting it and pasting its
// link under Verified accounts earns the verification as well.

import (
	"fmt"
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

func shareTexts(code, link string) (x, redTitle, reddit string) {
	x = "I'm earning Sequence tokens (SEQ) on Emissio by testing Sequentia, a Bitcoin sidechain for tokenized assets. " +
		"Simple wallet tasks, nothing to buy or deposit, no keys asked. Paid at mainnet launch.\n\n" +
		"Try it with my link: " + link + "\nMy Emissio code: " + code
	redTitle = "I'm testing Sequentia, a Bitcoin sidechain, and earning Sequence tokens (SEQ) for it"
	reddit = "Sequentia is a Bitcoin sidechain where any asset (tokenized shares, stablecoins, tokens anyone can issue) " +
		"lives on a Bitcoin wallet and swaps against BTC. Its testnet is open to everyone, and Emissio, the project's " +
		"rewards program, credits Sequence tokens (SEQ) for testing it: making a transaction, issuing an asset, " +
		"staking, running a node, reporting bugs.\n\n" +
		"Why this is not a scam or a chain letter:\n\n" +
		"- Nothing to buy and nothing to deposit. Testnet coins come from the faucet and have no value.\n" +
		"- Nobody asks for your seed phrase or private keys. Rewards are paid at mainnet launch to an address you generate yourself.\n" +
		"- Every task is checked against the public testnet chain, and the code of the network, the wallets and Emissio itself is open source: github.com/ConcatenaLabs\n" +
		fmt.Sprintf("- The referral bonus pays only once both people have earned %d SEQ each from real tasks, so fake accounts gain nothing.\n\n", referralThreshold) +
		"If you want to try it: " + link + "\n\n" +
		fmt.Sprintf("My Emissio code is %s. Join through the link and we each get %d SEQ once we have both earned %d.", code, referralBonus, referralThreshold)
	return x, redTitle, reddit
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
	v.XText, v.RedTitle, v.Reddit = shareTexts(user.ClaimCode, link)
	v.XURL = "https://x.com/intent/post?text=" + qesc(v.XText)
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
