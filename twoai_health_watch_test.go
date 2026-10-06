package main

import (
	"strings"
	"testing"
)

func TestTwoaiHealthDiabetesSubHub(t *testing.T) {
	cases := []struct{ text, want string }{
		{"Dexcom G8 continuous glucose monitor cleared", "dmed-tech"},
		{"Insulin pump outcomes in older adults", "dmed-tech"},
		{"FDA De Novo authorisation DEN250049: Libre Duo Continuous Dual Glucose Ketone Monitoring System", "dmed-tech"},
		{"CGM use in chronic kidney disease", "dmed-tech"},
		{"Stem cell-derived islets restore insulin production", "dmed-insulin-beta"},
		{"Once-weekly insulin icodec versus glargine", "dmed-insulin-beta"},
		{"Efsitora alfa in insulin-naive adults", "dmed-insulin-beta"},
		{"Zimislecel phase 3 results in type 1 diabetes", "dmed-insulin-beta"},
		{"Teplizumab delays stage 3 disease", "dmed-type1"},
		{"Autoimmune markers in newly diagnosed type 1 diabetes", "dmed-type1"},
		{"Semaglutide and kidney outcomes in type 2 diabetes (FLOW)", "dmed-kidney"},
		{"Diabetic nephropathy progression and albuminuria", "dmed-kidney"},
		{"Tirzepatide and MACE in SURPASS-CVOT", "dmed-heart"},
		{"Heart failure hospitalisation with dapagliflozin", "dmed-heart"},
		{"Retatrutide triple agonist phase 3 TRIUMPH", "dmed-nextgen"},
		{"Orforglipron versus oral semaglutide", "dmed-nextgen"},
		{"CagriSema REIMAGINE 2 topline", "dmed-nextgen"},
		{"MariTide maridebart cafraglutide 52-week data", "dmed-nextgen"},
		{"Survodutide in adults with obesity", "dmed-nextgen"},
		{"Semaglutide 7.2 mg dose response", "dmed-glp1"},
		{"GLP-1 receptor agonist discontinuation patterns", "dmed-glp1"},
		{"Dulaglutide adherence in routine care", "dmed-glp1"},
		{"Obesity prevalence among adolescents", "dmed-weight"},
		{"Type 2 diabetes remission after bariatric surgery", "dmed-weight"},
		{"HbA1c variability and glycemic control", "dmed-glucose"},
		{"Insulin resistance in adolescents", "dmed-glucose"},
		{"Insulin prices in the United States", "dmed-insulin-beta"},
		{"A phase 2 study of XYZ-123 in type 2 diabetes", "dmed-pipeline"},
		{"Diabetes care in rural clinics", "dmed"},
	}
	for _, c := range cases {
		got, why := twoaiHealthSubHub("diabetes", c.text)
		if got != c.want {
			t.Errorf("%q: got %s (matched %q), want %s", c.text, got, why, c.want)
		}
		if got != "dmed" && why == "" {
			t.Errorf("%q: classified %s without recording the match", c.text, got)
		}
	}
}

func TestTwoaiHealthLpaSubHub(t *testing.T) {
	cases := []struct{ text, want string }{
		{"Pelacarsen lowers lipoprotein(a) by 80 percent", "lpa-rna"},
		{"Olpasiran siRNA durable Lp(a) reduction", "lpa-rna"},
		{"Lepodisiran single dose effect", "lpa-rna"},
		{"Zerlasiran antisense comparison", "lpa-rna"},
		{"Muvalaplin oral Lp(a) inhibitor KRAKEN results", "lpa-oral"},
		{"CTX320 CRISPR gene editing for Lp(a)", "lpa-gene"},
		{"Lp(a) and aortic stenosis progression", "lpa-aortic"},
		{"Aortic valve calcification and lipoprotein(a)", "lpa-aortic"},
		{"Lp(a) testing in the 2026 ACC/AHA guideline", "lpa-testing"},
		{"Measurement of Lp(a) in nmol/L", "lpa-testing"},
		{"ESC/EAS consensus statement on lipoprotein(a)", "lpa-testing"},
		{"Lp(a)HORIZON outcome trial with pelacarsen", "lpa-trials"},
		{"OCEAN(a) outcomes trial of olpasiran", "lpa-trials"},
		{"ACCLAIM-Lp(a) lepodisiran NCT06292013", "lpa-trials"},
		{"MOVE-Lp(a) muvalaplin enrolment complete", "lpa-trials"},
		{"Lipoprotein(a) and myocardial infarction risk", "lpa-cvd"},
		{"PCSK9 inhibitors and Lp(a) lowering", "lpa-current"},
		{"Lipoprotein apheresis outcomes", "lpa-current"},
		{"Kringle IV type 2 repeats and apolipoprotein(a) isoforms", "lpa-understanding"},
		{"Novel approaches to Lp(a)", "lpa-future"},
		{"Lp(a) in South Asian patients", "lpa"},
	}
	for _, c := range cases {
		got, why := twoaiHealthSubHub("lpa", c.text)
		if got != c.want {
			t.Errorf("%q: got %s (matched %q), want %s", c.text, got, why, c.want)
		}
	}
}

func TestTwoaiHealthTopicOf(t *testing.T) {
	cases := []struct {
		text    string
		allowed []string
		want    string
	}{
		{"Lilly breaks ground in Houston manufacturing site", nil, ""},
		{"Lilly announces lepodisiran ACCLAIM-Lp(a) enrolment", nil, "lpa"},
		{"Zepbound approved for sleep apnea in adults with obesity", nil, "diabetes"},
		{"Abbott receives FDA approval for CardioMEMS HF System", nil, ""},
		{"Lipoprotein(a) in people with type 2 diabetes", nil, "lpa"},
		{"Lipoprotein(a) in people with type 2 diabetes", []string{"diabetes"}, "diabetes"},
		{"Novartis pelacarsen update", []string{"diabetes"}, ""},
		{"Ionis olezarsen for hypertriglyceridemia", []string{"lpa"}, ""},
		{"Human medicines EPAR: Mounjaro, tirzepatide, Revision: 20, Status: Authorised", nil, "diabetes"},
		{"Giredestrant plus Everolimus in Advanced Breast Cancer", nil, ""},
	}
	for _, c := range cases {
		got, _ := twoaiHealthTopicOf(c.text, c.allowed)
		if got != c.want {
			t.Errorf("%q %v: got %q, want %q", c.text, c.allowed, got, c.want)
		}
	}
}

