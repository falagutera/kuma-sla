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
- **Docker** to run the published image — or Go 1.24+ to build from source.

## Quick start (Docker)

The published image is `registry.gutocarvalho.net/open/kuma-sla:latest`.

```sh
docker run --rm -p 127.0.0.1:3002:3002 \
  -v /opt/uptimekuma/data:/data:ro \
  -v "$PWD/examples/domains":/config:ro \
  registry.gutocarvalho.net/open/kuma-sla:latest
# open http://127.0.0.1:3002  (send Host: example.com to hit example.com.yaml)
```

Or with Compose — see [`contrib/docker-compose.yml`](contrib/docker-compose.yml):

```sh
docker compose -f contrib/docker-compose.yml up -d
```

The whole Kuma data dir is mounted (not just `kuma.db`) so SQLite can read the
`-wal`/`-shm` files. It's mounted read-only — kuma-sla never writes.

<details><summary>Run from source (no Docker)</summary>

```sh
make build
KUMA_DB=/path/to/kuma.db CONFIG_DIR=./examples/domains LISTEN=127.0.0.1:3002 ./kuma-sla-linux-amd64
```
</details>

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

Set `label_by: host` at the top of a file to label rows by each monitor's
host/URL (URL host, or `hostname:port`) instead of its Kuma name; the default is
`name`. A per-entry `label` always overrides it.

See [`examples/domains/example.com.yaml`](examples/domains/example.com.yaml).

## Install (Docker, recommended)

Run it as a container next to Uptime Kuma.

```sh
# 1. lay out the host directory
sudo mkdir -p /opt/kuma-sla/domains

# 2. add one config per status domain (see examples/domains/)
sudo tee /opt/kuma-sla/domains/sla.example.com.yaml >/dev/null <<'YAML'
title: SLA · example.com
subtitle: our public services
label_by: host
groups:
  - name: Websites
    include:
      - "glob:*.example.com"
YAML

# 3. compose file
sudo tee /opt/kuma-sla/docker-compose.yml >/dev/null <<'YAML'
services:
  kuma-sla:
    image: registry.gutocarvalho.net/open/kuma-sla:latest
    container_name: kuma-sla
    restart: always
    ports:
      - "127.0.0.1:3002:3002"
    volumes:
      - /opt/uptimekuma/data:/data:ro   # Kuma's data dir (kuma.db + -wal/-shm)
      - /opt/kuma-sla/domains:/config:ro
YAML

# 4. start it
cd /opt/kuma-sla && sudo docker compose up -d
```

Then point a TLS reverse proxy (nginx/Caddy/Traefik) at `127.0.0.1:3002` for each
domain and aim each domain's DNS at the host.

**Updating** — CI builds and pushes the image on every push to `main`, so a
deploy is just pulling the new image:

```sh
cd /opt/kuma-sla && docker compose pull && docker compose up -d
# or, from a workstation with SSH access:
make deploy HOST=my-kuma-host
```

**Config changes** need no redeploy — edit the files under `domains/` (mounted
into the container) and they're hot-reloaded within ~60s.

> Not using Docker? A hardened systemd unit is provided in
> [`contrib/kuma-sla.service`](contrib/kuma-sla.service) as an alternative.

## Versions

Versions look like `X.Y.Z-BUILD`. `X.Y.Z` is in the `VERSION` file and follows SemVer, and `BUILD` is the GitLab pipeline number. A local build shows `X.Y.Z-dev`. The page footer shows the version and links to the [container registry](https://gitlab.gutocarvalho.net/open/kuma-sla/container_registry).

| Image tag | Moves? |
|---|---|
| `X.Y.Z-BUILD` | Never. Pin this one in production. |
| `X.Y.Z` | On each build of that version. |
| `latest` | On each build. |

## Roadmap

See [ROADMAP.md](ROADMAP.md) — notably a **MariaDB/MySQL backend** (for Kuma
instances not on SQLite) and an optional own-rollup store for history beyond
Kuma's retention.

## License

[MIT](LICENSE) © Guto Carvalho
