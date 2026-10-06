package main

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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
	resolved := []hrLeadLine{{claim: "Retatrutide phase 3 in 1,152 adults", doi: "10.1016/s0140-6736(26)01861-1", status: "found", attempts: 1}}
	open := []hrLeadLine{
		{claim: "ADA 2026 debate on frailty", status: "find primary", attempts: 3},
		{claim: "Endocrine Society, July 2026", status: "not found yet", attempts: 14},
	}
	b := twoaiHRBridgeBody(papers, 2, 1, "the last bridge row", 2026, resolved, open)
	for _, want := range []string{"dmed / insulin-beta: 1", "lpa / rna: 1", "10.1056/nejmoa2503822", "Top 2 new papers",
		"Leads resolved to a primary paper (1)", "Retatrutide phase 3 in 1,152 adults -> 10.1016/s0140-6736(26)01861-1",
		"Leads still without a primary paper (2)", "ADA 2026 debate on frailty (3 tries)", "(14 tries, not found yet, now tried weekly)"} {
		if !strings.Contains(b, want) {
			t.Errorf("bridge body lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "\u2014") {
		t.Error("bridge body has an em dash")
	}
	// Leads alone still make a message.
	if b := twoaiHRBridgeBody(nil, 0, 0, "x", 2026, nil, open); !strings.Contains(b, "Leads still without a primary paper (2)") {
		t.Errorf("a leads-only body lacks the open leads:\n%s", b)
	}
}

func TestHRStripJATS(t *testing.T) {
	cases := map[string]string{
		"": "",
		"<jats:title>Abstract</jats:title><jats:sec><jats:title>Background</jats:title><jats:p>Retatrutide lowered weight.</jats:p></jats:sec>":  "Background Retatrutide lowered weight.",
		"<jats:p>BMI over 30 kg/m<jats:sup>2</jats:sup> (<jats:italic>n</jats:italic> = 1152), <jats:italic>p</jats:italic> &lt;0.001.</jats:p>": "BMI over 30 kg/m2 (n = 1152), p <0.001.",
		"<abstract><p>Plain &amp; simple</p><p>Second paragraph</p></abstract>":                                                                  "Plain & simple Second paragraph",
		"Abstract: no tags at all":           "no tags at all",
		"<jats:p>Summary of trials</jats:p>": "Summary of trials",
	}
	for in, want := range cases {
		if got := twoaiHRStripJATS(in); got != want {
			t.Errorf("twoaiHRStripJATS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHRNumbers(t *testing.T) {
	got := twoaiHRNumbers("1,152 adults, 16.9% (9 mg), 143,715 people, 690 participants, EASD 2026, n=75, 41\u2009381 adults, -4.28 points, 16.925")
	for _, want := range []string{"1152", "143715", "690", "41381"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for _, not := range []string{"2026", "75", "16", "9", "428", "925"} {
		if got[not] {
			t.Errorf("%s should not count as a sample size: %v", not, got)
		}
	}
}

func TestHRDOIBatches(t *testing.T) {
	var dois []string
	for i := 0; i < 120; i++ {
		dois = append(dois, "10.1/x"+strconv.Itoa(i))
	}
	dois = append(dois, "10.1/a,b", "10.1/a|b")
	batches, apart := twoaiHRDOIBatches(dois, 50)
	if len(batches) != 3 || len(batches[0]) != 50 || len(batches[2]) != 20 {
		t.Fatalf("batches %d, sizes wrong", len(batches))
	}
	if len(apart) != 2 {
		t.Fatalf("a DOI with a comma or pipe must go apart, got %v", apart)
	}
}

// The leads the content project seeded on 2026-10-05, as the parser sees
// them.
func TestHRParseClaim(t *testing.T) {
	c := twoaiHRParseClaim("Retatrutide phase 3 in 1,152 adults with type 2 diabetes and obesity: 16.9% (9 mg) and 18.8% (12 mg) weight loss vs 5.1% placebo; EASD 2026 and The Lancet")
	if strings.Join(c.anchors, ",") != "retatrutide" || !c.numbers["1152"] || !c.conference || c.review {
		t.Errorf("retatrutide claim parsed as %+v", c)
	}
	if s := twoaiHRLeadSearch(c); !strings.HasPrefix(s, "(retatrutide) AND (") || !strings.Contains(s, `"type 2 diabetes"`) || strings.Contains(s, ",") {
		t.Errorf("search %q", s)
	}
	c = twoaiHRParseClaim("SURMOUNT-1 3-year post hoc, 690 participants with prediabetes: most regained under 5% after nadir, over 90% reverted to normoglycaemia")
	if strings.Join(c.anchors, ",") != "surmount-1" || !c.numbers["690"] {
		t.Errorf("SURMOUNT claim parsed as %+v", c)
	}
	if !c.anchorRes[0].MatchString("in the SURMOUNT 1 trial") {
		t.Error("a trial name must match without its hyphen")
	}
	c = twoaiHRParseClaim("Phase 2 trials of bimagrumab, apitegromab and trevogromab with GLP-1 RAs to preserve lean mass; side effects and higher cholesterol noted")
	if strings.Join(c.anchors, ",") != "bimagrumab,apitegromab,trevogromab" || c.class != "" {
		t.Errorf("myostatin claim parsed as %+v", c)
	}
	c = twoaiHRParseClaim("AUA 2026 abstract PD26-01 (Guillen-Lozoya): 1,629 men on semaglutide or tirzepatide, total and free testosterone rose")
	if strings.Join(c.anchors, ",") != "semaglutide,tirzepatide" || !c.conference {
		t.Errorf("AUA claim parsed as %+v", c)
	}
	c = twoaiHRParseClaim("ADA 2026 Scientific Sessions debate: no evidence GLP-1 RAs uniquely worsen frailty or sarcopenia")
	if len(c.anchors) != 0 || c.class != "glp-1" || !c.conference {
		t.Errorf("debate claim parsed as %+v", c)
	}
	c = twoaiHRParseClaim("Prospective cohort of 143,715 adults: 2 or more hours of resistance training weekly linked to 27% lower type 2 diabetes risk; JAMA Network Open")
	if len(c.anchors) != 0 || c.class != "" {
		t.Errorf("cohort claim parsed as %+v", c)
	}
	if s := twoaiHRLeadSearch(c); s != `"type 2 diabetes" AND "resistance training"` {
		t.Errorf("cohort search %q", s)
	}
}

func TestHRVenueAndAuthor(t *testing.T) {
	claim := "Remote pharmacist and navigator obesity medication programme, 75 adults, Obesity Science and Practice; access barriers"
	if !twoaiHRVenueNamed(claim, "Obesity Science & Practice") {
		t.Error("journal named in the claim was missed")
	}
	if twoaiHRVenueNamed(claim, "Obesity") || twoaiHRVenueNamed("type 2 diabetes in adults", "Diabetes") {
		t.Error("a one word journal title must not match an ordinary word")
	}
	if !twoaiHRVenueNamed("EASD 2026 and The Lancet", "The Lancet") || twoaiHRVenueNamed("EASD 2026 and The Lancet", "The Lancet Diabetes & Endocrinology") {
		t.Error("the Lancet check is wrong")
	}
	if a := twoaiHRAuthorNamed("AUA 2026 abstract PD26-01 (Guillen-Lozoya): 1,629 men", []string{"Ana Smith", "Juan Guill\u00e9n\u2010Lozoya"}); a == "" {
		t.Error("accented, unicode hyphenated surname was missed")
	}
	if a := twoaiHRAuthorNamed("Weight loss was larger in men", []string{"Mary Weight", "J. Larger"}); a != "" {
		t.Errorf("an ordinary word matched author %q", a)
	}
}

func hrTestCand(doi, title, abstract, venue, typ string) hrCandidate {
	return hrCandidate{doi: doi, title: title, abstract: abstract, venue: venue, typ: typ, from: "openalex", year: 2026}
}

func TestHRScoreLead(t *testing.T) {
	cl := twoaiHRParseClaim("Retatrutide phase 3 in 1,152 adults with type 2 diabetes and obesity: 16.9% (9 mg) and 18.8% (12 mg) weight loss vs 5.1% placebo; EASD 2026 and The Lancet")
	primary := hrTestCand("10.1016/s0140-6736(26)01861-1", "Retatrutide in adults with obesity and type 2 diabetes (TRIUMPH-2): a double-blind, randomised, placebo-controlled, phase 3 trial", "", "The Lancet", "article")
	review := hrTestCand("10.1007/s42399-026-02580-9", "Efficacy and Safety of High-Dose Retatrutide in Adults with Obesity and Type 2 Diabetes: A Systematic Review", "", "SN Comprehensive Clinical Medicine", "review")
	news := hrTestCand("10.1211/pj.2026.1.413195", "Phase III retatrutide study demonstrates 30% weight loss", "", "Pharmaceutical Journal", "journal-article")
	mdedge := hrTestCand("10.12788/x", "Retatrutide in type 2 diabetes and obesity", "", "MDedge Endocrinology", "journal-article")
	preprint := hrTestCand("10.21203/rs.3.rs-1/v1", "Retatrutide in adults with obesity and type 2 diabetes, phase 3", "", "Research Square", "preprint")
	peer := hrTestCand("10.1111/dom.71200/v1/review2", "Retatrutide in type 2 diabetes and obesity", "", "", "peer-review")
	other := hrTestCand("10.1/other", "Tirzepatide in adults with obesity and type 2 diabetes", "", "Diabetes Care", "article")

	if m := twoaiHRScoreLead(cl, primary); !m.ok || !m.journal {
		t.Errorf("the primary paper should match: %+v", m)
	}
	if m := twoaiHRScoreLead(cl, review); m.ok {
		t.Errorf("a review should not match a trial claim: %+v", m)
	}
	for _, c := range []hrCandidate{news, mdedge, peer} {
		if m := twoaiHRScoreLead(cl, c); m.ok {
			t.Errorf("%s (%s) should never match", c.doi, c.venue)
		}
	}
	if m := twoaiHRScoreLead(cl, other); m.ok {
		t.Errorf("a paper without the drug must not match: %+v", m)
	}
	best, _, note := twoaiHRPickLead(cl, []hrCandidate{review, news, preprint, primary, mdedge, peer, other})
	if best == nil || best.cand.doi != primary.doi {
		t.Fatalf("picked %+v (%s), want the Lancet paper", best, note)
	}
	if !strings.Contains(twoaiHRLeadNote(*best), "names retatrutide") {
		t.Errorf("note lacks the evidence: %s", twoaiHRLeadNote(*best))
	}
	// A preprint alone can match, a journal copy beats it.
	if best, _, _ := twoaiHRPickLead(cl, []hrCandidate{preprint}); best == nil {
		t.Error("a preprint with the drug and population should still match when nothing better exists")
	}

	// A sample size in the abstract picks the right post hoc paper.
	cl = twoaiHRParseClaim("SURMOUNT-1 3-year post hoc, 690 participants with prediabetes: most regained under 5% after nadir, over 90% reverted to normoglycaemia")
	right := hrTestCand("10.1/right", "Weight regain after nadir with tirzepatide in SURMOUNT-1", "A post hoc analysis of 690 participants with prediabetes.", "Diabetes Care", "article")
	wrong := hrTestCand("10.1/wrong", "Tirzepatide for obesity treatment and diabetes prevention (SURMOUNT-1)", "Of 1032 participants with prediabetes, three years of treatment.", "NEJM", "article")
	best, _, note = twoaiHRPickLead(cl, []hrCandidate{wrong, right})
	if best == nil || best.cand.doi != "10.1/right" || !best.strong {
		t.Errorf("picked %+v (%s), want the post hoc paper with the sample size", best, note)
	}

	// Two equally good candidates with nothing to tell them apart resolve
	// nothing.
	cl = twoaiHRParseClaim("Dapagliflozin lowered heart failure in type 2 diabetes")
	a := hrTestCand("10.1/a", "Dapagliflozin and heart failure in type 2 diabetes", "", "Diabetes Care", "article")
	b := hrTestCand("10.1/b", "Dapagliflozin, heart failure and type 2 diabetes outcomes", "", "Circulation", "article")
	if best, _, note := twoaiHRPickLead(cl, []hrCandidate{a, b}); best != nil || !strings.HasPrefix(note, "ambiguous") {
		t.Errorf("two equal candidates should be ambiguous, got %+v (%s)", best, note)
	}

	// No drug or trial: the sample size or journal must agree.
	cl = twoaiHRParseClaim("Prospective cohort of 143,715 adults: 2 or more hours of resistance training weekly linked to 27% lower type 2 diabetes risk; JAMA Network Open")
	cohort := hrTestCand("10.1001/jamanetworkopen.2026.1", "Resistance training and incident type 2 diabetes", "In this cohort of 143 715 adults, resistance training was associated with lower risk.", "JAMA Network Open", "article")
	vague := hrTestCand("10.1/vague", "Resistance training in type 2 diabetes", "Exercise helps.", "Sports Medicine", "article")
	if m := twoaiHRScoreLead(cl, cohort); !m.ok || !m.strong {
		t.Errorf("the cohort paper should match on its sample size and journal: %+v", m)
	}
	if m := twoaiHRScoreLead(cl, vague); m.ok {
		t.Errorf("a paper with only the subject must not match a claim without a drug: %+v", m)
	}

	// A debate names a class and nothing else: no paper can match it.
	cl = twoaiHRParseClaim("ADA 2026 Scientific Sessions debate: no evidence GLP-1 RAs uniquely worsen frailty or sarcopenia; most weight lost is fat; exercise and protein advised")
	if m := twoaiHRScoreLead(cl, hrTestCand("10.1/glp", "GLP-1 receptor agonists, frailty and sarcopenia in older adults", "weight loss and muscle", "Diabetes Care", "article")); m.ok {
		t.Errorf("the debate claim should not match a paper on the subject: %+v", m)
	}

	// A conference abstract matches a claim about one, with its author.
	cl = twoaiHRParseClaim("AUA 2026 abstract PD26-01 (Guillen-Lozoya): 1,629 men on semaglutide or tirzepatide, total and free testosterone rose, not explained by BMI change alone; retrospective")
	abs := hrCandidate{doi: "10.1097/01.ju.0001109944.1", title: "PD26-01 GLP-1 receptor agonists and testosterone in men", abstract: "1629 men on semaglutide or tirzepatide; testosterone rose.",
		venue: "The Journal of Urology", typ: "article", from: "crossref", authors: []string{"Juan Guillen-Lozoya"}}
	if m := twoaiHRScoreLead(cl, abs); !m.ok || m.kind != "conference" {
		t.Errorf("the AUA abstract should match as a conference abstract: %+v", m)
	}
	// Found live on 2026-10-05: two other meetings' abstracts on the same
	// drugs and testosterone, without the author or the count, must not.
	for _, c := range []hrCandidate{
		hrTestCand("10.1093/jsxmed/qdaf320.054", "(054) Early Outcomes of GLP-1 Receptor Agonists for Weight Management", "semaglutide or tirzepatide, testosterone in men, retrospective", "The Journal of Sexual Medicine", "article"),
		hrTestCand("10.1210/jendso/bvaf149.1941", "MON-708 Effect of Incretin-Based Weight Loss Drugs on Testosterone", "semaglutide or tirzepatide, testosterone in men, retrospective", "Journal of the Endocrine Society", "article"),
	} {
		if m := twoaiHRScoreLead(cl, c); m.ok || m.kind != "conference" {
			t.Errorf("%s should be a conference abstract that does not match: %+v", c.doi, m)
		}
	}
	// The abstract number alone is a strong sign.
	coded := hrTestCand("10.1097/01.ju.0001.2", "PD26-01 Testosterone in men on semaglutide or tirzepatide", "", "The Journal of Urology", "article")
	if m := twoaiHRScoreLead(cl, coded); !m.ok || !m.strong {
		t.Errorf("the abstract number should match: %+v", m)
	}

	// The real one, found live on Crossref: its title names neither drug, but
	// the author and the abstract number together are enough.
	real := hrCandidate{doi: "10.1097/01.ju.0001191720.97092.ae.01", title: "PD26-01 TESTOSTERONE LEVELS IMPROVE IN MEN UNDER GLP-1 RECEPTOR AGONIST THERAPY",
		venue: "Journal of Urology", typ: "journal-article", from: "crossref", authors: []string{"Andres H. Guillén-Lozoya"}}
	if m := twoaiHRScoreLead(cl, real); !m.ok || m.kind != "conference" || m.strongN != 2 {
		t.Errorf("the AUA abstract with author and number should match: %+v", m)
	}

	// Also found live: an ADA poster from the same trial with the same count
	// is not the paper an MDedge summary of a journal article describes.
	cl = twoaiHRParseClaim("SURMOUNT-1 3-year post hoc, 690 participants with prediabetes: most regained under 5% after nadir, over 90% reverted to normoglycaemia")
	poster := hrTestCand("10.2337/db25-1768-p", "1768-P: Sustainability of Waist-Height Ratio in Surmount-1 Three-Year Study", "690 participants with prediabetes, weight regain, post hoc", "Diabetes", "article")
	if m := twoaiHRScoreLead(cl, poster); m.ok || m.kind != "conference" {
		t.Errorf("a meeting poster must not match a claim about a paper: %+v", m)
	}
	// And an ACC abstract in JACC, title in capitals, naming only the trial
	// and weight loss.
	acc := hrTestCand("10.1016/s0735-1097(25)00861-7", "PARTICIPANTS WITH OBESITY ACHIEVING WEIGHT LOSS OF 10% IN SURMOUNT-1: A POST HOC ANALYSIS", "", "Journal of the American College of Cardiology", "article")
	if m := twoaiHRScoreLead(cl, acc); m.ok || m.kind != "conference" {
		t.Errorf("a capitalised meeting abstract must not match: %+v", m)
	}
	acc.title = "Participants with obesity achieving weight loss of 10% in SURMOUNT-1: a post hoc analysis"
	if m := twoaiHRScoreLead(cl, acc); m.ok {
		t.Errorf("one population of two plus a design is not enough: %+v", m)
	}

	// A meta-analysis claim wants a meta-analysis.
	cl = twoaiHRParseClaim("Meta-analysis of 10 studies, 41,381 adults: tirzepatide more weight loss (-4.28 points) and A1C (-0.29) than semaglutide, but more serious adverse events")
	mouse := hrTestCand("10.1126/sciadv.adu1589", "Hypophagia and body weight loss by tirzepatide are accompanied by fewer GI adverse events than semaglutide", "HbA1c", "Science Advances", "article")
	meta := hrTestCand("10.1/meta", "Tirzepatide versus semaglutide: a systematic review and meta-analysis", "Ten studies, 41 381 adults; weight loss, HbA1c and serious adverse events.", "Diabetes Care", "article")
	if m := twoaiHRScoreLead(cl, mouse); m.ok {
		t.Errorf("a paper that is not a meta-analysis must not match a meta-analysis claim: %+v", m)
	}
	if m := twoaiHRScoreLead(cl, meta); !m.ok {
		t.Errorf("the meta-analysis should match: %+v", m)
	}
	// Found live: a network meta-analysis on the same drugs printed on page
	// S61 of an Endocrine Practice supplement is a meeting abstract.
	suppl := hrTestCand("10.1016/j.eprac.2026.01.157", "Tirzepatide Versus Semaglutide for Glycemic Control and Weight Loss: Updated Network Meta-analysis", "", "Endocrine Practice", "review")
	suppl.page = "S61"
	if m := twoaiHRScoreLead(cl, suppl); m.ok || m.kind != "conference" {
		t.Errorf("a supplement abstract must not match: %+v", m)
	}
	// A meta-analysis whose abstract gives another count is a different one.
	otherMeta := hrTestCand("10.7759/cureus.86080", "Tirzepatide versus semaglutide for weight loss: a meta-analysis", "Twelve trials, 9 412 adults; weight loss, HbA1c, adverse events.", "Cureus", "review")
	if m := twoaiHRScoreLead(cl, otherMeta); m.ok {
		t.Errorf("a meta-analysis with another sample size must not match: %+v", m)
	}
}

func TestHRKind(t *testing.T) {
	cases := []struct {
		c    hrCandidate
		want string
	}{
		{hrTestCand("10.1016/s0140-6736(26)01861-1", "Retatrutide in adults", "", "The Lancet", "article"), "journal"},
		{hrTestCand("10.1093/jsxmed/qdaf381", "Effects of GLP-1 receptor agonists on male reproductive hormones", "", "The Journal of Sexual Medicine", "review"), "journal"},
		{hrTestCand("10.2337/db26-1734-p", "1734-P: Dual Incretin Therapy vs. GLP-1 Monotherapy", "", "Diabetes", "article"), "conference"},
		{hrTestCand("10.1093/eurheartj/ehae666.3317", "Dapagliflozin versus empagliflozin", "", "European Heart Journal", "article"), "conference"},
		{hrTestCand("10.1097/01.ju.0001109900.23127.a9.11", "IP10-11 PRELIMINARY ASSESSMENT", "", "The Journal of Urology", "article"), "conference"},
		{hrTestCand("10.1/x", "A title", "", "Some Congress Abstracts", "journal-article"), "conference"},
		{hrTestCand("10.21203/rs.3.rs-7103001/v1", "Efficacy of retatrutide", "", "Research Square", "preprint"), "preprint"},
		{hrTestCand("10.5281/zenodo.1", "Retatrutide notes", "", "Zenodo", "article"), "preprint"},
		{hrTestCand("10.5281/zenodo.2", "Data from: Retatrutide", "", "Zenodo", "article"), "skip"},
		{hrTestCand("10.1111/dom.71200/v1/review2", "Review for \"Retatrutide\"", "", "", "peer-review"), "skip"},
		{hrTestCand("10.1/news", "Retatrutide news", "", "MDedge", "journal-article"), "skip"},
		{hrTestCand("10.1/book", "Retatrutide", "", "A book", "book-chapter"), "skip"},
	}
	for _, c := range cases {
		if got := twoaiHRKind(c.c); got != c.want {
			t.Errorf("kind(%s, %q) = %s, want %s", c.c.doi, c.c.title, got, c.want)
		}
	}
}

func TestHRParsePubmed(t *testing.T) {
	xmlBody := `<?xml version="1.0" ?><PubmedArticleSet>
<PubmedArticle><MedlineCitation><PMID Version="1">42250575</PMID><Article><Abstract>
<AbstractText Label="BACKGROUND">Retatrutide is a <i>triple</i> agonist.</AbstractText>
<AbstractText Label="FINDINGS">HbA<sub>1c</sub> fell by 2&#xb7;0%, p&lt;0&#xb7;0001.</AbstractText>
</Abstract></Article><CommentsCorrectionsList><CommentsCorrections><PMID Version="1">39642862</PMID></CommentsCorrections></CommentsCorrectionsList></MedlineCitation></PubmedArticle>
<PubmedArticle><MedlineCitation><PMID Version="1">39326417</PMID><Article><Abstract><AbstractText>One patient, one year.</AbstractText></Abstract></Article></MedlineCitation></PubmedArticle>
<PubmedArticle><MedlineCitation><PMID Version="1">1</PMID><Article></Article></MedlineCitation></PubmedArticle>
</PubmedArticleSet>`
	got, err := twoaiHRParsePubmed([]byte(xmlBody))
	if err != nil {
		t.Fatal(err)
	}
	if want := "BACKGROUND: Retatrutide is a triple agonist. FINDINGS: HbA1c fell by 2·0%, p<0·0001."; got["42250575"] != want {
		t.Errorf("structured abstract = %q, want %q", got["42250575"], want)
	}
	if got["39326417"] != "One patient, one year." {
		t.Errorf("plain abstract = %q", got["39326417"])
	}
	if _, ok := got["1"]; ok || len(got) != 2 {
		t.Errorf("an article without an abstract must be absent: %v", got)
	}
}

func TestHRAuthorOrgs(t *testing.T) {
	claim := "Remote pharmacist and navigator obesity medication programme, 75 adults, Obesity Science and Practice"
	if a := twoaiHRAuthorNamed(claim, []string{"American Diabetes Association Professional Practice Committee for Obesity"}); a != "" {
		t.Errorf("an organisation matched as an author: %q", a)
	}
	if a := twoaiHRAuthorNamed("ENDO 2025 (Portillo Canales, SSM Health Saint Louis University)", []string{"GS Of Men`s Health", "Maria Portillo Canales"}); a != "Maria Portillo Canales" {
		t.Errorf("author match = %q, want Maria Portillo Canales", a)
	}
}

func TestHRClassifyLead(t *testing.T) {
	cases := []struct{ topic, text, want string }{
		{"dmed", "Semaglutide and testosterone in men with obesity", "testosterone"},
		{"dmed", "Functional hypogonadism in obese men", "testosterone"},
		{"dmed", "Retatrutide in adults with obesity and type 2 diabetes", "nextgen"},
		{"dmed", "Dapagliflozin and heart failure", "heart"},
		{"dmed", "Resistance training and incident diabetes", "overview"},
		{"lpa", "Olpasiran and Lp(a)", "rna"},
	}
	for _, c := range cases {
		if got, _ := twoaiHRClassifyLead(c.topic, c.text); got != c.want {
			t.Errorf("classify(%s, %q) = %q, want %q", c.topic, c.text, got, c.want)
		}
	}
}

// TestHRLeadsLive runs the matcher against OpenAlex and Crossref for the
// leads in the JSON lines file HR_LIVE_LEADS names, one object a line with
// id, topic, claim and found (YYYY-MM-DD). It reads, never writes, and is
// skipped unless the variable is set.
func TestHRLeadsLive(t *testing.T) {
	path := os.Getenv("HR_LIVE_LEADS")
	if path == "" {
		t.Skip("HR_LIVE_LEADS not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &hrRun{client: &http.Client{Timeout: 60 * time.Second}, stop: time.Now().Add(time.Hour)}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var in struct {
			ID                  int
			Topic, Claim, Found string
		}
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			t.Fatal(err)
		}
		found, _ := time.Parse("2006-01-02", in.Found)
		res, err := r.findPrimary(hrLead{id: in.ID, topic: in.Topic, claim: in.Claim, found: found}, twoaiHRCallCap)
		if err != nil {
			t.Logf("lead %d: %v", in.ID, err)
			continue
		}
		t.Logf("lead %d: %s", in.ID, truncate(in.Claim, 90))
		t.Logf("  openalex search: %s", res.search)
		if res.best != nil {
			sub, _ := twoaiHRClassifyLead(in.Topic, res.best.cand.title+" "+res.best.cand.abstract)
			t.Logf("  RESOLVED -> %s [%s] %s", res.best.cand.doi, sub, truncate(res.best.cand.title, 110))
			t.Logf("  note: %s", twoaiHRLeadNote(*res.best))
		} else {
			t.Logf("  NOT RESOLVED: %s", res.note)
		}
		for i, m := range res.ranked {
			if i == 4 {
				break
			}
			t.Logf("    #%d ok=%v score=%d %s %s %q (%s)", i+1, m.ok, m.score, m.kind, m.cand.doi, truncate(m.cand.title, 80), strings.Join(m.why, "; "))
		}
	}
	t.Logf("openalex calls: %d", r.calls)
}

// TestHRBackfillLive looks DOIs up the way the backfill does: OpenAlex,
// then Crossref, then PubMed. Read only, skipped unless HR_LIVE_ABS is set;
// HR_LIVE_DOIS, comma separated, replaces the default list.
func TestHRBackfillLive(t *testing.T) {
	if os.Getenv("HR_LIVE_ABS") == "" {
		t.Skip("HR_LIVE_ABS not set")
	}
	r := &hrRun{client: &http.Client{Timeout: 60 * time.Second}, stop: time.Now().Add(time.Hour)}
	dois := []string{"10.1016/s0140-6736(26)00967-0", "10.1016/s2213-8587(26)00136-1", "10.1016/j.cell.2024.09.004", "10.1097/hco.0000000000001144"}
	if v := os.Getenv("HR_LIVE_DOIS"); v != "" {
		dois = strings.Split(v, ",")
	}
	got := map[string]hrOAAbs{}
	batches, _ := twoaiHRDOIBatches(dois, twoaiHRAbsBatch)
	for _, b := range batches {
		m, err := r.oaAbstracts(b, twoaiHRCallCap)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			got[k] = v
		}
	}
	src := map[string]string{}
	text := map[string]string{}
	var pmQueue []string
	for _, d := range dois {
		if a := got[d].abstract; a != "" {
			src[d], text[d] = "openalex", a
			continue
		}
		a, err := twoaiHRCrossrefAbstract(r.client, d)
		if err != nil {
			t.Logf("%s: crossref error %v", d, err)
			continue
		}
		if a != "" {
			src[d], text[d] = "crossref", a
			continue
		}
		if got[d].pmid != "" {
			pmQueue = append(pmQueue, d)
		}
	}
	if len(pmQueue) > 0 {
		ids := make([]string, len(pmQueue))
		for i, d := range pmQueue {
			ids[i] = got[d].pmid
		}
		pm, err := twoaiHRPubmedAbstracts(r.client, ids)
		if err != nil {
			t.Fatalf("pubmed: %v", err)
		}
		for _, d := range pmQueue {
			if a := pm[got[d].pmid]; a != "" {
				src[d], text[d] = "pubmed", a
			}
		}
	}
	counts := map[string]int{}
	for _, d := range dois {
		s := src[d]
		if s == "" {
			s = "none"
		}
		counts[s]++
		t.Logf("%s: %s (pmid %s), %d chars: %s", d, s, got[d].pmid, len(text[d]), truncate(text[d], 110))
	}
	t.Logf("by source: %v, openalex calls %d", counts, r.calls)
}

func TestTwoaiHRLpaRe(t *testing.T) {
	for _, s := range []string{"Lipoprotein(a) and risk", "elevated Lp(a) levels", "olpasiran in OCEAN(a)", "apolipoprotein (a) isoforms", "CTX320 first in human"} {
		if !twoaiHRLpaRe.MatchString(s) {
			t.Errorf("missed %q", s)
		}
	}
	for _, s := range []string{"A potent epigenetic editor targeting human PCSK9", "LPA receptor signalling in fibrosis", "phosphate deficiency tolerance in rice"} {
		if twoaiHRLpaRe.MatchString(s) {
			t.Errorf("matched %q", s)
		}
	}
}
