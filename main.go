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
}

func loadConfig() Config {
	cfg := Config{
		Listen:        envOr("EMISSIO_LISTEN", "127.0.0.1:8095"),
		DBPath:        envOr("EMISSIO_DB", "emissio.db"),
		BasePath:      strings.TrimRight(envOr("EMISSIO_BASEPATH", ""), "/"),
		EsploraURL:    envOr("EMISSIO_ESPLORA", ""),
		SecureCookies: envOr("EMISSIO_SECURE", "0") == "1",
	}
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
}

func NewApp(cfg Config, db *sql.DB) *App {
	a := &App{
		cfg:       cfg,
		db:        db,
		authLimit: newRateLimiter(12, time.Minute),
	}
	a.tpls = parseTemplates(cfg)
	return a
}

func parseTemplates(cfg Config) map[string]*template.Template {
	funcs := template.FuncMap{
		"base": func() string { return cfg.BasePath },
		"seq":  formatSEQ,
		"usd":  formatUSD,
		"date": func(ts int64) string {
			if ts == 0 {
				return ""
			}
			return time.Unix(ts, 0).UTC().Format("2 Jan 2006")
		},
		"datetime": func(ts int64) string {
			if ts == 0 {
				return ""
			}
			return time.Unix(ts, 0).UTC().Format("2 Jan 2006 15:04 UTC")
		},
		"body":   renderBody,
		"prizes": parsePrizes,
		"add":    func(a, b int) int { return a + b },
		"tmap": func(t any, csrf string) map[string]any {
			task, _ := t.(*Task)
			return map[string]any{"Task": task, "CSRF": csrf}
		},
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
		id, err := createUser(db, email, hashPassword(pass), randomHex(5))
		if err != nil {
			log.Fatal(err)
		}
		if _, err := db.Exec("UPDATE users SET is_admin = 1 WHERE id = ?", id); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("created admin %s (id %d)\n", email, id)
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
	mux.HandleFunc("GET /competitions", a.handleCompetitions)
	mux.HandleFunc("GET /competitions/{slug}", a.handleCompetition)
	mux.HandleFunc("POST /competitions/{slug}/enter", a.handleCompetitionEnter)
	mux.HandleFunc("GET /security", a.handleSecurity)
	mux.HandleFunc("POST /security/report", a.handleSecurityReport)
	mux.HandleFunc("GET /guide", a.handleGuide)
	mux.HandleFunc("GET /rules", a.handleRules)
	mux.HandleFunc("GET /account", a.handleAccount)
	mux.HandleFunc("POST /account/address", a.handleAddress)
	mux.HandleFunc("GET /register", a.handleRegisterForm)
	mux.HandleFunc("POST /register", a.handleRegister)
	mux.HandleFunc("GET /login", a.handleLoginForm)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)

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
	mux.HandleFunc("GET /admin/users", a.admin(a.handleAdminUsers))
	mux.HandleFunc("POST /admin/adjust", a.admin(a.handleAdminAdjust))
	mux.HandleFunc("GET /admin/allocations.csv", a.admin(a.handleAdminExport))

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
