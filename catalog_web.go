package main

import (
	"context"

	"github.com/ameNZB/loon/catalog"

	"github.com/ameNZB/loon-plugins/scraper"
)

// Host side of the scraper enrichment flow. scraper.SetDeps runs BEFORE Boot,
// but the catalog capability (the sink + cover store) only exists AFTER Boot —
// so these resolve lazily off the web struct, whose catalog fields main.go
// fills once Boot has run. The scraper's jobs run post-Boot, so by call time
// the fields are set.

// lazySink is a pluginapi.CatalogSink that forwards to the catalog plugin once
// it's resolved.
type lazySink struct{ w *web }

func (l lazySink) Upsert(ctx context.Context, e catalog.CatalogEntry) error {
	if l.w.catalogSink == nil {
		return nil
	}
	return l.w.catalogSink.Upsert(ctx, e)
}

// catalogCandidates yields recent releases for the scraper's Catalog Match job.
func (w *web) catalogCandidates(ctx context.Context) ([]scraper.Candidate, error) {
	if w.usenet == nil {
		return nil, nil
	}
	rs, err := w.usenet.Browse(ctx, "", 200)
	if err != nil {
		return nil, err
	}
	out := make([]scraper.Candidate, 0, len(rs))
	for _, r := range rs {
		out = append(out, scraper.Candidate{ID: r.ID, Title: r.Title, Category: r.CategoryID})
	}
	return out, nil
}

// linkCover records a matched cover for a release (read back on the release page).
func (w *web) linkCover(ctx context.Context, releaseID int64, coverURL string) error {
	if w.catalogCovers == nil {
		return nil
	}
	return w.catalogCovers.SetReleaseCover(ctx, releaseID, coverURL)
}
