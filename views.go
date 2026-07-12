package main

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/core"

	"github.com/ameNZB/loon-baseline/password"
	"github.com/ameNZB/loon-baseline/session"
	"github.com/ameNZB/loon-baseline/webauth"

	"github.com/ameNZB/loon-plugins/pluginapi"
)

//go:embed web/templates web/static
var webFS embed.FS

// web is the demo's host-side HTTP surface: templates, static assets,
// username+password login, and the public pages. The auth plumbing (signed
// session cookie, bcrypt verify, current-user middleware) comes from
// loon-baseline — the shared host baseline loon omits by design — so the demo
// exercises the same code a real site would. It keeps two in-memory users whose
// password equals their username.
type web struct {
	users     map[string]*core.User // by username
	byID      map[int64]*core.User
	passwords map[string]string // username -> bcrypt hash (demo: hash of the username)
	hasher    password.Hasher
	auth      webauth.Auth
	log       *slog.Logger
	tmpls     map[string]*template.Template // page name -> parsed (base + page)

	// usenet plugin read capability, looked up on the extension registry after
	// Boot (the plugin's ADMIN surface is no longer consumed here — the plugin
	// renders its own views through loon's view system).
	usenet pluginapi.UsenetIndex
	rt     *core.Runtime // plugin runtime, for the /admin/plugins page

	// View-system lookup tables, filled by wireViews after Boot.
	adminNav       []navItem            // admin subnav: Settings + plugin pages + host pages
	settingsViews  []core.View          // sections on /admin/settings
	sitePages      []core.View          // public-facing pages at /p/<slug>
	siteWidgets    []core.View          // cards on the home page
	jobsWidgets    map[string]core.View // job-group name -> override widget
	siteNavEntries []siteNavEntry       // site pages, pre-sorted for the nav (built once at boot)
}

func newWeb(users map[string]*core.User, secret []byte, log *slog.Logger) *web {
	byID := make(map[int64]*core.User, len(users))
	w := &web{
		users:     users,
		byID:      byID,
		passwords: make(map[string]string, len(users)),
		hasher:    password.Hasher{}, // bare bcrypt for the demo (no pepper)
		log:       log,
		tmpls:     map[string]*template.Template{},
	}
	for name, u := range users {
		byID[u.ID] = u
		// Demo password == username, stored bcrypt-hashed so login exercises a
		// real hash-verify (not a plaintext compare).
		if h, err := w.hasher.Hash(name); err == nil {
			w.passwords[name] = h
		}
	}
	// Session + current-user middleware from the baseline — the exact prod
	// scheme (gin-contrib/sessions "mysession" cookie, login_at expiry,
	// password_changed_at invalidation). The demo's Meta is zero (no
	// password-change column, no IP salt); a real host returns the user's
	// password_changed_at here so changing it logs every session out, and sets
	// IPHash for admin IP pinning.
	w.auth = webauth.Auth{
		Session: session.Config{Secret: secret}, // "mysession", 7-day default; Secure off (plain-HTTP demo)
		Resolve: func(_ context.Context, id int64) (*core.User, webauth.Meta, bool) {
			u := byID[id]
			return u, webauth.Meta{}, u != nil
		},
	}
	for _, page := range []string{"home.html", "groups.html", "search.html", "login.html", "site_page.html", "admin_view.html", "admin_settings.html", "admin_jobs.html", "admin_plugins.html"} {
		w.tmpls[page] = template.Must(template.ParseFS(webFS,
			"web/templates/base.html", "web/templates/"+page))
	}
	return w
}

// currentUser resolves the request's user via the baseline session middleware.
// The login form is the only way in — no header back door.
func (w *web) currentUser(c *gin.Context) (*core.User, bool) {
	return w.auth.Current(c)
}

// ── routes + rendering ──────────────────────────────────────────────