func TestTwoaiHealthVerifyKey(t *testing.T) {
	cases := []struct{ topic, text, want string }{
		{"diabetes", "FDA De Novo authorisation DEN250049: Libre Duo Continuous Dual Glucose Ketone Monitoring System (Abbott Diabetes Care)", "abbott-ketone-monitor"},
		{"diabetes", "MannKind starts INHALE-1 Afrezza study in children", "afrezza-pediatric"},
		{"diabetes", "Amgen MariTide phase 3 MARITIME-1 data", "amgen-maritide"},
		{"diabetes", "GLP-1 agonists plus metformin in PCOS: a meta-analysis", "pcos-glp1-metformin"},
		{"diabetes", "Experts propose renaming PCOS to PMOS", "pcos-glp1-metformin"},
		{"diabetes", "One in eight U.S. adults has taken a GLP-1, KFF poll finds", "us-glp1-usage"},
		{"diabetes", "Vertex zimislecel pivotal results", "vertex-zimislecel"},
		{"diabetes", "First gene therapy trial in type 1 diabetes doses patient", "t1d-gene-therapy"},
		{"diabetes", "FDA expands Mounjaro label for cardiovascular risk reduction", "mounjaro-cv-indication"},
		{"diabetes", "Novo Nordisk files CagriSema with the FDA", "cagrisema-reimagine2"},
		{"diabetes", "Lilly plans retatrutide FDA submission by year end", "retatrutide-filing"},
		{"lpa", "Novartis presents full Lp(a)HORIZON results for pelacarsen", "lpa-horizon-presentation"},
		{"lpa", "Pelacarsen topline outcome data", "lpa-horizon-presentation"},
		// Real Google News headlines seen on 2026-10-05.
		{"diabetes", "FDA Authorizes First Wearable to Continuously Monitor Glucose and Ketones", "abbott-ketone-monitor"},
		{"diabetes", "Could Mounjaro's New Heart Benefit Unlock the Next Growth Phase for Lilly?", "mounjaro-cv-indication"},
		{"lpa", "Novartis pelacarsen Lp(a) trial fails, raising doubts for Amgen, Lilly", "lpa-horizon-presentation"},
		{"lpa", "Novartis' cholesterol-lowering medicine fails Phase 3, clouding Lp(a) horizons", "lpa-horizon-presentation"},
		{"diabetes", "Thermo Fisher Scientific accelerates market access by prioritizing the right KOLs", ""},
		{"diabetes", "Semaglutide and weight regain", ""},
		{"diabetes", "Lp(a)HORIZON results", ""}, // wrong topic, not tagged
	}
	for _, c := range cases {
		got, _ := twoaiHealthVerifyKey(c.topic, c.text)
		if got != c.want {
			t.Errorf("%s %q: got %q, want %q", c.topic, c.text, got, c.want)
		}
	}
	// Every verify key is unique and reachable from at least one query.
	seen := map[string]bool{}
	for _, v := range twoaiHealthVerify {
		if seen[v.key] {
			t.Errorf("duplicate verify key %s", v.key)
		}
		seen[v.key] = true
		if v.pubmed == "" && v.gnews == "" {
			t.Errorf("verify key %s has no query", v.key)
		}
		if v.topic != "diabetes" && v.topic != "lpa" {
			t.Errorf("verify key %s has topic %q", v.key, v.topic)
		}
	}
	if len(seen) != 11 {
		t.Errorf("verify list has %d keys, want 11", len(seen))
	}
}

func TestHwParsePubmedSummary(t *testing.T) {
	body := []byte(`{"header":{"type":"esummary"},"result":{"uids":["42831714","1"],
	"42831714":{"uid":"42831714","pubdate":"2026 Oct 3","source":"Diabetes Care","fulljournalname":"Diabetes care",
	"title":"Tirzepatide and &lt;i&gt;MACE&lt;/i&gt;  in adults.","sortpubdate":"2026/10/03 00:00","pubtype":["Journal Article"],
	"articleids":[{"idtype":"pubmed","value":"42831714"},{"idtype":"doi","value":"10.2337/dc26-0001"}]},
	"1":{"uid":"1","title":"Second","source":"Lancet","sortpubdate":"bad"}}}`)
	recs, err := hwParsePubmedSummary(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records", len(recs))
	}
	r := recs[0]
	if r.PMID != "42831714" || r.Title != "Tirzepatide and MACE in adults." || r.Journal != "Diabetes care" ||
		r.Date != "2026-10-03" || r.DOI != "10.2337/dc26-0001" {
		t.Errorf("unexpected record %+v", r)
	}
	if recs[1].Journal != "Lancet" || recs[1].Date != "" {
		t.Errorf("fallbacks wrong: %+v", recs[1])
	}
}

func TestHwParseTrials(t *testing.T) {
	body := []byte(`{"totalCount":2,"studies":[
	{"protocolSection":{"identificationModule":{"nctId":"NCT07684144","briefTitle":"Extension Trial of Maridebart Cafraglutide","acronym":"MARITIME-2-EXTENSION"},
	"statusModule":{"overallStatus":"RECRUITING","lastUpdatePostDateStruct":{"date":"2026-10-05"}},
	"sponsorCollaboratorsModule":{"leadSponsor":{"name":"Amgen"}},"conditionsModule":{"conditions":["Obesity"]},
	"designModule":{"phases":["PHASE3"]},"armsInterventionsModule":{"interventions":[{"name":"Maridebart cafraglutide"},{"name":"Placebo"}]}}},
	{"protocolSection":{"identificationModule":{"nctId":"","briefTitle":"no id"}}}],
	"nextPageToken":"abc"}`)
	trials, next, err := hwParseTrials(body)
	if err != nil {
		t.Fatal(err)
	}
	if next != "abc" || len(trials) != 1 {
		t.Fatalf("next=%q trials=%d", next, len(trials))
	}
	tr := trials[0]
	if tr.NCT != "NCT07684144" || tr.Sponsor != "Amgen" || tr.Updated != "2026-10-05" || tr.Status != "RECRUITING" ||
		len(tr.Phases) != 1 || tr.Phases[0] != "PHASE3" || len(tr.Interventions) != 2 || tr.Acronym != "MARITIME-2-EXTENSION" {
		t.Errorf("unexpected trial %+v", tr)
	}
	r := &hwRun{}
	it := r.trialItem(tr, "diabetes", "https://clinicaltrials.gov/study/"+tr.NCT)
	if hub, _ := twoaiHealthSubHub("diabetes", it.classify); hub != "dmed-nextgen" {
		t.Errorf("trial classified %s, want dmed-nextgen", hub)
	}
	if k, _ := twoaiHealthVerifyKey("diabetes", it.classify); k != "amgen-maritide" {
		t.Errorf("trial verify %q, want amgen-maritide", k)
	}
}

