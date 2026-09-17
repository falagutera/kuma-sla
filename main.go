// kuma-sla — dashboard de SLA por serviço a partir do banco do Uptime Kuma.
//
// Lê o kuma.db (SQLite) em modo read-only e calcula uptime por janela
// (today / 7d / 30d / 90d / 6m / 1y) para cada monitor ativo:
//   - janelas longas: tabela agregada stat_daily (up/down por dia)
//   - "today": tabela heartbeat (mais fresco, intradiário)
//
// O QUE cada hostname mostra é definido por um YAML por domínio (CONFIG_DIR):
// um arquivo <hostname>.yaml com grupos e seleção de monitores (nome exato,
// id:N ou glob:padrão). O request é roteado pelo Host. Sem config casando,
// cai num fallback que lista todos os monitores ativos.
//
// Binário estático: única dependência externa é o driver SQLite puro-Go.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Janelas exibidas, na ordem da tabela. "today" é tratado à parte (heartbeat).
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

// Service é o SLA calculado de um monitor.
type Service struct {
	ID    int
	Name  string
	Today *float64            // nil = sem dados
	SLA   map[string]*float64 // key da janela -> % (nil = sem dados)
}

type snapshot struct {
	Generated time.Time
	Services  []Service // ordenado por nome
	byID      map[int]*Service
	byName    map[string]*Service // chave = nome em minúsculas
	Err       string
}

// ---- config por domínio (YAML) ----

type domainConfig struct {
	Title    string  `yaml:"title"`
	Subtitle string  `yaml:"subtitle"`
	Include  []incl  `yaml:"include"` // grupo único implícito (sem cabeçalho)
	Groups   []group `yaml:"groups"`
}

type group struct {
	Name    string `yaml:"name"`
	Include []incl `yaml:"include"`
}

// incl é uma entrada de seleção: nome exato, "id:N", "glob:padrão" ou o mapa
// {select|name|id|glob, label}. Normaliza tudo em Select ("glob:..."/"id:..."/nome).
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
	configs map[string]*domainConfig // chave = hostname em minúsculas
}

func main() {
	dbPath := env("KUMA_DB", "/opt/uptimekuma/data/kuma.db")
	listen := env("LISTEN", "127.0.0.1:3002")
	cfgDir := env("CONFIG_DIR", "/opt/kuma-sla/domains")
	refresh := 60 * time.Second

	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		log.Fatalf("ping db (%s): %v", dbPath, err)
	}

	s := &server{db: db, cfgDir: cfgDir, tmpl: template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML))}
	s.reload() // primeira carga síncrona (dados + configs)

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
	mux.HandleFunc("/", s.handleIndex)

	log.Printf("kuma-sla ouvindo em %s (db=%s, config=%s)", listen, dbPath, cfgDir)
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// ---- render ----

type viewRow struct {
	Label string
	Today *float64
	SLA   map[string]*float64
}
type viewGroup struct {
	Name string
	Rows []viewRow
}
type view struct {
	Title     string
	Subtitle  string
	Host      string
	Generated time.Time
	Groups    []viewGroup
	Err       string
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

// buildView resolve a config do domínio contra o snapshot. Sem config -> lista
// todos os monitores ativos (fallback).
func buildView(host string, snap snapshot, cfg *domainConfig) view {
	v := view{Host: host, Generated: snap.Generated, Err: snap.Err}
	if cfg == nil {
		v.Title = "SLA · " + host
		v.Subtitle = "todos os monitores (sem config para este domínio)"
		rows := make([]viewRow, 0, len(snap.Services))
		for i := range snap.Services {
			rows = append(rows, rowFrom(&snap.Services[i], ""))
		}
		v.Groups = []viewGroup{{Rows: rows}}
		return v
	}
	v.Title = orDefault(cfg.Title, "SLA · "+host)
	v.Subtitle = cfg.Subtitle

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
				label = in.Label // label só faz sentido em match único
			}
			for _, svc := range matches {
				if seen[svc.ID] {
					continue
				}
				seen[svc.ID] = true
				vg.Rows = append(vg.Rows, rowFrom(svc, label))
			}
		}
		if len(vg.Rows) > 0 || g.Name != "" {
			v.Groups = append(v.Groups, vg)
		}
	}
	return v
}

func rowFrom(s *Service, label string) viewRow {
	if label == "" {
		label = s.Name
	}
	return viewRow{Label: label, Today: s.Today, SLA: s.SLA}
}

// resolve devolve os serviços que casam com um seletor (nome exato / id:N / glob:padrão).
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
		return out // Services já vem ordenado por nome
	default:
		if s := snap.byName[strings.ToLower(sel)]; s != nil {
			return []*Service{s}
		}
		return nil
	}
}

// ---- carga (dados + configs) ----

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
		return out // sem dir de config -> fallback pra todos
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
			log.Printf("config %s: yaml invalido: %v", name, err)
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml"))
		out[host] = &c
	}
	return out
}

