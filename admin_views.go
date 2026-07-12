package main

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/core"
)

// Host side of loon's view system: plugins render fragments, the demo wraps
// them in its chrome. Four surfaces are wired here:
//
//   /admin/settings   one page aggregating every SlotAdminSettings section
//   /admin/p/<slug>   standalone SlotAdminPage pages
//   jobs page         SlotJobsWidget fragments override a group's default table
//   /p/<slug>         SlotSitePage pages, gated by each view's Public/MinRole
//
// Plus SlotSiteWidget cards on the home page and the nav lists built from the
// registries. All generic — zero plugin-specific code.

type navItem struct {
	Href  string
	Label string
}

// wireViews mounts every registered view and stores the lookup tables the
// render path needs. Called once after core.Boot.
func (w *web) wireViews(c *core.Core, engine *gin.Engine, admin *gin.RouterGroup) {
	w.settingsViews = c.Views(core.SlotAdminSettings)
	w.sitePages = c.Views(core.SlotSitePage)
	w.siteWidgets = c.Views(core.SlotSiteWidget)
	w.jobsWidgets = map[string]core.View{}
	for _, v := range c.Views(core.SlotJobsWidget) {
		w.jobsWidgets[v.Anchor] = v
	}

	// Admin subnav: Settings, each admin page, then the host's own pages.
	w.adminNav = []navItem{{Href: "/admin/settings", Label: "Settings"}}
	for _, v := range c.Views(core.SlotAdminPage) {
		w.adminNav = append(w.adminNav, navItem{Href: "/admin/p/" + v.Slug, Label: v.Title})
	}
	w.adminNav = append(w.adminNav,
		navItem{Href: "/admin/jobs", Label: "Jobs"},
		navItem{Href: "/admin/plugins", Label: "Plugins"})

	// The aggregated settings page + each section's actions.
	admin.GET("/settings", w.adminSettings)
	for _, v := range w.settingsViews {
		v := v
		for name, fn := range v.Actions {
			admin.POST("/settings/"+v.Slug+"/"+name, w.settingsAction(v, fn))
		}
	}

	// Standalone admin pages.
	for _, v := range c.Views(core.SlotAdminPage) {
		v := v
		admin.GET("/p/"+v.Slug, w.viewPage(v))
		for name, fn := range v.Actions {
			admin.POST("/p/"+v.Slug+"/"+name, w.viewAction(v, fn))
		}
	}

	// Public-facing pages, gated per view (Public / MinRole).
	for _, v := range w.sitePages {
		v := v
		engine.GET("/p/"+v.Slug, w.sitePage(v))
		for name, fn := range v.Actions {
			engine.POST("/p/"+v.Slug+"/"+name, w.siteAction(v, fn))
		}
	}
}

// canView applies a view's visibility to the current request.
func (w *web) canView(v core.View, c *gin.Context) bool {
	u, _ := w.auth.Current(c)
	return v.AllowsUser(u)
}

// siteNav lists the site pages the current viewer may open (for the top nav).
func (w *web) siteNav(c *gin.Context) []navItem {
	var nav []navItem
	for _, v := range w.sitePages {
		if w.canView(v, c) {
			nav = append(nav, navItem{Href: "/p/" + v.Slug, Label: v.Title})
		}
	}
	return nav
}

// homeWidgets renders the site widgets the current viewer may see.
type widgetVM struct {
	Title    string
	Fragment template.HTML
}

func (w *web) homeWidgets(c *gin.Context) []widgetVM {
	var out []widgetVM
	for _, v := range w.siteWidgets {
		if !w.canView(v, c) {
			continue
		}
		frag, err := v.Render(c)
		if err != nil {
			w.log.Error("site widget", "slug", v.Slug, "err", err)
			continue
		}
		out = append(out, widgetVM{Title: v.Title, Fragment: frag})
	}
	return out
}

// ── /admin/settings (aggregated sections) ───────────────────────────

