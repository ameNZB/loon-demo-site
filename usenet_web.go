package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/the-loon-clan/loon-baseline/cache"
	"github.com/the-loon-clan/loon-plugins/pluginapi"
)

// Public usenet surface only: NZB download + the search/browse view models.
// The plugin's ADMIN pages (setup wizard, crawler status) are plugin-owned
// views mounted generically in main.go — see admin_views.go.

// newznabAPI is the Newznab/Torznab endpoint (/api + /rss). The plugin owns the
// XML; the host parses the query + serves the response. Open (no apikey check) —
// it's a demo; a real host validates apikey against its user store here.
//
// Responses are read through the host cache, using the SAME key + namespace as
// the loon-api read tier (pluginapi.NewznabCacheKey) so a shared Redis is
// hit-compatible. The events subscriber in main() clears this namespace on an
// ingest, so entries only go stale when new releases land — which is why the TTL
// can be long.
func (w *web) newznabAPI(c *gin.Context) {
	if w.usenetAPI == nil {
		c.String(http.StatusServiceUnavailable, "indexer not configured")
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	req := pluginapi.NewznabRequest{
		Function:   c.Query("t"),
		Query:      c.Query("q"),
		Categories: parseCats(c.Query("cat")),
		Limit:      limit,
		Offset:     offset,
		ID:         c.Query("id"),
		BaseURL:    requestBaseURL(c),
		Title:      "loon demo indexer",
		APIKey:     c.Query("apikey"),
	}

	// Cache read functions; t=get streams NZB bytes, don't hold those.
	cacheable := w.cache != nil && req.Function != "get"
	var key string
	if cacheable {
		key = pluginapi.NewznabCacheKey(req)
		var cached pluginapi.NewznabResult
		if ok, _ := cache.GetJSON(c.Request.Context(), w.cache, key, &cached); ok {
			writeNewznab(c, cached, "hit")
			return
		}
	}
	res, err := w.usenetAPI.Newznab(c.Request.Context(), req)
	if err != nil {
		c.String(http.StatusInternalServerError, "api error")
		return
	}
	if cacheable {
		// Long TTL is safe: an ingest invalidates the namespace, so entries stay
		// fresh until new releases land.
		_ = cache.SetJSON(c.Request.Context(), w.cache, key, res, time.Hour)
	}
	writeNewznab(c, res, "miss")
}

func writeNewznab(c *gin.Context, res pluginapi.NewznabResult, status string) {
	if res.Filename != "" {
		c.Header("Content-Disposition", `attachment; filename="`+res.Filename+`"`)
	}
	c.Header("X-Cache", status)
	c.Data(http.StatusOK, res.ContentType, res.Body)
}

func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

// parseCats splits a Newznab cat= value ("5070,2040") into category ids.
func parseCats(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// releasePage renders the detail view for one release (metadata, tags, file
// list, download button).
func (w *web) releasePage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "bad id")
		return
	}
	if w.usenet == nil {
		w.render(c, "release.html", map[string]any{"Title": "Release", "Missing": true})
		return
	}
	d, ok, err := w.usenet.ReleaseByID(c.Request.Context(), id)
	if err != nil || !ok {
		c.Status(http.StatusNotFound)
		w.render(c, "release.html", map[string]any{"Title": "Not found", "Missing": true})
		return
	}
	vm := toReleaseVM(d)
	if w.catalogCovers != nil {
		if url, has, _ := w.catalogCovers.ReleaseCover(c.Request.Context(), id); has {
			vm.Cover = url
		}
	}
	w.render(c, "release.html", map[string]any{"Title": d.Title, "Release": vm})
}

type releaseFileVM struct {
	Name     string
	Size     string
	Segments int
}

type releaseVM struct {
	ID       int64
	Title    string
	Size     string
	Posted   string
	Group    string
	Poster   string
	Category string
	Cover    string
	Tags     []string
	Files    []releaseFileVM
}

func toReleaseVM(d pluginapi.ReleaseDetail) releaseVM {
	vm := releaseVM{
		ID: d.ID, Title: d.Title, Size: humanBytes(d.Size),
		Group: d.Group, Poster: d.Poster, Category: d.Category, Posted: "—",
	}
	if !d.Posted.IsZero() {
		vm.Posted = d.Posted.Format("2006-01-02 15:04")
	}
	for _, t := range []string{d.Resolution, d.Source, d.Codec, d.Audio, d.Language} {
		if t != "" {
			vm.Tags = append(vm.Tags, t)
		}
	}
	for _, f := range d.Files {
		vm.Files = append(vm.Files, releaseFileVM{Name: f.Filename, Size: humanBytes(f.Bytes), Segments: f.Segments})
	}
	return vm
}

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

// ── search view model ───────────────────────────────────────────────

type searchRow struct {
	ID       int64
	Title    string
	Size     string
	Posted   string
	Category string
	Tags     []string
}

func toSearchRows(rs []pluginapi.Release) []searchRow {
	out := make([]searchRow, len(rs))
	for i, r := range rs {
		row := searchRow{ID: r.ID, Title: r.Title, Size: humanBytes(r.Size), Posted: "—", Category: r.Category}
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
