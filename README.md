# kuma-sla

A small, self-hosted **SLA dashboard for [Uptime Kuma](https://github.com/louislam/uptime-kuma)**.
It answers the one question a status page makes you squint for: *what's my uptime per service, per window?*

```
Service                 today     7d       30d      90d      6m       1y
mastodon | example.com  100.000%  99.786%  99.567%  99.073%  99.537%  99.748%
```

It reads Uptime Kuma's database **read-only** (never writes), computes uptime over
`today / 7d / 30d / 90d / 6m / 1y`, and serves a single, theme-aware HTML page.
Which monitors show up on which hostname is driven by a **YAML file per domain**, so
one instance can serve any number of status domains.

## Why

Uptime Kuma is great, but its status page doesn't give a compact, per-service SLA
table across long windows. `kuma-sla` reuses the data Kuma already keeps — the
`stat_daily` aggregates (a full year by default) for the long windows and the raw
`heartbeat` table for *today* — so you get real numbers with zero extra collection.

## How it works

- **Long windows** (`7d…1y`) come from Kuma's `stat_daily` table: `uptime% = up / (up + down)`.
- **today** comes from the `heartbeat` table (freshest, intraday). Maintenance and
  pending beats are excluded from the denominator.
- Everything is recomputed on a timer (~60s) and cached; each HTTP request just renders.
- Requests are routed by `Host` header to the matching domain config.

## Requirements

- **Uptime Kuma 2.x** with a **SQLite** database (the `stat_daily` table must exist).
- Read access to the `kuma.db` file (run this next to Kuma, or mount the volume read-only).
- Go 1.24+ to build (single static binary, no CGO).

## Quick start

```sh
# build a static linux/amd64 binary
make build

# run locally against a Kuma DB
KUMA_DB=/path/to/kuma.db CONFIG_DIR=./examples/domains LISTEN=127.0.0.1:3002 ./kuma-sla-linux-amd64
# open http://127.0.0.1:3002  (Host: example.com to hit examples/domains/example.com.yaml)
```

### Configuration (env)

| Variable     | Default                          | Description                                  |
|--------------|----------------------------------|----------------------------------------------|
| `KUMA_DB`    | `/opt/uptimekuma/data/kuma.db`   | Path to Kuma's SQLite DB (opened read-only). |
| `LISTEN`     | `127.0.0.1:3002`                 | Listen address. Put TLS in front.            |
| `CONFIG_DIR` | `/opt/kuma-sla/domains`          | Directory of per-domain YAML configs.        |

### Domain configs

One file per hostname, named `<hostname>.yaml`, in `CONFIG_DIR`. Add as many as you
like — there's no limit. Files are hot-reloaded (~60s). A request whose `Host`
matches no file falls back to listing every active monitor.

```yaml
title: SLA · example.com
subtitle: our public services
groups:
  - name: Websites
    include:
      - "www.example.com"          # exact monitor name (case-insensitive)
      - "id:12"                    # by monitor id
      - "glob:*.example.com"       # glob on the name
  - name: APIs
    include:
      - { name: "api.example.com", label: "Public API" }   # map form; label renames a single match
```

See [`examples/domains/example.com.yaml`](examples/domains/example.com.yaml).

## Deploy

`kuma-sla` is a single binary. A typical install:

1. Put the binary at `/opt/kuma-sla/kuma-sla` and your configs in `/opt/kuma-sla/domains/`.
2. Install the systemd unit from [`contrib/kuma-sla.service`](contrib/kuma-sla.service)
   (it runs hardened, as a `DynamicUser`, reading the DB read-only).
3. Point a TLS reverse proxy (nginx/Caddy/Traefik) at `127.0.0.1:3002` for each domain,
   and aim each domain's DNS at the host.

From a workstation with SSH access you can do all of the copy/restart in one step:

```sh
make deploy HOST=my-kuma-host
```

Local files under `domains/` (git-ignored) are treated as your real, deployment-specific
configs and shipped by `make deploy`.

## Roadmap

See [ROADMAP.md](ROADMAP.md) — notably a **MariaDB/MySQL backend** (for Kuma
instances not on SQLite) and an optional own-rollup store for history beyond
Kuma's retention.

## License

[MIT](LICENSE) © Guto Carvalho
