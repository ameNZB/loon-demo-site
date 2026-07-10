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

Then:

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

## What to read

- `main.go` — the host: builds `core.Deps` from adapters
  (`core.NewAuth`, `core.NewPoints`, `core.NewScheduler`, …),
  calls `core.New` (fails loud on any missing seam) then
  `core.Boot` (plugin migrations → topo-sort → Provision → Start).
- `plugins/guestbook/plugin.go` — the plugin: registers itself in
  `init()`, wires everything through the `*core.Core` mediator, and
  never imports anything host-specific.
- `plugins/guestbook/migrations/001_init.sql` — applied by the
  framework into the plugin's own Postgres schema on boot,
  tracked in `core.plugin_migrations`.

The reference production instance (ameNZB, ~15 plugins) is private;
this demo tracks the same framework version via the sibling-checkout
`replace` in `go.mod`.