func TestHwParseFeed(t *testing.T) {
	rss1 := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
 <channel rdf:about="https://www.thelancet.com/journals/landia/issues"><title>The Lancet Diabetes &amp; Endocrinology</title></channel>
 <item rdf:about="x"><title>[Comment] IL-2 in newly diagnosed type 1 diabetes</title>
  <link>https://www.thelancet.com/journals/landia/article/PIIS2213-8587(26)00236-6/fulltext?rss=yes</link>
  <dc:date>2026-10-01T22:30:02Z</dc:date></item>
</rdf:RDF>`)
	items, err := hwParseFeed(rss1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].URL(), "PIIS2213-8587") ||
		twoaiFeedDate(items[0].PubDate, items[0].Date) != "2026-10-01" {
		t.Fatalf("RSS 1.0 parse wrong: %+v", items)
	}

	gnews := []byte(`<rss version="2.0"><channel><title>Google News</title>
<item><title>Lilly touts Foundayo advantage over Novo's Ozempic pill - Reuters</title>
<link>https://news.google.com/rss/articles/CBMiabc?oc=5</link>
<pubDate>Tue, 29 Sep 2026 14:08:08 GMT</pubDate>
<source url="https://www.reuters.com">Reuters</source></item></channel></rss>`)
	items, err = hwParseFeed(gnews)
	if err != nil || len(items) != 1 {
		t.Fatalf("RSS 2.0 parse: %v %d", err, len(items))
	}
	if items[0].Source != "Reuters" {
		t.Errorf("publisher %q", items[0].Source)
	}
	if got := hwGNewsTitle(items[0].Title, items[0].Source); got != "Lilly touts Foundayo advantage over Novo's Ozempic pill" {
		t.Errorf("title %q", got)
	}
	if !strings.Contains(hwGNewsURL(`"Novo Nordisk" diabetes`, "3d"), "q=%22Novo+Nordisk%22+diabetes+when%3A3d&hl=en-US") {
		t.Errorf("gnews url %s", hwGNewsURL(`"Novo Nordisk" diabetes`, "3d"))
	}
}

func TestHwEMAKey(t *testing.T) {
	base := "https://www.ema.europa.eu/en/medicines/human/EPAR/mounjaro"
	cases := []struct{ link, title, want string }{
		{base, "Human medicines European public assessment report (EPAR): Mounjaro, tirzepatide, Date of authorisation: 15/09/2022, Revision: 20, Status: Authorised", base + "#revision-20"},
		{base, "Human medicines European public assessment report (EPAR): Mounjaro, tirzepatide, Status: Opinion", base + "#opinion"},
		{base, "Human medicines European public assessment report (EPAR): Mounjaro, tirzepatide, Status: Application withdrawn", base + "#withdrawn"},
		{"https://www.ema.europa.eu/en/news/x", "News item", "https://www.ema.europa.eu/en/news/x"},
	}
	for _, c := range cases {
		if got := hwEMAKey(c.link, c.title); got != c.want {
			t.Errorf("%q: got %s, want %s", c.title, got, c.want)
		}
	}
}

func TestHwDeviceURLAndDates(t *testing.T) {
	if got := hwDeviceURL("DEN250049"); got != "https://www.accessdata.fda.gov/scripts/cdrh/cfdocs/cfpmn/denovo.cfm?id=DEN250049" {
		t.Errorf("De Novo url %s", got)
	}
	if got := hwDeviceURL("K262153"); got != "https://www.accessdata.fda.gov/scripts/cdrh/cfdocs/cfpmn/pmn.cfm?ID=K262153" {
		t.Errorf("510k url %s", got)
	}
	if hwYMD("20260827") != "2026-08-27" || hwYMD("2026-08-25") != "2026-08-25" || hwYMD("x") != "" {
		t.Error("hwYMD")
	}
	if hwFamily("trial") != "trials" || hwFamily("company") != "companies" || hwFamily("pubmed") != "pubmed" {
		t.Error("hwFamily")
	}
}

func TestHwBridgeBodyStaysShort(t *testing.T) {
	var rows []hwRow
	for i := 0; i < 33; i++ {
		rows = append(rows, hwRow{kind: "pubmed", title: strings.Repeat("x", 300), source: "PubMed", date: "2026-10-05", url: "https://pubmed.ncbi.nlm.nih.gov/1/"})
	}
	rows[0].kind = "coverage"
	rows[1].verify = "amgen-maritide"
	hubs := []hwCount{{"dmed-glp1", 50}, {"dmed-tech", 10}}
	sources := []hwCount{{"PubMed", 55}, {"Reuters (coverage)", 5}}
	verify := []hwCount{{"amgen-maritide", 1}}
	var unresolved []hwRow
	for i := 0; i < 9; i++ {
		unresolved = append(unresolved, hwRow{kind: "coverage", title: "Dexcom stock fell 1.01 percent as RBC lifted its target", source: "Dexcom (coverage)", date: "2026-10-04", url: "https://www.ad-hoc-news.de/x"})
	}
	body := hwBridgeBody("Diabetes watch", "2026-10-05", "the first run", 60, hubs, sources, verify, rows, 4, unresolved)
	lines := strings.Split(body, "\n")
	if len(lines) > 50 {
		t.Errorf("bridge body has %d lines", len(lines))
	}
	for _, want := range []string{"60 new items", "except those marked coverage", "stored as skipped", "dmed-glp1 50", "[coverage] ",
		"[verify amgen-maritide] ", "and 28 more", "resolved to its primary since then: 4", "No primary found after 3 tries", "and 3 more"} {
		if !strings.Contains(body, want) {
			t.Errorf("bridge body lacks %q", want)
		}
	}
	if strings.ContainsRune(body, 0x2014) {
		t.Error("bridge body has an em-dash")
	}
	// A day with only given-up coverage still reads sensibly.
	body = hwBridgeBody("Lp(a) watch", "2026-10-05", "the first run", 0, nil, nil, nil, nil, 0, unresolved[:1])
	if strings.Contains(body, "Newest:") || strings.Contains(body, "resolved to its primary") || !strings.Contains(body, "No primary found") {
		t.Errorf("unresolved-only body wrong:\n%s", body)
	}
}

// ---------------------------------------------------------------------
// Skip at harvest. The titles are real rows of twoai_health_watch as the
// content project marked them on 2026-10-05 (bridge row 496).
// ---------------------------------------------------------------------

type hwSkipCase struct {
	kind, title, url string
	detail           map[string]any
}

// hwFeedCase builds a journal case the way feeds() stores it, the section
// label split off the title first.
func hwFeedCase(title, link string) hwSkipCase {
	sec, t := hwFeedSection(title)
	d := map[string]any{}
	if sec != "" {
		d["section"] = sec
	}
	return hwSkipCase{"journal", t, link, d}
}

func TestHwSkipReasonCatchesNoise(t *testing.T) {
	pt := func(types ...string) map[string]any { return map[string]any{"pubtype": types} }
	fda := func(appl, class, status string) map[string]any {
		return map[string]any{"application": appl, "submission_class": class, "status": status}
	}
	cases := []struct {
		c    hwSkipCase
		want string
	}{
		{hwSkipCase{"fda", "FDA approval, ANDA212330 EMPAGLIFLOZIN (empagliflozin): ORIG 1", "", fda("ANDA212330", "", "AP")}, "ANDA generic approval"},
		{hwSkipCase{"fda", "FDA tentative approval, ANDA220684 FINERENONE (finerenone): ORIG 1", "", fda("ANDA220684", "", "TA")}, "ANDA generic approval"},
		{hwSkipCase{"fda", "FDA approval, NDA204353 INVOKAMET (canagliflozin, metformin hydrochloride): SUPPL 48, Labeling", "", fda("NDA204353", "Labeling", "AP")}, "FDA labelling-only supplement"},
		{hwSkipCase{"fda", "FDA approval, NDA209637 OZEMPIC (semaglutide): SUPPL 44, Labeling", "", fda("NDA209637", "Labeling-Package Insert", "AP")}, "FDA labelling-only supplement"},
		{hwSkipCase{"fda", "FDA approval, NDA216203 INPEFA (sotagliflozin): SUPPL 3", "", map[string]any{"application": "NDA216203", "submission_class_code": "LABELING", "status": "AP"}}, "FDA labelling-only supplement"},
		{hwSkipCase{"fda", "FDA tentative approval, NDA220758 EMPAGLIFLOZIN (empagliflozin): ORIG 1, Type 3 - New Dosage Form", "", fda("NDA220758", "Type 3 - New Dosage Form", "TA")}, "FDA tentative approval"},
		{hwFeedCase("About the Artist: Brom Wikstrom", ""), "not an article"},
		{hwFeedCase("About the Editor: David Simmons, MD, MBBS, FRACP, FRCP, Rural Health Access and the TOBOGM Study", ""), "not an article"},
		{hwFeedCase("Issues and Events", ""), "not an article"},
		{hwFeedCase("Up Front", ""), "not an article"},
		{hwFeedCase("In This Issue of Diabetes Care", ""), "not an article"},
		{hwFeedCase("Cover", ""), "not an article"},
		{hwFeedCase("Table of Contents", ""), "not an article"},
		{hwFeedCase("Comment on Toki et al. Seasonal BMI Amplitude and Kidney Function Decline in Japanese Adults With Type 2 Diabetes (JDDM 82)", ""), "letter, comment or reply"},
		{hwFeedCase("Response to Comments on Toki et al. Seasonal BMI Amplitude and Risk of Kidney Function Decline in Japanese Adults With Type 2 Diabetes (JDDM 82)", ""), "letter, comment or reply"},
		{hwFeedCase("Type 2 diabetes subtypes, aetiology and pathophysiology. Reply to Vaag A [letter]", ""), "letter, comment or reply"},
		{hwFeedCase("In Reply: Tirzepatide and kidney outcomes", ""), "letter, comment or reply"},
		{hwFeedCase("Correction: The management of type 1 diabetes in adults. The updated 2026 consensus report by the American Diabetes Association (ADA) and the European Association for the Study of Diabetes (EASD)", ""), "correction or retraction notice"},
		{hwFeedCase("Erratum. Glycemic Targets in Pregnancy", ""), "correction or retraction notice"},
		{hwFeedCase("Retraction Note: Semaglutide and retinal outcomes", ""), "correction or retraction notice"},
		// The Lancet's own section labels, as its feeds publish them.
		{hwFeedCase("[Corrections] Correction to Lancet Diabetes Endocrinol 2023; 11: 402–13", "https://www.thelancet.com/journals/landia/article/PIIS2213-8587(26)00254-8/fulltext?rss=yes"), "correction or retraction notice"},
		{hwFeedCase("[Comment] IL-2 and immunoregulatory therapies in newly diagnosed type 1 diabetes", ""), "letter, comment or reply"},
		{hwFeedCase("[Correspondence] Time for personalised incretin titration", ""), "letter, comment or reply"},
		{hwFeedCase("[Editorial] Safeguarding paediatric metabolic health", ""), "editorial"},
		{hwFeedCase("[World Report] NHS campaigners target Palantir", ""), "not an article"},
		// NEJM marks the type in the DOI.
		{hwFeedCase("More on NETs in Lupus", "https://www.nejm.org/doi/full/10.1056/NEJMc2609920?af=R&rss=currentIssue"), "letter, comment or reply"},
		{hwFeedCase("Statins in Older Adults, Evidence at Last", "https://www.nejm.org/doi/full/10.1056/NEJMe2611127?af=R&rss=currentIssue"), "editorial"},
		{hwFeedCase("Thyroglossal Duct Cyst", "https://www.nejm.org/doi/full/10.1056/NEJMicm2607537?af=R&rss=currentIssue"), "not an article"},
		// PubMed publication types, and a reply PubMed files as an article.
		{hwSkipCase{"pubmed", `Comment on "effect of semaglutide with metformin for weight loss and fertility in polycystic ovary syndrome (PCOS) patients with obesity: A pilot prospective study".`, "", pt("Letter")}, "letter, comment or reply"},
		{hwSkipCase{"pubmed", `Reply - Letter to the editor: Comment on "effect of semaglutide with metformin for weight loss and fertility in polycystic ovary syndrome (PCOS) patients with obesity: A pilot prospective study.`, "", pt("Letter")}, "letter, comment or reply"},
		{hwSkipCase{"pubmed", "Assessing clinical relevance of off-target effects observed at suprapharmacological concentrations of SGLT2 inhibitors. Reply.", "", pt("Journal Article")}, "letter, comment or reply"},
		{hwSkipCase{"pubmed", "Erratum to: Semaglutide in heart failure", "", pt("Published Erratum")}, "correction or retraction notice"},
		{hwSkipCase{"pubmed", "A retracted trial", "", map[string]any{"pubtype": []any{"Journal Article", "Retracted Publication"}}}, "correction or retraction notice"},
	}
	for _, c := range cases {
		got, m := hwSkipReason(c.c.kind, c.c.title, c.c.url, c.c.detail)
		if got != c.want {
			t.Errorf("%s %q: got %q (matched %q), want %q", c.c.kind, c.c.title, got, m, c.want)
		}
		if got != "" && m == "" {
			t.Errorf("%q skipped without recording the match", c.c.title)
		}
	}
}

