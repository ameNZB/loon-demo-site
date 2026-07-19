// Package guestbook is the demo plugin: the smallest surface that
// still exercises every major framework seam — its own schema via
// declarative migrations, SchemaDB storage, mounted routes, auth
// gates, config, points, notifications, and a scheduler job.
package guestbook

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/the-loon-clan/loon/core"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() {
	core.RegisterPlugin("guestbook", func() core.Plugin { return &Plugin{} })
}

// Config is populated from the host's plugins.guestbook config
// section via Core.Config.PluginInto.
type Config struct {
	PointsPerEntry int `json:"points_per_entry"`
}

type Plugin struct {
	core *core.Core
	db   *core.SchemaDB
	cfg  Config
	job  core.Job
}

func (p *Plugin) Metadata() core.Metadata {
	return core.Metadata{
		Name:        "guestbook",
		Version:     "1.0.0",
		Description: "Sign the guestbook, earn points. The loon hello-world.",
		Migrations:  migrations,
	}
}

func (p *Plugin) Provision(c *core.Core) error {
	p.core = c
	p.db = c.Storage.SchemaDB("guestbook")
	if err := c.Config.PluginInto("guestbook", &p.cfg); err != nil {
		return fmt.Errorf("guestbook: config: %w", err)
	}

	g := c.Router.Mount("guestbook")
	if g == nil {
		return fmt.Errorf("guestbook: router not available")
	}
	g.GET("", p.list)

	signed := g.Group("")
	signed.Use(c.Auth.RequireUser(core.RoleUser)...)
	signed.POST("", p.sign)

	// The browsable guestbook page (views.go) — grouped under Community in
	// the site nav next to the stats plugin's page.
	if err := p.registerViews(c); err != nil {
		return err
	}

	p.job = c.Scheduler.RegisterJob("guestbook: stats", "logs the entry count")
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	// A manual trigger so "run now" works — including a cross-process run-now
	// drained from the jobtrigger queue by a worker (see the demo's poller).
	p.job.SetTrigger(func() { go p.runStats(ctx) })
	p.core.Scheduler.RunLoop(ctx, p.job, 5*time.Second, time.Minute, p.runStats)
	return nil
}

func (p *Plugin) runStats(ctx context.Context) {
	n, err := p.count(ctx)
	if err != nil {
		p.job.SetError(err.Error())
		return
	}
	p.job.Log("guestbook holds %d entries", n)
}

func (p *Plugin) Stop(ctx context.Context) error { return nil }

type entry struct {
	ID        int64     `db:"id" json:"id"`
	Author    string    `db:"author" json:"author"`
	Message   string    `db:"message" json:"message"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

func (p *Plugin) list(c *gin.Context) {
	var entries []entry
	err := p.db.WithTx(c.Request.Context(), func(tx *sqlx.Tx) error {
		return tx.SelectContext(c.Request.Context(), &entries,
			`SELECT id, author, message, created_at FROM entries ORDER BY id DESC LIMIT 50`)
	})
	if err != nil {
		p.core.Errors.HandlerError(c, "guestbook/list", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "entries": entries})
}

func (p *Plugin) sign(c *gin.Context) {
	u, _ := p.core.Auth.CurrentUser(c) // RequireUser already gated
	var req struct {
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "message is required"})
		return
	}

	ctx := c.Request.Context()
	err := p.db.WithTx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO entries (author, message) VALUES ($1, $2)`, u.Username, req.Message)
		return err
	})
	if err != nil {
		p.core.Errors.HandlerError(c, "guestbook/sign", err)
		return
	}

	balance, err := p.core.Points.Award(ctx, u.ID, p.cfg.PointsPerEntry,
		"earn_guestbook_entry", "Signed the guestbook", 0)
	if err != nil {
		p.core.Errors.Report(ctx, "guestbook/award", err)
	}

	// Tell the site owner (user 1 in the demo) someone signed. The
	// host skips delivery when actor == recipient.
	_ = p.core.Notifications.Notify(ctx, 1, core.Notification{
		Kind:      "guestbook_signed",
		Title:     u.Username + " signed the guestbook",
		Body:      req.Message,
		Link:      "/plugin/guestbook",
		ActorID:   u.ID,
		ActorName: u.Username,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true, "points_balance": balance})
}

func (p *Plugin) count(ctx context.Context) (int, error) {
	var n int
	err := p.db.WithTx(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &n, `SELECT COUNT(*) FROM entries`)
	})
	return n, err
}
