package main

import (
	"html/template"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBilingualPage(t *testing.T) {
	var cfg domainConfig
	src := "title: SLA · x\nsubtitle: { pt: \"serviços\", en: \"services\" }\ngroups:\n  - name: { pt: Ferramentas, en: Tools }\n    include: [\"a\"]\n  - name: Plain\n    include: [\"b\"]\nfooter:\n  - { text: { en: \"only en\" } }\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Footer[0].Text.PT != "only en" {
		t.Fatalf("fallback: %+v", cfg.Footer[0].Text)
	}
	snap := snapshot{byID: map[int]*Service{}, byName: map[string]*Service{}}
	for i, n := range []string{"a", "b"} {
		snap.Services = append(snap.Services, Service{ID: i + 1, Name: n, SLA: map[string]*float64{}})
	}
	for i := range snap.Services {
		snap.byName[snap.Services[i].Name] = &snap.Services[i]
	}
	v := buildView("sla.example.org", snap, &cfg)
	var b strings.Builder
	if err := template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML)).Execute(&b, v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`<span class="i-pt">Ferramentas</span><span class="i-en">Tools</span>`,
		`<h2>Plain</h2>`,
		`<span class="i-pt">serviços</span><span class="i-en">services</span>`,
		`data-en="SLA · x"`,
		`<span class="i-pt">Serviço</span>`,
		`kuma-sla-lang`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// the fallback page (no config) is bilingual too
	b.Reset()
	template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML)).Execute(&b, buildView("other.host", snap, nil))
	if !strings.Contains(b.String(), "todos os monitores") {
		t.Error("fallback subtitle not translated")
	}
}

// The example config always parses; the local domains/ dir (deployment
// configs, not tracked in git) is checked too when it exists.
func TestDomainConfigsParse(t *testing.T) {
	for _, dir := range []string{"examples/domains", "domains"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.y*ml"))
		if len(files) == 0 {
			if dir == "examples/domains" {
				t.Fatal("no example config")
			}
			continue
		}
		cfgs := (&server{cfgDir: dir}).loadConfigs()
		if len(cfgs) != len(files) {
			t.Fatalf("%s: %d of %d configs parsed", dir, len(cfgs), len(files))
		}
		for host, c := range cfgs {
			for _, g := range c.Groups {
				if g.Name.PT == "" || g.Name.EN == "" {
					t.Errorf("%s: group without name", host)
				}
			}
		}
	}
}
