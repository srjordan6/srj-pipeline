package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The abstract is rebuilt by the works spine's twoaiOAAbstract, reused here
// rather than copied. This pins the behaviour this stage relies on.
func TestHRAbstractReuse(t *testing.T) {
	inv := map[string][]int{"Lp(a)": {0, 4}, "is": {1}, "causal.": {2}, "Lowering": {3}, "matters.": {5}}
	if got, want := twoaiOAAbstract(inv), "Lp(a) is causal. Lowering Lp(a) matters."; got != want {
		t.Fatalf("abstract = %q, want %q", got, want)
	}
	if twoaiOAAbstract(nil) != "" {
		t.Fatal("empty index should give an empty abstract")
	}
}

func TestHRDOI(t *testing.T) {
	cases := map[string]string{
		"https://doi.org/10.1016/S0140-6736(25)01438-2": "10.1016/s0140-6736(25)01438-2",
		"http://dx.doi.org/10.1056/NEJMoa2506549":       "10.1056/nejmoa2506549",
		"doi:10.2337/db25-0037":                         "10.2337/db25-0037",
		"  10.1001/JAMA.2024.21957 ":                    "10.1001/jama.2024.21957",
		"":                                              "",
		"https://openalex.org/W123":                     "",
		"10.1234":                                       "",
	}
	for in, want := range cases {
		if got := twoaiHRDOI(in); got != want {
			t.Errorf("twoaiHRDOI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHRLicense(t *testing.T) {
	cases := map[string]string{
		"cc-by":        "cc-by",
		"CC BY 4.0":    "cc-by",
		"cc-by-nc-nd":  "cc-by-nc-nd",
		"cc_by_nc_sa":  "cc-by-nc-sa",
		"cc-by-nd-4.0": "cc-by-nd",
		"https://creativecommons.org/licenses/by-nc/4.0/":   "cc-by-nc",
		"http://creativecommons.org/publicdomain/zero/1.0/": "cc0",
		"cc0":                                "cc0",
		"public-domain":                      "public-domain",
		"publisher-specific-oa":              "publisher-specific-oa",
		"other-oa":                           "other-oa",
		"implied-oa":                         "implied-oa",
		"Elsevier-specific: OA user license": "elsevier-specific: oa user license",
		"":                                   "",
	}
	for in, want := range cases {
		if got := twoaiHRLicense(in); got != want {
			t.Errorf("twoaiHRLicense(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHRPermitted(t *testing.T) {
	for _, ok := range []string{"cc-by", "cc-by-sa", "cc-by-nc", "cc-by-nc-sa", "cc-by-nd", "cc-by-nc-nd", "cc0", "public-domain"} {
		if !twoaiHRPermitted(ok) {
			t.Errorf("%s should be permitted", ok)
		}
	}
	for _, no := range []string{"", "closed", "publisher-specific-oa", "other-oa", "implied-oa", "oa-no-licence", "arxiv-nonexclusive"} {
		if twoaiHRPermitted(no) {
			t.Errorf("%s should not be permitted", no)
		}
	}
	// Every raw form a CC licence arrives in must clear the gate once
	// normalised.
	for _, raw := range []string{"CC BY 4.0", "https://creativecommons.org/licenses/by-nc-nd/4.0/", "cc_by_sa"} {
		if !twoaiHRPermitted(twoaiHRLicense(raw)) {
			t.Errorf("%q should be permitted after normalising", raw)
		}
	}
}

var hrKeyOK = regexp.MustCompile(`^corpus/[A-Za-z0-9._/-]+$`)

func TestHRR2Key(t *testing.T) {
	if got, want := twoaiHRR2Key("10.1056/nejmoa2506549"), "corpus/health-papers/10.1056_nejmoa2506549.pdf"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if got := twoaiHRR2Key("https://doi.org/10.1056/NEJMoa2506549"); got != "corpus/health-papers/10.1056_nejmoa2506549.pdf" {
		t.Fatalf("key from a resolver URL = %q", got)
	}
	a := twoaiHRR2Key("10.1016/s0140-6736(25)01438-2")
	b := twoaiHRR2Key("10.1016/s0140-6736-25-01438-2")
	if a == b {
		t.Fatalf("lossy key collided with a plain DOI: %s", a)
	}
	for _, doi := range []string{"10.1016/s0140-6736(25)01438-2", "10.1000/a..b", "10.1000/x<y>;z", "10.1161/circ.152.suppl_3.4369873"} {
		k := twoaiHRR2Key(doi)
		// The srj site Worker's /api/archive route accepts exactly this.
		if !hrKeyOK.MatchString(k) || strings.Contains(k, "..") {
			t.Errorf("key %q for %q would be refused by the archive route", k, doi)
		}
		if !strings.HasPrefix(k, "corpus/health-papers/") || !strings.HasSuffix(k, ".pdf") {
			t.Errorf("key %q is outside corpus/health-papers/", k)
		}
	}
}

func hrQuery(t *testing.T, key string) twoaiHRQuery {
	for _, q := range twoaiHRQueries {
		if q.key == key {
			return q
		}
	}
	t.Fatalf("no query %s", key)
	return twoaiHRQuery{}
}

func TestHRSubtopicMapping(t *testing.T) {
	cases := []struct {
		key, text, want string
	}{
		{"lpa-olpasiran", "anything at all", "rna"},
		{"lpa-muvalaplin", "oral", "oral"},
		{"lpa-ctx320", "", "gene"},
		{"lpa-gene-editing", "", "gene"},
		{"lpa-aortic", "", "aortic"},
		{"dmed-glp1", "", "glp1"},
		{"dmed-triple-amylin", "", "nextgen"},
		{"dmed-oral-glp1", "", "nextgen"},
		{"dmed-weekly-insulin", "", "insulin-beta"},
		{"dmed-cgm-aid", "", "tech"},
		{"dmed-remission-surgery", "", "weight"},
		{"dmed-teplizumab-immuno", "", "type1"},
		{"dmed-stem-hypoimmune", "", "insulin-beta"},
		{"dmed-beta-regen", "", "insulin-beta"},
		// Split queries.
		{"dmed-sglt2-outcomes", "Dapagliflozin in chronic kidney disease", "kidney"},
		{"dmed-sglt2-outcomes", "Empagliflozin in heart failure with preserved ejection fraction", "heart"},
		{"dmed-sglt2-outcomes", "Kidney and cardiovascular outcomes with canagliflozin", "kidney"},
		{"dmed-sglt2-outcomes", "SGLT2 inhibitors and glycaemic control", "kidney"},
		{"dmed-gene-therapy", "Gene-edited hypoimmune islets in type 1 diabetes", "type1"},
		{"dmed-gene-therapy", "AAV gene therapy for type 2 diabetes in mice", "pipeline"},
		// The general Lp(a) search uses the health watch's rules.
		{"lpa-general", "Lipoprotein(a) and calcific aortic valve stenosis", "aortic"},
		{"lpa-general", "Pelacarsen lowers Lp(a) in a phase 2 study", "rna"},
		{"lpa-general", "Lp(a) and its relation to body weight", "overview"},
	}
	for _, c := range cases {
		got, _ := twoaiHRSubtopic(hrQuery(t, c.key), c.text)
		if got != c.want {
			t.Errorf("%s %q: subtopic %q, want %q", c.key, c.text, got, c.want)
		}
	}
	// Every subtopic a query can produce is a real sub-hub suffix (or the
	// content project's overview), stored without the hub prefix.
	valid := map[string]map[string]bool{
		"dmed": {"glp1": true, "nextgen": true, "glucose": true, "weight": true, "heart": true, "kidney": true, "tech": true, "insulin-beta": true, "type1": true, "pipeline": true},
		"lpa":  {"understanding": true, "testing": true, "cvd": true, "current": true, "rna": true, "oral": true, "gene": true, "trials": true, "aortic": true, "future": true, "overview": true},
	}
	seen := map[string]bool{}
	for _, q := range twoaiHRQueries {
		if seen[q.key] {
			t.Errorf("duplicate query key %s", q.key)
		}
		seen[q.key] = true
		if q.sub != "" && !valid[q.topic][q.sub] {
			t.Errorf("%s: subtopic %q is not a %s sub-hub", q.key, q.sub, q.topic)
		}
		if q.sub == "" && q.classify == nil {
			t.Errorf("%s has neither a subtopic nor a classifier", q.key)
		}
		if strings.Contains(q.sub, "dmed-") || strings.Contains(q.sub, "lpa-") {
			t.Errorf("%s: subtopic %q carries the hub prefix", q.key, q.sub)
		}
		if q.floor < 1990 || q.floor > 2026 {
			t.Errorf("%s: floor %d looks wrong", q.key, q.floor)
		}
		// The ambiguous queries must still land on a valid hub for any text.
		for _, text := range []string{"", "kidney", "heart failure", "type 1 diabetes", "aortic stenosis", "base editing", "statin", "phase 2"} {
			if sub, _ := twoaiHRSubtopic(q, text); !valid[q.topic][sub] {
				t.Errorf("%s %q: subtopic %q is not a %s sub-hub", q.key, text, sub, q.topic)
			}
		}
	}
}

func TestHRCureAndCitations(t *testing.T) {
	for _, c := range []struct {
		topic, sub string
		want       bool
	}{
		{"dmed", "insulin-beta", true}, {"dmed", "type1", true}, {"dmed", "pipeline", true},
		{"lpa", "gene", true}, {"lpa", "rna", true},
		{"dmed", "glp1", false}, {"lpa", "aortic", false}, {"lpa", "insulin-beta", false},
	} {
		if got := twoaiHRCure(c.topic, c.sub); got != c.want {
			t.Errorf("cure(%s,%s) = %v", c.topic, c.sub, got)
		}
	}
	if !twoaiHRHighCited(2010, 50, 2026) || twoaiHRHighCited(2010, 49, 2026) {
		t.Error("old papers need 50 citations")
	}
	if !twoaiHRHighCited(2025, 10, 2026) || !twoaiHRHighCited(2026, 12, 2026) || twoaiHRHighCited(2024, 30, 2026) {
		t.Error("papers from this year or last need 10, older ones 50")
	}
}

func TestHRPDFChecks(t *testing.T) {
	if !twoaiHRIsPDF([]byte("%PDF-1.7\n...")) || !twoaiHRIsPDF([]byte("\n\n%PDF-1.4")) {
		t.Error("a PDF header should be recognised")
	}
	if twoaiHRIsPDF([]byte("<!DOCTYPE html><html>Just a moment...</html>")) {
		t.Error("an HTML challenge page is not a PDF")
	}
	if !twoaiHRPDFType("application/pdf") || !twoaiHRPDFType("application/octet-stream") || twoaiHRPDFType("text/html; charset=utf-8") {
		t.Error("content type check is wrong")
	}
}

// The work shape must pick up the venue, the authors and the outer
// best_oa_location, which shadows the spine's narrower field.
func TestHRWorkDecode(t *testing.T) {
	raw := `{"id":"https://openalex.org/W1","doi":"https://doi.org/10.1/X","title":"T","publication_year":2025,
		"cited_by_count":7,"authorships":[{"author":{"display_name":"A One"}},{"author":{"display_name":"B Two"}}],
		"primary_location":{"source":{"display_name":"NEJM"}},
		"best_oa_location":{"is_oa":true,"pdf_url":"https://x/p.pdf","license":"cc-by"}}`
	var w twoaiHRWork
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	if w.venue() != "NEJM" || w.CitedBy != 7 || w.PubYear != 2025 {
		t.Fatalf("decoded %+v", w)
	}
	if w.authorsJSON() != `["A One","B Two"]` {
		t.Fatalf("authors %s", w.authorsJSON())
	}
	if w.Best == nil || !w.Best.IsOA || w.Best.PDFURL != "https://x/p.pdf" || w.Best.License != "cc-by" {
		t.Fatalf("best location %+v", w.Best)
	}
	if twoaiHRDOI(w.DOI) != "10.1/x" {
		t.Fatalf("doi %s", twoaiHRDOI(w.DOI))
	}
}

func TestHRBridgeBody(t *testing.T) {
	papers := []hrNewPaper{
		{"Islets without immunosuppression", "10.1056/nejmoa2503822", "dmed", "insulin-beta", 2025, 64},
		{"Olpasiran extension", "10.1016/j.jacc.2024.05.058", "lpa", "rna", 2024, 67},
	}
	b := twoaiHRBridgeBody(papers, 2, 1, "the last bridge row", 2026)
	for _, want := range []string{"dmed / insulin-beta: 1", "lpa / rna: 1", "10.1056/nejmoa2503822", "Top 2 new papers"} {
		if !strings.Contains(b, want) {
			t.Errorf("bridge body lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "\u2014") {
		t.Error("bridge body has an em dash")
	}
}