// None of the 44 rows the content project used on 2026-10-05 is skipped.
func TestHwSkipReasonNeverSkipsUsed(t *testing.T) {
	pt := func(types ...string) map[string]any { return map[string]any{"pubtype": types} }
	none := map[string]any{}
	used := []hwSkipCase{
		{"company", "Landmark study in people with Type 2 diabetes using basal insulin shows continuous glucose monitoring, including Abbott’s Libre technology, associated with lower risk of death and cardiovascular complications", "", none},
		{"company", "An open letter from Eli Lilly and Company warning of potential patient safety risks associated with tirzepatide compounded with vitamin B12", "", none},
		{"company", "Lilly calls on online platforms, payment companies and regulators to shut down the illegal retatrutide black market", "", none},
		{"company", "Ionis partner Novartis announces Lp(a)HORIZON Phase 3 topline results for pelacarsen in patients with elevated Lp(a) and established cardiovascular disease (CVD)", "", none},
		{"company", "MannKind and Rose Pharma Inc Enter Licensing and Collaboration Agreement to Develop an Inhaled Rapid-Acting, Short-Duration GLP-1 for Weight Management", "", none},
		{"company", "Medtronic Launches Exchange Offer to Complete Separation of MiniMed Group, Inc.", "", none},
		{"ema", "Human medicines European public assessment report (EPAR): Bydureon, exenatide, Date of authorisation: 17/06/2011, Revision: 27, Status: Withdrawn", "", none},
		{"ema", "Human medicines European public assessment report (EPAR): Byetta, exenatide, Date of authorisation: 20/11/2006, Revision: 30, Status: Withdrawn", "", none},
		{"ema", "Human medicines European public assessment report (EPAR): Bysumlog, insulin lispro, Date of authorisation: 06/05/2026, Revision: 2, Status: Authorised", "", none},
		{"fda", "FDA Authorizes First Wearable Device That Continuously Monitors Both Ketone Levels and Blood Sugar", "", none},
		{"fda", "FDA 510(k) clearance K262153: Pivot Insulin Delivery System (Modular Medical, Inc.)", "", map[string]any{"number": "K262153"}},
		{"fda", "FDA 510(k) clearance K262472: Stelo Glucose Biosensor System (Dexcom, Inc.)", "", map[string]any{"number": "K262472"}},
		{"fda", "FDA De Novo authorisation DEN250049: Libre Duo 10 Day Continuous Dual Glucose Ketone Monitoring System (Abbott Diabetes Care)", "", map[string]any{"number": "DEN250049"}},
		{"fda", "FDA approval, BLA761408 ONSWIK (insulin efsitora alfa-gobe): ORIG 1, Type 1 - New Molecular Entity", "", map[string]any{"application": "BLA761408", "submission_class": "Type 1 - New Molecular Entity", "submission_class_code": "TYPE 1", "status": "AP"}},
		{"fda", "FDA approval, NDA215866 MOUNJARO / MOUNJARO KWIKPEN / MOUNJARO (AUTOINJECTOR) (tirzepatide): SUPPL 44, Efficacy", "", map[string]any{"application": "NDA215866", "submission_class": "Efficacy", "submission_class_code": "EFFICACY", "status": "AP"}},
		hwFeedCase("Automated Insulin Delivery Is Beneficial in Adults With Insulin-Treated Type 2 Diabetes With and Without GAD65 Antibodies", ""),
		hwFeedCase("Estimating the True MACE Benefits From Tirzepatide in SURPASS-CVOT Using an Imputed Placebo Analysis of REWIND", ""),
		hwFeedCase("Fulminant Secondary HLH Following EBV Reactivation in an Adult With Trisomy 21 After Teplizumab Treatment for Stage 2 Type 1 Diabetes", ""),
		hwFeedCase("Delayed time to stage 3 type 1 diabetes after GAD-alum treatment in positive children: follow-up of the randomised placebo-controlled Diabetes Prevention – Immune Tolerance trial", ""),
		hwFeedCase("Management of type 2 diabetes, 2026. A consensus report by the American Diabetes Association (ADA) and the European Association for the Study of Diabetes (EASD)", ""),
		hwFeedCase("Retatrutide, a Triple Hormone Receptor Agonist, for Treatment of Obesity", "https://www.nejm.org/doi/full/10.1056/NEJMoa2604169?af=R&rss=currentIssue"),
		hwFeedCase("Survodutide Once Weekly in Adults with Obesity and Type 2 Diabetes", "https://www.nejm.org/doi/full/10.1056/NEJMoa2607219?af=R&rss=currentIssue"),
		hwFeedCase("[Articles] Cardiovascular safety of orforglipron versus insulin glargine in adults with type 2 diabetes at increased cardiovascular risk (ACHIEVE-4): a phase 3, event-driven, randomised, open-label, non-inferiority, active comparator trial", ""),
		hwFeedCase("[Articles] Retatrutide in adults with obesity and type 2 diabetes (TRIUMPH-2): a double-blind, parallel-group, randomised, placebo-controlled, phase 3 trial", ""),
		hwFeedCase("[Articles] Effect of semaglutide on kidney outcomes in the SELECT, FLOW, and SOUL trials: a prespecified pooled analysis", ""),
		hwFeedCase("[Articles] Efficacy and safety of low-dose IL-2 in people with newly diagnosed type 1 diabetes (DIABIL-2): a double-blind, multicentre, randomised, placebo-controlled, phase 2b trial", ""),
		hwFeedCase("[Articles] Efficacy and safety of mazdutide in adults with obesity or overweight: a US-based, multicentre, phase 2, randomised, placebo-controlled clinical trial", ""),
		hwFeedCase("[Articles] Ketoacidosis with SGLT2 inhibitors in routine clinical practice of type 2 diabetes: Scandinavian cohort and nested case–control study", ""),
		hwFeedCase("[Articles] Long-term safety of oral orforglipron in Japanese participants with type 2 diabetes (ACHIEVE-J): a multicentre, randomised, open-label, parallel-group phase 3 trial", ""),
		hwFeedCase("[Articles] Once-weekly IcoSema versus once-daily insulin glargine U100 in type 2 diabetes management (COMBINE 4): an open-label, multicentre, treat-to-target, randomised, phase 3b trial", ""),
		hwFeedCase("[Articles] Petrelintide, a human amylin analogue for the treatment of obesity (ZUPREME 1): a randomised, double-blind, placebo-controlled, phase 2 trial", ""),
		{"pubmed", "Association between lipoprotein(a) and structural degeneration of bioprosthetic aortic valves: A systematic review and meta-analysis.", "", pt("Journal Article")},
		{"pubmed", "Effects of semaglutide on kidney disease in type 2 diabetes: a randomized placebo-controlled trial.", "", pt("Journal Article")},
		{"pubmed", "Hospitalization outcomes in adults with type 2 diabetes mellitus treated with once-weekly insulin efsitora versus once-daily basal insulins: A post hoc analysis of the QWINT-1 to -4 randomized trials.", "", pt("Journal Article")},
		{"pubmed", "Lipoprotein(a) After HORIZON: Causal Culprit, Risk Marker, or Unfulfilled Therapeutic Promise?", "", pt("Editorial")},
		{"pubmed", "Lipoprotein(a) in Japan: An Expert Consensus Statement.", "", pt("Journal Article")},
		{"pubmed", "Lipoprotein(a) reduction with PCSK9-targeting pharmacotherapy: A prospective real-world registry study.", "", pt("Journal Article")},
		{"pubmed", "Maximizing weight loss with cagrisema: a systematic review and GRADE-assessed meta-analysis of randomized controlled trials.", "", pt("Journal Article", "Review")},
		{"pubmed", "Tirzepatide and the Incidence of Atrial Fibrillation in Adults With Overweight or Obesity: An Updated Meta-Analysis of Randomized Controlled Trials.", "", pt("Journal Article", "Meta-Analysis", "Research Support, Non-U.S. Gov't")},
		{"trial", "A Clinical Trial of MK-7262 and Enlicitide in Participants With High Lipoprotein(a) (MK-7262-004)", "", none},
		{"trial", "Assessing the Impact of Muvalaplin on Major Cardiovascular Events in Adults With Elevated Lipoprotein(a) (MOVE-Lp(a))", "", none},
		{"trial", "Evaluating the Impact of Maridebart Cafraglutide on Cardiovascular Outcomes in Participants With Atherosclerotic Cardiovascular Disease and Overweight or Obesity (MARITIME-CV)", "", none},
		{"trial", "VV-14299 and VV-14300 for the Treatment of Type 1 Diabetes (PROGRESS)", "", none},
		{"trial", "VV-14300 for the Treatment of Type 1 Diabetes (RELIEVE)", "", none},
	}
	if len(used) != 44 {
		t.Fatalf("used list has %d rows, want the 44 the content project used", len(used))
	}
	for _, c := range used {
		if got, m := hwSkipReason(c.kind, c.title, c.url, c.detail); got != "" {
			t.Errorf("used row would be skipped: %s %q (%s, matched %q)", c.kind, c.title, got, m)
		}
	}
	// Ordinary research that merely starts with a skip word stays.
	for _, title := range []string{"Correction of hyperglycaemia with weekly insulin", "Letters of intent and trial start-up in type 1 diabetes", "Coverage of CGM by Medicaid", "Replying to patient messages about insulin"} {
		if got, _ := hwSkipReason("journal", title, "", map[string]any{}); got != "" {
			t.Errorf("%q skipped as %s", title, got)
		}
	}
}

