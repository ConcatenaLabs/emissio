package main

import (
	"bufio"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static
var staticFS embed.FS

type Config struct {
	Listen        string
	DBPath        string
	BasePath      string // e.g. "/emissio" when served under a path prefix; "" at root
	EsploraURL    string // e.g. "https://sequentiatestnet.com/explorer/api"
	SecureCookies bool
	RedditBase    string // overridable in tests
	TelegramBase  string
	XBase         string // the keyless syndication endpoint that renders embedded posts
	RedditOAuth   string // token endpoint base; the API host is derived from it in production
	RedditID      string // Reddit app client id; with the secret, enables the automatic Reddit check
	RedditSecret  string
	RedditToken   string // secret shared with the Reddit app that issues signed ownership tokens
	PolicyAsset   string // asset id of tSEQ on this chain, for the fee and stake checks
	NodeCLI       string // sequentia-cli path; with NodeDatadir, enables peer-based checks
	NodeDatadir   string
	TgBotToken    string // BotFather token; enables ID-based Telegram age checks
	TgBotName     string // bot username shown to users, without @

	// Task checks against the public services (checks.go).
	CompagesURL string   // the bridge's API base, e.g. https://sequentiatestnet.com/bridge/api
	PoolsURL    string   // the pool board's feed, e.g. https://sequentiatestnet.com/pools/pools.json
	SumsURL     string   // the download page's checksum list
	FaucetAddrs []string // addresses the faucet pays from

	// Emissio's own testnet wallet (counterparty.go), held by the node
	// NodeCLI reaches. Empty: the counterparty tasks are off.
	Wallet         string
	WalletAssets   []string // assets it may pay in; empty: any it holds
	WalletDailyCap int64

	// Mail (mail.go): an SMTP submission service. Without a host and a
	// sender, email verification and reset by email are off.
	SMTPHost  string
	SMTPPort  string
	SMTPUser  string
	SMTPPass  string
	MailFrom  string
	PublicURL string // the public address links in mail point at, e.g. https://sequentiatestnet.com/emissio

	// The payout (payout.go). PayoutURL is the payment service's base URL,
	// without /v1; empty turns payout runs off. PayoutNetwork is "testnet"
	// (the rehearsal: tSEQ to each account's testnet wallet key, no balance
	// moved) or "mainnet" (SEQ to each account's payout key, a collected
	// allocation debited). PayoutHRPs are the address prefixes a testnet
	// wallet key may be given as a taproot address in.
	PayoutURL         string
	PayoutNetwork     string
	PayoutAsset       string
	PayoutAtomsPerSEQ int64
	PayoutHRPs        []string
	PayoutPoll        time.Duration
	// PayoutLaunchKey has the account page ask a bc1q account for its
	// address's public key: needed only when the launch payout is paid into
	// wallets as claims, never for an on-chain payment. Off unless set.
	PayoutLaunchKey bool
}

func loadConfig() Config {
	cfg := Config{
		Listen:        envOr("EMISSIO_LISTEN", "127.0.0.1:8095"),
		DBPath:        envOr("EMISSIO_DB", "emissio.db"),
		BasePath:      strings.TrimRight(envOr("EMISSIO_BASEPATH", ""), "/"),
		EsploraURL:    envOr("EMISSIO_ESPLORA", ""),
		SecureCookies: envOr("EMISSIO_SECURE", "0") == "1",
		RedditBase:    envOr("EMISSIO_REDDIT", "https://www.reddit.com"),
		TelegramBase:  envOr("EMISSIO_TELEGRAM", "https://t.me"),
		XBase:         envOr("EMISSIO_X", "https://cdn.syndication.twimg.com"),
		RedditOAuth:   envOr("EMISSIO_REDDIT_OAUTH", "https://oauth.reddit.com"),
		RedditID:      envOr("EMISSIO_REDDIT_CLIENT_ID", ""),
		RedditSecret:  envOr("EMISSIO_REDDIT_CLIENT_SECRET", ""),
		RedditToken:   envOr("EMISSIO_REDDIT_TOKEN_SECRET", ""),
		PolicyAsset:   envOr("EMISSIO_POLICY_ASSET", "c8eccacf0953e1931cd31e434d8319101cc36e6c38b0e2104d8687552fae3e40"),
		NodeCLI:       envOr("EMISSIO_CLI", ""),
		NodeDatadir:   envOr("EMISSIO_DATADIR", ""),
		TgBotToken:    envOr("EMISSIO_TG_BOT_TOKEN", ""),
		TgBotName:     envOr("EMISSIO_TG_BOT_NAME", ""),
		CompagesURL:   envOr("EMISSIO_COMPAGES", ""),
		PoolsURL:      envOr("EMISSIO_POOLS", ""),
		SumsURL:       envOr("EMISSIO_SUMS", ""),
		FaucetAddrs:   strings.Split(envOr("EMISSIO_FAUCET_ADDRESSES", "tb1phruj5gk2nqrd4atn58rjr7msakm64fxsh5famwwerraz5wu29qnqkk43tk"), ","),
		Wallet:        envOr("EMISSIO_WALLET", ""),
		WalletAssets:  strings.Split(envOr("EMISSIO_WALLET_ASSETS", ""), ","),
		SMTPHost:      envOr("EMISSIO_SMTP_HOST", ""),
		SMTPPort:      envOr("EMISSIO_SMTP_PORT", "587"),
		SMTPUser:      envOr("EMISSIO_SMTP_USER", ""),
		SMTPPass:      envOr("EMISSIO_SMTP_PASS", ""),
		MailFrom:      envOr("EMISSIO_MAIL_FROM", ""),
		PublicURL:     envOr("EMISSIO_PUBLIC_URL", ""),
		PayoutURL:     envOr("EMISSIO_PAYOUT_URL", ""),
		PayoutNetwork: envOr("EMISSIO_PAYOUT_NETWORK", "testnet"),
		PayoutHRPs:    strings.Split(envOr("EMISSIO_PAYOUT_HRPS", "tb"), ","),
	}
	// The payout asset is tSEQ on the testnet unless named, and must be named
	// for any other payout: SEQ is not assumed anywhere else.
	cfg.PayoutAsset = envOr("EMISSIO_PAYOUT_ASSET", "")
	if cfg.PayoutAsset == "" && cfg.PayoutNetwork != "mainnet" {
		cfg.PayoutAsset = cfg.PolicyAsset
	}
	cfg.PayoutAtomsPerSEQ, _ = strconv.ParseInt(envOr("EMISSIO_PAYOUT_ATOMS_PER_SEQ", "100000000"), 10, 64)
	if cfg.PayoutAtomsPerSEQ <= 0 {
		log.Fatal("EMISSIO_PAYOUT_ATOMS_PER_SEQ must be positive")
	}
	if cfg.PayoutNetwork != "testnet" && cfg.PayoutNetwork != "mainnet" {
		log.Fatal("EMISSIO_PAYOUT_NETWORK is testnet or mainnet")
	}
	secs, _ := strconv.Atoi(envOr("EMISSIO_PAYOUT_POLL_SECS", "60"))
	if secs < 1 {
		secs = 60
	}
	cfg.PayoutPoll = time.Duration(secs) * time.Second
	cfg.PayoutLaunchKey = envOr("EMISSIO_PAYOUT_LAUNCH_KEY", "0") == "1"
	cfg.WalletDailyCap, _ = strconv.ParseInt(envOr("EMISSIO_WALLET_DAILY_CAP", "200"), 10, 64)
	return cfg
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type App struct {
	cfg       Config
	db        *sql.DB
	tpls      map[string]*template.Template
	authLimit *rateLimiter
	// The wallet problem-report intake: per address and per account code.
	intakeIPLimit   *slidingLimiter
	intakeCodeLimit *slidingLimiter
	mailer          Mailer // nil: this instance sends no mail
	mailLimit       *slidingLimiter
	mailIPLimit     *slidingLimiter
	faucetAddr      map[string]bool
}

func NewApp(cfg Config, db *sql.DB) *App {
	a := &App{
		cfg:             cfg,
		db:              db,
		authLimit:       newRateLimiter(12, time.Minute),
		intakeIPLimit:   newSlidingLimiter(limitRule{time.Hour, 5}, limitRule{24 * time.Hour, 20}),
		intakeCodeLimit: newSlidingLimiter(limitRule{24 * time.Hour, 10}),
		mailer:          newMailer(cfg),
		mailLimit:       newSlidingLimiter(limitRule{time.Hour, 3}, limitRule{24 * time.Hour, 10}),
		mailIPLimit:     newSlidingLimiter(limitRule{time.Hour, 10}, limitRule{24 * time.Hour, 40}),
	}
	a.faucetAddr = map[string]bool{}
	for _, s := range cfg.FaucetAddrs {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			a.faucetAddr[s] = true
		}
	}
	a.tpls = parseTemplates(cfg)
	return a
}

func parseTemplates(cfg Config) map[string]*template.Template {
	funcs := template.FuncMap{
		"base":        func() string { return cfg.BasePath },
		"seq":         formatSEQ,
		"usd":         formatUSD,
		"launchprice": func() string { return strconv.FormatFloat(launchPriceUSD, 'f', -1, 64) },
		"date": func(ts int64) string {
			if ts == 0 {
				return ""
			}
			return time.Unix(ts, 0).UTC().Format("2 Jan 2006")
		},
		"pgpfpr": func() string { return securityPGPFingerprint },
		"handlefmt": func(platform, handle string) string {
			if platform == "reddit" {
				return "u/" + handle
			}
			return "@" + handle
		},
		"encrypted": func(body string) bool {
			return strings.HasPrefix(strings.TrimSpace(body), "-----BEGIN PGP MESSAGE-----")
		},
		"isodate": func(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02") },
		"datetime": func(ts int64) string {
			if ts == 0 {
				return ""
			}
			return time.Unix(ts, 0).UTC().Format("2 Jan 2006 15:04 UTC")
		},
		"body":     renderBody,
		"prooftag": proofTag,
		"atoms":    atomsText,
		"prizes":   parsePrizes,
		"add":      func(a, b int) int { return a + b },
		"tmap": func(t any, csrf string) map[string]any {
			task, _ := t.(*Task)
			return map[string]any{"Task": task, "CSRF": csrf, "Checks": checkKinds, "Surfaces": surfaces}
		},
		"surfacename":  surfaceName,
		"surfacetitle": surfaceTitle,
		"amounttext":   amountText,
		"short": func(s string) string {
			if len(s) <= 16 {
				return s
			}
			return s[:8] + "..." + s[len(s)-6:]
		},
	}
	root := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(tplFS, "templates/layout.html"))
	pages, err := fs.Glob(tplFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	out := map[string]*template.Template{}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t := template.Must(root.Clone())
		out[name] = template.Must(t.ParseFS(tplFS, p))
	}
	return out
}

// formatSEQ renders a whole-SEQ amount with thousands separators.
func formatSEQ(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d", v)
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatUSD converts whole SEQ to USD at the launch price.
func formatUSD(seq int64) string {
	v := float64(seq) * launchPriceUSD
	if v == float64(int64(v)) {
		return formatSEQ(int64(v))
	}
	whole := int64(v)
	cents := int64((v-float64(whole))*100 + 0.5)
	if cents == 100 {
		whole++
		cents = 0
	}
	return fmt.Sprintf("%s.%02d", formatSEQ(whole), cents)
}

// renderBody turns stored plain text into safe HTML: paragraphs on blank
// lines, "- " lines as list items, "## " lines as subheadings.
func renderBody(s string) template.HTML {
	esc := template.HTMLEscapeString(s)
	blocks := strings.Split(strings.ReplaceAll(esc, "\r\n", "\n"), "\n\n")
	var b strings.Builder
	for _, blk := range blocks {
		blk = strings.TrimSpace(blk)
		if blk == "" {
			continue
		}
		lines := strings.Split(blk, "\n")
		if strings.HasPrefix(lines[0], "- ") {
			b.WriteString("<ul>")
			for _, ln := range lines {
				b.WriteString("<li>" + strings.TrimPrefix(strings.TrimSpace(ln), "- ") + "</li>")
			}
			b.WriteString("</ul>")
			continue
		}
		if strings.HasPrefix(blk, "## ") {
			b.WriteString("<h3>" + strings.TrimPrefix(blk, "## ") + "</h3>")
			continue
		}
		if len(lines) == 1 && !strings.Contains(blk, " ") {
			// A lone word/heading line, e.g. "Judging".
			b.WriteString("<h3>" + blk + "</h3>")
			continue
		}
		b.WriteString("<p>" + strings.Join(lines, "<br>") + "</p>")
	}
	return template.HTML(b.String())
}

func parsePrizes(s string) []int64 {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		var v int64
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &v); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	cfg := loadConfig()
	db := mustOpenDB(cfg.DBPath)
	defer db.Close()
	seedDB(db)

	if len(os.Args) > 1 {
		runCommand(db, os.Args[1:])
		return
	}

	app := NewApp(cfg, db)
	app.startPeerPoller()
	app.startPayoutFollower()
	log.Printf("emissio listening on %s (base path %q, db %s)", cfg.Listen, cfg.BasePath, cfg.DBPath)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func runCommand(db *sql.DB, args []string) {
	switch args[0] {
	case "createadmin":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: emissio createadmin <email>  (password read from stdin)")
			os.Exit(2)
		}
		email := strings.ToLower(strings.TrimSpace(args[1]))
		fmt.Fprintln(os.Stderr, "password:")
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			os.Exit(2)
		}
		pass := strings.TrimSpace(sc.Text())
		if len(pass) < 10 {
			fmt.Fprintln(os.Stderr, "password must be at least 10 characters")
			os.Exit(2)
		}
		if u, err := getUserByEmail(db, email); err == nil {
			if _, err := db.Exec("UPDATE users SET pass_hash = ?, is_admin = 1 WHERE id = ?", hashPassword(pass), u.ID); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("updated %s: admin, password reset\n", email)
			return
		}
		id, err := createUser(db, email, hashPassword(pass), randomHex(5), "")
		if err != nil {
			log.Fatal(err)
		}
		if _, err := db.Exec("UPDATE users SET is_admin = 1, email_verified_at = ? WHERE id = ?", now(), id); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("created admin %s (id %d)\n", email, id)
	case "reseed-tasks":
		// Refresh title, category and body of the seeded tasks from the
		// current seed copy, and insert any seeded task the database does
		// not have yet. Rewards, caps and active flags of existing tasks are
		// left alone so admin tuning survives.
		updated, inserted := 0, 0
		for _, t := range seedTasks {
			res, err := db.Exec("UPDATE tasks SET title = ?, category = ?, body = ?, surface = ?, check_kind = ?, needs_txid = ? WHERE slug = ?",
				t.title, t.category, t.body, t.surface, t.check, t.needsTxid, t.slug)
			if err != nil {
				log.Fatal(err)
			}
			if c, _ := res.RowsAffected(); c > 0 {
				updated++
				continue
			}
			if _, err := db.Exec(`INSERT INTO tasks (slug, title, category, body, surface, check_kind, reward, cap, needs_txid, active, sort)
				VALUES (?,?,?,?,?,?,?,?,?,1,?)`,
				t.slug, t.title, t.category, t.body, t.surface, t.check, t.reward, t.cap, t.needsTxid, t.sort); err != nil {
				log.Fatal(err)
			}
			inserted++
		}
		comps := 0
		for _, c := range seedComps {
			res, err := db.Exec("UPDATE competitions SET title = ?, body = ? WHERE slug = ?", c.title, c.body, c.slug)
			if err != nil {
				log.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				comps++
			}
		}
		retired := 0
		for _, slug := range retiredTasks {
			res, err := db.Exec("UPDATE tasks SET active = 0 WHERE slug = ? AND active = 1", slug)
			if err != nil {
				log.Fatal(err)
			}
			if c, _ := res.RowsAffected(); c > 0 {
				retired++
			}
		}
		fmt.Printf("updated copy of %d seeded tasks, inserted %d, retired %d; refreshed %d seeded competitions\n", updated, inserted, retired, comps)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		os.Exit(2)
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	mux.HandleFunc("GET /{$}", a.handleHome)
	mux.HandleFunc("GET /tasks", a.handleTasks)
	mux.HandleFunc("GET /tasks/{slug}", a.handleTask)
	mux.HandleFunc("POST /tasks/{slug}/submit", a.handleTaskSubmit)
	mux.HandleFunc("POST /tasks/{slug}/request", a.handlePaymentRequest)
	mux.HandleFunc("GET /competitions", a.handleCompetitions)
	mux.HandleFunc("GET /competitions/{slug}", a.handleCompetition)
	mux.HandleFunc("POST /competitions/{slug}/enter", a.handleCompetitionEnter)
	mux.HandleFunc("GET /security", a.handleSecurity)
	mux.HandleFunc("POST /security/report", a.handleSecurityReport)
	mux.HandleFunc("GET /bugs", a.handleBugs)
	mux.HandleFunc("POST /bugs", a.handleBugSubmit)
	mux.HandleFunc("GET /bugs/{id}/screenshot", a.handleBugFile)
	mux.HandleFunc("POST /api/problem-reports", a.handleIntake)
	mux.HandleFunc("OPTIONS /api/problem-reports", a.handleIntakeOptions)
	mux.HandleFunc("GET /guide", a.handleGuide)
	mux.HandleFunc("GET /rules", a.handleRules)
	mux.HandleFunc("GET /account", a.handleAccount)
	mux.HandleFunc("POST /account/address", a.handleAddress)
	mux.HandleFunc("POST /account/verify", a.handleVerify)
	mux.HandleFunc("GET /r/{code}", a.handleReferral)
	mux.HandleFunc("GET /register", a.handleRegisterForm)
	mux.HandleFunc("POST /register", a.handleRegister)
	mux.HandleFunc("GET /login", a.handleLoginForm)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)
	mux.HandleFunc("GET /verify-email", a.handleVerifyEmail)
	mux.HandleFunc("POST /account/verify-email", a.handleResendVerification)
	mux.HandleFunc("GET /reset", a.handleResetForm)
	mux.HandleFunc("POST /reset", a.handleResetRequest)
	mux.HandleFunc("GET /reset/confirm", a.handleResetConfirmForm)
	mux.HandleFunc("POST /reset/confirm", a.handleResetConfirm)

	mux.HandleFunc("GET /admin", a.admin(a.handleAdminHome))
	mux.HandleFunc("GET /admin/submissions", a.admin(a.handleAdminSubmissions))
	mux.HandleFunc("POST /admin/submissions/{id}/review", a.admin(a.handleAdminReview))
	mux.HandleFunc("GET /admin/tasks", a.admin(a.handleAdminTasks))
	mux.HandleFunc("POST /admin/tasks/save", a.admin(a.handleAdminTaskSave))
	mux.HandleFunc("GET /admin/competitions", a.admin(a.handleAdminCompetitions))
	mux.HandleFunc("POST /admin/competitions/save", a.admin(a.handleAdminCompSave))
	mux.HandleFunc("GET /admin/competitions/{slug}", a.admin(a.handleAdminCompetition))
	mux.HandleFunc("POST /admin/competitions/{slug}/award", a.admin(a.handleAdminAward))
	mux.HandleFunc("POST /admin/competitions/{slug}/status", a.admin(a.handleAdminCompStatus))
	mux.HandleFunc("GET /admin/reports", a.admin(a.handleAdminReports))
	mux.HandleFunc("GET /admin/reports/{id}", a.admin(a.handleAdminReport))
	mux.HandleFunc("POST /admin/reports/{id}/review", a.admin(a.handleAdminReportReview))
	mux.HandleFunc("GET /admin/bugs", a.admin(a.handleAdminBugs))
	mux.HandleFunc("GET /admin/bugs/{id}", a.admin(a.handleAdminBug))
	mux.HandleFunc("POST /admin/bugs/{id}/review", a.admin(a.handleAdminBugReview))
	mux.HandleFunc("GET /admin/verifications", a.admin(a.handleAdminVerifications))
	mux.HandleFunc("POST /admin/verifications/{id}/review", a.admin(a.handleAdminVerifyReview))
	mux.HandleFunc("GET /admin/users", a.admin(a.handleAdminUsers))
	mux.HandleFunc("POST /admin/adjust", a.admin(a.handleAdminAdjust))
	mux.HandleFunc("POST /admin/users/{id}/reset-link", a.admin(a.handleAdminResetLink))
	mux.HandleFunc("GET /admin/allocations.csv", a.admin(a.handleAdminExport))
	mux.HandleFunc("GET /admin/payouts", a.admin(a.handleAdminPayouts))
	mux.HandleFunc("POST /admin/payouts/new", a.admin(a.handleAdminPayoutNew))
	mux.HandleFunc("POST /admin/payouts/collection", a.admin(a.handleAdminPayoutCollection))
	mux.HandleFunc("GET /admin/payouts/{id}", a.admin(a.handleAdminPayout))
	mux.HandleFunc("GET /admin/payouts/{id}/list.txt", a.admin(a.handleAdminPayoutList))
	mux.HandleFunc("POST /admin/payouts/{id}/reference", a.admin(a.handleAdminPayoutReference))
	mux.HandleFunc("POST /account/payout-key", a.handlePayoutKey)
	mux.HandleFunc("POST /account/rehearsal-key", a.handleRehearsalKey)

	var h http.Handler = mux
	if base := a.cfg.BasePath; base != "" {
		// Links are generated with the prefix, but the reverse proxy may or
		// may not strip it before forwarding. Accept both.
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := strings.CutPrefix(r.URL.Path, base); ok {
				if p == "" {
					p = "/"
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = p
				mux.ServeHTTP(w, r2)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	return a.securityHeaders(h)
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
