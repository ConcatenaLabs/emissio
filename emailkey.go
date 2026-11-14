package main

// One mailbox, one account. Most mail providers deliver name+anything@ to
// name@, and Gmail also ignores dots in the name, so a single mailbox can
// register as many "different" addresses as its owner likes. Each account
// therefore carries the key of its mailbox, and registration refuses a key
// that is already taken. Sign-in still uses the address as typed.

import (
	"database/sql"
	"log"
	"strings"
)

// gmailDomains are the domains whose mailboxes ignore dots in the name.
var gmailDomains = map[string]bool{"gmail.com": true, "googlemail.com": true}

// emailKey is the mailbox an address delivers to: lower case, without a
// +tag, and, for Gmail, without dots and under one domain.
func emailKey(email string) string {
	e := strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(e, "@")
	if at <= 0 {
		return e
	}
	local, domain := e[:at], e[at+1:]
	if i := strings.Index(local, "+"); i > 0 {
		local = local[:i]
	}
	if gmailDomains[domain] {
		local = strings.ReplaceAll(local, ".", "")
		domain = "gmail.com"
	}
	return local + "@" + domain
}

// backfillEmailKeys gives every account made before the key existed its key.
// Accounts that already share a mailbox keep existing; only new ones are
// refused.
func backfillEmailKeys(db *sql.DB) {
	rows, err := db.Query("SELECT id, email FROM users WHERE email_key = ''")
	if err != nil {
		log.Printf("email keys: %v", err)
		return
	}
	type row struct {
		id    int64
		email string
	}
	var todo []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.email) == nil {
			todo = append(todo, r)
		}
	}
	rows.Close()
	for _, r := range todo {
		db.Exec("UPDATE users SET email_key = ? WHERE id = ?", emailKey(r.email), r.id)
	}
}

// mailboxTaken says whether an account already uses the mailbox of email.
func mailboxTaken(db *sql.DB, email string) bool {
	var n int
	db.QueryRow("SELECT COUNT(*) FROM users WHERE email_key = ?", emailKey(email)).Scan(&n)
	return n > 0
}
