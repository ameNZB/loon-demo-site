// loon-demo-site is the smallest useful host for the loon
// framework: it wires every core.Deps seam with an in-memory or
// logging implementation, boots the plugin runtime against a real
// Postgres, and serves one demo plugin (guestbook).
//
// Everything in this file is the HOST side of the contract — the
// part a real site implements over its own sessions, job registry,
// and ledger. The plugin side lives in plugins/guestbook.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/ameNZB/loon/catalog"
	"github.com/ameNZB/loon/core"
	"github.com/ameNZB/loon/schedule"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ameNZB/loon-baseline/account"
	"github.com/ameNZB/loon-baseline/adminusers"
	"github.com/ameNZB/loon-baseline/apikey"
	"github.com/ameNZB/loon-baseline/authtoken"
	cachememory "github.com/ameNZB/loon-baseline/cache/memory"
	cacheredis "github.com/ameNZB/loon-baseline/cache/redis"
	"github.com/ameNZB/loon-baseline/captcha"
	"github.com/ameNZB/loon-baseline/jobsettings"
	"github.com/ameNZB/loon-baseline/loginlog"
	"github.com/ameNZB/loon-baseline/notify"
	"github.com/ameNZB/loon-baseline/profile"
	"github.com/ameNZB/loon-baseline/password"
	"github.com/ameNZB/loon-baseline/users"

	// Plugins register themselves Caddy-style at init time. The loon-plugins
	// ones are named imports because the host injects their deps via SetDeps.
	"github.com/ameNZB/loon-plugins/backups"
	_ "github.com/ameNZB/loon-plugins/catalog"
	_ "github.com/ameNZB/loon-plugins/dailyreward"
	"github.com/ameNZB/loon-plugins/pluginapi"
	_ "github.com/ameNZB/loon-plugins/pointstore"
	"github.com/ameNZB/loon-plugins/scraper"
	"github.com/ameNZB/loon-plugins/scraper/sources/anidb"
	"github.com/ameNZB/loon-plugins/scraper/sources/theporndb"
	"github.com/ameNZB/loon-plugins/stats"
	_ "github.com/ameNZB/loon-plugins/usenet"

	_ "github.com/ameNZB/loon-demo-site/plugins/guestbook"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	dsn := os.Getenv("LOON_DEMO_DSN")
	if dsn == "" {
		dsn = "postgres://demo:demo@localhost:5544/loon_demo?sslmode=disable"
	}
	db, err := connect(dsn)
	if err != nil {
		logger.Error("database unreachable — run `docker compose up -d db` first", "err", err)
		os.Exit(1)
	}

	engine := gin.Default()

	// --- Demo users + username/password login. A real host wires its session
	// store + users table here; the demo keeps two in-memory users whose
	// password (bcrypt-verified) equals their username, and signs an HMAC
	// session cookie on login. The web struct (views.go) owns the templates,
	// static assets, session cookie, and the public/login pages.
	sessionSecret := []byte(getenvDefault("LOON_DEMO_SESSION_SECRET", "dev-insecure-demo-secret-change-me"))
	// User store: loon-baseline's Postgres reference impl (a real host implements
	// users.Store over its own table). Migrate the reference table + seed the two
	// demo accounts (password == username).
	userStore := users.NewPGStore(db.DB)
	if err := userStore.Migrate(context.Background()); err != nil {
		logger.Error("users migrate", "err", err)
		os.Exit(1)
	}
	// Login-attempt audit (loon-baseline): the host records each attempt at its
	// login handler; the store + views live in the baseline.
	loginLog := loginlog.NewPGStore(db.DB)
	if err := loginLog.Migrate(context.Background()); err != nil {
		logger.Error("loginlog migrate", "err", err)
		os.Exit(1)
	}
	// Password-reset + email-verification token store (loon-baseline).
	tokenStore := authtoken.NewPGStore(db.DB)
	if err := tokenStore.Migrate(context.Background()); err != nil {
		logger.Error("authtoken migrate", "err", err)
		os.Exit(1)
	}
	// Admin-editable job/service settings (loon-baseline). This is the
	// persistence behind loon's schedule config vars. We register the "Search
	// API" read tier as a REMOTE service: its run loop lives in loon-api (a
	// separate process against this same DB), but declaring it here — with
	// MarkRemote — surfaces its cache-TTL settings on this web admin's config
	// page. Edit here; loon-api reads the same job_settings rows. That's the
	// cross-process settings path from LOON-DISTRIBUTED.
	jobSettings := jobsettings.NewPGStore(db.DB)
	if err := jobSettings.Migrate(context.Background()); err != nil {
		logger.Error("jobsettings migrate", "err", err)
		os.Exit(1)
	}
	// Newznab API keys (loon-baseline): one per user, shown + regenerated on the
	// self-service /p/api-key page. loon-api (against this same DB) validates the
	// ?apikey= a client sends against this table.
	apiKeys := apikey.NewPGStore(db.DB)
	if err := apiKeys.Migrate(context.Background()); err != nil {
		logger.Error("apikey migrate", "err", err)
		os.Exit(1)
	}

	apiSvc := schedule.RegisterService("Search API", "Newznab/Torznab read tier (runs in loon-api)")
	apiSvc.DeclareConfig(jobSettings,
		schedule.JobConfigVar{Key: "cache_ttl_secs", Label: "Search cache TTL (seconds)", Type: schedule.JobConfigInt, Default: "90",
			Description: "How long search/tvsearch/movie/rss responses stay cached in the API tier."},
		schedule.JobConfigVar{Key: "caps_ttl_secs", Label: "Caps cache TTL (seconds)", Type: schedule.JobConfigInt, Default: "3600",
			Description: "How long the caps (category tree) response stays cached — nearly static."},
		schedule.JobConfigVar{Key: "rate_per_min", Label: "Requests per minute", Type: schedule.JobConfigInt, Default: "60",
			Description: "Per-API-key (or IP) request cap per minute in the API tier — burst protection. 0 disables."},
		schedule.JobConfigVar{Key: "rate_per_day", Label: "Requests per day", Type: schedule.JobConfigInt, Default: "10000",
			Description: "Per-API-key (or IP) request cap per day in the API tier — the daily quota. 0 disables."},
		schedule.JobConfigVar{Key: "rate_contributor_mult", Label: "Contributor limit multiplier", Type: schedule.JobConfigInt, Default: "3",
			Description: "Contributors get this multiple of the base API limits; mods/admins are exempt entirely."},
	)
	apiSvc.MarkRemote() // its loop lives in loon-api; here it's a config stub
	seedDemoUsers(userStore, logger)
	wsrv := newWeb(userStore, sessionSecret, logger)
	wsrv.loginLog = loginLog
	wsrv.ipSalt = string(sessionSecret) // demo salt; a real host uses a dedicated ip_salt secret
	// Cloudflare Turnstile hook (loon-baseline). Disabled unless both keys are
	// set, so the demo runs without CF; set TURNSTILE_SITEKEY + TURNSTILE_SECRET
	// (or the CF test keys) to see it gate login + register.
	wsrv.captcha = captcha.New(captcha.Config{
		SiteKey: os.Getenv("TURNSTILE_SITEKEY"),
		Secret:  os.Getenv("TURNSTILE_SECRET"),
	})
	// Page cache (loon-baseline). In-memory by default so the demo needs no
	// Redis; set REDIS_ADDR to use the shared redis impl instead — no call site
	// changes, just the backend.
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		wsrv.cache = cacheredis.New(goredis.NewClient(&goredis.Options{Addr: addr}))
		logger.Info("cache backend", "kind", "redis", "addr", addr)
	} else {
		wsrv.cache = cachememory.New()
		logger.Info("cache backend", "kind", "memory")
	}
	// Reset/verify flow. The demo "mailer" just logs the message (link included)
	// so you can follow it in the logs; a real host sends via SMTP.
	wsrv.resetFlow = authtoken.Flow{
		Tokens: tokenStore, Users: userStore, Hasher: password.Hasher{},
		BaseURL: getenvDefault("LOON_DEMO_BASE_URL", "http://localhost:8090"),
		Mail: func(to, subject, body string) error {
			logger.Info("email (demo mailer)", "to", to, "subject", subject, "body", body)
			return nil
		},
	}
	// gin-contrib session middleware (the prod scheme) must be installed before
	// any route that logs in or reads the user.
	engine.Use(wsrv.auth.Session.Middleware())
	wsrv.mount(engine)

	// Hand loon the session policy through the baseline's core.Auth adapter.
	auth := wsrv.auth.CoreAuth()
	usersSvc := core.NewUsers(wsrv.usersAdapter())

	// --- In-memory points ledger. A real host writes the ledger
	// row + balance update atomically; the demo keeps a map.
	points := &demoPoints{balances: map[int64]int{}}
	pointsSvc := core.NewPoints(points.adapter())
	wsrv.points = pointsSvc // navbar balance readout

	// Notification fan-out (loon-baseline): core's single NotifyFn becomes a HOOK
	// point — every registered Sink gets each notification (the bell/inbox store,
	// a logger, and any channel a plugin adds by looking up the fanout capability).
	inbox := notify.NewPGStore(db.DB)
	if err := inbox.Migrate(context.Background()); err != nil {
		logger.Error("notify migrate", "err", err)
		os.Exit(1)
	}
	notifications := notify.NewFanout(
		notify.InboxSink(inbox),
		notify.LogSink(func(userID int64, n core.Notification) {
			logger.Info("notification", "to", userID, "kind", n.Kind, "title", n.Title)
		}),
	)
	wsrv.inbox = inbox // navbar unread-count bell

	// The scheduler is loon's batteries-included one: jobs land in
	// schedule.Default (a host admin page would render its
	// GetAllSnapshots), and LogSink mirrors job log lines to stdout
	// so the demo's once-a-minute stats job stays visible.
	schedule.LogSink = func(jobName, line string) {
		logger.Info("job", "name", jobName, "line", line)
	}

	c, err := core.New(core.Deps{
		Process:   "all",
		Users:     usersSvc,
		Auth:      auth,
		RBAC:      core.NewRBAC(),
		Storage:   core.NewStorage(db),
		Scheduler: schedule.CoreScheduler(schedule.Default),
		Router: core.NewRouter(core.RouterAdapter{
			Engine:          engine,
			AdminMiddleware: wsrv.auth.Require(core.RoleAdmin),
		}),
		Logger: logger,
		Config: core.NewConfig(map[string]any{
			"guestbook": map[string]any{"points_per_entry": 5},
		}),
		Notifications: core.NewNotifications(core.NotificationsAdapter{NotifyFn: notifications.Deliver}),
		Points:     pointsSvc,
		HTTPClient: core.NewHTTPClient(),
		Errors:     core.NewErrorReporter(core.ErrorAdapter{}), // stderr fallback
	})
	if err != nil {
		logger.Error("core.New", "err", err)
		os.Exit(1)
	}

	// --- loon-plugins wiring (all worker plugins; they boot under Process
	// "all"). The scraper needs the shared catalog.Registry on the extension
	// registry — empty here until a source module lands — plus a write sink;
	// backups needs a place to put entries; stats needs a cache. The demo
	// impls just log (or write to a temp dir), the way a real host would swap
	// in its catalog_entry table / archive store / Redis cache.
	// The shared catalog.Registry + its metadata sources. Sources are idle until
	// their key/client is set via env (hook up now, test later):
	//   TPDB_API_KEY → ThePornDB (xxx) · ANIDB_CLIENT → AniDB (anime)
	reg := catalog.NewRegistry()
	if src := theporndb.New(os.Getenv("TPDB_API_KEY"), ""); src != nil {
		_ = reg.RegisterSource(src)
	}
	_ = reg.RegisterSource(anidb.New(os.Getenv("ANIDB_CLIENT"), nil))
	for _, s := range reg.Sources() {
		logger.Info("catalog source registered", "domain", s.Domain().Key, "priority", s.Domain().Priority)
	}
	if err := c.Register(catalog.RegistryExtension, reg); err != nil {
		logger.Error("register catalog registry", "err", err)
		os.Exit(1)
	}
	// Publish the Turnstile verifier as a cross-cutting capability so plugins
	// (e.g. the dailyreward claim button) can require a captcha without importing
	// loon-baseline. Registered before Boot so plugin Provision can Lookup it; a
	// disabled verifier means plugins gate nothing (graceful).
	if err := c.Register("captcha", wsrv.captcha); err != nil {
		logger.Error("register captcha capability", "err", err)
	}
	// Publish the notification fan-out so a plugin can Add its own delivery
	// channel (Lookup "notify.fanout" + Add a sink) during Provision.
	if err := c.Register("notify.fanout", notifications); err != nil {
		logger.Error("register notify.fanout capability", "err", err)
	}
	// Scraper enrichment: persist entries + link covers via the catalog plugin
	// (resolved lazily after Boot), fed release candidates from the usenet index.
	scraper.SetDeps(scraper.Deps{
		Sink:       lazySink{w: wsrv},
		Candidates: wsrv.catalogCandidates,
		Link:       wsrv.linkCover,
	})
	stats.SetDeps(stats.Deps{Cache: func(_ context.Context, s []pluginapi.Stat) error {
		logger.Info("stats snapshot cached", "metrics", len(s))
		return nil
	}})
	backups.SetDeps(backups.Deps{OpenEntry: demoBackupOpener(logger)})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := core.Boot(ctx, c)
	if err != nil {
		logger.Error("core.Boot", "err", err)
		os.Exit(1)
	}

	// --- Admin dashboard. core.AdminHandler renders the plugin manifest;
	// schedule.JobsAdminHandler renders the jobs/services table with manual
	// run/pause controls. Both sit behind the same admin role gate the plugins
	// use — log in as an admin (alice) in the browser to reach them.
	// The demo renders its admin pages (plugins/jobs/usenet) in its own layout
	// for a consistent look, using loon's data (rt.Plugins, schedule snapshots).
	wsrv.rt = rt
	admin := engine.Group("/admin", wsrv.auth.Require(core.RoleAdmin)...)
	admin.GET("/plugins", wsrv.adminPlugins)
	admin.GET("/jobs", wsrv.adminJobs)
	admin.POST("/jobs/control", wsrv.adminJobsControl)
	// Per-job/-service settings — loon's bundled config form (self-contained
	// page). The demo's jobs table links here via a Config button for any job
	// that declares settings (HasConfig).
	admin.GET("/jobs/config", schedule.JobConfigHandler(nil))
	admin.POST("/jobs/config", schedule.JobConfigSaveHandler(nil))

	// Wire the usenet plugin's READ capability into the public pages — the
	// plugin publishes it on the extension registry during Provision.
	if v, ok := c.Lookup(pluginapi.UsenetIndexName); ok {
		wsrv.usenet, _ = v.(pluginapi.UsenetIndex)
	}
	if v, ok := c.Lookup(pluginapi.UsenetNewznabName); ok {
		wsrv.usenetAPI, _ = v.(pluginapi.UsenetNewznab)
	}
	// Catalog plugin: its service also implements the sink + cover store the
	// scraper writes to and the release page reads.
	if v, ok := c.Lookup(pluginapi.CatalogName); ok {
		if cat, ok := v.(pluginapi.Catalog); ok {
			wsrv.catalog = cat // taxonomy + names for the /browse page
			wsrv.catalogSink, _ = cat.(pluginapi.CatalogSink)
			wsrv.catalogCovers, _ = cat.(pluginapi.CatalogCovers)
		}
	}
	// Newznab / Torznab API (Sonarr/Radarr/Prowlarr consume these).
	engine.GET("/api", wsrv.newznabAPI)
	engine.GET("/rss", wsrv.newznabAPI)

	// loon-baseline's batteries-included admin views (user management) plug
	// into the SAME view system the plugins use — the host just registers
	// them on the Core after Boot and wireViews mounts them at /admin/p/users.
	// This is the reusable admin chrome a real host adopts instead of
	// hand-rolling a users page.
	if bviews, err := adminusers.Views(userStore, password.Hasher{}); err != nil {
		logger.Error("adminusers.Views", "err", err)
	} else {
		for _, v := range bviews {
			if err := c.RegisterView(v); err != nil {
				logger.Error("register admin view", "slug", v.Slug, "err", err)
			}
		}
	}
	// loon-baseline self-service account page (profile + change password) —
	// same view-system path, mounted at /p/account for any logged-in user.
	// Closes the loop on authflow.ChangePassword (logic existed; this is its UI).
	if aviews, err := account.Views(wsrv.flow, wsrv.currentUser); err != nil {
		logger.Error("account.Views", "err", err)
	} else {
		for _, v := range aviews {
			if err := c.RegisterView(v); err != nil {
				logger.Error("register account view", "slug", v.Slug, "err", err)
			}
		}
	}
	// loon-baseline self-service API key page: /p/api-key shows the user's
	// Newznab key (created on first visit) + a Regenerate button. loon-api
	// validates the key against the same table.
	if kviews, err := apikey.Views(apiKeys, wsrv.currentUser); err != nil {
		logger.Error("apikey.Views", "err", err)
	} else {
		for _, v := range kviews {
			if err := c.RegisterView(v); err != nil {
				logger.Error("register apikey view", "slug", v.Slug, "err", err)
			}
		}
	}
	// loon-baseline login audit views: /admin/p/login-log (all attempts) and
	// /p/sign-ins (the current user's own history).
	if lviews, err := loginlog.Views(loginLog, wsrv.currentUser); err != nil {
		logger.Error("loginlog.Views", "err", err)
	} else {
		for _, v := range lviews {
			if err := c.RegisterView(v); err != nil {
				logger.Error("register loginlog view", "slug", v.Slug, "err", err)
			}
		}
	}
	// loon-baseline profile summary (SlotUserWidget on /u/<name>). Resolves the
	// profile subject by id off the user store.
	if pviews, err := profile.Views(func(ctx context.Context, id int64) (*core.User, bool) {
		u, err := userStore.ByID(ctx, id)
		if err != nil {
			return nil, false
		}
		return u.ToCore(), true
	}); err != nil {
		logger.Error("profile.Views", "err", err)
	} else {
		for _, v := range pviews {
			if err := c.RegisterView(v); err != nil {
				logger.Error("register profile view", "slug", v.Slug, "err", err)
			}
		}
	}
	// Notification inbox page (/p/inbox). The navbar bell reads UnreadCount.
	if nviews, err := notify.InboxViews(inbox, wsrv.currentUser); err != nil {
		logger.Error("notify.InboxViews", "err", err)
	} else {
		for _, v := range nviews {
			if err := c.RegisterView(v); err != nil {
				logger.Error("register inbox view", "slug", v.Slug, "err", err)
			}
		}
	}

	// Plugin views (loon's view system): plugins render their settings
	// sections, admin/status pages, public pages, and widgets as fragments;
	// the demo mounts every slot generically and wraps the fragments in its
	// layout. Zero plugin-specific UI code host-side.
	wsrv.wireViews(c, engine, admin)

	srv := &http.Server{Addr: ":8090", Handler: engine}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", "err", err)
			stop()
		}
	}()
	logger.Info("loon demo site up",
		"url", "http://localhost:8090/",
		"login", "alice/alice (admin) or bob/bob")

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	rt.Stop(shutCtx)
}

