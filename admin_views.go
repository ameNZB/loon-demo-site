package main

import (
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/core"
)

// Plugin admin views (loon's AdminView seam): the PLUGIN renders the page
// content as a fragment; the demo wraps it in its base layout + admin subnav.
// One generic mount per view — the host has zero plugin-specific admin code.

type navItem struct {
	Href  string
	Label string
}

// adminNavFrom builds the shared admin subnav: every registered plugin view,
// then the host's own pages (jobs, plugins).
func adminNavFrom(views []core.AdminView) []navItem {
	nav := make([]navItem, 0, len(views)+2)
	for _, v := range views {
		nav = append(nav, navItem{Href: "/admin/p/" + v.Slug, Label: v.Title})
	}
	return append(nav,
		navItem{Href: "/admin/jobs", Label: "Jobs"},
		navItem{Href: "/admin/plugins", Label: "Plugins"})
}

func (w *web) viewPage(v core.AdminView) gin.HandlerFunc {
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

func (w *web) viewAction(v core.AdminView, fn func(*gin.Context) (template.HTML, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		frag, err := fn(c)
		if err != nil {
			w.log.Error("admin view action", "slug", v.Slug, "err", err)
			c.String(http.StatusInternalServerError, "action on %s failed", v.Slug)
			return
		}
		if frag == "" {
			return // action already responded (redirect)
		}
		// Non-empty fragment: re-render in place (e.g. test-connection keeping
		// the submitted form values).
		w.render(c, "admin_view.html", map[string]any{"Title": v.Title, "Fragment": frag})
	}
}
