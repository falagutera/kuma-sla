// kuma-sla — a per-service SLA dashboard built from Uptime Kuma's database.
//
// It reads kuma.db (SQLite) read-only and computes uptime per window
// (today / 7d / 30d / 90d / 6m / 1y) for each active monitor:
//   - long windows: the aggregated stat_daily table (up/down per day)
//   - "today": the heartbeat table (freshest, intraday)
//
// WHAT each hostname shows is defined by one YAML per domain (CONFIG_DIR):
// a file <hostname>.yaml with groups and monitor selectors (exact name,
// id:N or glob:pattern). Requests are routed by Host. When no config
// matches, it falls back to listing every active monitor.
//
// Static binary: the only external dependencies are the pure-Go SQLite
// driver and a YAML parser.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"

	"kuma-sla/internal/relnotes"
)

// Windows shown, in table order. "today" is handled separately (heartbeat).
var windows = []struct {
	Key  string
	Days int
}{
	{"7d", 7},
	{"30d", 30},
	{"90d", 90},
	{"6m", 180},
	{"1y", 365},
}

// coverageSlackDays is how many days short of a window a monitor's history may
// be and still have that window reported.
const coverageSlackDays = 2

// Service is the computed SLA of a monitor.
type Service struct {
	ID     int
	Name   string
	Target string              // host from url, or hostname[:port] (for label_by: host)
	Today  *float64            // nil = no data
	SLA    map[string]*float64 // window key -> % (nil = no data)
}

type snapshot struct {
	Generated time.Time
	Services  []Service // sorted by name
	byID      map[int]*Service
	byName    map[string]*Service // key = lowercased name
	Err       string
}

// ---- per-domain config (YAML) ----

type domainConfig struct {
	Title    text    `yaml:"title"`
	Subtitle text    `yaml:"subtitle"`
	LabelBy  string  `yaml:"label_by"` // "name" (default) or "host" (url host / hostname[:port])
	Include  []incl  `yaml:"include"`  // implicit single group (no heading)
	Groups   []group `yaml:"groups"`
	Footer   []link  `yaml:"footer"` // footer items; empty = default footer
}

// link is a footer item: a text and an optional URL.
type link struct {
	Text text   `yaml:"text"`
	URL  string `yaml:"url"`
}

// text is a bilingual string. In YAML it is either a plain string (same in
// both languages) or a map {pt: ..., en: ...}; a missing side falls back to
// the other one.
type text struct {
	PT, EN string
}

func (t *text) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		t.PT = strings.TrimSpace(node.Value)
		t.EN = t.PT
		return nil
	}
	var m struct {
		PT string `yaml:"pt"`
		EN string `yaml:"en"`
	}
	if err := node.Decode(&m); err != nil {
		return err
	}
	t.PT, t.EN = strings.TrimSpace(m.PT), strings.TrimSpace(m.EN)
	if t.PT == "" {
		t.PT = t.EN
	}
	if t.EN == "" {
		t.EN = t.PT
	}
	return nil
}

func both(s string) text { return text{PT: s, EN: s} }

// IsZero reports an empty text.
func (t text) IsZero() bool { return t.PT == "" && t.EN == "" }

// HTML renders the text for the page: one span per language when they differ.
func (t text) HTML() template.HTML {
	pt, en := template.HTMLEscapeString(t.PT), template.HTMLEscapeString(t.EN)
	if pt == en {
		return template.HTML(pt)
	}
	return template.HTML(`<span class="i-pt">` + pt + `</span><span class="i-en">` + en + `</span>`)
}

// defaultFooter is used when a domain defines no `footer:` (keeps the template
// neutral for anyone using the project outside its original deployment).
var defaultFooter = []link{
	{Text: text{PT: "feito com kuma-sla", EN: "powered by kuma-sla"}, URL: "https://gitlab.gutocarvalho.net/open/gcn/kuma-sla"},
	{Text: both("MIT")},
}

type group struct {
	Name    text   `yaml:"name"`
	Include []incl `yaml:"include"`
}

// incl is a selector entry: an exact name, "id:N", "glob:pattern", or the map
// {select|name|id|glob, label}. Everything normalizes to Select
// ("glob:..."/"id:..."/name).
type incl struct {
	Select string
	Label  string
}

