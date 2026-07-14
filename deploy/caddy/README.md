# Reverse proxy (Caddy) — the "site is down" front door

Two kinds of down need two mechanisms; this directory is the second one.

| Scenario | Who serves the page | Mechanism |
|---|---|---|
| **Planned** maintenance (you flip a switch to upgrade) | the **app** (it's up) | `loon-baseline/maintenance` — a 503 page + `/admin/p/maintenance` toggle. The API tier stays up. |
| **Unplanned** outage (crash, redeploy, OOM, restart) | the **proxy** (the app is dead) | this Caddy config — `502/504 → maintenance.html`, self-clearing. |

A job or middleware inside the app can only handle the planned case: a dead
process can't serve its own page. That's why the unplanned fallback has to live
in front, in the proxy.

## What the config does

- **Reverse-proxies** to the web app, with **active health checks** (`/healthz`)
  so you get **zero-downtime deploys**: drain a replica, deploy, and Caddy stops
  routing to it until it's healthy again.
- On **502 (unreachable) / 504 (timeout)** it serves the static
  `maintenance.html` and retries the origin on the next request — so it clears
  itself the moment the app is back.
- It deliberately does **not** catch **503**, so the app's own
  planned-maintenance page passes through.
- Optional commented block routes `/api` + `/rss` to a separate read tier
  (`loon-api`) so the Newznab API keeps serving while the web app is down.

## Run it

`LOON_UPSTREAM` defaults to the compose service name `web:8090`; override it to
point anywhere.

```sh
docker run --rm -p 80:80 \
  -e LOON_UPSTREAM=host.docker.internal:8090 \
  -v "$PWD/Caddyfile:/etc/caddy/Caddyfile" \
  -v "$PWD/maintenance.html:/srv/maintenance.html" \
  caddy:2-alpine
```

Then stop the app and reload — you'll get `maintenance.html`; start it and the
site returns on the next request.
