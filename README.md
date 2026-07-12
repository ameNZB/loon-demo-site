# loon-demo-site

The smallest useful site built on [loon](../loon) — the plugin
framework extracted from the ameNZB indexer. This repo is the
framework's living documentation: `main.go` is what a HOST looks
like (every `core.Deps` seam wired with a minimal in-memory or
logging implementation), and `plugins/guestbook` is what a PLUGIN
looks like (own schema via declarative migrations, mounted routes,
auth gates, typed config, points, notifications, a scheduler job).

## Run it

```
docker compose up -d db
go run .
```

Then open **http://localhost:8090/** in a browser — a dark-themed indexer shell
with News / Search / Groups pages and a login. Log in as `alice` (admin) or
`bob` (user) — demo auth is a signed session cookie, no password. Admin pages
(`/admin/jobs`, `/admin/plugins`) then work in the browser for `alice`.

The Groups and Search pages render empty states until a Usenet crawler is wired
(the next step on the public-demo path). The guestbook JSON API still works too:

```
# anyone can read
curl http://localhost:8090/plugin/guestbook

# signing requires a user (demo auth = the X-Demo-User header)
curl -X POST http://localhost:8090/plugin/guestbook \
     -H "X-Demo-User: bob" -H "Content-Type: application/json" \
     -d '{"message":"hello from bob"}'
```

Signing earns points (`plugins.guestbook.points_per_entry` in the
host config, 5 here), notifies the site owner, and the response
carries the new balance. Watch the process log for the notification
line and the once-a-minute stats job.

## Admin dashboard

The demo also mounts an admin surface (gated by the admin role — in demo
auth that's `X-Demo-User: alice`):

```
# plugin manifest (core.AdminHandler)
curl -H "X-Demo-User: alice" http://localhost:8090/admin/plugins

# jobs + services table with run/pause controls (schedule.JobsAdminHandler)
curl -H "X-Demo-User: alice" http://localhost:8090/admin/jobs

# trigger a job manually
curl -X POST -H "X-Demo-User: alice" \
     "http://localhost:8090/admin/jobs/control?name=Metadata%20Fill&action=trigger"
```

Both pages are self-contained inline templates (they render without any host
template wiring). Alongside `guestbook`, the demo wires three plugins from the
sibling [`loon-plugins`](../loon-plugins) repo — `scraper`, `backups`, `stats` —
so their jobs (Metadata Fill, Backup, Stats Cache) show up on the jobs page.
Browser access needs the `X-Demo-User` header, which browsers can't set without
an extension — that's a demo limitation until real sessions land (Phase 1 of
the public-demo path).

## What to read

- `main.go` — the host: builds `core.Deps` from adapters
  (`core.NewAuth`, `core.NewPoints`, …), uses loon's
  batteries-included scheduler (`schedule.CoreScheduler` — job
  registry, run loop, off-peak gating; `schedule.LogSink` mirrors
  job logs to stdout), calls `core.New` (fails loud on any missing
  seam) then `core.Boot` (plugin migrations → topo-sort →
  Provision → Start).
- `plugins/guestbook/plugin.go` — the plugin: registers itself in
  `init()`, wires everything through the `*core.Core` mediator, and
  never imports anything host-specific.
- `plugins/guestbook/migrations/001_init.sql` — applied by the
  framework into the plugin's own Postgres schema on boot,
  tracked in `core.plugin_migrations`.

The reference production instance (ameNZB, ~15 plugins) is private;
this demo tracks the same framework version via the sibling-checkout
`replace` in `go.mod`.
