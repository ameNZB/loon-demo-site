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

	// Plugins register themselves Caddy-style at init time. The loon-plugins
	// ones are named imports because the host injects their deps via SetDeps.
	"github.com/ameNZB/loon-plugins/backups"
	"github.com/ameNZB/loon-plugins/pluginapi"
	"github.com/ameNZB/loon-plugins/scraper"
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
	users := map[string]*core.User{
		"alice": {ID: 1, Username: "alice", Role: core.RoleAdmin, CreatedAt: time.Now()},
		"bob":   {ID: 2, Username: "bob", Role: core.RoleUser, CreatedAt: time.Now()},
	}
	wsrv := newWeb(users, sessionSecret, logger)
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
		Notifications: core.NewNotifications(core.NotificationsAdapter{
			NotifyFn: func(_ context.Context, userID int64, n core.Notification) error {
				logger.Info("notification", "to", userID, "kind", n.Kind, "title", n.Title, "body", n.Body)
				return nil
			},
		}),
		Points:     core.NewPoints(points.adapter()),
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
	if err := c.Register(catalog.RegistryExtension, catalog.NewRegistry()); err != nil {
		logger.Error("register catalog registry", "err", err)
		os.Exit(1)
	}
	scraper.SetDeps(scraper.Deps{Sink: catalogLogSink{log: logger}})
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

	// Wire the usenet plugin's capabilities into the pages — the plugin publishes
	// them on the extension registry during Provision; look them up now Boot ran.
	if v, ok := c.Lookup(pluginapi.UsenetIndexName); ok {
		wsrv.usenet, _ = v.(pluginapi.UsenetIndex)
	}
	if v, ok := c.Lookup(pluginapi.UsenetAdminName); ok {
		wsrv.usenetAdmin, _ = v.(pluginapi.UsenetAdmin)
	}
	admin.GET("/usenet", wsrv.adminUsenet)
	admin.POST("/usenet/server", wsrv.adminUsenetSaveServer)
	admin.POST("/usenet/test", wsrv.adminUsenetTest)
	admin.POST("/usenet/fetch-groups", wsrv.adminUsenetFetch)
	admin.POST("/usenet/group", wsrv.adminUsenetGroup)
	admin.POST("/usenet/crawl", wsrv.adminUsenetCrawl)

	// Live crawl + backfill status (coverage bars, backfill controls).
	admin.GET("/crawlers", wsrv.adminCrawlers)
	admin.POST("/crawlers/crawl", wsrv.adminCrawlersCrawl)
	admin.POST("/crawlers/backfill", wsrv.adminCrawlersBackfill)
	admin.POST("/crawlers/reset-backfill", wsrv.adminCrawlersReset)

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
