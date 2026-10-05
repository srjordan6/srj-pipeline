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
	body := hwBridgeBody("Diabetes watch", "2026-10-05", "the first run", 60, hubs, sources, verify, rows)
	lines := strings.Split(body, "\n")
	if len(lines) > 41 {
		t.Errorf("bridge body has %d lines", len(lines))
	}
	for _, want := range []string{"60 new items", "except those marked coverage", "dmed-glp1 50", "[coverage] ", "[verify amgen-maritide] ", "and 28 more"} {
		if !strings.Contains(body, want) {
			t.Errorf("bridge body lacks %q", want)
		}
	}
	if strings.ContainsRune(body, 0x2014) {
		t.Error("bridge body has an em-dash")
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
