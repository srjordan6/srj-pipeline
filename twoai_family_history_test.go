package main

import (
	"os"
	"strings"
	"testing"
)

func TestFamHistVerify(t *testing.T) {
	src := "Gemini was announced on December 6, 2023. Gemini 1.5 followed in February 2024. In 2025 the company added more."
	cases := []struct{ y, m, d, want string }{
		{"2023", "12", "06", "2023-12-06"}, // full date stated
		{"2024", "02", "15", "2024-02"},    // day not stated, cut to month
		{"2025", "03", "", "2025"},         // month not stated, cut to year
	}
	for _, c := range cases {
		got, ok := famHistVerify(c.y, c.m, c.d, src)
		if !ok || got != c.want {
			t.Errorf("%s-%s-%s: got %q %v, want %q", c.y, c.m, c.d, got, ok, c.want)
		}
	}
	if _, ok := famHistVerify("2019", "", "", src); ok {
		t.Error("a year the sources never state must be dropped")
	}
}

func TestFamCopied(t *testing.T) {
	src := "Gemini is a family of multimodal large language models developed by Google DeepMind and the successor to LaMDA and PaLM 2."
	g := famGrams(src)
	if !famCopied("Google says Gemini is a family of multimodal large language models developed by Google DeepMind.", g) {
		t.Error("an eight-word run from the source must be caught")
	}
	if famCopied("Google DeepMind built Gemini to replace its earlier LaMDA and PaLM 2 models.", g) {
		t.Error("own words flagged as copied")
	}
}

func TestFamHistCatalog(t *testing.T) {
	f := famHistFamily{name: "IBM Granite", members: []map[string]any{
		{"name": "IBM: Granite 4.2 8B", "released": "2026-08-31"},
		{"name": "IBM: Granite 4.0 Micro", "released": "2025-10-20"},
	}}
	h := famHistCatalog(f)
	if h.Basis != "catalog" || len(h.Entries) != 2 || h.Entries[0].Date != "2025-10-20" || strings.HasPrefix(h.Entries[0].Text, "IBM:") {
		t.Errorf("unexpected catalog history: %+v", h)
	}
}

// Reads the live Gemini article; run with TWOAI_NET_TEST=1.
func TestFamWikiFetchLive(t *testing.T) {
	if os.Getenv("TWOAI_NET_TEST") == "" {
		t.Skip("network")
	}
	w, err := famWikiFetch("Gemini_(language_model)")
	if err != nil {
		t.Fatal(err)
	}
	src := famHistSource(w.Extract)
	t.Logf("title=%q rev=%d size=%d links=%d source=%d chars", w.Title, w.RevID, w.Size, len(w.Links), len(src))
	if w.RevID == 0 || len(src) < 2000 {
		t.Error("article too thin")
	}
}