// seedDemoUsers creates the two demo accounts (password == username) directly
// via the store — bypassing the register flow's password-strength rule, since
// seeding is a privileged setup step, not a user signup. Login still exercises
// the real store; new signups still get the 8-char minimum.
func seedDemoUsers(store users.Store, log *slog.Logger) {
	hasher := password.Hasher{}
	for _, s := range []struct {
		name string
		role core.Role
	}{{"alice", core.RoleAdmin}, {"bob", core.RoleUser}} {
		if _, err := store.ByUsername(context.Background(), s.name); err == nil {
			continue // already seeded
		} else if !errors.Is(err, users.ErrNotFound) {
			log.Error("seed lookup", "user", s.name, "err", err)
			continue
		}
		hash, err := hasher.Hash(s.name) // password == username
		if err != nil {
			log.Error("seed hash", "user", s.name, "err", err)
			continue
		}
		if _, err := store.Create(context.Background(), &users.User{Username: s.name, PasswordHash: hash, Role: s.role}); err != nil {
			log.Error("seed create", "user", s.name, "err", err)
		}
	}
}

func connect(dsn string) (*sqlx.DB, error) {
	var err error
	for i := 0; i < 10; i++ {
		var db *sqlx.DB
		if db, err = sqlx.Connect("postgres", dsn); err == nil {
			return db, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("after 10 attempts: %w", err)
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// demoPoints is the in-memory PointsService backing. Deduct
// enforces the non-negative-balance rule with the framework's
// typed sentinel so plugins can errors.Is against it.
type demoPoints struct {
	mu       sync.Mutex
	balances map[int64]int
}

func (p *demoPoints) adapter() core.PointsAdapter {
	change := func(userID int64, delta int) (int, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.balances[userID]+delta < 0 {
			return p.balances[userID], core.ErrInsufficientPoints
		}
		p.balances[userID] += delta
		return p.balances[userID], nil
	}
	return core.PointsAdapter{
		BalanceFn: func(_ context.Context, userID int64) (int, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.balances[userID], nil
		},
		AwardFn: func(_ context.Context, userID int64, n int, _, _ string, _ int64) (int, error) {
			return change(userID, n)
		},
		DeductFn: func(_ context.Context, userID int64, n int, _, _ string, _ int64) (int, error) {
			return change(userID, -n)
		},
		RefundFn: func(_ context.Context, userID int64, n int, _, _ string, _ int64) (int, error) {
			return change(userID, n)
		},
	}
}

// catalogLogSink is the demo's pluginapi.CatalogSink: a real host writes each
// scraped entry into its unified catalog_entry table; the demo just logs. It's
// never called until a MetadataSource is registered (Phase 3), but the scraper
// plugin still boots and appears on the jobs page with it wired.
type catalogLogSink struct{ log *slog.Logger }

func (s catalogLogSink) Upsert(_ context.Context, e catalog.CatalogEntry) error {
	s.log.Info("catalog upsert", "kind", e.Ref.Kind, "id", e.Ref.ID, "title", e.Title)
	return nil
}

// demoBackupOpener returns the backups plugin's OpenEntry seam, writing each
// backup entry to a temp dir. A real host would stream into a tar/dated dir or
// an object store.
func demoBackupOpener(log *slog.Logger) func(context.Context, string) (io.WriteCloser, error) {
	dir := filepath.Join(os.TempDir(), "loon-demo-backups")
	return func(_ context.Context, name string) (io.WriteCloser, error) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		log.Info("backup entry", "path", filepath.Join(dir, name))
		return os.Create(filepath.Join(dir, name))
	}
}
