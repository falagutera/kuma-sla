# Roadmap

Rough, unordered list of things we'd like to add. PRs welcome.

## Backends
- [ ] **MariaDB/MySQL backend.** Uptime Kuma 2.x can be configured to use MariaDB
      instead of SQLite. Support reading `stat_daily`/`heartbeat` from MariaDB via a
      `KUMA_DB_TYPE` (sqlite|mariadb) + DSN, so kuma-sla works against either backend.
      The SQL is nearly identical; the main work is a second driver + connection config.
- [ ] Graceful fallback when `stat_daily` is absent (older Kuma 1.x): derive long
      windows from `heartbeat` within its retention.

## Data & accuracy
- [ ] Optional **own daily rollup store** so history can exceed Kuma's retention
      (keep SLA beyond the ~1y `stat_daily` window, independent of Kuma pruning).
- [ ] Duration-weighted uptime option (vs. current count-weighted `up/(up+down)`).
- [ ] **Coverage / gap awareness.** Time when Kuma itself was down produces no
      beats, so it silently drops out of `up/(up+down)` instead of showing as a gap.
      Derive expected beats per window from each monitor's interval, show coverage %
      next to the SLA, and optionally count unobserved time as unknown or down.
- [ ] Explicit handling / display of maintenance windows.
- [ ] Configurable SLA color thresholds and window set.

## UI
- [ ] Themeable output (CSS variables / drop-in stylesheet).
- [ ] Per-service detail (incident list, monthly breakdown).
- [ ] Group-level rollups (a single SLA number per group).

## Ops
- [x] Container image + example docker-compose (sidecar to Uptime Kuma).
- [ ] Multi-arch image (arm64 in addition to amd64).
- [ ] Prometheus `/metrics` export of the computed SLAs.
- [ ] GitHub Actions workflow mirroring the GitLab CI.
