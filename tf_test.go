package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Replay against real operator pages captured 2026-08-30. Each expectation is
// a figure read off the page by hand first, so a parser change that quietly
// starts reading a different number fails here.
func TestFacilityParseReplay(t *testing.T) {
	cases := []struct {
		file     string
		wantMW   float64
		wantSq   int64
		wantCert string
	}{
		// Figures read off each page by hand before the parser saw them.
		{"/tmp/fx_deer_valley.html", 5.02, 109476, "PCI DSS"},
		{"/tmp/fx_centennial.html", 1.6, 43294, "PCI DSS"},
		{"/tmp/cs_ch1.html", 0, 180000, "ISO 27001"}, // CoreSite publishes area, no megawatts
	}
	for _, c := range cases {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Skipf("no capture at %s", filepath.Base(c.file))
			return
		}
		r, ok := tfParse(string(b))
		t.Logf("%s -> mw=%v sqft=%v certs=%v postal=%q addr=%v ok=%v",
			filepath.Base(c.file), r.mw, r.sqft, r.certs, r.postal, r.address, ok)
		if c.wantMW > 0 && r.mw != c.wantMW {
			t.Errorf("%s: mw=%v want %v", c.file, r.mw, c.wantMW)
		}
		if c.wantSq > 0 && r.sqft != c.wantSq {
			t.Errorf("%s: sqft=%v want %v", c.file, r.sqft, c.wantSq)
		}
	}
}
