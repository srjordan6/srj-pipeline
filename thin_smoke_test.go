package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Offline replay of the 2026-08-30 CyrusOne capture: every saved page must
// parse to the same totals the hand-seeded ingest verified (25 campuses,
// 980 MW, 10,875,527 sqft). This test does not touch the network or the DB.
func TestThinParseCyrusOneReplay(t *testing.T) {
	files, _ := filepath.Glob("/tmp/c1/*.html")
	if len(files) == 0 {
		t.Skip("no captured pages present")
	}
	var mw float64
	var sq int64
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		slug := strings.TrimSuffix(filepath.Base(f), ".html")
		row, ok := thinParseCyrusOne(slug, string(b))
		if !ok {
			t.Errorf("%s: did not parse", slug)
			continue
		}
		n++
		mw += row.mw
		sq += row.profile["technical_space_sqft"].(int64)
		if row.state == "" || row.city == "" || row.profile["postal_code"] == "" {
			t.Errorf("%s: missing loc fields: %+v", slug, row)
		}
	}
	t.Logf("parsed=%d total_mw=%.0f total_sqft=%d", n, mw, sq)
	if n != 25 || mw != 980 || sq != 10875527 {
		t.Errorf("totals drifted: parsed=%d mw=%.0f sqft=%d (want 25 / 980 / 10875527)", n, mw, sq)
	}
}
