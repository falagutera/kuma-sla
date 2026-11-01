# Changelog

All notable changes to kuma-sla. Versions follow [SemVer](https://semver.org/);
container images are published as `registry.gutocarvalho.net/images/kuma-sla:<version>`.

## 0.3.1 - 2026-10-31

- Versions are now `X.Y.Z-BUILD`, from the `VERSION` file and the CI pipeline.

## v0.3.0 — 2026-09-28

### Added
- **Version on the page**: shown in the top summary bar and as a footer link
  to the releases page. Set at build time (`-X main.version=...`); the image
  build passes the git tag (or short commit) as `VERSION`.

### Changed
- **New colour bands**: 100% is green (the accent colour), 99–<100% is
  purple, 95–<99% amber, <95% red. Perfect rows (★) use green.

## v0.2.1 — 2026-09-28

### Changed
- The ★ on perfect rows is larger and glows, so it no longer reads as a dot.
- A row is "perfect" when it is 100% in today and in every window that has
  data, with at least 30 days of history (v0.2.0 demanded a full year, which
  no real service met).

## v0.2.0 — 2026-09-28

### Added
- **Perfect-uptime highlight.** Cells that are exactly 100% (no down beats)
  render in a new purple `--perfect` color (theme-aware). Rows that are 100%
  in *every* window get a purple service label, a ★ and an accent bar. The
  legend gains a matching swatch.

### Changed
- **Windows now require history.** A long window (7d/30d/90d/6m/1y) is only
  reported once the monitor's history in `stat_daily` covers it (2 days of
  slack). Before, a monitor created yesterday showed "100% over 1y"; now those
  windows render as "—". This can turn previously shown values into "—" for
  young monitors — the numbers were not backed by data.

## v0.1.0 — 2026-09-17

First public version: per-service SLA (today/7d/30d/90d/6m/1y) read-only from
the Uptime Kuma 2.x SQLite database, per-domain YAML curation with hot reload,
`label_by: host`, configurable footer, dark/light theme, container image.
