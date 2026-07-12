<p align="center">
  <img src="img/logo.png" alt="loon" width="180">
</p>

<h1 align="center">loon-demo-site</h1>

<p align="center">A working reference site built on the <a href="https://github.com/ameNZB/loon">loon</a> plugin framework.</p>

---

A small but real site on loon: it wires every `core.Deps` seam, boots the plugin
runtime against Postgres, and serves a browsable, dark-themed **Usenet indexer** —
news / search / groups / NZB download, an admin dashboard, and a setup wizard.
`main.go` is what a HOST looks like; the plugins come from
[loon-plugins](https://github.com/ameNZB/loon-plugins).

## Run it

```
docker compose up -d db
go run .
```

Open **http://localhost:8090/** and log in as **alice** (admin) or **bob** (user)
— demo auth is a signed session cookie, no password.

### Index some Usenet

1. Log in as **alice** → **Setup** (`/admin/usenet`).
2. Enter an NNTP server → **Test connection** → **Fetch group list**.
3. Enable a low-volume group → **Crawl now**.
4. Watch **Jobs** (`/admin/jobs`), then **Search** for a title and download the `.nzb`.

The indexer keeps only the last few days of posts (configurable), assembles
multi-file releases into a single NZB, and parses quality tags
(resolution / source / codec / audio / language) shown as badges in search.

## What's wired

- **Auth** — signed-cookie sessions + login (the `X-Demo-User` header still works
  for `curl`).
- **Admin** — `/admin/plugins` + `/admin/jobs` (both from loon) + the
  `/admin/usenet` setup wizard.
- **Plugins** (from loon-plugins) — `usenet` (the indexer), `scraper`, `backups`,
  `stats` — plus the local `guestbook` demo plugin.

## What to read

- `main.go` — the host: builds `core.Deps` from adapters, uses loon's scheduler
  (`schedule.CoreScheduler`), then `core.New` → `core.Boot`.
- `views.go` / `usenet_web.go` — the host-side pages, the session cookie, and the
  usenet capability wiring.
- `plugins/guestbook/` — the smallest possible plugin (own schema, routes, points,
  a job): the hello-world for writing your own.

The reference production instance (ameNZB) is private; this demo tracks the same
framework version via the sibling-checkout `replace` in `go.mod`.

## License

MIT
