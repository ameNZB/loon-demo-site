package main

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon-plugins/pluginapi"
)

// Public usenet surface only: NZB download + the search/browse view models.
// The plugin's ADMIN pages (setup wizard, crawler status) are plugin-owned
// views mounted generically in main.go — see admin_views.go.

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
