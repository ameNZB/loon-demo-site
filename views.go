package main

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/core"

	"github.com/ameNZB/loon-baseline/authflow"
	"github.com/ameNZB/loon-baseline/password"
	"github.com/ameNZB/loon-baseline/session"
	"github.com/ameNZB/loon-baseline/users"
	"github.com/ameNZB/loon-baseline/webauth"

	"github.com/ameNZB/loon-plugins/pluginapi"
)

//go:embed web/templates web/static
var webFS embed.FS

// web is the demo's host-side HTTP surface: templates, static assets,
// username+password login + registration, and the public pages. The whole auth
// stack — user store, session cookie, bcrypt verify, register/login flow,
// current-user middleware — comes from loon-baseline (the host baseline loon
// omits by design), so the demo exercises the exact code a real site would.
// Users live in a real Postgres table (loon-baseline users.PGStore), seeded
// with alice/bob (password == username).
type web struct {
	store users.Store   // loon-baseline user store (Postgres reference impl)
	flow  authflow.Flow // register / authenticate / change-password
	auth  webauth.Auth
	log   *slog.Logger
	tmpls map[string]*template.Template // page name -> parsed (base + page)

	// usenet plugin read capability, looked up on the extension registry after
	// Boot (the plugin's ADMIN surface is no longer consumed here — the plugin
	// renders its own views through loon's view system).
	usenet        pluginapi.UsenetIndex
	usenetAPI     pluginapi.UsenetNewznab // Newznab /api + /rss
	catalogSink   pluginapi.CatalogSink   // scraper write side (filled after Boot)
	catalogCovers pluginapi.CatalogCovers // release↔cover store (filled after Boot)
	rt            *core.Runtime           // plugin runtime, for the /admin/plugins page

	// View-system lookup tables, filled by wireViews after Boot.
	adminNav       []navItem            // admin subnav: Settings + plugin pages + host pages
	settingsViews  []core.View          // sections on /admin/settings
	sitePages      []core.View          // public-facing pages at /p/<slug>
	siteWidgets    []core.View          // cards on the home page
	jobsWidgets    map[string]core.View // job-group name -> override widget
	siteNavEntries []siteNavEntry       // site pages, pre-sorted for the nav (built once at boot)
}

func newWeb(store users.Store, secret []byte, log *slog.Logger) *web {
	w := &web{
		store: store,
		flow:  authflow.Flow{Users: store, Hasher: password.Hasher{}, DefaultRole: core.RoleUser},
		log:   log,
		tmpls: map[string]*template.Template{},
	}
	// Session + current-user middleware from the baseline — the exact prod
	// scheme (gin-contrib/sessions "mysession" cookie, login_at expiry). Resolve
	// reads the user store; a richer host returns password_changed_at + IPHash
	// for session invalidation + admin IP pinning.
	w.auth = webauth.Auth{
		Session: session.Config{Secret: secret}, // "mysession", 7-day default; Secure off (plain-HTTP demo)
		Resolve: func(ctx context.Context, id int64) (*core.User, webauth.Meta, bool) {
			u, err := store.ByID(ctx, id)
			if err != nil {
				return nil, webauth.Meta{}, false
			}
			return u.ToCore(), webauth.Meta{}, true
		},
	}
	for _, page := range []string{"home.html", "groups.html", "search.html", "release.html", "login.html", "register.html", "site_page.html", "admin_view.html", "admin_settings.html", "admin_jobs.html", "admin_plugins.html"} {
		w.tmpls[page] = template.Must(template.ParseFS(webFS,
			"web/templates/base.html", "web/templates/"+page))
	}
	return w
}

// currentUser resolves the request's user via the baseline session middleware.
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
	e.GET("/release/:id", w.releasePage)
	e.GET("/nzb/:id", w.nzbDownload)
	e.GET("/login", w.loginPage)
	e.POST("/login", w.loginPost)
	e.GET("/register", w.registerPage)
	e.POST("/register", w.registerPost)
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
	u, err := w.flow.Authenticate(c.Request.Context(), c.PostForm("username"), c.PostForm("password"))
	if err != nil {
		c.Status(http.StatusUnauthorized)
		w.render(c, "login.html", map[string]any{"Title": "Log in", "Error": "Invalid username or password."})
		return
	}
	if err := w.flow.Issue(c, u); err != nil {
		w.log.Error("session issue", "err", err)
	}
	c.Redirect(http.StatusSeeOther, "/")
}

func (w *web) registerPage(c *gin.Context) {
	w.render(c, "register.html", map[string]any{"Title": "Register"})
}

func (w *web) registerPost(c *gin.Context) {
	name := strings.TrimSpace(c.PostForm("username"))
	email := strings.TrimSpace(c.PostForm("email"))
	u, err := w.flow.Register(c.Request.Context(), name, email, c.PostForm("password"))
	if err != nil {
		c.Status(http.StatusBadRequest)
		w.render(c, "register.html", map[string]any{"Title": "Register", "Error": err.Error(), "Username": name, "Email": email})
		return
	}
	if err := w.flow.Issue(c, u); err != nil {
		w.log.Error("session issue", "err", err)
	}
	c.Redirect(http.StatusSeeOther, "/")
}

func (w *web) logout(c *gin.Context) {
	_ = session.Clear(c)
	c.Redirect(http.StatusSeeOther, "/")
}

// usersAdapter builds the core.UsersService backing from the user store.
func (w *web) usersAdapter() core.UsersAdapter {
	coreByID := func(ctx context.Context, id int64) (*core.User, error) {
		u, err := w.store.ByID(ctx, id)
		if errors.Is(err, users.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return u.ToCore(), nil
	}
	return core.UsersAdapter{
		GetByIDFn: coreByID,
		GetByUsernameFn: func(ctx context.Context, name string) (*core.User, error) {
			u, err := w.store.ByUsername(ctx, name)
			if errors.Is(err, users.ErrNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return u.ToCore(), nil
		},
		DisplayNameFn: func(ctx context.Context, id int64) (string, error) {
			if u, err := coreByID(ctx, id); err == nil && u != nil {
				return u.Username, nil
			}
			return "", nil
		},
		BulkDisplayNamesFn: func(ctx context.Context, ids []int64) (map[int64]string, error) {
			out := make(map[int64]string, len(ids))
			for _, id := range ids {
				if u, err := coreByID(ctx, id); err == nil && u != nil {
					out[id] = u.Username
				}
			}
			return out, nil
		},
	}
}