func TestHwFeedSection(t *testing.T) {
	cases := []struct{ in, sec, title string }{
		{"[Comment] IL-2 in newly diagnosed type 1 diabetes", "Comment", "IL-2 in newly diagnosed type 1 diabetes"},
		{"[Department of Error] Department of Error", "Department of Error", "Department of Error"},
		{"[Articles] Retatrutide in adults (TRIUMPH-2)", "Articles", "Retatrutide in adults (TRIUMPH-2)"},
		{"Lp(a) [nmol/L] thresholds", "", "Lp(a) [nmol/L] thresholds"},
		{"Plain title", "", "Plain title"},
	}
	for _, c := range cases {
		sec, title := hwFeedSection(c.in)
		if sec != c.sec || title != c.title {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.in, sec, title, c.sec, c.title)
		}
	}
}

// ---------------------------------------------------------------------
// Coverage resolution.
// ---------------------------------------------------------------------

func TestHwTitleMatch(t *testing.T) {
	cases := []struct {
		coverage, candidate string
		want                bool
	}{
		// A drug name and one more distinctive term.
		{"Zealand Pharma’s survodutide delivers up to 13.1% weight loss in Phase III obesity trial",
			"Survodutide delivers up to 13.1% weight loss in SYNCHRONIZE-1 phase III trial", true},
		{"Amgen Highlights MariTide, IMDELLTRA Pipeline Progress and Growth Priorities",
			"Amgen Highlights MariTide and IMDELLTRA Progress at Investor Event", true},
		// Two named drugs.
		{"Novo Nordisk's CagriSema beats semaglutide in REDEFINE 4", "CagriSema versus semaglutide: REDEFINE 4 topline results", true},
		// A class word needs two more distinctive terms.
		{"FDA approves Lilly’s once-weekly insulin injection for type 2 diabetes",
			"FDA approves Lilly's Onswik (insulin efsitora alfa), the first once-weekly insulin for adults with type 2 diabetes", true},
		{"FDA Authorizes First Wearable to Continuously Monitor Glucose and Ketones",
			"FDA Authorizes First Wearable Device That Continuously Monitors Both Ketone Levels and Blood Sugar", true},
		// Development codes count as names.
		{"Lilly's LY3457263 cuts body weight in phase 1", "Nisotirostide (LY3457263) phase 1 body weight data", true},
		// Company names, news verbs and topic words do not.
		{"Insulet stock carries RBC Buy rating as growth remains strong", "Insulet announces November results date", false},
		{"Dexcom announces Team Novo Nordisk partnership. Dexcom stock costs EUR 76.20", "Dexcom stock fell 1.01 percent as RBC lifted its target", false},
		// One shared drug name alone is not enough.
		{"Semaglutide and weight regain", "Semaglutide 7.2 mg dose response", false},
		{"Survodutide Once Weekly in Adults with Obesity and Type 2 Diabetes",
			"Zealand Pharma’s survodutide delivers up to 13.1% weight loss in Phase III obesity trial", false},
		// A class word and one other term is not enough.
		{"Novo Nordisk cuts insulin prices again", "Sanofi insulin pricing update", false},
		// News cadence words do not count (a real false match in the trial).
		{"Weekly Recap: Buyback status and Boehringer survodutide results", "Survodutide Once Weekly in Adults with Obesity and Type 2 Diabetes", false},
		// Spam with no drug name never matches.
		{"novo nordisk is cutting prices on its weight loss drugs", "Novo Nordisk cuts cash prices for weight-loss drugs", false},
	}
	for _, c := range cases {
		got, shared := hwTitleMatch(c.coverage, c.candidate)
		if got != c.want {
			t.Errorf("%q vs %q: got %v (shared %v), want %v", c.coverage, c.candidate, got, shared, c.want)
		}
		if got && len(shared) < 2 {
			t.Errorf("%q matched on %v, fewer than two terms recorded", c.coverage, shared)
		}
	}
}

