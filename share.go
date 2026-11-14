package main

// The invite kit: the account's own code and referral link, ready for X,
// Reddit and Telegram, so passing Emissio on needs no copying of codes from
// the account page.
//
// On X the kit writes nothing but the link and the code: the tester writes
// the rest. X flags posts from many accounts that look alike as coordinated
// engagement, so a shared ready-made text would put every tester's account,
// and Sequentia's, at risk. Any post by the tester that contains the code
// verifies their X account; the check looks for the code and nothing else.
//
// With EMISSIO_X_QUOTE_POST set to the project's official post, the kit also
// offers to quote it, with the same link and code above it: the official
// post introduces Sequentia and Emissio, and a plain repost would carry no
// link, so it would bring the tester no referrals.
//
// On Reddit the kit writes the link alone, with no title: Reddit removes a
// text repeated across accounts and communities as spam and filters links by
// domain, so one ready-made post from every tester would put the domain the
// whole testnet lives on at risk. The code has no use there: Reddit keeps a
// link whole, the link carries the referral by itself, and a Reddit account
// is verified from the account page, not by a post. What a friend needs to
// know before joining is on the page the link opens (registerData).

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

type shareView struct {
	Code     string
	Link     string
	XText    string // only the link and the code: the tester writes the rest
	XURL     string
	QuoteURL string // empty unless the official post is configured
	Official string // the official post itself, to open and read
	RedURL   string // a text post with the link alone, no title
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

// shareTexts gives the X text (link and code alone, for a post or a quote),
// the Reddit text (the link alone) and the Telegram line, which goes under
// the link in a chat the tester chooses.
func shareTexts(code, link string) (x, reddit, tg string) {
	// The blank lines leave room on top for the tester's own words.
	x = "\n\n" + link + "\n" + code
	reddit = "\n\n" + link
	tg = "I'm testing Sequentia, a Bitcoin sidechain for tokenized assets, and Emissio credits every task on its testnet."
	return x, reddit, tg
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
	x, reddit, tg := shareTexts(user.ClaimCode, link)
	v.XText = x
	v.XURL = "https://x.com/intent/post?text=" + qesc(v.XText)
	if a.cfg.XQuotePost != "" {
		// X attaches a post given as url as a quote of it.
		v.QuoteURL = "https://x.com/intent/post?text=" + qesc(v.XText) + "&url=" + qesc(a.cfg.XQuotePost)
		v.Official = a.cfg.XQuotePost
	}
	v.RedURL = "https://www.reddit.com/submit?type=TEXT&text=" + qesc(reddit)
	v.TGURL = "https://t.me/share/url?url=" + qesc(link) + "&text=" + qesc(tg)
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
