package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/the-loon-clan/loon-baseline/cache/memory"
	"github.com/the-loon-clan/loon-baseline/sitemap"
)

// Every URL the index advertises must actually resolve. An index linking a
// sub-sitemap that 404s is worse than no sitemap: the crawler follows what we
// told it to and finds nothing.
//
// This is the bug the demo caught and unit tests could not. The route was
// "/sitemap-:name.xml", which reads as obviously correct — but gin captures the
// whole segment after a literal prefix, so the extension belongs to the param,
// and a pattern with .xml AFTER the placeholder matches with an EMPTY param:
//
//	"/sitemap-:name.xml" + GET /sitemap-static.xml -> 200, param="" (!)
//	"/sitemap-:name"     + GET /sitemap-static.xml -> 200, param="static.xml"
//
// No panic, no error — every sub-sitemap silently 404'd while /sitemap.xml
// looked perfect.
func TestSitemapRoutes_IndexLinksResolve(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c := memory.New()
	gen := sitemap.New(sitemap.Config{
		BaseURL:     "http://localhost:8090",
		StaticPaths: []string{"/", "/search"},
		TTL:         time.Hour,
	}, c)
	if _, err := gen.Generate(context.Background()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	w := &web{cache: c}
	r := gin.New()
	r.GET("/sitemap.xml", func(gc *gin.Context) { w.serveSitemap(gc, "index") })
	r.GET("/sitemap-:name", func(gc *gin.Context) {
		w.serveSitemap(gc, strings.TrimSuffix(gc.Param("name"), ".xml"))
	})

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}

	code, body := get("/sitemap.xml")
	if code != 200 {
		t.Fatalf("GET /sitemap.xml = %d, want 200", code)
	}

	// Follow every <loc> the index advertises, exactly as a crawler would.
	locs := extractLocs(body)
	if len(locs) == 0 {
		t.Fatal("index advertises no sitemaps")
	}
	for _, loc := range locs {
		path := strings.TrimPrefix(loc, "http://localhost:8090")
		code, _ := get(path)
		if code != 200 {
			t.Errorf("index links %s but GET %s = %d — a crawler following the index finds nothing", loc, path, code)
		}
	}
}

// Before generation there must be no sitemap at all. An empty 200 tells a
// crawler the site has no pages, which is a delisting signal; 404 says "later".
func TestSitemapRoutes_404BeforeGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := &web{cache: memory.New()}
	r := gin.New()
	r.GET("/sitemap.xml", func(gc *gin.Context) { w.serveSitemap(gc, "index") })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/sitemap.xml", nil))
	if rec.Code != 404 {
		t.Errorf("ungenerated sitemap = %d, want 404 — an empty 200 reads as 'this site has no pages'", rec.Code)
	}
}

func extractLocs(xml string) []string {
	var out []string
	rest := xml
	for {
		i := strings.Index(rest, "<loc>")
		if i < 0 {
			return out
		}
		rest = rest[i+len("<loc>"):]
		j := strings.Index(rest, "</loc>")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j:]
	}
}