func TestHwTokens(t *testing.T) {
	got := strings.Join(hwTokens("Novartis’ Lp(a)HORIZON results in 2026: pelacarsen & 13.1% weight loss"), " ")
	if got != "novartis lpahorizon result in pelacarsen 13.1 weight loss" {
		t.Errorf("tokens %q", got)
	}
	if !hwIsDrugTerm("ly3457263") || !hwIsDrugTerm("amg-133") || !hwIsDrugTerm("survodutide") || hwIsDrugTerm("nct06292013") || hwIsDrugTerm("covid-19") {
		t.Error("hwIsDrugTerm")
	}
}

func TestHwPrimaryURL(t *testing.T) {
	cases := []struct{ in, want, kind string }{
		{"https://clinicaltrials.gov/ct2/show/NCT06292013?term=x", "https://clinicaltrials.gov/study/NCT06292013", "trial"},
		{"https://pubmed.ncbi.nlm.nih.gov/42831714/?utm_source=x", "https://pubmed.ncbi.nlm.nih.gov/42831714/", "pubmed"},
		{"https://doi.org/10.1056/NEJMoa2604169.", "https://doi.org/10.1056/NEJMoa2604169", "journal"},
		{"https://www.ema.europa.eu/en/medicines/human/EPAR/mounjaro#tab", "https://www.ema.europa.eu/en/medicines/human/EPAR/mounjaro", "ema"},
		{"https://investor.lilly.com/news-releases/news-release-details/fda-approves-onswik", "https://investor.lilly.com/news-releases/news-release-details/fda-approves-onswik", "company"},
		{"https://www.globenewswire.com/news-release/2026/10/02/1/0/en/Zealand-Pharma-x.html", "https://www.globenewswire.com/news-release/2026/10/02/1/0/en/Zealand-Pharma-x.html", "company"},
		{"https://www.globenewswire.com/Tracker?data=abc", "", ""},
		{"https://www.lilly.com/news", "", ""},
		{"https://www.dexcom.com/", "", ""},
		{"https://www.fda.gov/media/12345/logo.png", "", ""},
		{"https://www.accessdata.fda.gov/drugsatfda_docs/label/2023/209637s020s021lbl.pdf", "", ""},
		{"https://www.reuters.com/business/healthcare/novo-cagrisema-2026-10-02/", "", ""},
		{"mailto:press@novonordisk.com", "", ""},
	}
	for _, c := range cases {
		got, d, ok := hwPrimaryURL(c.in)
		if got != c.want || ok != (c.want != "") || (ok && hwRowKind(d) != c.kind) {
			t.Errorf("%s: got %q %v kind %q, want %q kind %q", c.in, got, ok, hwRowKind(d), c.want, c.kind)
		}
	}
}

