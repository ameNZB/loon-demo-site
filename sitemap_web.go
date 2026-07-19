package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/the-loon-clan/loon-baseline/sitemap"
	"github.com/the-loon-clan/loon-plugins/pluginapi"
	"github.com/the-loon-clan/loon/schedule"
)

// Sitemap wiring: the reference example of loon-baseline/sitemap.
//
// The package generates and caches; the host decides what it contains, when to
// regenerate, and how to serve. That split is the whole design — this file is
// all three of the host's halves, and it is deliberately small.

const (
	sitemapTTL         = 25 * time.Hour // > the regen interval, or the cache 404s in the gap
	sitemapIntervalMin = 24 * 60
	sitemapBootDelay   = 30 * time.Second
)

// releaseSource publishes the demo's Usenet releases.
//
// It reads through the usenet plugin's UsenetIndex capability rather than the
// database: the host does not own the releases table, the plugin does, and a
// sitemap is not a reason to reach around a capability boundary.
type releaseSource struct {
	idx     pluginapi.UsenetIndex
	baseURL string
}

func (s releaseSource) Kind() string { return "releases" }

// Count leans on Feed's total. Asking for one row to learn the count is a
// little wasteful, but it keeps the Source honest: the count and the pages come
// from the same query shape, so they cannot disagree about what is published.
func (s releaseSource) Count(ctx context.Context) (int, error) {
	_, total, err := s.idx.Feed(ctx, nil, 1, 0)
	return total, err
}

func (s releaseSource) Page(ctx context.Context, limit, offset int) ([]sitemap.Entry, error) {
	rel, _, err := s.idx.Feed(ctx, nil, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]sitemap.Entry, 0, len(rel))
	for _, r := range rel {
		out = append(out, sitemap.Entry{
			Loc:     fmt.Sprintf("%s/release/%d", s.baseURL, r.ID),
			Lastmod: r.Posted,
		})
	}
	return out, nil
}

// wireSitemap builds the generator, registers the daily job, and mounts the
// routes. Call after Boot, once the usenet capability has been looked up.
func (w *web) wireSitemap(engine *gin.Engine, baseURL string) {
	cfg := sitemap.Config{
		BaseURL:     baseURL,
		StaticPaths: []string{"/", "/search", "/groups", "/guestbook"},
		TTL:         sitemapTTL,
	}

	// Sources are what the site HAS. A demo with no usenet plugin configured
	// still gets a valid static-only sitemap rather than a boot failure —
	// absence of a capability is a smaller site, not a broken one.
	var sources []sitemap.Source
	if w.usenet != nil {
		sources = append(sources, releaseSource{idx: w.usenet, baseURL: baseURL})
	}
	gen := sitemap.New(cfg, w.cache, sources...)

	job := schedule.RegisterJob("Sitemap",
		"Regenerates sitemap.xml and its sub-sitemaps from the site's content sources, cached for serving.")
	job.IntervalMin = sitemapIntervalMin
	job.SetTrigger(func() { go w.runSitemap(context.Background(), gen, job) })
	go schedule.ServiceLoop(context.Background(), job,
		sitemapBootDelay, sitemapIntervalMin*time.Minute,
		func(ctx context.Context) { w.runSitemap(ctx, gen, job) })

	// Serving is a cache read. The generator never touches HTTP, so a slow or
	// failed regen degrades to a stale sitemap rather than a slow request.
	engine.GET("/sitemap.xml", func(c *gin.Context) { w.serveSitemap(c, "index") })

	// NOT "/sitemap-:name.xml". gin captures the whole segment after a literal
	// prefix, so the extension belongs to the param, and a pattern with .xml
	// after the placeholder MATCHES with an empty param — no panic, no error,
	// just an empty name that misses the cache and 404s every sub-sitemap the
	// index links. Measured:
	//
	//   "/sitemap-:name.xml" + GET /sitemap-static.xml -> 200, param="" (!)
	//   "/sitemap-:name"     + GET /sitemap-static.xml -> 200, param="static.xml"
	//
	// So take the whole segment and trim the extension ourselves.
	engine.GET("/sitemap-:name", func(c *gin.Context) {
		w.serveSitemap(c, strings.TrimSuffix(c.Param("name"), ".xml"))
	})
}

func (w *web) runSitemap(ctx context.Context, gen *sitemap.Generator, job *schedule.JobInfo) {
	job.SetRunning()
	defer job.SetIdle(time.Now().Add(sitemapIntervalMin * time.Minute))

	res, err := gen.Generate(ctx)
	if err != nil {
		// Loud: a sitemap that silently omits a content type reads to a crawler
		// as delisting, and nothing else in the system would notice.
		job.Log("ERROR generating sitemap: %v", err)
		return
	}
	for kind, k := range res.PerKind {
		job.Log("%s: %d file(s), %d URL(s)", kind, k.Files, k.URLs)
	}
	job.Log("Sitemap complete — %d file(s), %d URL(s)", res.Files, res.URLs)
}

func (w *web) serveSitemap(c *gin.Context, name string) {
	body, ok, err := w.cache.Get(c.Request.Context(), name)
	if err != nil || !ok || len(body) == 0 {
		// 404 rather than an empty 200: an empty sitemap tells a crawler the
		// site has no pages, which is worse than telling it to come back.
		c.String(http.StatusNotFound, "sitemap not generated yet")
		return
	}
	c.Data(http.StatusOK, "application/xml; charset=utf-8", body)
}
