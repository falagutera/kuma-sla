# AGENTS.md

Notes for coding agents (and people) changing kuma-sla. The README says what it does and how to
install it; this file says how to change it without breaking the promises the README makes.

## What it is

One Go binary (`main.go`), pure-Go SQLite driver (`modernc.org/sqlite`) and a YAML parser, no
JavaScript framework. It opens Uptime Kuma's `kuma.db` **read-only**, recomputes every 60 s
(`server.reload`, 20 s timeout) and serves one HTML page per `Host`, plus `/healthz`,
`/release-notes` and `/release-notes.json`.

## Commands

```sh
gofmt -l .        # must print nothing
go vet ./...
go test ./...     # no external services: the SLA test builds its own SQLite file
```

CI (`.gitlab-ci.yml`) runs only `go vet` and `gofmt`, then builds; it does **not** run `go test`,
so run it yourself before every commit.

To look at the page, run it against a small fixture database (`sqlite3` CLI, or a copy of a real
`kuma.db` with its `-wal`/`-shm` files) and send the `Host` of a config:

```sh
P=55691   # any free port
D=$(mktemp -d) && sqlite3 $D/kuma.db "CREATE TABLE monitor (id INTEGER PRIMARY KEY, name TEXT, active INTEGER, url TEXT, hostname TEXT, port INTEGER);
  CREATE TABLE stat_daily (monitor_id INTEGER, timestamp INTEGER, up INTEGER, down INTEGER);
  CREATE TABLE heartbeat (monitor_id INTEGER, status INTEGER, time TEXT);
  INSERT INTO monitor VALUES (1,'www.example.com',1,'https://www.example.com',NULL,NULL);" &&
  KUMA_DB=$D/kuma.db CONFIG_DIR=examples/domains LISTEN=127.0.0.1:$P go run .
curl -H 'Host: example.com' http://127.0.0.1:$P/      # another Host = the fallback page
```

`make vet` (vet + gofmt, fails on unformatted files like CI), `make test` and `make run` (`go run .`
with the env above) are shortcuts for the same.

## Map

| Where | What |
|---|---|
| `main.go` `main`, `openDB` | env (`KUMA_DB`, `LISTEN`, `CONFIG_DIR`), read-only DB, routes, refresh loop |
| `main.go` `compute` | the three queries: `monitor` (active only), `stat_daily` (long windows), `heartbeat` (today, UTC) |
| `main.go` `windows`, `coverageSlackDays`, `pct` | window set, history-coverage rule, `up / (up + down)` |
| `main.go` `domainConfig`, `text`, `incl`, `loadConfigs` | per-domain YAML (`<host>.yaml`), bilingual texts, selectors |
| `main.go` `buildView`, `resolve`, `rowFrom`, `viewRow.Perfect` | config → page: groups, `id:N` / `glob:` / exact name, labels, the ★ |
| `main.go` `tmplFuncs`, `pageHTML` | colour bands in `cell`, the whole page (CSS and the PT/EN switch script inline) |
| `version.go`, `VERSION`, `CHANGELOG.md` | version string and the release notes (embedded) |
| `internal/relnotes/` | copy of the shared relnotes library: never edit here, it is overwritten by its `sync.sh` |
| `examples/domains/` | the documented config example; `domains/` is local deployment config, not tracked |
| `contrib/` | docker-compose and a hardened systemd unit |
| `Makefile` | `build` (static linux/amd64), `vet`, `test`, `run`, `image`; `deploy HOST=<host>` pulls the image and restarts compose on that host over SSH |

Tests: `sla_test.go` (`TestComputeFromKumaDB`: fixture Kuma DB in a temp dir), `render_test.go`
(`TestBilingualPage`, `TestDomainConfigsParse`: `examples/domains`, and `domains/` when present),
`changelog_test.go` (`TestChangelogHasVersion`).

## Rules that must hold

1. **Never write to Kuma's database.** `openDB` uses `mode=ro` and `query_only`; the container
   mounts the data dir `:ro`. Only `SELECT`s, and only on `monitor`, `stat_daily`, `heartbeat`.
   `TestComputeFromKumaDB` fails if a write succeeds.
2. **Honest numbers.** A window is reported only when the monitor's first `stat_daily` day covers
   it (minus `coverageSlackDays`); otherwise it is `nil` and renders `—`. Today counts status 1 (up)
   and 0 (down) only; pending (2) and maintenance (3) are out of the denominator. No samples → `nil`,
   never 100%. The ★ (`Perfect`) needs 7d and 30d data and no value under 100.
   Guarded by `TestComputeFromKumaDB`.
3. **What a hostname shows is the config's choice.** Only active monitors; a monitor appears once
   per page (`seen` in `buildView`); a `label` applies only to a single match. A `Host` with no
   config lists every active monitor by name (documented fallback): keep it that way or change the
   README too.
4. **Both languages.** Every fixed text is a `span.i-pt` + `span.i-en` pair; config texts accept a
   plain string or `{pt, en}`, a missing side falls back to the other (`TestBilingualPage`).
5. **Escaping.** Monitor names, labels and config texts reach the page through `html/template`
   or `text.HTML` (which escapes). Any new `template.HTML` must escape its input.
6. **Bounded work.** One refresh per 60 s with a 20 s timeout and 2 DB connections; requests only
   render the cached snapshot. Do not query the database from a handler (except `/healthz`'s ping).
7. **Config compatibility.** Existing YAML keeps working: selectors (`"name"`, `"id:N"`,
   `"glob:…"`, map form `select|name|id|glob` + `label`), `label_by: name|host`, top-level
   `include` without groups, `footer`. Update `examples/domains/example.com.yaml` and the README
   with any new key.

## The usual change (a new option or column)

0. **Check it is not there already**: read `domainConfig` and the example config; the ROADMAP
   lists what is planned.
1. Code in `main.go`; new SQL goes in `compute` (never in a handler) and reads only Kuma tables.
2. A fixture row and an assertion in `TestComputeFromKumaDB` (for numbers) or `TestBilingualPage`
   (for the page), including the case that must not show.
3. Texts in both languages; the option in `examples/domains/example.com.yaml` and the README.
   A new **text key** (one that takes a string or `{pt, en}`) is documented in three places: the
   README's YAML block, the README sentence "Every text in a domain file (`title`, `subtitle`, …)",
   and the header comment of the example YAML ("Texts (title, subtitle, …)").
4. `gofmt`, `go vet`, `go test`, then look at the page (see Commands).

## Releasing

- `VERSION` holds `X.Y.Z`; CI builds `X.Y.Z-BUILD` (pipeline number via `-X main.build`). A local
  build shows `X.Y.Z-dev`.
- Bump: a fix or small visual change = patch; a new feature or option = minor; a change that needs
  new config at install = minor, with that line in **bold** at the top of the entry; breaking =
  major; docs, tests or CI only = no bump. Work not released yet goes under `## Unreleased`
  (the release notes page skips it).
- Every version needs its `CHANGELOG.md` entry (`TestChangelogHasVersion`); it is also the
  `/release-notes` page, so write it for the people who run kuma-sla.
- Code, comments, commits and the README in English. Small commits with a message that says why.

## Security checks

The page is public and has no login. Before a release, check that a change does not expose more
than the configured monitors (names, URLs, error text) and that nothing user-controlled from the
request (`Host`, query) is rendered unescaped.
