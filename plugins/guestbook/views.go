package guestbook

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/ameNZB/loon/core"
)

// A site.page view: the guestbook as a browsable page (the JSON API in
// plugin.go stays for programmatic use). Grouped with "Site stats" under the
// Community dropdown via the Nav hint. Default visibility: any logged-in user.

var pageTmpl = template.Must(template.New("page").Parse(`
{{if .Msg}}<div class="alert" style="border-color:var(--success);color:var(--success)">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert" style="border-color:var(--danger);color:var(--danger)">{{.Err}}</div>{{end}}

<div class="card">
    <h2>Sign it</h2>
    <form method="post" action="/p/guestbook/sign" style="display:flex;gap:.5rem">
        <input type="text" name="message" placeholder="say something nice…" style="flex:1" maxlength="500">
        <button class="btn btn-primary" type="submit">Sign</button>
    </form>
</div>

{{if .Entries}}
<div class="card">
    {{range .Entries}}
    <div style="padding:.5rem 0;border-bottom:1px solid var(--border)">
        <div>{{.Message}}</div>
        <div class="text-muted small">— <strong>{{.Author}}</strong>, {{.CreatedAt.Format "2006-01-02 15:04"}}</div>
    </div>
    {{end}}
</div>
{{else}}
<div class="empty">Nobody has signed yet — be the first.</div>
{{end}}
`))

func (p *Plugin) registerViews(c *core.Core) error {
	return c.RegisterView(core.View{
		Slug: "guestbook", Title: "Guestbook", Slot: core.SlotSitePage,
		Public: true, // anyone can read the guestbook; signing (the action) still needs login
		Nav:    core.NavHint{Group: "Community", Weight: 10},
		Render: p.renderPage,
		Actions: map[string]func(*gin.Context) (template.HTML, error){
			"sign": p.actionSign,
		},
	})
}

func (p *Plugin) renderPage(c *gin.Context) (template.HTML, error) {
	var entries []entry
	err := p.db.WithTx(c.Request.Context(), func(tx *sqlx.Tx) error {
		return tx.SelectContext(c.Request.Context(), &entries,
			`SELECT id, author, message, created_at FROM entries ORDER BY id DESC LIMIT 50`)
	})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, map[string]any{
		"Entries": entries, "Msg": c.Query("msg"), "Err": c.Query("err"),
	}); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// actionSign is the form-post twin of the JSON sign handler: insert, award
// points, notify the owner, bounce back to the page.
func (p *Plugin) actionSign(c *gin.Context) (template.HTML, error) {
	u, ok := p.core.Auth.CurrentUser(c)
	if !ok {
		c.Redirect(http.StatusSeeOther, "/login")
		return "", nil
	}
	msg := strings.TrimSpace(c.PostForm("message"))
	if msg == "" {
		c.Redirect(http.StatusSeeOther, "/p/guestbook?err=message+is+required")
		return "", nil
	}

	ctx := c.Request.Context()
	err := p.db.WithTx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO entries (author, message) VALUES ($1, $2)`, u.Username, msg)
		return err
	})
	if err != nil {
		return "", err
	}
	if _, err := p.core.Points.Award(ctx, u.ID, p.cfg.PointsPerEntry,
		"earn_guestbook_entry", "Signed the guestbook", 0); err != nil {
		p.core.Errors.Report(ctx, "guestbook/award", err)
	}
	_ = p.core.Notifications.Notify(ctx, 1, core.Notification{
		Kind:  "guestbook_signed",
		Title: u.Username + " signed the guestbook",
		Body:  msg, Link: "/p/guestbook",
		ActorID: u.ID, ActorName: u.Username,
	})
	c.Redirect(http.StatusSeeOther, "/p/guestbook?msg=signed+—+thanks!")
	return "", nil
}