func TestHwPrimaryLinks(t *testing.T) {
	page := `<html><body>
<a href="https://www.fda.gov/news-events/press-announcements/fda-approves-first-once-weekly-insulin">FDA</a>
<a class="x" href='https://investor.lilly.com/news-releases/news-release-details/fda-approves-lillys-onswik'>Lilly</a>
<a href="https://www.dexcom.com/">Dexcom</a>
<a href="https://www.globenewswire.com/Tracker?data=abc">tracker</a>
<a href="https://www.globenewswire.com/news-release/2026/10/02/3160000/0/en/Zealand-Pharma-announces.html">wire</a>
<a href="https://twitter.com/intent/tweet?url=x">share</a>
<a href="/health/other-story-about-novo">same site</a>
<a href="https://www.sec.gov/Archives/edgar/data/1234/000123456726000001/form8k.htm">8-K</a>
<a href="https://doi.org/10.1056/NEJMoa2604169">paper</a>
<p>The trial (NCT06292013) was reported in doi:10.2337/dc26-0001.</p>
<a href="https://www.novonordisk.com/news-and-media/news-and-ir-materials/news-details.html?id=916123&amp;x=1">Novo</a>
<a href="https://www.fda.gov/news-events/press-announcements/fda-approves-first-once-weekly-insulin">FDA again</a>
</body></html>`
	got := hwPrimaryLinks(page, "https://bolnews.com/health/story", 10)
	want := []string{
		"https://www.fda.gov/news-events/press-announcements/fda-approves-first-once-weekly-insulin",
		"https://investor.lilly.com/news-releases/news-release-details/fda-approves-lillys-onswik",
		"https://www.globenewswire.com/news-release/2026/10/02/3160000/0/en/Zealand-Pharma-announces.html",
		"https://www.novonordisk.com/news-and-media/news-and-ir-materials/news-details.html?id=916123&x=1",
		"https://doi.org/10.1056/NEJMoa2604169",
		"https://clinicaltrials.gov/study/NCT06292013",
		"https://doi.org/10.2337/dc26-0001",
		"https://www.sec.gov/Archives/edgar/data/1234/000123456726000001/form8k.htm",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("links:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := len(hwPrimaryLinks(page, "https://bolnews.com/health/story", 4)); n != 4 {
		t.Errorf("max not applied, got %d", n)
	}
	// A page on a primary domain does not count links to itself.
	self := `<a href="https://www.fda.gov/news-events/press-announcements/other">x</a>`
	if got := hwPrimaryLinks(self, "https://www.fda.gov/news-events/press-announcements/this", 4); len(got) != 0 {
		t.Errorf("self links kept: %v", got)
	}
}

func TestHwPageTitleAndDate(t *testing.T) {
	page := `<html><head><title>ignored | FDA</title>
<meta property="og:title" content="FDA Approves First Once-Weekly Insulin for Type 2 Diabetes | FDA">
<meta property="article:published_time" content="2026-10-02T08:00:00Z"></head></html>`
	if got := hwPageTitle(page); got != "FDA Approves First Once-Weekly Insulin for Type 2 Diabetes" {
		t.Errorf("og title %q", got)
	}
	if got := hwPageDate(page); got != "2026-10-02" {
		t.Errorf("date %q", got)
	}
	page = `<title>Zealand Pharma - Completion of share buy-back program</title>`
	if got := hwPageTitle(page); got != "Zealand Pharma - Completion of share buy-back program" {
		t.Errorf("title tag %q", got)
	}
	page = `<meta content="Novo Nordisk A/S: CagriSema submitted to the FDA" name="twitter:title"><script>{"datePublished":"2026-09-30T06:00:00+02:00"}</script>`
	if hwPageTitle(page) != "Novo Nordisk A/S: CagriSema submitted to the FDA" || hwPageDate(page) != "2026-09-30" {
		t.Errorf("reversed meta or JSON-LD date: %q %q", hwPageTitle(page), hwPageDate(page))
	}
	if hwOwnerIn("Dexcom announces Team Novo Nordisk partnership", "GlobeNewswire") != "Dexcom" ||
		hwOwnerIn("Quarterly dividend declared", "Business Wire") != "Business Wire" {
		t.Error("hwOwnerIn")
	}
	// Coverage trails its primary: FDA 2026-08-25, Medical Dialogues 2026-09-07.
	if !hwInWindow("2026-09-07", "2026-08-25") || !hwInWindow("2026-10-02", "2026-10-05") ||
		hwInWindow("2026-10-02", "2026-10-07") || hwInWindow("2026-10-05", "2026-09-20") || hwInWindow("", "2026-10-05") {
		t.Error("hwInWindow")
	}
	// novonordisk.com serves the same og:title on every release, the real
	// headline is in a heading (probed 2026-10-05).
	page = `<title>News Details</title><meta property="og:title" content="News Details"/>
<h1 class="plaintexttitle" v-html="applyContentStyle('Press release')"></h1>
<h2 class="subsubheader h2 m-m-bottom" id="sectionHeading">Novo’s Ozempic® (semaglutide) 2 mg associated with lower risk of major adverse cardiovascular events in adults with type 2 diabetes compared to switching to Mounjaro®</h2>`
	got := hwPageTitle(page)
	if !strings.HasPrefix(got, "Novo’s Ozempic® (semaglutide) 2 mg associated") {
		t.Errorf("heading fallback %q", got)
	}
	if ok, m := hwTitleMatch("Higher Ozempic Dose vs. Mounjaro: Which is Better For Heart Health?", got); !ok {
		t.Errorf("Healthline story did not match the Novo release, shared %v", m)
	}
}

// Every primary domain maps to a known row kind and owner, with no host
// listed twice.
func TestTwoaiHealthPrimaryDomains(t *testing.T) {
	kinds := map[string]bool{"company": true, "society": true, "fda": true, "ema": true, "trial": true, "pubmed": true, "journal": true}
	seen := map[string]bool{}
	for _, d := range twoaiHealthPrimaryDomains {
		if seen[d.domain] {
			t.Errorf("domain %s listed twice", d.domain)
		}
		seen[d.domain] = true
		if !kinds[hwRowKind(d)] || d.owner == "" || strings.Contains(d.domain, "/") {
			t.Errorf("bad domain entry %+v", d)
		}
	}
	for _, host := range []string{"investor.lilly.com", "www.novonordisk.com", "news.sanofi.us", "accessdata.fda.gov", "newsroom.heart.org"} {
		if _, ok := hwPrimaryDomain(host); !ok {
			t.Errorf("host %s not primary", host)
		}
	}
	if _, ok := hwPrimaryDomain("notlilly.com"); ok {
		t.Error("notlilly.com matched lilly.com")
	}
}

// Every list entry carries a known kind and topics, so a typo cannot turn a
// source into one that stores nothing.
func TestTwoaiHealthSourceLists(t *testing.T) {
	kinds := map[string]bool{"fda": true, "ema": true, "journal": true, "company": true, "society": true}
	for _, f := range twoaiHealthFeeds {
		if !kinds[f.kind] || len(f.topics) == 0 || !strings.HasPrefix(f.url, "https://") {
			t.Errorf("bad feed %+v", f)
		}
		for _, tp := range f.topics {
			if tp != "diabetes" && tp != "lpa" {
				t.Errorf("feed %s has topic %q", f.source, tp)
			}
		}
	}
	for _, c := range twoaiHealthCoverage {
		if len(c.topics) == 0 || c.query == "" || c.window == "" {
			t.Errorf("bad coverage %+v", c)
		}
	}
	if len(twoaiHealthTrackedTrials) != 4 {
		t.Errorf("tracked trials %v", twoaiHealthTrackedTrials)
	}
}
