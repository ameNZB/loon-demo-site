package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ameNZB/loon/schedule"
)

// The demo renders /admin/jobs and /admin/plugins in its own base layout (nav +
// footer) instead of loon's self-contained inline pages, so every admin page
// looks consistent. The DATA still comes from loon (schedule snapshots + the
// plugin runtime); this is just the host rendering its own chrome.

type jobRow struct {
	Name        string
	Description string
	Status      string
	LastRun     string
	NextRun     string
	Interval    string
	Activity    string
	Runs        int64
	Triggerable bool
	Paused      bool
}

func (w *web) adminJobs(c *gin.Context) {
	var services, jobs []jobRow
	for _, s := range schedule.GetAllSnapshots() {
		row := toJobRow(s)
		if s.Kind == "service" {
			services = append(services, row)
		} else {
			jobs = append(jobs, row)
		}
	}
	w.render(c, "admin_jobs.html", map[string]any{"Title": "Jobs", "Services": services, "Jobs": jobs})
}

func toJobRow(s schedule.JobSnapshot) jobRow {
	r := jobRow{
		Name: s.Name, Description: s.Description, Status: s.Status,
		Runs: s.RunCount, Triggerable: s.Triggerable, Paused: s.Paused,
		LastRun: fmtJobTime(s.LastRun), NextRun: fmtJobTime(s.NextRun), Interval: "—",
	}
	if s.IntervalMin > 0 {
		r.Interval = fmt.Sprintf("%dm", s.IntervalMin)
	}
	if s.LastError != "" {
		r.Activity = s.LastError
	} else if len(s.Logs) > 0 {
		r.Activity = s.Logs[len(s.Logs)-1]
	}
	return r
}

func fmtJobTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04:05")
}

// adminJobsControl applies a manual control and redirects back (loon's
// JobsControlHandler returns JSON, which is wrong for a browser form).
func (w *web) adminJobsControl(c *gin.Context) {
	name := c.PostForm("name")
	switch c.PostForm("action") {
	case "trigger":
		schedule.TriggerJob(name)
	case "pause":
		schedule.PauseJob(name)
	case "resume":
		schedule.ResumeJob(name)
	case "stop":
		schedule.StopJob(name)
	}
	c.Redirect(http.StatusSeeOther, "/admin/jobs")
}

type pluginRow struct {
	Name        string
	Version     string
	Description string
	Requires    string
}

func (w *web) adminPlugins(c *gin.Context) {
	var rows []pluginRow
	if w.rt != nil {
		for _, p := range w.rt.Plugins() {
			md := p.Metadata()
			rows = append(rows, pluginRow{
				Name: md.Name, Version: md.Version,
				Description: md.Description, Requires: strings.Join(md.Requires, ", "),
			})
		}
	}
	w.render(c, "admin_plugins.html", map[string]any{"Title": "Plugins", "Plugins": rows})
}
