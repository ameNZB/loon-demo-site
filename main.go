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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/ameNZB/loon/core"

	// Plugins register themselves Caddy-style at init time.
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

	// --- Demo users + auth. A real host wires its session
	// middleware here; the demo authenticates via the X-Demo-User
	// header so curl can exercise the role gates.
	users := map[string]*core.User{
		"alice": {ID: 1, Username: "alice", Role: core.RoleAdmin, CreatedAt: time.Now()},
		"bob":   {ID: 2, Username: "bob", Role: core.RoleUser, CreatedAt: time.Now()},
	}
	currentUser := func(c *gin.Context) (*core.User, bool) {
		u, ok := users[c.GetHeader("X-Demo-User")]
		return u, ok
	}
	requireAtLeast := func(min core.Role) gin.HandlersChain {
		return gin.HandlersChain{func(c *gin.Context) {
			u, ok := currentUser(c)
			if !ok {
				c.AbortWithStatusJSON(http.StatusUnauthorized,
					gin.H{"ok": false, "error": "set the X-Demo-User header to alice or bob"})
				return
			}
			if u.Role < min {
				c.AbortWithStatusJSON(http.StatusForbidden,
					gin.H{"ok": false, "error": "insufficient role"})
				return
			}
			c.Next()
		}}
	}
	auth := core.NewAuth(core.AuthAdapter{
		OptionalFn:     func() gin.HandlersChain { return gin.HandlersChain{} },
		AuthenticateFn: func() gin.HandlersChain { return gin.HandlersChain{} }, // public mode
		RequireUserFn:  requireAtLeast,
		RequireRoleFn: func(role core.Role) gin.HandlersChain {
			return gin.HandlersChain{func(c *gin.Context) {
				if u, ok := currentUser(c); !ok || u.Role != role {
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "wrong role"})
					return
				}
				c.Next()
			}}
		},
		CurrentUserFn: currentUser,
	})

	byID := func(id int64) *core.User {
		for _, u := range users {
			if u.ID == id {
				return u
			}
		}
		return nil
	}
	usersSvc := core.NewUsers(core.UsersAdapter{
		GetByIDFn: func(_ context.Context, id int64) (*core.User, error) { return byID(id), nil },
		GetByUsernameFn: func(_ context.Context, name string) (*core.User, error) {
			return users[name], nil
		},
		DisplayNameFn: func(_ context.Context, id int64) (string, error) {
			if u := byID(id); u != nil {
				return u.Username, nil
			}
			return "", nil
		},
		BulkDisplayNamesFn: func(_ context.Context, ids []int64) (map[int64]string, error) {
			out := make(map[int64]string, len(ids))
			for _, id := range ids {
				if u := byID(id); u != nil {
					out[id] = u.Username
				}
			}
			return out, nil
		},
	})

	// --- In-memory points ledger. A real host writes the ledger
	// row + balance update atomically; the demo keeps a map.
	points := &demoPoints{balances: map[int64]int{}}

	c, err := core.New(core.Deps{
		Process:   "all",
		Users:     usersSvc,
		Auth:      auth,
		RBAC:      core.NewRBAC(),
		Storage:   core.NewStorage(db),
		Scheduler: core.NewScheduler(demoScheduler(logger)),
		Router: core.NewRouter(core.RouterAdapter{
			Engine:          engine,
			AdminMiddleware: requireAtLeast(core.RoleAdmin),
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := core.Boot(ctx, c)
	if err != nil {
		logger.Error("core.Boot", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{Addr: ":8090", Handler: engine}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", "err", err)
			stop()
		}
	}()
	logger.Info("loon demo site up", "addr", "http://localhost:8090/plugin/guestbook")

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

// demoScheduler is the minimal SchedulerService backing: jobs log
// through slog, and RunLoop is a plain ticker honouring the boot
// delay and root-context cancellation. A real host wires its job
// registry (admin visibility, off-peak gating, manual triggers).
func demoScheduler(logger *slog.Logger) core.SchedulerAdapter {
	return core.SchedulerAdapter{
		RegisterJobFn: func(name, desc string) core.Job {
			return &demoJob{logger: logger.With("job", name)}
		},
		RunLoopFn: func(ctx context.Context, job core.Job, bootDelay, interval time.Duration, runFn func(context.Context)) {
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(bootDelay):
				}
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for {
					job.SetRunning()
					runFn(ctx)
					job.SetIdle(time.Now().Add(interval))
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
		},
	}
}

type demoJob struct{ logger *slog.Logger }

func (j *demoJob) SetRunning()          {}
func (j *demoJob) SetIdle(time.Time)    {}
func (j *demoJob) SetError(msg string)  { j.logger.Error(msg) }
func (j *demoJob) Log(f string, a ...any) { j.logger.Info(fmt.Sprintf(f, a...)) }
func (j *demoJob) MarkOffPeak() core.Job { return j }
func (j *demoJob) SetTrigger(func())     {}