func (i *incl) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		i.Select = strings.TrimSpace(node.Value)
		return nil
	}
	var m struct {
		Select string `yaml:"select"`
		Name   string `yaml:"name"`
		ID     int    `yaml:"id"`
		Glob   string `yaml:"glob"`
		Label  string `yaml:"label"`
	}
	if err := node.Decode(&m); err != nil {
		return err
	}
	i.Label = strings.TrimSpace(m.Label)
	switch {
	case m.Select != "":
		i.Select = strings.TrimSpace(m.Select)
	case m.Glob != "":
		i.Select = "glob:" + strings.TrimSpace(m.Glob)
	case m.ID != 0:
		i.Select = "id:" + strconv.Itoa(m.ID)
	case m.Name != "":
		i.Select = strings.TrimSpace(m.Name)
	}
	return nil
}

type server struct {
	db     *sql.DB
	tmpl   *template.Template
	cfgDir string

	mu      sync.RWMutex
	snap    snapshot
	configs map[string]*domainConfig // key = lowercased hostname
}

func main() {
	dbPath := env("KUMA_DB", "/opt/uptimekuma/data/kuma.db")
	listen := env("LISTEN", "127.0.0.1:3002")
	cfgDir := env("CONFIG_DIR", "/opt/kuma-sla/domains")
	refresh := 60 * time.Second

	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("open db (%s): %v", dbPath, err)
	}

	s := &server{db: db, cfgDir: cfgDir, tmpl: template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML))}
	s.reload() // first synchronous load (data + configs)

	go func() {
		t := time.NewTicker(refresh)
		defer t.Stop()
		for range t.C {
			s.reload()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	relnotes.Register(mux, relnotes.PageOptions{
		App: "kuma-sla", Version: version, LangKey: "kuma-sla-lang",
		Source: "https://gitlab.gutocarvalho.net/open/gcn/kuma-sla",
		Style:  relnotes.StyleOf(pageHTML),
	}, releases)
	mux.HandleFunc("/", s.handleIndex)

	log.Printf("kuma-sla listening on %s (db=%s, config=%s)", listen, dbPath, cfgDir)
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// openDB opens Kuma's SQLite database read-only: mode=ro and query_only, so
// kuma-sla can never write to it (TestComputeFromKumaDB checks a write fails).
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ---- render ----

type viewRow struct {
	Label string
	Today *float64
	SLA   map[string]*float64
}

// Perfect reports whether the row is exactly 100% (no down beats at all) in
// today and in every window that has data, with at least 30 days of history
// (so a brand-new monitor never earns the highlight).
func (r viewRow) Perfect() bool {
	if r.SLA["7d"] == nil || r.SLA["30d"] == nil {
		return false
	}
	for _, v := range append([]*float64{r.Today}, slaValues(r.SLA)...) {
		if v != nil && *v < 100 {
			return false
		}
	}
	return true
}

func slaValues(m map[string]*float64) []*float64 {
	out := make([]*float64, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

type viewGroup struct {
	Name text
	Rows []viewRow
}
type view struct {
	Title     text
	Subtitle  text
	Host      string
	BrandHead string // first label of the host (e.g. "sla")
	BrandTLD  string // the rest of the host (e.g. "bolha.io")
	Count     int    // total services shown
	Generated time.Time
	Groups    []viewGroup
	Footer    []link
	Err       string
}

// brandParts splits the host at the first dot: "sla.bolha.io" -> ("sla","bolha.io").
func brandParts(host string) (string, string) {
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[:i], host[i+1:]
	}
	return host, ""
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	host := strings.ToLower(r.Host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}

	s.mu.RLock()
	snap := s.snap
	cfg := s.configs[host]
	s.mu.RUnlock()

	v := buildView(host, snap, cfg)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, v); err != nil {
		log.Printf("template: %v", err)
	}
}

// buildView resolves the domain config against the snapshot. With no config it
// lists every active monitor (fallback).
func buildView(host string, snap snapshot, cfg *domainConfig) view {
	v := view{Host: host, Generated: snap.Generated, Err: snap.Err, Footer: defaultFooter}
	v.BrandHead, v.BrandTLD = brandParts(host)
	if cfg == nil {
		v.Title = both("SLA · " + host)
		v.Subtitle = text{PT: "todos os monitores (sem configuração para este domínio)", EN: "all monitors (no config for this domain)"}
		rows := make([]viewRow, 0, len(snap.Services))
		for i := range snap.Services {
			rows = append(rows, rowFrom(&snap.Services[i], "", ""))
		}
		v.Groups = []viewGroup{{Rows: rows}}
		v.Count = len(rows)
		return v
	}
	v.Title = cfg.Title
	if v.Title.IsZero() {
		v.Title = both("SLA · " + host)
	}
	v.Subtitle = cfg.Subtitle
	if len(cfg.Footer) > 0 {
		v.Footer = cfg.Footer
	}

	groups := cfg.Groups
	if len(groups) == 0 && len(cfg.Include) > 0 {
		groups = []group{{Include: cfg.Include}}
	}
	seen := map[int]bool{}
	for _, g := range groups {
		vg := viewGroup{Name: g.Name}
		for _, in := range g.Include {
			matches := snap.resolve(in.Select)
			label := ""
			if len(matches) == 1 {
				label = in.Label // a label only makes sense for a single match
			}
			for _, svc := range matches {
				if seen[svc.ID] {
					continue
				}
				seen[svc.ID] = true
				vg.Rows = append(vg.Rows, rowFrom(svc, label, cfg.LabelBy))
			}
		}
		if len(vg.Rows) > 0 || !g.Name.IsZero() {
			v.Groups = append(v.Groups, vg)
			v.Count += len(vg.Rows)
		}
	}
	return v
}

// rowFrom builds a table row. An explicit YAML label always wins; otherwise the
// label is the monitor's host (when labelBy == "host" and a target exists) or its name.
func rowFrom(s *Service, explicit, labelBy string) viewRow {
	label := explicit
	if label == "" {
		if labelBy == "host" && s.Target != "" {
			label = s.Target
		} else {
			label = s.Name
		}
	}
	return viewRow{Label: label, Today: s.Today, SLA: s.SLA}
}

// resolve returns the services matching a selector (exact name / id:N / glob:pattern).
func (snap snapshot) resolve(sel string) []*Service {
	sel = strings.TrimSpace(sel)
	switch {
	case strings.HasPrefix(sel, "id:"):
		if id, err := strconv.Atoi(strings.TrimPrefix(sel, "id:")); err == nil {
			if s := snap.byID[id]; s != nil {
				return []*Service{s}
			}
		}
		return nil
	case strings.HasPrefix(sel, "glob:"):
		pat := strings.ToLower(strings.TrimPrefix(sel, "glob:"))
		var out []*Service
		for i := range snap.Services {
			if ok, _ := filepath.Match(pat, strings.ToLower(snap.Services[i].Name)); ok {
				out = append(out, &snap.Services[i])
			}
		}
		return out // Services is already sorted by name
	default:
		if s := snap.byName[strings.ToLower(sel)]; s != nil {
			return []*Service{s}
		}
		return nil
	}
}

// ---- load (data + configs) ----

func (s *server) reload() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfgs := s.loadConfigs()
	s.mu.Lock()
	s.configs = cfgs
	s.mu.Unlock()

	snap, err := s.compute(ctx)
	if err != nil {
		log.Printf("compute: %v", err)
		s.mu.Lock()
		s.snap.Err = err.Error()
		s.snap.Generated = time.Now()
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

func (s *server) loadConfigs() map[string]*domainConfig {
	out := map[string]*domainConfig{}
	entries, err := os.ReadDir(s.cfgDir)
	if err != nil {
		return out // no config dir -> fall back to listing everything
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.cfgDir, name))
		if err != nil {
			log.Printf("config %s: %v", name, err)
			continue
		}
		var c domainConfig
		if err := yaml.Unmarshal(b, &c); err != nil {
			log.Printf("config %s: invalid yaml: %v", name, err)
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml"))
		out[host] = &c
	}
	return out
}

func (s *server) compute(ctx context.Context) (snapshot, error) {
	now := time.Now().UTC()

	rows, err := s.db.QueryContext(ctx, `SELECT id, name, url, hostname, port FROM monitor WHERE active=1 ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return snapshot{}, fmt.Errorf("monitors: %w", err)
	}
	snap := snapshot{byID: map[int]*Service{}, byName: map[string]*Service{}}
	for rows.Next() {
		var id int
		var name string
		var url, hostname sql.NullString
		var port sql.NullInt64
		if err := rows.Scan(&id, &name, &url, &hostname, &port); err != nil {
			rows.Close()
			return snapshot{}, err
		}
		snap.Services = append(snap.Services, Service{
			ID:     id,
			Name:   name,
			Target: deriveTarget(url.String, hostname.String, port.Int64),
			SLA:    map[string]*float64{},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snapshot{}, err
	}
	for i := range snap.Services {
		sv := &snap.Services[i]
		snap.byID[sv.ID] = sv
		snap.byName[strings.ToLower(sv.Name)] = sv
	}

	// long windows from stat_daily (conditional aggregation in a single pass)
	var sb strings.Builder
	for _, w := range windows {
		th := now.AddDate(0, 0, -w.Days).Unix()
		fmt.Fprintf(&sb, ",SUM(CASE WHEN timestamp>=%d THEN up ELSE 0 END) AS up_%s"+
			",SUM(CASE WHEN timestamp>=%d THEN down ELSE 0 END) AS down_%s", th, w.Key, th, w.Key)
	}
	q := "SELECT monitor_id, MIN(timestamp)" + sb.String() + " FROM stat_daily GROUP BY monitor_id"
	drows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return snapshot{}, fmt.Errorf("stat_daily: %w", err)
	}
	for drows.Next() {
		vals := make([]any, 2+2*len(windows))
		var mid int
		var first sql.NullInt64
		vals[0] = &mid
		vals[1] = &first
		cells := make([]sql.NullInt64, 2*len(windows))
		for i := range cells {
			vals[i+2] = &cells[i]
		}
		if err := drows.Scan(vals...); err != nil {
			drows.Close()
			return snapshot{}, err
		}
		sv, ok := snap.byID[mid]
		if !ok {
			continue
		}
		for i, w := range windows {
			// A window is only reported once the monitor's history covers it;
			// otherwise a monitor created yesterday would show "100% over 1y".
			// Two days of slack absorb day-boundary rounding and retention pruning.
			if !first.Valid || first.Int64 > now.AddDate(0, 0, -w.Days+coverageSlackDays).Unix() {
				continue // stays nil -> rendered as "—" (no data)
			}
			sv.SLA[w.Key] = pct(cells[2*i].Int64, cells[2*i+1].Int64)
		}
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return snapshot{}, err
	}

	// "today" from heartbeat (UTC). status: 1=up, 0=down, 2=pending, 3=maintenance
	startToday := now.Format("2006-01-02") + " 00:00:00"
	hrows, err := s.db.QueryContext(ctx, `
		SELECT monitor_id,
		       SUM(CASE WHEN status=1 THEN 1 ELSE 0 END) AS up,
		       SUM(CASE WHEN status=0 THEN 1 ELSE 0 END) AS down
		FROM heartbeat WHERE time >= ? GROUP BY monitor_id`, startToday)
	if err != nil {
		return snapshot{}, fmt.Errorf("heartbeat: %w", err)
	}
	for hrows.Next() {
		var mid int
		var up, down sql.NullInt64
		if err := hrows.Scan(&mid, &up, &down); err != nil {
			hrows.Close()
			return snapshot{}, err
		}
		if sv, ok := snap.byID[mid]; ok {
			sv.Today = pct(up.Int64, down.Int64)
		}
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		return snapshot{}, err
	}

	sort.SliceStable(snap.Services, func(i, j int) bool {
		return strings.ToLower(snap.Services[i].Name) < strings.ToLower(snap.Services[j].Name)
	})
	snap.Generated = time.Now()
	return snap, nil
}

// deriveTarget builds the "host" label for a monitor: the hostname[:port] for
// port/ping monitors, or the host parsed from the URL for HTTP monitors. Returns
// "" when neither is available (the caller then falls back to the monitor name).
func deriveTarget(rawURL, hostname string, port int64) string {
	if hostname != "" {
		if port != 0 {
			return hostname + ":" + strconv.FormatInt(port, 10)
		}
		return hostname
	}
	if rawURL != "" {
		if u, err := neturl.Parse(rawURL); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return ""
}

// pct returns the uptime % or nil when there are no samples in the window.
func pct(up, down int64) *float64 {
	total := up + down
	if total == 0 {
		return nil
	}
	v := 100.0 * float64(up) / float64(total)
	return &v
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var tmplFuncs = template.FuncMap{
	"windows": func() []string {
		out := []string{"today"}
		for _, w := range windows {
			out = append(out, w.Key)
		}
		return out
	},
	"cell": func(row viewRow, key string) template.HTML {
		var v *float64
		if key == "today" {
			v = row.Today
		} else {
			v = row.SLA[key]
		}
		if v == nil {
			return template.HTML(`<td class="sla none">—</td>`)
		}
		// 100% green, 99–<100 purple, 95–<99 amber, <95 red
		cls := "ok"
		switch {
		case *v < 95:
			cls = "bad"
		case *v < 99:
			cls = "warn"
		case *v < 100:
			cls = "high"
		}
		if *v >= 99.9 {
			cls += " hi"
		}
		return template.HTML(fmt.Sprintf(`<td class="sla %s">%.3f%%</td>`, cls, *v))
	},
	"version": func() string { return version },
	"fmtTime": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") },
}

const pageHTML = `<!doctype html>
<html lang="pt">
<meta charset="utf-8">
<title data-pt="{{.Title.PT}}" data-en="{{.Title.EN}}">{{.Title.PT}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="{{.Title.EN}} — uptime per service (today/7d/30d/90d/6m/1y), via Uptime Kuma.">
<meta name="color-scheme" content="dark light">
<style>
  :root{
    --mono:ui-monospace,"SF Mono","Cascadia Code","JetBrains Mono",Menlo,Consolas,monospace;
    --sans:system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
    /* dark-navy world, mint signal accent */
    --ground:#0c111b;--panel:#111826;--panel-2:#0f1522;--surface:#141c2b;
    --ink:#e6ecf5;--muted:#93a1b5;--faint:#66748a;--line:#20293a;--line-2:#1a2230;
    --accent:#2fd8a6;--accent-ink:#5fe6bd;--accent-dim:#173a33;
    --ok:#2fd8a6;--warn:#e3a13a;--bad:#ef5f5f;--high:#b794ff;
    --shadow:0 1px 2px rgba(0,0,0,.35),0 12px 34px rgba(0,0,0,.4);
  }
  :root[data-theme="light"]{
    --ground:#eef2f7;--panel:#fff;--panel-2:#f5f8fb;--surface:#fff;
    --ink:#132033;--muted:#4f6076;--faint:#8091a5;--line:#dde5ef;--line-2:#e9eef5;
    --accent:#0f9d78;--accent-ink:#0c7e60;--accent-dim:#d6f2e8;
    --ok:#0f9d78;--warn:#b9791a;--bad:#d33c3c;--high:#7c4dff;
    --shadow:0 1px 2px rgba(20,30,45,.05),0 10px 26px rgba(20,30,45,.07);
  }
  @media (prefers-color-scheme:light){:root:not([data-theme]){
    --ground:#eef2f7;--panel:#fff;--panel-2:#f5f8fb;--surface:#fff;
    --ink:#132033;--muted:#4f6076;--faint:#8091a5;--line:#dde5ef;--line-2:#e9eef5;
    --accent:#0f9d78;--accent-ink:#0c7e60;--accent-dim:#d6f2e8;
    --ok:#0f9d78;--warn:#b9791a;--bad:#d33c3c;--high:#7c4dff;
    --shadow:0 1px 2px rgba(20,30,45,.05),0 10px 26px rgba(20,30,45,.07);
  }}
  *{box-sizing:border-box}
  body{margin:0;background:var(--ground);color:var(--ink);font-family:var(--sans);line-height:1.6;-webkit-font-smoothing:antialiased}
  .wrap{max-width:1200px;margin:0 auto;padding:clamp(20px,4vw,40px) clamp(16px,4vw,32px) 64px}
  a{color:inherit}

  .conn{display:flex;flex-wrap:wrap;align-items:center;gap:10px 26px;padding:12px 16px;
    background:var(--panel-2);border:1px solid var(--line);border-radius:12px;font-family:var(--mono);font-size:12.5px;margin-bottom:clamp(26px,5vw,44px)}
  .conn .k{color:var(--faint);letter-spacing:.12em;text-transform:uppercase;font-size:11px;margin-right:8px}
  .conn .v{color:var(--ink);font-variant-numeric:tabular-nums}
  .conn .live{margin-left:auto;display:inline-flex;align-items:center;gap:8px;color:var(--faint)}
  .conn .live .dot{width:7px;height:7px;border-radius:50%;background:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 22%,transparent)}

  header{margin-bottom:clamp(22px,4vw,36px)}
  .brand{font-family:var(--mono);font-weight:700;font-size:clamp(26px,5vw,40px);letter-spacing:-.02em;margin:0}
  .brand .dot{color:var(--accent)}
  .brand .tld{color:var(--faint);font-weight:600}
  .lede{margin:12px 0 0;max-width:62ch;color:var(--muted);font-size:clamp(15px,2vw,16.5px)}

  .cat{margin-top:clamp(24px,4vw,38px)}
  .cat-head{display:flex;align-items:center;gap:12px;margin:0 0 14px}
  .cat-head h2{font-family:var(--mono);font-size:12px;letter-spacing:.2em;text-transform:uppercase;color:var(--muted);margin:0;font-weight:600}
  .cat-head .rule{height:1px;background:var(--line);flex:1}

  .panel{background:var(--surface);border:1px solid var(--line);border-radius:14px;box-shadow:var(--shadow);overflow-x:auto}
  table{border-collapse:collapse;width:100%;min-width:640px}
  thead th{font-family:var(--mono);font-size:11px;letter-spacing:.08em;text-transform:uppercase;color:var(--faint);font-weight:600;text-align:right;padding:13px 16px;border-bottom:1px solid var(--line);white-space:nowrap}
  thead th.svc{text-align:left}
  tbody td{padding:11px 16px;text-align:right;border-bottom:1px solid var(--line-2);white-space:nowrap}
  tbody tr:last-child td{border-bottom:none}
  tbody tr:hover td{background:var(--panel-2)}
  td.svc{text-align:left;font-family:var(--mono);font-size:13.5px;color:var(--ink)}
  .sla{font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:13px;color:var(--muted)}
  .sla.ok{color:var(--ok)} .sla.warn{color:var(--warn)} .sla.bad{color:var(--bad)}
  .sla.hi{font-weight:700}
  .sla.high{color:var(--high)}
  tr.perfect td.svc{color:var(--ok);box-shadow:inset 3px 0 0 var(--ok)}
  tr.perfect td.svc::after{content:"★";display:inline-block;margin-left:10px;font-size:18px;line-height:1;vertical-align:-2px;text-shadow:0 0 10px color-mix(in srgb,var(--ok) 60%,transparent)}
  .sla.none{color:var(--faint)}
  .empty{color:var(--faint);font-family:var(--mono);font-size:13px;padding:16px}

  .legend{display:flex;gap:16px;flex-wrap:wrap;color:var(--faint);font-family:var(--mono);font-size:12px;margin:16px 2px 0}
  .legend .sw{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;vertical-align:middle}
  .err{background:color-mix(in srgb,var(--bad) 12%,transparent);border:1px solid var(--bad);color:var(--bad);padding:10px 14px;border-radius:12px;margin-bottom:16px;font-family:var(--mono);font-size:13px}

  footer{margin-top:48px;padding-top:20px;border-top:1px solid var(--line);display:flex;flex-wrap:wrap;gap:6px 18px;font-family:var(--mono);font-size:12.5px;color:var(--faint)}
  footer a{color:var(--muted);text-decoration:none;border-bottom:1px dotted var(--line)}
  footer a:hover{color:var(--accent-ink)}
  :root:not([lang="en"]) .i-en{display:none}
  :root[lang="en"] .i-pt{display:none}
  .langsw{display:flex;justify-content:flex-end;gap:6px;margin-bottom:12px}
  .langsw button{font-family:var(--mono);font-size:12px;color:var(--muted);background:transparent;
    border:1px solid var(--line);border-radius:999px;padding:3px 11px;cursor:pointer}
  .langsw button:hover{color:var(--accent-ink);border-color:color-mix(in srgb,var(--accent) 45%,var(--line))}
  .langsw button[aria-current="true"]{color:var(--ground);background:var(--accent);border-color:var(--accent)}
  @media (prefers-reduced-motion:reduce){*{transition:none!important}}
</style>

<div class="wrap">
  <div class="langsw" role="group" aria-label="Idioma / Language">
    <button type="button" data-set="pt">PT</button>
    <button type="button" data-set="en">EN</button>
  </div>
  <div class="conn" aria-label="summary">
    <span><span class="k">SLA</span><span class="v">{{.Count}} <span class="i-pt">serviços</span><span class="i-en">services</span></span></span>
    <span><span class="k"><span class="i-pt">fonte</span><span class="i-en">source</span></span><span class="v">Uptime Kuma</span></span>
    <span><span class="k"><span class="i-pt">versão</span><span class="i-en">version</span></span><span class="v">{{version}}</span></span>
    <span><span class="k"><span class="i-pt">atualizado</span><span class="i-en">updated</span></span><span class="v">{{fmtTime .Generated}}</span></span>
    <span class="live"><span class="dot"></span><span class="i-pt">ao vivo</span><span class="i-en">live</span></span>
  </div>

  <header>
    <h1 class="brand">{{.BrandHead}}{{if .BrandTLD}}<span class="dot">.</span><span class="tld">{{.BrandTLD}}</span>{{end}}</h1>
    {{if not .Subtitle.IsZero}}<p class="lede">{{.Subtitle.HTML}}</p>{{end}}
  </header>

  {{if .Err}}<div class="err"><span class="i-pt">erro ao ler os dados:</span><span class="i-en">error reading data:</span> {{.Err}}</div>{{end}}

  {{range .Groups}}
  <section class="cat">
    {{if not .Name.IsZero}}<div class="cat-head"><h2>{{.Name.HTML}}</h2><span class="rule"></span></div>{{end}}
    <div class="panel">
    {{if .Rows}}<table>
      <thead><tr><th class="svc"><span class="i-pt">Serviço</span><span class="i-en">Service</span></th>{{range windows}}<th>{{.}}</th>{{end}}</tr></thead>
      <tbody>
      {{range .Rows}}<tr{{if .Perfect}} class="perfect"{{end}}><td class="svc">{{.Label}}</td>{{$row := .}}{{range windows}}{{cell $row .}}{{end}}</tr>
      {{end}}
      </tbody>
    </table>{{else}}<div class="empty"><span class="i-pt">nenhum monitor corresponde à seleção deste grupo</span><span class="i-en">no monitors matched this group's selection</span></div>{{end}}
    </div>
  </section>
  {{end}}

  <div class="legend">
    <span><span class="sw" style="background:var(--ok)"></span>100% <span class="i-pt">(★ em todas as janelas, ≥ 30 dias de histórico)</span><span class="i-en">(★ every window, ≥ 30 days of history)</span></span>
    <span><span class="sw" style="background:var(--high)"></span>99–100%</span>
    <span><span class="sw" style="background:var(--warn)"></span>95–99%</span>
    <span><span class="sw" style="background:var(--bad)"></span>&lt; 95%</span>
    <span>— <span class="i-pt">sem dados (monitor mais novo que a janela)</span><span class="i-en">no data (monitor younger than the window)</span></span>
  </div>

  <footer>
    {{range .Footer}}{{if .URL}}<a href="{{.URL}}">{{.Text.HTML}}</a>{{else}}<span>{{.Text.HTML}}</span>{{end}}{{end}}
    <a href="/release-notes"><span class="i-pt">novidades</span><span class="i-en">release notes</span></a>
    <a href="https://gitlab.gutocarvalho.net/images/kuma-sla/container_registry">kuma-sla {{version}}</a>
  </footer>
</div>
<script>
(function(){
  var key="kuma-sla-lang", root=document.documentElement, b=document.querySelectorAll(".langsw button"), title=document.querySelector("title");
  function set(l){
    root.setAttribute("lang",l);
    try{ localStorage.setItem(key,l); }catch(e){}
    if(title) document.title=title.getAttribute("data-"+l)||document.title;
    for(var i=0;i<b.length;i++) b[i].setAttribute("aria-current", b[i].getAttribute("data-set")===l?"true":"false");
  }
  var l=null; try{ l=localStorage.getItem(key); }catch(e){}
  var q=/[?&]lang=(pt|en)/.exec(location.search); if(q) l=q[1];
  if(!l) l=(navigator.language||"pt").toLowerCase().indexOf("pt")===0?"pt":"en";
  set(l);
  for(var i=0;i<b.length;i++) b[i].addEventListener("click",function(){ set(this.getAttribute("data-set")); });
})();
</script>`
