package main

import (
	"html/template"
	"net/http"
	"sort"
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
	w.userWidgets = c.Views(core.SlotUserWidget) // /u/<name> profile cards
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

	// Build the site-nav entry list ONCE, sorted by group then weight then
	// registration order (a plugin's Nav hint suggests placement — the
	// WordPress/Drupal pattern). Per request we only role-filter this, never
	// re-sort. NavHidden views are mounted above but omitted from the nav.
	w.siteNavEntries = w.siteNavEntries[:0]
	for _, v := range w.sitePages {
		if v.Nav.Menu == core.NavHidden {
			continue
		}
		w.siteNavEntries = append(w.siteNavEntries, siteNavEntry{
			href: "/p/" + v.Slug, label: v.Title,
			group: v.Nav.Group, weight: v.Nav.Weight, view: v,
		})
	}
	sort.SliceStable(w.siteNavEntries, func(i, j int) bool {
		a, b := w.siteNavEntries[i], w.siteNavEntries[j]
		if a.group != b.group {
			return a.group < b.group // "" (ungrouped) sorts first
		}
		return a.weight < b.weight
	})
}

// siteNavEntry is a site page pre-resolved for the nav (built at boot).
type siteNavEntry struct {
	href, label, group string
	weight             int
	view               core.View
}

// navNode is one rendered top-level nav item: a plain link (Children nil) or a
// dropdown (Children set, Href "").
type navNode struct {
	Label    string
	Href     string
	Children []navItem
}

// canView applies a view's visibility to the current request.
func (w *web) canView(v core.View, c *gin.Context) bool {
	u, _ := w.auth.Current(c)
	return v.AllowsUser(u)
}

// siteNav builds the top nav for the current viewer from the pre-sorted
// entries: ungrouped pages become plain links; a named group with 2+ visible
// pages collapses into a dropdown; a group with a single visible page flattens
// to a plain link (no one-item dropdowns). The user is resolved ONCE and the
// entries are already sorted, so this is a linear role-filter — nothing hot.
func (w *web) siteNav(c *gin.Context) []navNode {
	u, _ := w.auth.Current(c)
	var nodes []navNode
	for i := 0; i < len(w.siteNavEntries); {
		e := w.siteNavEntries[i]
		if e.group == "" {
			if e.view.AllowsUser(u) {
				nodes = append(nodes, navNode{Label: e.label, Href: e.href})
			}
			i++
			continue
		}
		// entries are contiguous by group — gather the run, keep visible ones
		var kids []navItem
		for i < len(w.siteNavEntries) && w.siteNavEntries[i].group == e.group {
			ge := w.siteNavEntries[i]
			if ge.view.AllowsUser(u) {
				kids = append(kids, navItem{Href: ge.href, Label: ge.label})
			}
			i++
		}
		switch len(kids) {
		case 0:
			// nothing visible in this group
		case 1:
			nodes = append(nodes, navNode{Label: kids[0].Label, Href: kids[0].Href})
		default:
			nodes = append(nodes, navNode{Label: e.group, Children: kids})
		}
	}
	return nodes
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
