package main

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/schedule"

	"github.com/ameNZB/loon-plugins/pluginapi"
)

// adminCrawlers renders the live crawl + backfill status page: per-group coverage
// bars, backfill progress, and what the usenet jobs are currently doing. The
// plugin owns the numbers (Stats + coverage watermarks); loon owns the job
// snapshots; the host just renders them together.
func (w *web) adminCrawlers(c *gin.Context) {
	if w.usenetAdmin == nil {
		w.render(c, "admin_crawlers.html", map[string]any{"Title": "Crawlers", "Unavailable": true})
		return
	}
	stats, _ := w.usenetAdmin.Stats(c.Request.Context())
	jobs, active := usenetJobs()
	w.render(c, "admin_crawlers.html", map[string]any{
		"Title":       "Crawlers",
		"Stats":       stats,
		"Groups":      toCrawlerGroups(stats.Groups),
		"Jobs":        jobs,
		"AutoRefresh": active, // reload while work is in flight
		"Msg":         c.Query("msg"),
		"Err":         c.Query("err"),
	})
}

func (w *web) adminCrawlersCrawl(c *gin.Context) {
	if w.usenetAdmin != nil {
		w.usenetAdmin.TriggerCrawl()
	}
	redirectCrawlers(c, "msg", "crawl triggered")
}

func (w *web) adminCrawlersBackfill(c *gin.Context) {
	if w.usenetAdmin != nil {
		w.usenetAdmin.TriggerBackfill()
	}
	redirectCrawlers(c, "msg", "backfill triggered")
}

func (w *web) adminCrawlersReset(c *gin.Context) {
	name := c.PostForm("name")
	if w.usenetAdmin != nil {
		_ = w.usenetAdmin.ResetBackfill(c.Request.Context(), name)
	}
	redirectCrawlers(c, "msg", "backfill re-armed for "+name)
}

func redirectCrawlers(c *gin.Context, key, msg string) {
	c.Redirect(http.StatusSeeOther, "/admin/crawlers?"+key+"="+url.QueryEscape(msg))
}

// ── view models ─────────────────────────────────────────────────────

type crawlerGroup struct {
	Name         string
	NZBs, Staged int
	Cover        pluginapi.CoverageBar
	FwdDate      string
	BackDate     string
	BackfillDone bool
	Remaining    int64 // articles still below the back watermark
	LastCrawl    string
}

func toCrawlerGroups(gs []pluginapi.GroupStat) []crawlerGroup {
	out := make([]crawlerGroup, len(gs))
	for i, g := range gs {
		cg := crawlerGroup{
			Name: g.Name, NZBs: g.NZBs, Staged: g.Staged,
			Cover: g.Coverage(), BackfillDone: g.BackfillDone,
			FwdDate: fmtDate(g.HighWatermarkDate), BackDate: fmtDate(g.BackWatermarkDate),
			LastCrawl: fmtJobTime(g.LastCrawl),
		}
		if !g.BackfillDone && g.BackWatermark > g.ServerLow {
			cg.Remaining = g.BackWatermark - g.ServerLow
		}
		out[i] = cg
	}
	return out
}

type crawlerJob struct {
	Name     string
	Status   string
	Activity string
	Running  bool
}

// usenetJobs pulls the usenet plugin's job snapshots (Crawler, Backfill, Builder,
// Tag Fill, Prune) so the page can show "what it's working on" and auto-refresh
// while any is running.
func usenetJobs() (jobs []crawlerJob, anyRunning bool) {
	for _, s := range schedule.GetAllSnapshots() {
		if !strings.HasPrefix(s.Name, "Usenet") && !strings.HasPrefix(s.Name, "NZB") {
			continue
		}
		j := crawlerJob{Name: s.Name, Status: s.Status}
		if s.LastError != "" {
			j.Activity = s.LastError
		} else if len(s.Logs) > 0 {
			j.Activity = s.Logs[len(s.Logs)-1]
		}
		if s.Status == "running" || s.ElapsedSecs > 0 {
			j.Running, anyRunning = true, true
		}
		jobs = append(jobs, j)
	}
	return jobs, anyRunning
}

func fmtDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02")
}