func (w *web) mount(e *gin.Engine) {
	sub, _ := fs.Sub(webFS, "web/static")
	e.StaticFS("/static", http.FS(sub))
	e.GET("/", w.home)
	e.GET("/groups", w.groups)
	e.GET("/search", w.search)
	e.GET("/nzb/:id", w.nzbDownload)
	e.GET("/login", w.loginPage)
	e.POST("/login", w.loginPost)
	e.GET("/logout", w.logout)
}

func (w *web) render(c *gin.Context, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	u, _ := w.currentUser(c)
	data["User"] = u
	data["Path"] = c.Request.URL.Path
	data["AdminNav"] = w.adminNav
	data["SiteNav"] = w.siteNav(c) // plugin site pages the viewer may open
	t := w.tmpls[page]
	if t == nil {
		c.String(http.StatusInternalServerError, "unknown page %q", page)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(c.Writer, "base.html", data); err != nil {
		w.log.Error("render", "page", page, "err", err)
	}
}

func (w *web) home(c *gin.Context) {
	data := map[string]any{"Title": "Home", "Widgets": w.homeWidgets(c)}
	if w.usenet != nil {
		if res, err := w.usenet.Browse(c.Request.Context(), "", 25); err == nil {
			data["Recent"] = toSearchRows(res)
		}
	}
	w.render(c, "home.html", data)
}

func (w *web) groups(c *gin.Context) {
	data := map[string]any{"Title": "Groups", "Configured": w.usenet != nil}
	if w.usenet != nil {
		if gs, err := w.usenet.Groups(c.Request.Context()); err == nil {
			data["Groups"] = gs
		}
	}
	w.render(c, "groups.html", data)
}

func (w *web) search(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	group := strings.TrimSpace(c.Query("group"))
	data := map[string]any{"Title": "Search", "Query": q, "Group": group, "Configured": w.usenet != nil}
	if w.usenet != nil {
		var res []pluginapi.Release
		var err error
		switch {
		case group != "":
			res, err = w.usenet.Browse(c.Request.Context(), group, 100)
		case q != "":
			res, err = w.usenet.Search(c.Request.Context(), q, 50)
		}
		if err == nil {
			data["Results"] = toSearchRows(res)
		}
	}
	w.render(c, "search.html", data)
}

func (w *web) loginPage(c *gin.Context) {
	w.render(c, "login.html", map[string]any{"Title": "Log in"})
}

func (w *web) loginPost(c *gin.Context) {
	name := strings.TrimSpace(c.PostForm("username"))
	pass := c.PostForm("password")
	u, ok := w.users[name]
	if valid, _ := w.hasher.Verify(w.passwords[name], pass); !ok || !valid {
		c.Status(http.StatusUnauthorized)
		w.render(c, "login.html", map[string]any{"Title": "Log in", "Error": "Invalid username or password."})
		return
	}
	// Prod stamp: ipHash "" (no IP salt in the demo), pwChangedAt 0 (no column).
	if err := session.Issue(c, u.ID, "", 0); err != nil {
		w.log.Error("session issue", "err", err)
	}
	c.Redirect(http.StatusSeeOther, "/")
}

func (w *web) logout(c *gin.Context) {
	_ = session.Clear(c)
	c.Redirect(http.StatusSeeOther, "/")
}

// usersAdapter builds the core.UsersService backing from the in-memory map.
func (w *web) usersAdapter() core.UsersAdapter {
	return core.UsersAdapter{
		GetByIDFn:       func(_ context.Context, id int64) (*core.User, error) { return w.byID[id], nil },
		GetByUsernameFn: func(_ context.Context, name string) (*core.User, error) { return w.users[name], nil },
		DisplayNameFn: func(_ context.Context, id int64) (string, error) {
			if u := w.byID[id]; u != nil {
				return u.Username, nil
			}
			return "", nil
		},
		BulkDisplayNamesFn: func(_ context.Context, ids []int64) (map[int64]string, error) {
			out := make(map[int64]string, len(ids))
			for _, id := range ids {
				if u := w.byID[id]; u != nil {
					out[id] = u.Username
				}
			}
			return out, nil
		},
	}
}
