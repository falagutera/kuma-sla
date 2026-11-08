package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestComputeFromKumaDB builds a tiny Kuma database (the three tables
// kuma-sla reads) and checks the SLA rules: read-only access, inactive
// monitors hidden, a window shown only once history covers it, and pending
// or maintenance beats left out of today's denominator.
func TestComputeFromKumaDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kuma.db")
	w, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	day := func(ago int) int64 { return now.AddDate(0, 0, -ago).Unix() }
	stmts := []string{
		`CREATE TABLE monitor (id INTEGER PRIMARY KEY, name TEXT, active INTEGER, url TEXT, hostname TEXT, port INTEGER)`,
		`CREATE TABLE stat_daily (monitor_id INTEGER, timestamp INTEGER, up INTEGER, down INTEGER)`,
		`CREATE TABLE heartbeat (monitor_id INTEGER, status INTEGER, time TEXT)`,
		`INSERT INTO monitor VALUES (1,'old',1,'https://old.example/x',NULL,NULL),(2,'young',1,NULL,'young.example',443),(3,'off',0,NULL,NULL,NULL)`,
		// old: 400 days of history, one bad day 10 days ago; young: 10 days, all up
		fmt.Sprintf(`INSERT INTO stat_daily VALUES (1,%d,100,0),(1,%d,90,10),(1,%d,100,0),(2,%d,100,0),(2,%d,100,0),(3,%d,1,0)`,
			day(400), day(10), day(1), day(10), day(1), day(1)),
	}
	ts := now.Format("2006-01-02") + " 00:00:01"
	// today for old: 3 up, 1 down, plus pending (2) and maintenance (3) that must not count
	for _, st := range []int{1, 1, 1, 0, 2, 3} {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO heartbeat VALUES (1,%d,'%s')`, st, ts))
	}
	for _, q := range stmts {
		if _, err := w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	w.Close()

	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM monitor`); err == nil {
		t.Fatal("database opened writable")
	}

	snap, err := (&server{db: db}).compute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Services) != 2 || snap.byID[3] != nil {
		t.Fatalf("inactive monitor listed: %+v", snap.Services)
	}
	old, young := snap.byID[1], snap.byID[2]
	if old.Target != "old.example" || young.Target != "young.example:443" {
		t.Errorf("targets: %q %q", old.Target, young.Target)
	}
	if v := old.Today; v == nil || *v != 75 {
		t.Errorf("today = %v, want 75 (pending and maintenance excluded)", v)
	}
	if v := old.SLA["7d"]; v == nil || *v != 100 {
		t.Errorf("old 7d = %v, want 100", v)
	}
	if v := old.SLA["30d"]; v == nil || *v != 95 {
		t.Errorf("old 30d = %v, want 95", v)
	}
	if young.SLA["7d"] == nil || young.SLA["30d"] != nil || young.SLA["1y"] != nil {
		t.Errorf("young windows: 7d=%v 30d=%v 1y=%v (only windows its history covers)", young.SLA["7d"], young.SLA["30d"], young.SLA["1y"])
	}
	if rowFrom(young, "", "").Perfect() {
		t.Error("a monitor without 30 days of history got the ★")
	}
}
