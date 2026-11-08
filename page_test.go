package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"html/template"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer opens a Kuma database built by stmts and loads it once.
func newTestServer(t *testing.T, stmts ...string) *server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kuma.db")
	w, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append([]string{`CREATE TABLE placeholder (x INTEGER)`}, stmts...) {
		if _, err := w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	w.Close()
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &server{db: db, cfgDir: t.TempDir(), tmpl: template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML))}
	s.reload()
	return s
}

func get(t *testing.T, s *server, path string) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest("GET", "http://sla.example.org"+path, nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b), rec.Header().Get("Content-Security-Policy")
}

// A failed refresh is logged; visitors see a generic notice, never the
// database error (table names, file paths).
func TestDatabaseErrorNotShown(t *testing.T) {
	s := newTestServer(t) // no Kuma tables: compute fails with "no such table"
	if !s.snap.Failed {
		t.Fatal("refresh did not fail")
	}
	page, _ := get(t, s, "/")
	if strings.Contains(page, "no such table") || strings.Contains(page, "monitors:") {
		t.Error("page shows the database error")
	}
	if !strings.Contains(page, "could not read the Uptime Kuma data") || !strings.Contains(page, "não foi possível ler") {
		t.Error("page lacks the generic notice in both languages")
	}
}

// Every page carries a CSP that allows exactly its own inline blocks, and the
// pages have no inline style="" or event-handler attributes the CSP would block.
func TestContentSecurityPolicy(t *testing.T) {
	s := newTestServer(t,
		`CREATE TABLE monitor (id INTEGER PRIMARY KEY, name TEXT, active INTEGER, url TEXT, hostname TEXT, port INTEGER)`,
		`CREATE TABLE stat_daily (monitor_id INTEGER, timestamp INTEGER, up INTEGER, down INTEGER)`,
		`CREATE TABLE heartbeat (monitor_id INTEGER, status INTEGER, time TEXT)`,
		`INSERT INTO monitor VALUES (1,'www.example.com',1,'https://www.example.com',NULL,NULL)`,
	)
	if s.snap.Failed {
		t.Fatal("refresh failed")
	}
	for _, path := range []string{"/", "/release-notes"} {
		page, csp := get(t, s, path)
		for _, want := range []string{"default-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows unsafe inline code: %q", path, csp)
		}
		blocks := inlineRe.FindAllStringSubmatch(page, -1)
		if len(blocks) == 0 {
			t.Errorf("%s: no inline blocks found", path)
		}
		for _, m := range blocks {
			sum := sha256.Sum256([]byte(m[2]))
			if !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
				t.Errorf("%s: inline <%s> not allowed by the CSP", path, m[1])
			}
		}
		if n := strings.Count(page, "<script") + strings.Count(page, "<style"); n != len(blocks) {
			t.Errorf("%s: %d script/style tags, %d hashed (tag with attributes?)", path, n, len(blocks))
		}
		for _, bad := range []string{" style=", " onclick=", " onload="} {
			if strings.Contains(page, bad) {
				t.Errorf("%s: inline attribute %q is blocked by the CSP", path, bad)
			}
		}
	}
}

// The edge injects the bolha-bar <script> before </head> (or after <head>, or before </body>):
// the page must keep those tags or the menu never shows.
func TestPageHasHeadForTheEdgeBar(t *testing.T) {
	tmpl := template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML))
	var b strings.Builder
	if err := tmpl.Execute(&b, buildView("example.org", snapshot{}, nil)); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"<head>", "</head>", "<body>", "</body>"} {
		if !strings.Contains(b.String(), tag) {
			t.Errorf("page has no %s", tag)
		}
	}
}