func (s *server) compute(ctx context.Context) (snapshot, error) {
	now := time.Now().UTC()

	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM monitor WHERE active=1 ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return snapshot{}, fmt.Errorf("monitores: %w", err)
	}
	snap := snapshot{byID: map[int]*Service{}, byName: map[string]*Service{}}
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return snapshot{}, err
		}
		snap.Services = append(snap.Services, Service{ID: id, Name: name, SLA: map[string]*float64{}})
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

	// janelas longas via stat_daily (agregação condicional numa passada só)
	var sb strings.Builder
	for _, w := range windows {
		th := now.AddDate(0, 0, -w.Days).Unix()
		fmt.Fprintf(&sb, ",SUM(CASE WHEN timestamp>=%d THEN up ELSE 0 END) AS up_%s"+
			",SUM(CASE WHEN timestamp>=%d THEN down ELSE 0 END) AS down_%s", th, w.Key, th, w.Key)
	}
	q := "SELECT monitor_id" + sb.String() + " FROM stat_daily GROUP BY monitor_id"
	drows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return snapshot{}, fmt.Errorf("stat_daily: %w", err)
	}
	for drows.Next() {
		vals := make([]any, 1+2*len(windows))
		var mid int
		vals[0] = &mid
		cells := make([]sql.NullInt64, 2*len(windows))
		for i := range cells {
			vals[i+1] = &cells[i]
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
			sv.SLA[w.Key] = pct(cells[2*i].Int64, cells[2*i+1].Int64)
		}
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return snapshot{}, err
	}

	// "today" via heartbeat (UTC). status: 1=up,0=down,2=pending,3=maint
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

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
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
		cls := "green"
		switch {
		case *v < 95:
			cls = "red"
		case *v < 99:
			cls = "amber"
		case *v < 99.9:
			cls = "teal"
		}
		return template.HTML(fmt.Sprintf(`<td class="sla %s">%.3f%%</td>`, cls, *v))
	},
	"fmtTime": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") },
}

const pageHTML = `<!doctype html>
<html lang="pt-br"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#f7f7f8;--fg:#1c1c1e;--muted:#6b7280;--card:#fff;--line:#e5e7eb;
 --green:#16a34a;--teal:#0891b2;--amber:#d97706;--red:#dc2626;}
@media (prefers-color-scheme:dark){:root{--bg:#0f1113;--fg:#e6e7e9;--muted:#9aa0a6;--card:#17191c;--line:#2a2d31;}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;padding:24px 16px}
.wrap{max-width:920px;margin:0 auto}
h1{font-size:20px;margin:0 0 2px}
.sub{color:var(--muted);font-size:13px;margin-bottom:18px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;overflow-x:auto;margin-bottom:18px}
.gname{font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);padding:12px 12px 0}
table{border-collapse:collapse;width:100%;min-width:640px}
th,td{padding:9px 12px;text-align:right;border-bottom:1px solid var(--line);white-space:nowrap}
th{font-size:11px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);font-weight:600}
th.svc,td.svc{text-align:left;width:40%}
tr:last-child td{border-bottom:none}
td.svc{font-weight:500}
.sla{font-variant-numeric:tabular-nums;font-feature-settings:"tnum"}
.green{color:var(--green)} .teal{color:var(--teal)} .amber{color:var(--amber)} .red{color:var(--red);font-weight:600}
.none{color:var(--muted)}
.legend{display:flex;gap:14px;flex-wrap:wrap;color:var(--muted);font-size:12px;margin:4px 2px 0}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:5px;vertical-align:middle}
.err{background:#dc26261a;border:1px solid var(--red);color:var(--red);padding:10px 12px;border-radius:10px;margin-bottom:14px}
.empty{color:var(--muted);padding:12px}
</style></head><body><div class="wrap">
<h1>{{.Title}}</h1>
<div class="sub">{{if .Subtitle}}{{.Subtitle}} · {{end}}fonte: Uptime Kuma · atualizado {{fmtTime .Generated}}</div>
{{if .Err}}<div class="err">erro ao ler dados: {{.Err}}</div>{{end}}
{{range .Groups}}
<div class="card">
{{if .Name}}<div class="gname">{{.Name}}</div>{{end}}
{{if .Rows}}<table>
<thead><tr><th class="svc">Serviço</th>{{range windows}}<th>{{.}}</th>{{end}}</tr></thead>
<tbody>
{{range .Rows}}<tr><td class="svc">{{.Label}}</td>{{$row := .}}{{range windows}}{{cell $row .}}{{end}}</tr>
{{end}}
</tbody></table>{{else}}<div class="empty">nenhum monitor casou com a seleção deste grupo</div>{{end}}
</div>
{{end}}
<div class="legend">
<span><span class="dot" style="background:var(--green)"></span>≥ 99,9%</span>
<span><span class="dot" style="background:var(--teal)"></span>99–99,9%</span>
<span><span class="dot" style="background:var(--amber)"></span>95–99%</span>
<span><span class="dot" style="background:var(--red)"></span>&lt; 95%</span>
<span>— sem dados na janela</span>
</div>
</div></body></html>`
