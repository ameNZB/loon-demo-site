package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon-plugins/pluginapi"
)

// nzbDownload serves the decompressed .nzb bytes for a release id.
func (w *web) nzbDownload(c *gin.Context) {
	if w.usenet == nil {
		c.String(http.StatusServiceUnavailable, "indexer not configured")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "bad id")
		return
	}
	data, filename, err := w.usenet.NZB(c.Request.Context(), id)
	if err != nil || len(data) == 0 {
		c.String(http.StatusNotFound, "not found")
		return
	}
	if filename == "" {
		filename = "download.nzb"
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/x-nzb", data)
}

// ── admin /admin/usenet wizard ──────────────────────────────────────

func (w *web) adminUsenet(c *gin.Context) {
	if w.usenetAdmin == nil {
		w.render(c, "admin_usenet.html", map[string]any{"Title": "Usenet", "Unavailable": true})
		return
	}
	ctx := c.Request.Context()
	srv, _ := w.usenetAdmin.Server(ctx)
	groups, _ := w.usenetAdmin.AllGroups(ctx, 1000)
	w.render(c, "admin_usenet.html", map[string]any{
		"Title":  "Usenet",
		"Server": srv,
		"Groups": groups,
		"Msg":    c.Query("msg"),
		"Err":    c.Query("err"),
	})
}

func (w *web) adminUsenetSaveServer(c *gin.Context) {
	if w.usenetAdmin == nil {
		c.Redirect(http.StatusSeeOther, "/admin/usenet")
		return
	}
	if err := w.usenetAdmin.SetServer(c.Request.Context(), usenetFormServer(c)); err != nil {
		redirectMsg(c, "err", err.Error())
		return
	}
	redirectMsg(c, "msg", "server saved")
}

func (w *web) adminUsenetTest(c *gin.Context) {
	if w.usenetAdmin == nil {
		c.Redirect(http.StatusSeeOther, "/admin/usenet")
		return
	}
	if err := w.usenetAdmin.TestConnect(c.Request.Context(), usenetFormServer(c)); err != nil {
		redirectMsg(c, "err", "connection failed: "+err.Error())
		return
	}
	redirectMsg(c, "msg", "connection ok")
}

func (w *web) adminUsenetFetch(c *gin.Context) {
	if w.usenetAdmin == nil {
		c.Redirect(http.StatusSeeOther, "/admin/usenet")
		return
	}
	n, err := w.usenetAdmin.FetchGroups(c.Request.Context())
	if err != nil {
		redirectMsg(c, "err", "fetch failed: "+err.Error())
		return
	}
	redirectMsg(c, "msg", fmt.Sprintf("fetched %d new group(s)", n))
}

func (w *web) adminUsenetGroup(c *gin.Context) {
	if w.usenetAdmin != nil {
		_ = w.usenetAdmin.SetGroupActive(c.Request.Context(),
			c.PostForm("name"), c.PostForm("active") == "true")
	}
	c.Redirect(http.StatusSeeOther, "/admin/usenet")
}

func (w *web) adminUsenetCrawl(c *gin.Context) {
	if w.usenetAdmin != nil {
		w.usenetAdmin.TriggerCrawl()
	}
	redirectMsg(c, "msg", "crawl triggered — watch /admin/jobs")
}

func usenetFormServer(c *gin.Context) pluginapi.Server {
	port, _ := strconv.Atoi(c.PostForm("port"))
	if port == 0 {
		port = 119
	}
	tls := c.PostForm("tls")
	return pluginapi.Server{
		Host:     strings.TrimSpace(c.PostForm("host")),
		Port:     port,
		TLS:      tls == "on" || tls == "true",
		Username: c.PostForm("username"),
		Password: c.PostForm("password"),
		Enabled:  true,
	}
}

func redirectMsg(c *gin.Context, key, msg string) {
	c.Redirect(http.StatusSeeOther, "/admin/usenet?"+key+"="+url.QueryEscape(msg))
}

// ── search view model ───────────────────────────────────────────────

type searchRow struct {
	ID     int64
	Title  string
	Size   string
	Posted string
	Tags   []string
}

func toSearchRows(rs []pluginapi.Release) []searchRow {
	out := make([]searchRow, len(rs))
	for i, r := range rs {
		row := searchRow{ID: r.ID, Title: r.Title, Size: humanBytes(r.Size), Posted: "—"}
		if !r.Posted.IsZero() {
			row.Posted = r.Posted.Format("2006-01-02")
		}
		for _, t := range []string{r.Resolution, r.Source, r.Codec, r.Audio, r.Language} {
			if t != "" {
				row.Tags = append(row.Tags, t)
			}
		}
		out[i] = row
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