type settingsSection struct {
	Slug     string
	Title    string
	Fragment template.HTML
}

func (w *web) adminSettings(c *gin.Context) {
	w.renderSettingsPage(c, "", "")
}

// renderSettingsPage renders every settings section; when an action returned a
// fragment (form-preserving re-render), that section shows the override while
// the others render fresh.
func (w *web) renderSettingsPage(c *gin.Context, overrideSlug string, override template.HTML) {
	sections := make([]settingsSection, 0, len(w.settingsViews))
	for _, v := range w.settingsViews {
		frag := override
		if v.Slug != overrideSlug {
			f, err := v.Render(c)
			if err != nil {
				w.log.Error("settings section", "slug", v.Slug, "err", err)
				f = template.HTML(`<div class="alert">section failed to render — see logs</div>`)
			}
			frag = f
		}
		sections = append(sections, settingsSection{Slug: v.Slug, Title: v.Title, Fragment: frag})
	}
	w.render(c, "admin_settings.html", map[string]any{"Title": "Settings", "Sections": sections})
}

func (w *web) settingsAction(v core.View, fn func(*gin.Context) (template.HTML, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		frag, err := fn(c)
		if err != nil {
			w.log.Error("settings action", "slug", v.Slug, "err", err)
			c.String(http.StatusInternalServerError, "action on %s failed", v.Slug)
			return
		}
		if frag == "" {
			return // action already responded (redirect)
		}
		w.renderSettingsPage(c, v.Slug, frag)
	}
}

// ── /admin/p/<slug> (standalone admin pages) ────────────────────────

func (w *web) viewPage(v core.View) gin.HandlerFunc {
	return func(c *gin.Context) {
		frag, err := v.Render(c)
		if err != nil {
			w.log.Error("admin view render", "slug", v.Slug, "err", err)
			c.String(http.StatusInternalServerError, "view %s failed", v.Slug)
			return
		}
		w.render(c, "admin_view.html", map[string]any{"Title": v.Title, "Fragment": frag})
	}
}

func (w *web) viewAction(v core.View, fn func(*gin.Context) (template.HTML, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		frag, err := fn(c)
		if err != nil {
			w.log.Error("admin view action", "slug", v.Slug, "err", err)
			c.String(http.StatusInternalServerError, "action on %s failed", v.Slug)
			return
		}
		if frag == "" {
			return
		}
		w.render(c, "admin_view.html", map[string]any{"Title": v.Title, "Fragment": frag})
	}
}

// ── /p/<slug> (public-facing pages, per-view visibility) ────────────

func (w *web) siteGate(v core.View, c *gin.Context) bool {
	if w.canView(v, c) {
		return true
	}
	if _, loggedIn := w.auth.Current(c); !loggedIn && strings.Contains(c.GetHeader("Accept"), "text/html") {
		c.Redirect(http.StatusSeeOther, "/login")
		c.Abort()
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"ok": false, "error": "insufficient role"})
	return false
}

func (w *web) sitePage(v core.View) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !w.siteGate(v, c) {
			return
		}
		frag, err := v.Render(c)
		if err != nil {
			w.log.Error("site page render", "slug", v.Slug, "err", err)
			c.String(http.StatusInternalServerError, "page %s failed", v.Slug)
			return
		}
		w.render(c, "site_page.html", map[string]any{"Title": v.Title, "Fragment": frag})
	}
}

func (w *web) siteAction(v core.View, fn func(*gin.Context) (template.HTML, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !w.siteGate(v, c) {
			return
		}
		frag, err := fn(c)
		if err != nil {
			w.log.Error("site page action", "slug", v.Slug, "err", err)
			c.String(http.StatusInternalServerError, "action on %s failed", v.Slug)
			return
		}
		if frag == "" {
			return
		}
		w.render(c, "site_page.html", map[string]any{"Title": v.Title, "Fragment": frag})
	}
}
