package main

// twoai_health_research: the research library behind the diabetes and Lp(a)
// hubs, kept in twoai_health_papers.
//
// The content project created the table on 2026-10-05 and seeded it with 35
// papers found through Consensus. This stage keeps it growing from OpenAlex
// and fetches open access full text for the papers that matter most to the
// cure hubs. It never touches the content project's own judgement columns:
// promising, full_text_wanted, our_note and consensus_url are read, never
// written, status is written only as 'pending' on a row this stage inserts,
// and found_via is written only on insert.
//
// HARVEST. Twenty OpenAlex searches, one per subtopic the content project
// named, each walking backward one publication year per call: the current
// year first, so 2026, 2025 and 2024 arrive before anything older, then a
// year further back each time the query gets a turn, down to a floor set per
// query (a drug first named in 2018 has nothing to find in 2001). Inside a
// year the 50 most cited works are taken, which is the notability cut: the
// broad queries have thousands of works a year and the long tail is not what
// a reader of a hub page needs. Where each query stands is kept in
// twoai_health_research_state, so a run picks up where the last stopped.
// Once a day each query also re-reads the current and previous year, which
// is where new papers appear and where citation counts move fastest.
//
// The search strings were run against OpenAlex on 2026-10-05 before they
// were listed. Two traps showed up and are designed around. OpenAlex drops
// the "(a)" in "lipoprotein(a)" as a stopword, so the bare term matched
// 14,602 works in 2025, almost all about other lipoproteins, while requiring
// the abbreviation as well brought it to 902 that are about Lp(a). And
// "beta cell regeneration" as a phrase matched wound dressings and bone
// hydrogels, so it is anchored to diabetes. CTX320 has no works in OpenAlex
// yet, so its query is cheap and waits for the first paper.
//
// THE SUBTOPIC IS STORED WITHOUT THE HUB PREFIX, the content project's
// convention: insulin-beta, not dmed-insulin-beta. The page builder shows a
// paper on the sub-hub whose slug ends in its subtopic. Most queries name
// their subtopic outright. Three span hubs and classify each paper from its
// title and abstract: the general Lp(a) search uses the health watch's own
// sub-hub rules (twoaiHealthSubHub), so both stages place a paper the same
// way, and falls back to overview; the SGLT2 search splits kidney from heart;
// the gene therapy search splits type 1 from pipeline. AGENTS.md rule 4: the
// query key and the words that placed the paper are stored in harvest_match.
//
// FULL TEXT, OPEN ACCESS ONLY. A row is checked when the content project
// asked for it (full_text_wanted) or when this stage found it, it sits in a
// cure subtopic (dmed insulin-beta, type1, pipeline, lpa gene, rna) and it is
// highly cited by the rule in twoaiHRHighCited. Each check reads OpenAlex's
// locations and Unpaywall's, and downloads a PDF only from a location that
// is open access under a licence that permits keeping a copy: CC BY in any of
// its six forms, CC0 or public domain. Publisher-specific and other-oa
// licences name terms only a person can read, so they are recorded as they
// are and not downloaded. Nothing is fetched from anywhere but the locations
// OpenAlex and Unpaywall list as open access, no login is attempted and no
// mirror is consulted. A checked row is checked again after 30 days.
//
// WHERE THE PDFS GO, AND WHY NOT THE CONTENT BUCKET. The site's content
// bucket, twoai-content, is publicly readable at its r2.dev address
// (twoai-site/scripts/fetch-content.mjs reads the manifest and bundle from
// there), so any object put in it is one URL away from anyone, whether or
// not the bundle includes it. The PDFs go instead to the private srj-uploads
// bucket through the srj site Worker's bearer-gated /api/archive route, the
// same door archive_news and export_corpus use. That bucket has no public URL
// and the route accepts only corpus/ keys, so the key is
// corpus/health-papers/<doi>.pdf. Nothing in the site build reads srj-uploads.
//
// ABSTRACT BACKFILL (bridge 492). A row with an empty abstract, the content
// project's seed rows among them, gets one from OpenAlex, looked up by DOI
// fifty at a time with the doi:a|b|c filter, and when OpenAlex has none,
// from Crossref, whose abstract is JATS XML and is stored as plain text,
// then from PubMed by the PMID OpenAlex gives, since Elsevier deposits no
// abstracts with Crossref. abstract_source says which: openalex, crossref
// or pubmed. A filled abstract is never overwritten, and a row no source
// could fill is asked again after 30 days, not every run.
//
// LEADS (bridge 492). twoai_health_leads holds claims the content project
// read in secondary sources, MDedge summaries and news reports, and wants
// traced to the paper. Once a day per lead the stage searches OpenAlex and
// Crossref and scores each candidate with twoaiHRScoreLead. A lead resolves
// only on a clear match: the drug or trial the claim names plus its
// population or outcome in the candidate's title or abstract, or, for a
// claim that names no drug or trial, the claim's sample size, journal or
// first author agreeing as well. A journal paper beats a preprint or a
// conference abstract unless the claim itself is a conference abstract.
// The paper goes into twoai_health_papers with found_via 'lead' and the lead
// gets primary_doi, status 'found' and the evidence in resolved_note. The
// secondary source is never stored as the paper. A lead tried 14 days
// without a match becomes 'not found yet' and is tried again weekly.
//
// INTERNAL ONLY. Abstracts and full text are for research and never rendered:
// this stage writes no page and adds nothing to any page document. That holds
// for Crossref and PubMed abstracts too, which are publisher text kept for
// reading only.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	twoaiHRStage = "twoai_health_research"
	// OpenAlex calls a run may make, every step together. List calls cost
	// $0.10 per thousand on the metered API, so 60 a run at eight runs a day
	// is about five cents, well inside the free key's $1 a day and small
	// beside the works spine's 150 pages a run. The split: the harvest stops
	// at 40, the abstract backfill may take 3 (150 DOIs, which clears the 258
	// empty abstracts of 2026-10-05 in two runs), the leads 10 (one search
	// per lead, ten leads a run) and full text has at least 7 plus whatever
	// the backfill and the leads left. Full text reads Unpaywall first, so a
	// short OpenAlex budget costs it the citation refresh, not the licence
	// check.
	twoaiHRCallCap = 60
	// Calls kept back from the harvest for the abstract backfill.
	twoaiHRAbsCalls = 3
	// Calls kept back from the harvest for the leads.
	twoaiHRLeadCalls = 10
	// Calls kept back from the harvest for the full text step.
	twoaiHRFTReserve = 7
	// DOIs per OpenAlex doi filter call, the API's limit for OR values.
	twoaiHRAbsBatch = 50
	// Rows the backfill looks at per run.
	twoaiHRAbsPerRun = twoaiHRAbsCalls * twoaiHRAbsBatch
	// Crossref lookups the backfill makes per run, one per row OpenAlex has
	// no abstract for: every row the run takes, about 45 seconds at most.
	// PubMed then needs one call for all of them.
	twoaiHRCRPerRun = twoaiHRAbsPerRun
	// A row no source could give an abstract is asked again after this.
	twoaiHRAbsRecheck = 30 * 24 * time.Hour
	// Leads tried per run, each at most once in twoaiHRLeadEvery.
	twoaiHRLeadsPerRun = 10
	twoaiHRLeadEvery   = 20 * time.Hour
	// After this many daily tries without a match a lead becomes 'not found
	// yet', which is then tried once a week.
	twoaiHRLeadGiveUp = 14
	twoaiHRLeadRetry  = 7 * 24 * time.Hour
	// How far back from the date the lead was read a paper may be published.
	twoaiHRLeadLookback = 2
	// Rows checked for full text per run.
	twoaiHRFTPerRun = 10
	// Works taken per year window, the most cited first.
	twoaiHRPerPage = 50
	// A row checked for full text is checked again after this long.
	twoaiHRFTRecheck = 30 * 24 * time.Hour
	// The current and previous year are re-read once in this interval.
	twoaiHRRefreshEvery = 20 * time.Hour
	// The largest PDF kept.
	twoaiHRMaxPDF = 40 << 20
	// Time kept back from the stage deadline for the bridge row and the
	// summary line.
	twoaiHRReserve = 90 * time.Second
	twoaiHRUA      = "srj-pipeline/1.0 (srjconsultingservices.com; mailto:" + twoaiOAMailto + ")"
	twoaiHRSelect  = "id,doi,title,display_name,publication_year,type,authorships,cited_by_count,abstract_inverted_index,primary_location"
)

// twoaiHRQuery is one OpenAlex search. sub is the subtopic every result
// gets; when it is empty, classify places each paper and says why.
type twoaiHRQuery struct {
	key, topic, sub, search string
	floor                   int
	classify                func(text string) (sub, why string)
}

var (
	twoaiHRLpaRe    = regexp.MustCompile(`(?i)lipoprotein\s*\(\s*a\s*\)|\blp\s*\(\s*a\s*\)|apolipoprotein\s*\(\s*a\s*\)|\bapo\s*\(\s*a\s*\)|pelacarsen|olpasiran|lepodisiran|zerlasiran|muvalaplin|\bctx-?320\b`)
	twoaiHRKidneyRe = regexp.MustCompile(`(?i)kidney|\brenal\b|\bckd\b|\bdkd\b|nephropath\w*|albuminuri\w*|\begfr\b|dialysis`)
	twoaiHRHeartRe  = regexp.MustCompile(`(?i)heart failure|cardiovascular|\bcardiac\b|\bmace\b|myocardial|\bhfpef\b|\bhfref\b|coronary|\bstroke\b`)
	twoaiHRType1Re  = regexp.MustCompile(`(?i)type 1 diabet\w*|\bt1d\b|\bt1dm\b|autoimmun\w*|\bislets?\b|beta[- ]cells?|β[- ]cells?|hypoimmune`)
)

// twoaiHRClassifyLpa places a paper from the general Lp(a) search with the
// health watch's sub-hub rules, so both stages agree. A paper no rule
// claims is overview, the content project's word for a general item.
func twoaiHRClassifyLpa(text string) (string, string) {
	hub, m := twoaiHealthSubHub("lpa", text)
	sub := strings.TrimPrefix(hub, "lpa-")
	if hub == "lpa" || sub == "" {
		return "overview", ""
	}
	return sub, m
}

// twoaiHRClassifySGLT2 splits SGLT2 outcome papers between the kidney and
// heart hubs. Kidney is tested first, as the health watch does, so a paper
// on kidney and heart outcomes together lands on kidney.
func twoaiHRClassifySGLT2(text string) (string, string) {
	if m := twoaiHRKidneyRe.FindString(text); m != "" {
		return "kidney", m
	}
	if m := twoaiHRHeartRe.FindString(text); m != "" {
		return "heart", m
	}
	return "kidney", ""
}

// twoaiHRClassifyGene splits diabetes gene therapy between the type 1 hub,
// which carries cell and immune work for type 1, and the pipeline hub for
// everything else.
func twoaiHRClassifyGene(text string) (string, string) {
	if m := twoaiHRType1Re.FindString(text); m != "" {
		return "type1", m
	}
	return "pipeline", ""
}

// twoaiHRQueries is the search list. Every string was run against OpenAlex
// on 2026-10-05. Floors are the first year a query can sensibly find
// anything: drug names a year or two before the first trial reports.
var twoaiHRQueries = []twoaiHRQuery{
	// Lp(a).
	{key: "lpa-general", topic: "lpa", search: `"lipoprotein(a)" AND "lp(a)"`, floor: 2000, classify: twoaiHRClassifyLpa},
	{key: "lpa-pelacarsen", topic: "lpa", sub: "rna", search: `pelacarsen OR "TQJ230" OR "AKCEA-APO(a)-LRx"`, floor: 2015},
	{key: "lpa-olpasiran", topic: "lpa", sub: "rna", search: `olpasiran OR "AMG 890"`, floor: 2018},
	{key: "lpa-lepodisiran", topic: "lpa", sub: "rna", search: `lepodisiran OR "LY3819469"`, floor: 2020},
	{key: "lpa-zerlasiran", topic: "lpa", sub: "rna", search: `zerlasiran OR "SLN360"`, floor: 2020},
	{key: "lpa-muvalaplin", topic: "lpa", sub: "oral", search: `muvalaplin OR "LY3473329"`, floor: 2020},
	{key: "lpa-ctx320", topic: "lpa", sub: "gene", search: `CTX320 OR "CTX-320"`, floor: 2023},
	{key: "lpa-gene-editing", topic: "lpa", sub: "gene", search: `("lp(a)" OR "lipoprotein(a)") AND ("gene editing" OR "base editing" OR "epigenetic editing" OR CRISPR)`, floor: 2012},
	{key: "lpa-aortic", topic: "lpa", sub: "aortic", search: `"aortic stenosis" AND "lp(a)"`, floor: 2000},
	// Diabetes and metabolic medicine.
	{key: "dmed-glp1", topic: "dmed", sub: "glp1", search: `"glp-1" OR "glucagon-like peptide-1" OR incretin`, floor: 2000},
	{key: "dmed-triple-amylin", topic: "dmed", sub: "nextgen", search: `retatrutide OR "triple agonist" OR amylin OR cagrilintide OR cagrisema OR amycretin`, floor: 2005},
	{key: "dmed-oral-glp1", topic: "dmed", sub: "nextgen", search: `orforglipron OR "oral semaglutide" OR "oral glp-1"`, floor: 2012},
	{key: "dmed-sglt2-outcomes", topic: "dmed", search: `(empagliflozin OR dapagliflozin OR canagliflozin OR "sglt2" OR "sodium-glucose cotransporter 2") AND (kidney OR renal OR "heart failure" OR cardiovascular)`, floor: 2008, classify: twoaiHRClassifySGLT2},
	{key: "dmed-weekly-insulin", topic: "dmed", sub: "insulin-beta", search: `"insulin icodec" OR efsitora OR "once-weekly insulin" OR "basal insulin fc"`, floor: 2015},
	{key: "dmed-cgm-aid", topic: "dmed", sub: "tech", search: `"continuous glucose monitoring" OR "automated insulin delivery" OR "artificial pancreas" OR "hybrid closed-loop"`, floor: 2000},
	{key: "dmed-remission-surgery", topic: "dmed", sub: "weight", search: `"diabetes remission" OR "remission of type 2 diabetes" OR "type 2 diabetes remission" OR "metabolic surgery"`, floor: 2000},
	{key: "dmed-teplizumab-immuno", topic: "dmed", sub: "type1", search: `teplizumab OR ("type 1 diabetes" AND (immunotherapy OR "disease-modifying"))`, floor: 2000},
	{key: "dmed-stem-hypoimmune", topic: "dmed", sub: "insulin-beta", search: `("stem cell" OR "stem cells" OR hypoimmune) AND (islet OR islets) AND diabetes`, floor: 2000},
	{key: "dmed-beta-regen", topic: "dmed", sub: "insulin-beta", search: `("beta cell" OR "beta cells") AND regeneration AND diabetes`, floor: 2000},
	{key: "dmed-gene-therapy", topic: "dmed", search: `("gene therapy" OR "gene editing" OR "gene-edited") AND ("type 1 diabetes" OR "type 2 diabetes" OR islet OR islets)`, floor: 2000, classify: twoaiHRClassifyGene},
}

// twoaiHRSubtopic returns the subtopic for a paper found by q and the words
// that decided it. A fixed subtopic reports "fixed".
func twoaiHRSubtopic(q twoaiHRQuery, text string) (string, string) {
	if q.sub != "" {
		return q.sub, "fixed"
	}
	if q.classify == nil {
		return "overview", ""
	}
	return q.classify(text)
}

// twoaiHRCure reports whether a subtopic is one of the cure hubs whose
// papers get full text and count toward "promising".
func twoaiHRCure(topic, sub string) bool {
	switch topic {
	case "dmed":
		return sub == "insulin-beta" || sub == "type1" || sub == "pipeline"
	case "lpa":
		return sub == "gene" || sub == "rna"
	}
	return false
}

// twoaiHRHighCited is the citation rule for "promising" and for automatic
// full text: 50 citations, or 10 for a paper published this year or last,
// which has not had time to reach 50. Checked against the seed rows on
// 2026-10-05: the Cell stem cell islet paper (2024, 247) and the NEJM
// islet papers (2025, 112 and 64) pass, as do the 2026 papers the content
// project flagged by hand once they reach ten.
func twoaiHRHighCited(year, citations, nowYear int) bool {
	if citations >= 50 {
		return true
	}
	return year >= nowYear-1 && citations >= 10
}

// twoaiHRDOI normalises a DOI to the table's key: lowercase, no resolver
// prefix, no doi: scheme. Empty when the input does not look like a DOI.
func twoaiHRDOI(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	for _, p := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi.org/", "doi:"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "10.") || !strings.Contains(s, "/") {
		return ""
	}
	return s
}

var twoaiHRCCRe = regexp.MustCompile(`^cc-by(-nc)?(-sa|-nd)?`)

// twoaiHRLicense normalises a licence string from OpenAlex or Unpaywall to
// one short code. CC licences become cc-by, cc-by-nc-nd and so on whatever
// their version or spelling; anything else comes back lowercased as given,
// so a publisher-specific string is kept for a person to read.
func twoaiHRLicense(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if strings.Contains(s, "publicdomain/zero") || s == "cc0" || strings.HasPrefix(s, "cc0-") || strings.HasPrefix(s, "cc0 ") {
		return "cc0"
	}
	if s == "public-domain" || s == "pd" || s == "public domain" || strings.Contains(s, "publicdomain/mark") {
		return "public-domain"
	}
	if i := strings.Index(s, "creativecommons.org/licenses/"); i >= 0 {
		seg := s[i+len("creativecommons.org/licenses/"):]
		if j := strings.IndexAny(seg, "/?#"); j >= 0 {
			seg = seg[:j]
		}
		s = "cc-" + seg
	}
	if m := twoaiHRCCRe.FindString(strings.NewReplacer(" ", "-", "_", "-").Replace(s)); m != "" {
		return m
	}
	return s
}

// twoaiHRPermitted reports whether a normalised licence permits keeping a
// copy of the full text for internal research.
func twoaiHRPermitted(code string) bool {
	switch code {
	case "cc-by", "cc-by-sa", "cc-by-nc", "cc-by-nc-sa", "cc-by-nd", "cc-by-nc-nd", "cc0", "public-domain":
		return true
	}
	return false
}

// twoaiHRR2Key turns a DOI into the object key for its PDF. The archive
// route accepts only letters, digits, dot, underscore, slash and hyphen
// under corpus/, so slash becomes underscore and anything else outside that
// set becomes a hyphen. When a character had to be replaced the key gains
// eight characters of the DOI's hash, so two DOIs that differ only in a
// replaced character can never share a key.
func twoaiHRR2Key(doi string) string {
	doi = twoaiHRDOI(doi)
	var b strings.Builder
	lossy := false
	for _, r := range doi {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		case r == '/':
			b.WriteRune('_')
		default:
			b.WriteRune('-')
			lossy = true
		}
	}
	safe := b.String()
	if strings.Contains(safe, "..") {
		safe = strings.ReplaceAll(safe, "..", "-")
		lossy = true
	}
	if lossy {
		sum := sha256.Sum256([]byte(doi))
		safe += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return "corpus/health-papers/" + safe + ".pdf"
}

// twoaiHRLoc is one OpenAlex location.
type twoaiHRLoc struct {
	IsOA    bool   `json:"is_oa"`
	PDFURL  string `json:"pdf_url"`
	Landing string `json:"landing_page_url"`
	License string `json:"license"`
	Source  *struct {
		Name string `json:"display_name"`
	} `json:"source"`
}

// twoaiHRWork is the works spine's document shape plus the locations this
// stage reads. The outer best_oa_location shadows the spine's narrower one.
type twoaiHRWork struct {
	twoaiOADoc
	Primary   *twoaiHRLoc  `json:"primary_location"`
	Best      *twoaiHRLoc  `json:"best_oa_location"`
	Locations []twoaiHRLoc `json:"locations"`
	// Read by the lead search only: a first page like S61 marks a
	// supplement, where meeting abstracts are printed.
	Biblio *struct {
		FirstPage string `json:"first_page"`
	} `json:"biblio"`
}

func (w twoaiHRWork) venue() string {
	if w.Primary != nil && w.Primary.Source != nil {
		return strings.TrimSpace(w.Primary.Source.Name)
	}
	return ""
}

func (w twoaiHRWork) authorsJSON() string {
	names := []string{}
	for i, a := range w.Authorships {
		if i >= 25 {
			break
		}
		if n := strings.TrimSpace(a.Author.Name); n != "" {
			names = append(names, n)
		}
	}
	b, _ := json.Marshal(names)
	return string(b)
}

// errHRStop ends the OpenAlex part of a run cleanly: the call cap is spent,
// the daily budget is gone, or OpenAlex asked us to wait longer than a run
// should.
var errHRStop = errors.New("openalex stop")

type hrRun struct {
	db      *sql.DB
	client  *http.Client
	stop    time.Time
	nowYear int

	calls, queriesRun          int
	newRows, refreshed         int
	ftChecked, ftSaved, closed int
	offTopic                   int
	backfilled, crAbstracts    int
	pmAbstracts                int
	leadsTried, leadsFound     int
	leadsOpen                  int
	oaStopped                  bool
	notices                    []string
}

func (r *hrRun) notice(format string, a ...any) {
	r.notices = append(r.notices, fmt.Sprintf(format, a...))
}

func (r *hrRun) late() bool { return time.Now().After(r.stop) }

// oaGet makes one OpenAlex call, counted against the run's cap. A 429 that
// carries a short Retry-After is waited out once; a long one, a spent daily
// budget or the cap itself stops OpenAlex for the rest of the run.
func (r *hrRun) oaGet(u string, budget int) ([]byte, int, error) {
	if r.oaStopped {
		return nil, 0, errHRStop
	}
	if !strings.Contains(u, "mailto=") {
		u += "&mailto=" + url.QueryEscape(twoaiOAMailto)
	}
	if k := twoaiEnv("OPENALEX_API_KEY"); k != "" {
		u += "&api_key=" + url.QueryEscape(k)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if r.calls >= budget {
			return nil, 0, errHRStop
		}
		if r.late() {
			r.oaStopped = true
			return nil, 0, errHRStop
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", twoaiHRUA)
		r.calls++
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 40<<20))
		resp.Body.Close()
		code := resp.StatusCode
		if code == 200 || code == 404 {
			return body, code, nil
		}
		if code == 429 {
			// The same three refusals the works spine learned to tell apart:
			// a spent budget and a paid-plan filter never clear by waiting.
			if strings.Contains(string(body), "Insufficient budget") {
				r.oaStopped = true
				r.notice("openalex daily budget spent, harvest stopped for this run")
				return nil, code, errHRStop
			}
			if strings.Contains(string(body), "Plan upgrade required") {
				return nil, code, fmt.Errorf("openalex refused a filter the plan does not include: %s", truncate(string(body), 160))
			}
			wait := 10 * time.Second
			if s, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && s >= 0 {
				wait = time.Duration(s) * time.Second
			}
			if wait > 60*time.Second || attempt == 1 {
				r.oaStopped = true
				r.notice("openalex rate limited (Retry-After %s), harvest stopped cleanly for this run", wait)
				return nil, code, errHRStop
			}
			time.Sleep(wait)
			continue
		}
		if code >= 500 && attempt == 0 {
			time.Sleep(5 * time.Second)
			continue
		}
		return nil, code, fmt.Errorf("openalex http %d: %s", code, truncate(string(body), 160))
	}
	return nil, 0, fmt.Errorf("openalex retries exhausted")
}

func twoaiHREnsure(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE twoai_health_papers
		ADD COLUMN IF NOT EXISTS authors jsonb,
		ADD COLUMN IF NOT EXISTS openalex_id text,
		ADD COLUMN IF NOT EXISTS harvested_at timestamptz,
		ADD COLUMN IF NOT EXISTS discovered_at timestamptz,
		ADD COLUMN IF NOT EXISTS harvest_match text,
		ADD COLUMN IF NOT EXISTS ft_checked_at timestamptz,
		ADD COLUMN IF NOT EXISTS ft_error text,
		ADD COLUMN IF NOT EXISTS abstract_checked_at timestamptz`); err != nil {
		return fmt.Errorf("alter twoai_health_papers: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_health_research_state (
		query_key text PRIMARY KEY,
		search text NOT NULL,
		next_year int NOT NULL,
		floor_year int NOT NULL,
		walk_done bool NOT NULL DEFAULT false,
		refreshed_at timestamptz NOT NULL DEFAULT now(),
		last_year int,
		last_count int,
		last_error text,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create twoai_health_research_state: %w", err)
	}
	// One bridge row per calendar day.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_health_research_bridge (
		sent_on date PRIMARY KEY DEFAULT current_date,
		sent_at timestamptz NOT NULL DEFAULT now(),
		new_papers int NOT NULL,
		promising int NOT NULL,
		full_texts int NOT NULL,
		bridge_topic text)`); err != nil {
		return fmt.Errorf("create twoai_health_research_bridge: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE twoai_health_research_bridge
		ADD COLUMN IF NOT EXISTS leads_found int,
		ADD COLUMN IF NOT EXISTS leads_open int`); err != nil {
		return fmt.Errorf("alter twoai_health_research_bridge: %w", err)
	}
	return nil
}

// twoaiHREnsureLeads adds the columns this stage keeps on the content
// project's leads table. The table is theirs, so a failure here skips the
// leads for the run instead of failing the stage.
func twoaiHREnsureLeads(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE twoai_health_leads
		ADD COLUMN IF NOT EXISTS resolved_note text,
		ADD COLUMN IF NOT EXISTS attempts int NOT NULL DEFAULT 0,
		ADD COLUMN IF NOT EXISTS attempted_at timestamptz,
		ADD COLUMN IF NOT EXISTS resolved_at timestamptz`)
	return err
}

// twoaiHealthResearch is the stage. Per-query and per-paper failures are
// notices; only a table that cannot be prepared fails it.
func twoaiHealthResearch(db *sql.DB) error {
	start := time.Now()
	if err := twoaiHREnsure(db); err != nil {
		return err
	}
	limit := twoaiStageDeadlineDefault
	if d, ok := twoaiStageDeadline[twoaiHRStage]; ok {
		limit = d
	}
	r := &hrRun{
		db: db, client: &http.Client{Timeout: 90 * time.Second},
		stop: start.Add(limit - twoaiHRReserve), nowYear: time.Now().UTC().Year(),
	}

	leadsReady := true
	if err := twoaiHREnsureLeads(db); err != nil {
		leadsReady = false
		r.notice("leads table not ready, leads skipped this run: %v", err)
	}

	r.harvest()
	r.backfill()
	if leadsReady {
		r.leads()
		r.db.QueryRow(`SELECT count(*) FROM twoai_health_leads
			WHERE primary_doi IS NULL AND status IN ('find primary','not found yet')`).Scan(&r.leadsOpen)
	}
	r.fullText()
	r.bridge(leadsReady)

	for _, n := range r.notices {
		// Stdout, not stderr: PowerShell logs stderr as a NativeCommandError.
		fmt.Println("twoai_health_research: notice:", n)
	}
	fmt.Printf("twoai_health_research: queries=%d calls=%d new=%d refreshed=%d backfilled=%d (crossref=%d pubmed=%d) leads_tried=%d leads_found=%d leads_open=%d ft_checked=%d ft_saved=%d closed=%d off_topic=%d ok=true\n",
		r.queriesRun, r.calls, r.newRows, r.refreshed, r.backfilled, r.crAbstracts, r.pmAbstracts, r.leadsTried, r.leadsFound, r.leadsOpen,
		r.ftChecked, r.ftSaved, r.closed, r.offTopic)
	return nil
}

// hrState is one query's place in its walk.
type hrState struct {
	q         twoaiHRQuery
	nextYear  int
	walkDone  bool
	refresh   []int // years still to re-read this run
	attempted bool
}

func (r *hrRun) loadState(q twoaiHRQuery) (*hrState, error) {
	st := &hrState{q: q}
	var search string
	var refreshedAt time.Time
	err := r.db.QueryRow(`SELECT search, next_year, walk_done, refreshed_at
		FROM twoai_health_research_state WHERE query_key=$1`, q.key).Scan(&search, &st.nextYear, &st.walkDone, &refreshedAt)
	if err == sql.ErrNoRows {
		st.nextYear = r.nowYear
		_, err = r.db.Exec(`INSERT INTO twoai_health_research_state (query_key, search, next_year, floor_year)
			VALUES ($1,$2,$3,$4)`, q.key, q.search, st.nextYear, q.floor)
		return st, err
	}
	if err != nil {
		return nil, err
	}
	// A changed search string walks again from the current year: the old
	// walk answered a different question. Upserts make the repeat harmless.
	if search != q.search {
		st.nextYear, st.walkDone = r.nowYear, false
		r.db.Exec(`UPDATE twoai_health_research_state SET search=$1, next_year=$2, floor_year=$3, walk_done=false,
			refreshed_at=now(), updated_at=now() WHERE query_key=$4`, q.search, st.nextYear, q.floor, q.key)
		r.notice("%s: search changed, walking again from %d", q.key, st.nextYear)
		return st, nil
	}
	if time.Since(refreshedAt) > twoaiHRRefreshEvery {
		for _, y := range []int{r.nowYear, r.nowYear - 1} {
			// A year the walk has not reached yet will be read by the walk.
			if st.walkDone || y > st.nextYear {
				st.refresh = append(st.refresh, y)
			}
		}
	}
	return st, nil
}

// harvest gives every query one call per round, refresh years first, until
// the call budget, the clock or the work runs out. The starting query
// rotates by day so a budget that runs short does not always short the same
// queries.
func (r *hrRun) harvest() {
	var states []*hrState
	for _, q := range twoaiHRQueries {
		st, err := r.loadState(q)
		if err != nil {
			r.notice("%s: state: %v", q.key, err)
			continue
		}
		states = append(states, st)
	}
	if len(states) == 0 {
		return
	}
	budget := twoaiHRCallCap - twoaiHRAbsCalls - twoaiHRLeadCalls - twoaiHRFTReserve
	offset := time.Now().UTC().YearDay() % len(states)
	for {
		progressed := false
		for i := range states {
			if r.oaStopped || r.calls >= budget || r.late() {
				return
			}
			st := states[(offset+i)%len(states)]
			var year int
			refreshing := false
			switch {
			case len(st.refresh) > 0:
				year, refreshing = st.refresh[0], true
			case !st.walkDone:
				year = st.nextYear
			default:
				continue
			}
			if !st.attempted {
				st.attempted = true
				r.queriesRun++
			}
			n, err := r.harvestYear(st.q, year, budget)
			if errors.Is(err, errHRStop) {
				return
			}
			progressed = true
			if err != nil {
				r.notice("%s %d: %v", st.q.key, year, err)
				r.db.Exec(`UPDATE twoai_health_research_state SET last_year=$1, last_error=$2, updated_at=now() WHERE query_key=$3`,
					year, truncate(err.Error(), 300), st.q.key)
				// A failing query waits for the next run rather than
				// spending this run's budget on the same refusal.
				st.refresh, st.walkDone = nil, true
				continue
			}
			if refreshing {
				st.refresh = st.refresh[1:]
				if len(st.refresh) == 0 {
					r.db.Exec(`UPDATE twoai_health_research_state SET refreshed_at=now(), last_year=$1, last_count=$2,
						last_error=NULL, updated_at=now() WHERE query_key=$3`, year, n, st.q.key)
				}
				continue
			}
			st.nextYear--
			if st.nextYear < st.q.floor {
				st.walkDone = true
			}
			r.db.Exec(`UPDATE twoai_health_research_state SET next_year=$1, walk_done=$2, last_year=$3, last_count=$4,
				last_error=NULL, updated_at=now() WHERE query_key=$5`, st.nextYear, st.walkDone, year, n, st.q.key)
		}
		if !progressed {
			return
		}
	}
}

// harvestYear reads the most cited works for one query in one publication
// year and upserts them. It returns how many works came back.
func (r *hrRun) harvestYear(q twoaiHRQuery, year, budget int) (int, error) {
	filter := fmt.Sprintf("title_and_abstract.search:%s,publication_year:%d,type:article|review,has_doi:true", q.search, year)
	u := fmt.Sprintf("https://api.openalex.org/works?filter=%s&sort=cited_by_count:desc&per-page=%d&select=%s",
		url.QueryEscape(filter), twoaiHRPerPage, url.QueryEscape(twoaiHRSelect))
	body, _, err := r.oaGet(u, budget)
	if err != nil {
		return 0, err
	}
	var out struct {
		Results []twoaiHRWork `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	for _, w := range out.Results {
		doi := twoaiHRDOI(w.DOI)
		if doi == "" {
			continue // the table is keyed on DOI; a work without one is skipped
		}
		if err := r.upsert(q, doi, w); err != nil {
			r.notice("%s %s: %v", q.key, doi, err)
		}
	}
	time.Sleep(350 * time.Millisecond)
	return len(out.Results), nil
}

// upsert inserts a new paper or refreshes an existing one. An existing row
// gets only its citation count, OpenAlex id, authors and, when it has none,
// an abstract; every column the content project curates is left alone.
func (r *hrRun) upsert(q twoaiHRQuery, doi string, w twoaiHRWork) error {
	title := strings.TrimSpace(w.Title)
	if title == "" {
		title = strings.TrimSpace(w.Display)
	}
	if title == "" {
		return fmt.Errorf("work has no title")
	}
	abstract := twoaiOAAbstract(w.AbstractII)
	// OpenAlex search drops "(a)", so "lp(a)" matched any "lp" and the gene
	// editing query brought PCSK9 editors and a paper on phosphate tolerance
	// in plants. An Lp(a) paper must name Lp(a) or one of its drugs.
	if q.topic == "lpa" && !twoaiHRLpaRe.MatchString(title+" "+abstract) {
		r.offTopic++
		return nil
	}
	sub, why := twoaiHRSubtopic(q, title+" "+abstract)
	match := "query=" + q.key + " subtopic=" + sub
	if why != "" {
		match += " matched=" + why
	}
	var year any
	if w.PubYear > 0 {
		year = w.PubYear
	}
	oid := strings.TrimPrefix(w.ID, "https://openalex.org/")
	var inserted bool
	err := r.db.QueryRow(`INSERT INTO twoai_health_papers
		(doi, title, year, journal, citations, topic, subtopic, found_via, abstract, abstract_source,
		 status, added_on, authors, openalex_id, harvested_at, discovered_at, harvest_match)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,'openalex',NULLIF($8,''),CASE WHEN $8 = '' THEN NULL ELSE 'openalex' END,
		 'pending',current_date,$9::jsonb,NULLIF($10,''),now(),now(),$11)
		ON CONFLICT (doi) DO UPDATE SET
			citations = EXCLUDED.citations,
			openalex_id = COALESCE(EXCLUDED.openalex_id, twoai_health_papers.openalex_id),
			authors = CASE WHEN EXCLUDED.authors = '[]'::jsonb THEN twoai_health_papers.authors ELSE EXCLUDED.authors END,
			abstract_source = CASE WHEN COALESCE(twoai_health_papers.abstract,'') = '' AND EXCLUDED.abstract IS NOT NULL
				THEN 'openalex' ELSE twoai_health_papers.abstract_source END,
			abstract = CASE WHEN COALESCE(twoai_health_papers.abstract,'') = '' THEN EXCLUDED.abstract
				ELSE twoai_health_papers.abstract END,
			harvested_at = now()
		RETURNING (xmax = 0)`,
		doi, title, year, w.venue(), w.CitedBy, q.topic, sub, abstract,
		w.authorsJSON(), oid, match).Scan(&inserted)
	if err != nil {
		return err
	}
	if inserted {
		r.newRows++
	} else {
		r.refreshed++
	}
	return nil
}

// hrCand is one place a full text might be, with the licence it carries.
type hrCand struct {
	pdf, landing, license, from string
}

// fullText checks the rows that want full text, newest asks first.
func (r *hrRun) fullText() {
	rows, err := r.db.Query(`SELECT doi, topic, COALESCE(subtopic,''), COALESCE(year,0), COALESCE(citations,0), full_text_wanted
		FROM twoai_health_papers
		WHERE full_text_r2 IS NULL
		  AND (ft_checked_at IS NULL OR ft_checked_at < now() - make_interval(secs => $1))
		  AND (full_text_wanted
		       OR (found_via = 'openalex'
		           AND ((topic = 'dmed' AND subtopic IN ('insulin-beta','type1','pipeline'))
		             OR (topic = 'lpa' AND subtopic IN ('gene','rna')))
		           AND (citations >= 50 OR (year >= $2 AND citations >= 10))))
		ORDER BY full_text_wanted DESC, citations DESC NULLS LAST, doi
		LIMIT $3`, twoaiHRFTRecheck.Seconds(), r.nowYear-1, twoaiHRFTPerRun)
	if err != nil {
		r.notice("full text queue: %v", err)
		return
	}
	type ftRow struct {
		doi, topic, sub string
		year, cites     int
		wanted          bool
	}
	var todo []ftRow
	for rows.Next() {
		var f ftRow
		if rows.Scan(&f.doi, &f.topic, &f.sub, &f.year, &f.cites, &f.wanted) == nil {
			todo = append(todo, f)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}
	endpoint, token := os.Getenv("ARCHIVE_ENDPOINT"), os.Getenv("ARCHIVE_TOKEN")
	canStore := endpoint != "" && token != ""
	if !canStore {
		r.notice("ARCHIVE_ENDPOINT or ARCHIVE_TOKEN not set: licences are checked but no PDF is stored, and permitted rows stay queued")
	}
	for _, f := range todo {
		if r.late() {
			r.notice("deadline reached, %d full text checks left for the next run", len(todo)-r.ftChecked)
			return
		}
		r.checkOne(f.doi, endpoint, token, canStore)
		time.Sleep(time.Second)
	}
}

// checkOne resolves one DOI's open access locations, records the licence,
// and stores a permitted PDF.
func (r *hrRun) checkOne(doi, endpoint, token string, canStore bool) {
	var cands []hrCand
	anyOA, sourced := false, false

	// Unpaywall first: it lists every OA copy with its licence, including
	// repository copies OpenAlex's best location may not prefer.
	if up, err := twoaiHRUnpaywall(r.client, doi); err != nil {
		r.notice("unpaywall %s: %v", doi, err)
	} else if up != nil {
		sourced = true
		anyOA = anyOA || up.IsOA
		locs := up.Locations
		if up.Best != nil {
			locs = append([]twoaiHRUPLoc{*up.Best}, locs...)
		}
		for _, l := range locs {
			cands = append(cands, hrCand{pdf: l.PDF, landing: firstNonEmpty(l.Landing, l.URL), license: twoaiHRLicense(l.License), from: "unpaywall"})
		}
	}

	// OpenAlex, which also refreshes the row's own fields.
	u := fmt.Sprintf("https://api.openalex.org/works/doi:%s?select=%s", url.QueryEscape(doi),
		url.QueryEscape(twoaiHRSelect+",best_oa_location,locations"))
	if body, code, err := r.oaGet(u, twoaiHRCallCap); err == nil && code == 200 {
		var w twoaiHRWork
		if json.Unmarshal(body, &w) == nil {
			sourced = true
			r.refreshExisting(doi, w)
			locs := w.Locations
			if w.Best != nil {
				locs = append([]twoaiHRLoc{*w.Best}, locs...)
			}
			for _, l := range locs {
				if !l.IsOA {
					continue
				}
				anyOA = true
				cands = append(cands, hrCand{pdf: l.PDFURL, landing: l.Landing, license: twoaiHRLicense(l.License), from: "openalex"})
			}
		}
	} else if err != nil && !errors.Is(err, errHRStop) {
		r.notice("openalex %s: %v", doi, err)
	} else if code == 404 {
		sourced = true
	}
	if !sourced {
		// Neither source answered: try again next run, not in 30 days.
		r.db.Exec(`UPDATE twoai_health_papers SET ft_error='no open access source answered' WHERE doi=$1`, doi)
		return
	}
	r.ftChecked++

	var permitted []hrCand
	notPermitted := ""
	for _, c := range cands {
		if twoaiHRPermitted(c.license) {
			permitted = append(permitted, c)
		} else if c.license != "" && notPermitted == "" {
			notPermitted = c.license
		}
	}

	if len(permitted) == 0 {
		lic, oaURL := "closed", ""
		switch {
		case notPermitted != "":
			lic = notPermitted
		case anyOA:
			lic = "oa-no-licence"
		}
		if anyOA && len(cands) > 0 {
			oaURL = firstNonEmpty(cands[0].landing, cands[0].pdf)
		}
		if lic == "closed" {
			r.closed++
		}
		r.db.Exec(`UPDATE twoai_health_papers SET oa_license=$1, oa_url=COALESCE(NULLIF($2,''), oa_url),
			ft_checked_at=now(), ft_error=NULL WHERE doi=$3`, lic, oaURL, doi)
		return
	}

	best := permitted[0]
	if !canStore {
		r.db.Exec(`UPDATE twoai_health_papers SET oa_license=$1, oa_url=COALESCE(NULLIF($2,''), oa_url),
			ft_error='permitted, not stored: archive endpoint not configured' WHERE doi=$3`,
			best.license, firstNonEmpty(best.pdf, best.landing), doi)
		return
	}
	var lastErr error
	tried := map[string]bool{}
	for _, c := range permitted {
		if c.pdf == "" || tried[c.pdf] {
			continue
		}
		tried[c.pdf] = true
		if len(tried) > 3 {
			break
		}
		pdf, err := twoaiHRFetchPDF(r.client, c.pdf)
		if err != nil {
			lastErr = err
			continue
		}
		key := twoaiHRR2Key(doi)
		if err := archivePut(endpoint, token, key, "application/pdf", pdf); err != nil {
			lastErr = err
			break
		}
		r.db.Exec(`UPDATE twoai_health_papers SET oa_license=$1, oa_url=$2, full_text_r2=$3,
			ft_checked_at=now(), ft_error=NULL WHERE doi=$4`, c.license, c.pdf, key, doi)
		r.ftSaved++
		return
	}
	msg := "permitted licence but no PDF link"
	if lastErr != nil {
		msg = truncate(lastErr.Error(), 300)
	}
	r.db.Exec(`UPDATE twoai_health_papers SET oa_license=$1, oa_url=COALESCE(NULLIF($2,''), oa_url),
		ft_checked_at=now(), ft_error=$3 WHERE doi=$4`, best.license, firstNonEmpty(best.pdf, best.landing), msg, doi)
}

// refreshExisting writes the same refresh the harvest does, for a row the
// full text step looked up by DOI.
func (r *hrRun) refreshExisting(doi string, w twoaiHRWork) {
	abstract := twoaiOAAbstract(w.AbstractII)
	oid := strings.TrimPrefix(w.ID, "https://openalex.org/")
	r.db.Exec(`UPDATE twoai_health_papers SET
		citations = $1,
		openalex_id = COALESCE(NULLIF($2,''), openalex_id),
		authors = CASE WHEN $3::jsonb = '[]'::jsonb THEN authors ELSE $3::jsonb END,
		abstract_source = CASE WHEN COALESCE(abstract,'') = '' AND $4 <> '' THEN 'openalex' ELSE abstract_source END,
		abstract = CASE WHEN COALESCE(abstract,'') = '' THEN NULLIF($4,'') ELSE abstract END,
		harvested_at = now()
		WHERE doi = $5`, w.CitedBy, oid, w.authorsJSON(), abstract, doi)
}

// twoaiHRUPLoc is one Unpaywall location.
type twoaiHRUPLoc struct {
	URL     string `json:"url"`
	PDF     string `json:"url_for_pdf"`
	Landing string `json:"url_for_landing_page"`
	License string `json:"license"`
}

type twoaiHRUnpaywallResp struct {
	IsOA      bool           `json:"is_oa"`
	Best      *twoaiHRUPLoc  `json:"best_oa_location"`
	Locations []twoaiHRUPLoc `json:"oa_locations"`
}

// twoaiHRDOIPath escapes a DOI for a URL path, keeping its slashes.
func twoaiHRDOIPath(doi string) string {
	parts := strings.Split(doi, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// twoaiHRUnpaywall looks a DOI up in Unpaywall. A DOI Unpaywall does not
// know returns nil and no error.
func twoaiHRUnpaywall(client *http.Client, doi string) (*twoaiHRUnpaywallResp, error) {
	u := "https://api.unpaywall.org/v2/" + twoaiHRDOIPath(doi) + "?email=" + url.QueryEscape(twoaiOAMailto)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", twoaiHRUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	var out twoaiHRUnpaywallResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// twoaiHRFetchPDF downloads one open access PDF. It sends no cookies and no
// credentials, identifies itself, and keeps the body only when the server
// says it is a PDF (or a generic binary) and the bytes agree.
func twoaiHRFetchPDF(client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", twoaiHRUA)
	req.Header.Set("Accept", "application/pdf")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pdf http %d from %s", resp.StatusCode, truncate(u, 120))
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !twoaiHRPDFType(ct) {
		return nil, fmt.Errorf("not a PDF (content type %q) at %s", ct, truncate(u, 120))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, twoaiHRMaxPDF+1))
	if err != nil {
		return nil, err
	}
	if len(body) > twoaiHRMaxPDF {
		return nil, fmt.Errorf("PDF over %d MB at %s", twoaiHRMaxPDF>>20, truncate(u, 120))
	}
	if !twoaiHRIsPDF(body) {
		return nil, fmt.Errorf("body is not a PDF (no %%PDF header) at %s", truncate(u, 120))
	}
	return body, nil
}

// twoaiHRPDFType accepts a PDF content type, or a generic binary one that
// the magic bytes must then confirm.
func twoaiHRPDFType(ct string) bool {
	return strings.Contains(ct, "pdf") || strings.Contains(ct, "octet-stream") || strings.Contains(ct, "binary")
}

// twoaiHRIsPDF looks for the %PDF- header within the first kilobyte, where
// the PDF specification allows it to sit.
func twoaiHRIsPDF(b []byte) bool {
	head := b
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(head, []byte("%PDF-"))
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// Abstract backfill.
// ---------------------------------------------------------------------

// twoaiHRDOIBatches splits DOIs into groups for OpenAlex's doi filter. A
// DOI holding a comma or a pipe would break the filter syntax, so it is
// returned apart, for Crossref alone.
func twoaiHRDOIBatches(dois []string, size int) (batches [][]string, apart []string) {
	var cur []string
	for _, d := range dois {
		if strings.ContainsAny(d, ",|") {
			apart = append(apart, d)
			continue
		}
		cur = append(cur, d)
		if len(cur) == size {
			batches = append(batches, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, apart
}

// backfill fills empty abstracts, OpenAlex first, Crossref for what OpenAlex
// lacks, and PubMed for what both lack when OpenAlex knows the PMID. PubMed
// was added because Elsevier deposits no abstracts with Crossref and OpenAlex
// has none for recent Lancet papers either: on 2026-10-05 neither source had
// one for the content project's two named seed rows, while PubMed had both.
// The content project's own rows come first, since the harvest has already
// asked OpenAlex about its own.
func (r *hrRun) backfill() {
	rows, err := r.db.Query(`SELECT doi FROM twoai_health_papers
		WHERE COALESCE(abstract,'') = ''
		  AND (abstract_checked_at IS NULL OR abstract_checked_at < now() - make_interval(secs => $1))
		ORDER BY abstract_checked_at NULLS FIRST, (found_via = 'openalex'), citations DESC NULLS LAST, doi
		LIMIT $2`, twoaiHRAbsRecheck.Seconds(), twoaiHRAbsPerRun)
	if err != nil {
		r.notice("abstract backfill queue: %v", err)
		return
	}
	var dois []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			dois = append(dois, d)
		}
	}
	rows.Close()
	if len(dois) == 0 {
		return
	}
	budget := r.calls + twoaiHRAbsCalls
	if budget > twoaiHRCallCap {
		budget = twoaiHRCallCap
	}
	batches, apart := twoaiHRDOIBatches(dois, twoaiHRAbsBatch)
	fromOA := map[string]string{}
	pmids := map[string]string{}
	// asked marks the DOIs OpenAlex has answered for this run, so a row no
	// source can fill is set aside for 30 days only when every source really
	// was asked.
	asked := map[string]bool{}
	for _, d := range apart {
		asked[d] = true
	}
	for _, batch := range batches {
		got, err := r.oaAbstracts(batch, budget)
		if err != nil {
			if !errors.Is(err, errHRStop) {
				r.notice("abstract backfill openalex: %v", err)
			}
			break
		}
		for _, d := range batch {
			asked[d] = true
		}
		for d, w := range got {
			if w.abstract != "" {
				fromOA[d] = w.abstract
			}
			if w.pmid != "" {
				pmids[d] = w.pmid
			}
		}
	}
	setAside := func(doi string) {
		if asked[doi] {
			r.db.Exec(`UPDATE twoai_health_papers SET abstract_checked_at = now() WHERE doi = $1 AND COALESCE(abstract,'') = ''`, doi)
		}
	}
	crCalls, crErrs := 0, 0
	var crErr error
	var pmQueue []string // DOIs Crossref could not fill that PubMed may know
	for _, doi := range dois {
		if a := fromOA[doi]; a != "" {
			r.fillAbstract(doi, a, "openalex")
			continue
		}
		if crCalls >= twoaiHRCRPerRun || r.late() {
			continue // left unmarked, so the next run asks again
		}
		crCalls++
		a, err := twoaiHRCrossrefAbstract(r.client, doi)
		time.Sleep(200 * time.Millisecond)
		if err != nil {
			crErrs++
			crErr = err
			continue
		}
		if a != "" {
			if r.fillAbstract(doi, a, "crossref") {
				r.crAbstracts++
			}
			continue
		}
		if pmids[doi] != "" {
			pmQueue = append(pmQueue, doi)
			continue
		}
		setAside(doi)
	}
	if crErrs > 0 {
		r.notice("abstract backfill: %d Crossref lookups failed, the last: %v", crErrs, crErr)
	}
	if len(pmQueue) == 0 || r.late() {
		return
	}
	ids := make([]string, len(pmQueue))
	for i, d := range pmQueue {
		ids[i] = pmids[d]
	}
	got, err := twoaiHRPubmedAbstracts(r.client, ids)
	if err != nil {
		r.notice("abstract backfill pubmed: %v", err)
		return
	}
	for _, doi := range pmQueue {
		if a := got[pmids[doi]]; a != "" {
			if r.fillAbstract(doi, a, "pubmed") {
				r.pmAbstracts++
			}
			continue
		}
		setAside(doi)
	}
}

// hrOAAbs is what the backfill reads from OpenAlex for one DOI.
type hrOAAbs struct {
	abstract, pmid string
}

// oaAbstracts looks up to fifty DOIs up in one OpenAlex call and returns,
// keyed by normalised DOI, the abstract and the PMID it has for each.
func (r *hrRun) oaAbstracts(dois []string, budget int) (map[string]hrOAAbs, error) {
	u := fmt.Sprintf("https://api.openalex.org/works?filter=%s&per-page=%d&select=%s",
		url.QueryEscape("doi:"+strings.Join(dois, "|")), twoaiHRAbsBatch, url.QueryEscape("doi,ids,abstract_inverted_index"))
	body, code, err := r.oaGet(u, budget)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("openalex http %d", code)
	}
	var out struct {
		Results []twoaiOADoc `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	got := map[string]hrOAAbs{}
	for _, w := range out.Results {
		d := twoaiHRDOI(w.DOI)
		if d == "" {
			continue
		}
		prev := got[d]
		if a := twoaiOAAbstract(w.AbstractII); a != "" {
			prev.abstract = a
		}
		if p := strings.TrimRight(strings.TrimPrefix(w.IDs.PMID, "https://pubmed.ncbi.nlm.nih.gov/"), "/"); p != "" {
			prev.pmid = p
		}
		got[d] = prev
	}
	time.Sleep(350 * time.Millisecond)
	return got, nil
}

// twoaiHRPubmedSet is the part of a PubMed efetch XML response the backfill
// reads.
type twoaiHRPubmedSet struct {
	Articles []struct {
		PMID  string `xml:"MedlineCitation>PMID"`
		Parts []struct {
			Label string `xml:"Label,attr"`
			Inner string `xml:",innerxml"`
		} `xml:"MedlineCitation>Article>Abstract>AbstractText"`
	} `xml:"PubmedArticle"`
}

// twoaiHRParsePubmed returns each article's abstract keyed by PMID, a
// structured abstract's sections joined as "LABEL: text".
func twoaiHRParsePubmed(body []byte) (map[string]string, error) {
	var set twoaiHRPubmedSet
	if err := xml.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range set.Articles {
		var parts []string
		for _, p := range a.Parts {
			t := twoaiHRStripJATS(p.Inner)
			if t == "" {
				continue
			}
			if l := strings.TrimSpace(p.Label); l != "" {
				t = l + ": " + t
			}
			parts = append(parts, t)
		}
		if pmid := strings.TrimSpace(a.PMID); pmid != "" && len(parts) > 0 {
			out[pmid] = strings.Join(parts, " ")
		}
	}
	return out, nil
}

// twoaiHRPubmedAbstracts fetches abstracts for up to 200 PMIDs in one NCBI
// E-utilities call, with the health watch's tool name and key.
func twoaiHRPubmedAbstracts(client *http.Client, pmids []string) (map[string]string, error) {
	if len(pmids) > 200 {
		pmids = pmids[:200]
	}
	v := url.Values{}
	v.Set("db", "pubmed")
	v.Set("id", strings.Join(pmids, ","))
	v.Set("retmode", "xml")
	v.Set("tool", twoaiHealthNCBITool)
	v.Set("email", twoaiHealthNCBIEmail)
	if k := twoaiEnv("NCBI_API_KEY"); k != "" {
		v.Set("api_key", k)
	}
	req, _ := http.NewRequest("POST", "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/efetch.fcgi", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", twoaiHRUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("efetch http %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	return twoaiHRParsePubmed(body)
}

// fillAbstract writes an abstract into a row that still has none. It
// reports whether the row took it.
func (r *hrRun) fillAbstract(doi, abstract, source string) bool {
	res, err := r.db.Exec(`UPDATE twoai_health_papers SET abstract = $1, abstract_source = $2, abstract_checked_at = now()
		WHERE doi = $3 AND COALESCE(abstract,'') = ''`, abstract, source, doi)
	if err != nil {
		r.notice("abstract %s: %v", doi, err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		r.backfilled++
		return true
	}
	return false
}

// ---------------------------------------------------------------------
// Crossref.
// ---------------------------------------------------------------------

const twoaiHRCrossrefSelect = "DOI,title,type,container-title,publisher,abstract,is-referenced-by-count,author,issued,page"

// twoaiHRCRWork is one Crossref work, the fields this stage reads.
type twoaiHRCRWork struct {
	DOI       string   `json:"DOI"`
	Type      string   `json:"type"`
	Title     []string `json:"title"`
	Container []string `json:"container-title"`
	Publisher string   `json:"publisher"`
	Abstract  string   `json:"abstract"`
	Page      string   `json:"page"`
	Cited     int      `json:"is-referenced-by-count"`
	Author    []struct {
		Given  string `json:"given"`
		Family string `json:"family"`
		Name   string `json:"name"`
	} `json:"author"`
	Issued struct {
		Parts [][]int `json:"date-parts"`
	} `json:"issued"`
}

// candidate turns a Crossref work into a lead candidate.
func (w twoaiHRCRWork) candidate() hrCandidate {
	c := hrCandidate{doi: twoaiHRDOI(w.DOI), typ: w.Type, from: "crossref", cites: w.Cited, abstract: twoaiHRStripJATS(w.Abstract), page: w.Page}
	if len(w.Title) > 0 {
		c.title = strings.TrimSpace(w.Title[0])
	}
	if len(w.Container) > 0 {
		c.venue = strings.TrimSpace(w.Container[0])
	}
	if len(w.Issued.Parts) > 0 && len(w.Issued.Parts[0]) > 0 {
		c.year = w.Issued.Parts[0][0]
	}
	for _, a := range w.Author {
		if n := strings.TrimSpace(strings.TrimSpace(a.Given) + " " + strings.TrimSpace(a.Family)); n != "" {
			c.authors = append(c.authors, n)
		} else if n := strings.TrimSpace(a.Name); n != "" {
			c.authors = append(c.authors, n)
		}
	}
	return c
}

// twoaiHRCrossrefGet makes one Crossref call in the polite pool, named by
// the mailto parameter and the user agent. A 404 returns no body and no
// error.
func twoaiHRCrossrefGet(client *http.Client, u string) ([]byte, error) {
	if !strings.Contains(u, "mailto=") {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "mailto=" + url.QueryEscape(twoaiOAMailto)
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", twoaiHRUA)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == 200:
			return body, nil
		case resp.StatusCode == 404:
			return nil, nil
		case (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt == 0:
			time.Sleep(5 * time.Second)
			continue
		}
		return nil, fmt.Errorf("crossref http %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	return nil, fmt.Errorf("crossref retries exhausted")
}

// twoaiHRCrossrefAbstract returns a DOI's abstract from Crossref as plain
// text, or "" when Crossref has none.
func twoaiHRCrossrefAbstract(client *http.Client, doi string) (string, error) {
	body, err := twoaiHRCrossrefGet(client, "https://api.crossref.org/works/"+twoaiHRDOIPath(doi))
	if err != nil || body == nil {
		return "", err
	}
	var out struct {
		Message twoaiHRCRWork `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	return twoaiHRStripJATS(out.Message.Abstract), nil
}

var (
	twoaiHRBlockTagRe = regexp.MustCompile(`(?i)</?(?:jats:)?(?:p|title|sec|list|list-item|label|abstract|break|br)\b[^>]*>`)
	twoaiHRTagRe      = regexp.MustCompile(`<[^>]*>`)
	twoaiHRSpaceBefRe = regexp.MustCompile(`\s+([,.;:)\]])`)
	twoaiHRSpaceAftRe = regexp.MustCompile(`([(\[])\s+`)
	twoaiHRAbsTitleRe = regexp.MustCompile(`(?i)<(?:jats:)?title\b[^>]*>\s*(?:abstract|summary)\s*</(?:jats:)?title>`)
	twoaiHRAbsHeadRe  = regexp.MustCompile(`(?i)^abstract(?:\s*[:.]\s*|\s+)`)
)

// twoaiHRStripJATS turns a Crossref JATS abstract into plain text. Block
// tags (paragraphs, section titles) become a space and inline tags (italic,
// superscript) vanish, so "m<sup>2</sup>" stays "m2". Entities are decoded
// after the tags are gone, so an escaped "&lt;0.001" survives as text. An
// "Abstract" or "Summary" heading is dropped, and so is a bare leading
// "Abstract".
func twoaiHRStripJATS(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = twoaiHRAbsTitleRe.ReplaceAllString(s, " ")
	s = twoaiHRBlockTagRe.ReplaceAllString(s, " ")
	s = twoaiHRTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	s = twoaiHRSpaceBefRe.ReplaceAllString(s, "$1")
	s = twoaiHRSpaceAftRe.ReplaceAllString(s, "$1")
	s = twoaiHRAbsHeadRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------
// Leads.
// ---------------------------------------------------------------------

// hrLead is one row of twoai_health_leads.
type hrLead struct {
	id           int
	topic, claim string
	status       string
	found        time.Time
	attempts     int
}

// hrCandidate is one paper a lead search returned, from either source.
type hrCandidate struct {
	doi, title, abstract, venue, typ, from, oaID, page string
	year, cites                                        int
	authors                                            []string
}

// hrConcept is a population, outcome or study design a claim can name: the
// words that search for it and the pattern that finds it in a paper.
type hrConcept struct {
	name, search string
	re           *regexp.Regexp
}

func hrc(name, search, pattern string) hrConcept {
	return hrConcept{name: name, search: search, re: regexp.MustCompile(`(?i)` + pattern)}
}

// twoaiHRConcepts are the populations and outcomes. A candidate must name at
// least one the claim names.
var twoaiHRConcepts = []hrConcept{
	hrc("type 2 diabetes", `"type 2 diabetes"`, `type 2 diabet\w*|\bt2d\b|\bt2dm\b`),
	hrc("type 1 diabetes", `"type 1 diabetes"`, `type 1 diabet\w*|\bt1d\b|\bt1dm\b`),
	hrc("obesity", `obesity`, `\bobes\w*|overweight`),
	hrc("prediabetes", `prediabetes`, `\bpre-?diabet\w*|normoglyc\w*|impaired (glucose|fasting)`),
	hrc("weight", `"weight loss"`, `weight[- ](loss|reduction|regain)|body weight|\bregain\w*`),
	hrc("a1c", `hba1c`, `\bhba1c\b|\ba1c\b|glycated ha?emoglobin|glyc(a)?emic control`),
	hrc("cardiovascular events", `cardiovascular`, `\bmace\b|major adverse cardiovascular|cardiovascular (events?|outcomes?|death)`),
	hrc("heart failure", `"heart failure"`, `heart failure|\bhhf\b`),
	hrc("liver fibrosis", `fibrosis`, `\bfib-?4\b|liver fibrosis|hepatic fibrosis|\bmasld\b|\bnafld\b|\bmash\b|steatotic`),
	hrc("testosterone", `testosterone`, `testosterone`),
	hrc("hypogonadism", `hypogonadism`, `hypogonad\w*`),
	hrc("frailty", `frailty`, `\bfrail\w*`),
	hrc("sarcopenia", `sarcopenia`, `sarcopen\w*`),
	hrc("lean mass", `"lean mass"`, `lean (body )?mass|muscle mass|fat-free mass|lean tissue|\bmuscle\b`),
	hrc("resistance training", `"resistance training"`, `resistance (training|exercise)|strength training|muscle-strengthening|weight training`),
	hrc("remote care", `pharmacist`, `pharmacists?\b|navigators?\b|telehealth|telemedicine|remote (care|model|programme|program|delivery)`),
	hrc("adverse events", `"adverse events"`, `adverse events?`),
	hrc("cholesterol", `cholesterol`, `cholesterol|\bldl\b|lipid profile`),
	hrc("Lp(a)", `"lipoprotein(a)"`, `lipoprotein\s*\(\s*a\s*\)|\blp\s*\(\s*a\s*\)`),
	hrc("men", `men`, `\bmen\b|\bmales?\b`),
}

// twoaiHRDesigns are study designs. They add to a score and can tip a match,
// never make one alone.
var twoaiHRDesigns = []hrConcept{
	hrc("phase 3", "", `phase (3|iii)\b`),
	hrc("phase 2", "", `phase (2|ii)\b`),
	hrc("post hoc", "", `post[- ]hoc|secondary analysis|exploratory analysis`),
	hrc("meta-analysis", "", `meta-analys\w*|systematic review`),
	hrc("cohort", "", `\bcohort\b|\bprospective\b`),
	hrc("randomised", "", `randomi[sz]ed|placebo`),
	hrc("retrospective", "", `retrospective`),
}

var (
	// A drug's generic name by its stem: semaglutide, dapagliflozin,
	// bimagrumab, olpasiran, pelacarsen, muvalaplin.
	twoaiHRDrugRe  = regexp.MustCompile(`(?i)\b[a-z]{3,}(?:tide|flozin|gliptin|mab|siran|rsen|aplin)\b`)
	twoaiHRNotDrug = map[string]bool{"peptide": true, "polypeptide": true, "dipeptide": true, "tripeptide": true, "nucleotide": true, "oligonucleotide": true, "riptide": true}
	// A trial name: SURMOUNT-1, TRIUMPH, DECLARE. Codes with digits inside
	// (PD26-01, an abstract number) do not count.
	twoaiHRTrialRe  = regexp.MustCompile(`\b[A-Z]{4,}(?:-\d+)?\b`)
	twoaiHRNotTrial = map[string]bool{"EASD": true, "ENDO": true, "JAMA": true, "NEJM": true, "MACE": true, "AACE": true, "ESPEN": true, "NICE": true, "USPSTF": true, "NOTE": true, "ACCP": true, "KDIGO": true}
	// A drug class, used only when the claim names no drug.
	twoaiHRClassRe = regexp.MustCompile(`(?i)\bglp-?1\b|\bsglt-?2\b|\bdpp-?4\b|\bincretins?\b`)
	// A claim about a conference abstract, where an abstract is the primary.
	twoaiHRConfClaimRe   = regexp.MustCompile(`(?i)\babstract\b|scientific sessions|\bcongress\b|annual meeting|\b(?:easd|endo|aua|ada|acc|aha|esc|eco|obesityweek)\s*20\d\d\b`)
	twoaiHRReviewClaimRe = regexp.MustCompile(`(?i)meta-analys\w*|systematic review|\breview\b|pooled analysis`)
	// A count with its thousands separated by a comma, a thin space or a
	// plain space: OpenAlex rebuilds abstracts word by word, so a Lancet
	// "1 152" arrives with a plain space.
	twoaiHRNumRe = regexp.MustCompile(`\d{1,3}(?:[, \x{2009}\x{202f}\x{00a0}]\d{3})+|\d{3,}`)

	// Candidates that are not papers, or are the secondary sources leads
	// come from. MDedge and the news sites are never stored as the paper.
	twoaiHRSecondaryRe = regexp.MustCompile(`(?i)mdedge|medscape|healio|medpage|doximity|healthline|nbc news|pharmaceutical journal|clinical advisor|physician'?s weekly|frontline medical`)
	twoaiHRNotPaperRe  = regexp.MustCompile(`(?i)^(?:data from|review for|decision letter|author response|correction|erratum|corrigendum|retraction|reply|response to)\b`)
	twoaiHRPeerRevRe   = regexp.MustCompile(`(?i)/v\d+/(?:review|decision|reply)\d*$`)
	twoaiHRPreprintRe  = regexp.MustCompile(`(?i)research square|medrxiv|biorxiv|\bssrn\b|preprints|arxiv|zenodo|figshare|authorea|qeios`)
	twoaiHRConfVenueRe = regexp.MustCompile(`(?i)\babstracts?\b|supplement|proceedings|congress|meeting|scientific sessions`)
	// Meeting abstracts by DOI: supplements, the Journal of Urology's AUA
	// abstracts, the ADA's Diabetes abstracts (db25-1768-p) and Oxford's
	// supplement abstracts (qdaf320.054, bvaf149.1941, ehae666.3317).
	twoaiHRConfDOIRe = regexp.MustCompile(`(?i)suppl|/01\.ju\.0|^10\.2337/d[bc]\d{2}-\d+-(?:p|or|lb)$|/[a-z]{4}\d{2,4}\.\d{2,5}$`)
	// Meeting abstracts by title: "1768-P:", "(054)", "MON-708", "IP10-11".
	twoaiHRConfTitleRe = regexp.MustCompile(`^(?:\(\d{1,5}\)|\d{1,5}-(?:P|OR|LB)\b|[A-Z]{2,4}\d{0,3}-\d{1,4}\b)`)
	// A supplement page, S61: Endocrine Practice prints the AACE meeting
	// abstracts there under ordinary article DOIs.
	twoaiHRSupplPageRe = regexp.MustCompile(`^[Ss]\d`)
	// An abstract number in a claim, PD26-01.
	twoaiHRCodeRe = regexp.MustCompile(`\b[A-Z]{1,4}\d{1,3}-\d{1,4}\b`)
	// Words that mark an organisation listed as an author: the ADA's
	// "Professional Practice Committee for Obesity" must not match a claim
	// that names the journal Obesity Science and Practice.
	twoaiHROrgAuthorRe = regexp.MustCompile(`(?i)committee|group|association|society|consortium|investigators|collaborat\w*|\bstudy\b|\btrial\b|health|council|network|foundation|institute|university|\bof\b`)
	twoaiHRReviewRe    = regexp.MustCompile(`(?i)\breview\b|meta-analys\w*|\boverview\b|narrative|perspective|\bupdate\b|game changer|current evidence|state of the art`)
	twoaiHRTestoRe     = regexp.MustCompile(`(?i)testosterone|hypogonad\w*|androgen\w*`)
)

// hrClaim is what a lead's claim names, read once.
type hrClaim struct {
	raw        string
	anchors    []string // drug and trial names, lowercased
	anchorRes  []*regexp.Regexp
	class      string // a drug class, only when no drug or trial is named
	classRe    *regexp.Regexp
	concepts   []hrConcept
	designs    []hrConcept
	numbers    map[string]bool
	codes      []string // meeting abstract numbers, PD26-01
	conference bool
	review     bool
}

// twoaiHRNumbers returns the counts in a text, digits only: 1,152 and 1152
// both give 1152. Numbers under 100 are too common to identify a study, and
// a bare four digit number from 1900 to 2099 is a year.
func twoaiHRNumbers(s string) map[string]bool {
	out := map[string]bool{}
	for _, loc := range twoaiHRNumRe.FindAllStringIndex(s, -1) {
		if loc[0] > 0 && (s[loc[0]-1] == '.' || (s[loc[0]-1] >= '0' && s[loc[0]-1] <= '9')) {
			continue // the decimals of 16.925 or the tail of a longer token
		}
		raw := s[loc[0]:loc[1]]
		d := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, raw)
		n, err := strconv.Atoi(d)
		if err != nil || n < 100 {
			continue
		}
		if raw == d && len(d) == 4 && n >= 1900 && n <= 2099 {
			continue
		}
		out[d] = true
	}
	return out
}

// twoaiHRTermRe matches a drug or trial name with the hyphen optional, so
// SURMOUNT-1 also finds "SURMOUNT 1" and "SURMOUNT1".
func twoaiHRTermRe(term string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + strings.ReplaceAll(regexp.QuoteMeta(term), "-", "[- ]?") + `\b`)
}

// twoaiHRParseClaim reads the drug and trial names, populations, outcomes,
// designs and counts out of a claim.
func twoaiHRParseClaim(claim string) hrClaim {
	c := hrClaim{raw: claim, numbers: twoaiHRNumbers(claim)}
	seen := map[string]bool{}
	add := func(a string) {
		a = strings.ToLower(a)
		if seen[a] {
			return
		}
		seen[a] = true
		c.anchors = append(c.anchors, a)
		c.anchorRes = append(c.anchorRes, twoaiHRTermRe(a))
	}
	for _, m := range twoaiHRDrugRe.FindAllString(claim, -1) {
		if !twoaiHRNotDrug[strings.ToLower(m)] {
			add(m)
		}
	}
	for _, m := range twoaiHRTrialRe.FindAllString(claim, -1) {
		if !twoaiHRNotTrial[strings.SplitN(m, "-", 2)[0]] {
			add(m)
		}
	}
	if len(c.anchors) == 0 {
		if m := twoaiHRClassRe.FindString(claim); m != "" {
			c.class = strings.ToLower(m)
			c.classRe = twoaiHRTermRe(c.class)
		}
	}
	for _, k := range twoaiHRConcepts {
		if k.re.MatchString(claim) {
			c.concepts = append(c.concepts, k)
		}
	}
	for _, k := range twoaiHRDesigns {
		if k.re.MatchString(claim) {
			c.designs = append(c.designs, k)
		}
	}
	c.codes = twoaiHRCodeRe.FindAllString(claim, -1)
	c.conference = twoaiHRConfClaimRe.MatchString(claim)
	c.review = twoaiHRReviewClaimRe.MatchString(claim)
	return c
}

// twoaiHRLeadSearch builds the OpenAlex title and abstract search for a
// claim: its drug or trial names, any of them, and any of its populations
// and outcomes. A claim naming no drug or trial searches its populations
// and outcomes together. Empty when the claim gives nothing to search.
func twoaiHRLeadSearch(c hrClaim) string {
	quote := func(s string) string {
		if strings.ContainsAny(s, " -") {
			return `"` + s + `"`
		}
		return s
	}
	var pop []string
	for _, k := range c.concepts {
		if k.name == "men" || k.search == "" {
			continue
		}
		pop = append(pop, k.search)
		if len(pop) == 4 {
			break
		}
	}
	var parts []string
	switch {
	case len(c.anchors) > 0:
		as := make([]string, len(c.anchors))
		for i, a := range c.anchors {
			as[i] = quote(a)
		}
		parts = append(parts, "("+strings.Join(as, " OR ")+")")
		if len(pop) > 0 {
			parts = append(parts, "("+strings.Join(pop, " OR ")+")")
		}
	case c.class != "":
		parts = append(parts, quote(c.class))
		if len(pop) > 0 {
			parts = append(parts, "("+strings.Join(pop, " OR ")+")")
		}
	default:
		if len(pop) > 3 {
			pop = pop[:3]
		}
		parts = pop
	}
	return strings.Join(parts, " AND ")
}

// twoaiHRBiblio turns a claim into Crossref's free text bibliographic query.
func twoaiHRBiblio(claim string) string {
	s := strings.Map(func(r rune) rune {
		if r == ',' {
			return -1 // 1,152 stays one number
		}
		if r == '-' || r == '(' || r == ')' || r == '.' || r == '%' || r == ';' || r == ':' {
			return ' '
		}
		return r
	}, claim)
	return truncate(strings.Join(strings.Fields(s), " "), 300)
}

// twoaiHRFold straightens hyphens and drops the accents common in author
// names, keeping case, so an accented name written with a Unicode hyphen
// matches its plain spelling in a claim.
var twoaiHRFold = strings.NewReplacer(string(rune(0x2010)), "-", string(rune(0x2011)), "-", string(rune(0x2013)), "-",
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i", "ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ñ", "n", "ç", "c",
	"Á", "A", "É", "E", "Í", "I", "Ó", "O", "Ú", "U", "Ñ", "N")

// twoaiHRAuthorNamed returns the candidate author whose surname the claim
// names, capitalised as a name is. The claim's first word is not a name.
func twoaiHRAuthorNamed(claim string, authors []string) string {
	claim = twoaiHRFold.Replace(claim)
	if f := strings.Fields(claim); len(f) > 0 {
		claim = strings.TrimPrefix(claim, f[0])
	}
	for _, a := range authors {
		f := strings.Fields(twoaiHRFold.Replace(a))
		if len(f) == 0 || len(f) > 5 || twoaiHROrgAuthorRe.MatchString(a) {
			continue // an organisation, not a person
		}
		fam := f[len(f)-1]
		if len([]rune(fam)) < 4 || strings.ToLower(fam) == fam {
			continue
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(fam) + `\b`).MatchString(claim) {
			return a
		}
	}
	return ""
}

// twoaiHRVenueNamed reports whether the claim names the candidate's journal.
// A one word journal title counts only for the Lancet, the BMJ and JAMA,
// since "Diabetes" or "Obesity" as a title would match any claim.
func twoaiHRVenueNamed(claim, venue string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.ReplaceAll(s, "&", " and "))
		s = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return ' '
		}, s)
		return strings.Join(strings.Fields(s), " ")
	}
	v := strings.TrimPrefix(norm(venue), "the ")
	if v == "" {
		return false
	}
	if len(strings.Fields(v)) < 2 && v != "lancet" && v != "bmj" && v != "jama" {
		return false
	}
	return strings.Contains(" "+norm(claim)+" ", " "+v+" ")
}

// twoaiHRKind sorts a candidate into journal, conference or preprint, or
// skip for anything that is not a paper or is a secondary source.
func twoaiHRKind(c hrCandidate) string {
	if c.doi == "" || c.title == "" || twoaiHRNotPaperRe.MatchString(c.title) || twoaiHRPeerRevRe.MatchString(c.doi) ||
		twoaiHRSecondaryRe.MatchString(c.venue) {
		return "skip"
	}
	switch strings.ToLower(c.typ) {
	case "article", "review", "journal-article", "":
	case "preprint", "posted-content":
		return "preprint"
	case "proceedings-article":
		return "conference"
	default:
		return "skip"
	}
	if twoaiHRPreprintRe.MatchString(c.venue) {
		return "preprint"
	}
	for _, p := range []string{"10.21203/", "10.1101/", "10.2139/ssrn", "10.5281/", "10.6084/", "10.22541/", "10.32388/"} {
		if strings.HasPrefix(c.doi, p) {
			return "preprint"
		}
	}
	if twoaiHRConfVenueRe.MatchString(c.venue) || twoaiHRConfDOIRe.MatchString(c.doi) || twoaiHRConfTitleRe.MatchString(c.title) ||
		twoaiHRSupplPageRe.MatchString(c.page) || twoaiHRShouting(c.title) {
		return "conference"
	}
	return "journal"
}

// twoaiHRShouting reports a title set in capitals, the house style of the
// AUA and ACC meeting abstracts that the Journal of Urology and JACC print
// under ordinary DOIs. Twenty letters at least, nine in ten of them capitals.
func twoaiHRShouting(title string) bool {
	letters, upper := 0, 0
	for _, r := range title {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	return letters >= 20 && upper*10 >= letters*9
}

// hrMatch is one candidate scored against a claim.
type hrMatch struct {
	cand            hrCandidate
	kind            string
	score           int
	ok              bool
	strong, journal bool
	strongN         int // how many strong signs: sample size, author, abstract number
	why             []string
}

// twoaiHRScoreLead scores a candidate against a claim and decides whether it
// clearly matches. Points: each drug or trial name 3, each population or
// outcome 2, each design 1, the claim's sample size 4, the claim's journal
// 3, an author or abstract number the claim names 4, a journal paper 1, a
// conference abstract 1 when the claim is one and minus 2 when not, a
// preprint minus 3, and a review when the claim is not about one minus 3.
// The sample size, an author and an abstract number are the strong signs.
//
// A clear match needs a population or outcome from the claim in every case.
// With drug or trial names, it needs at least two thirds of them (all of
// one or two, two of three) and one more sign: a second population or
// outcome when the claim names two, a strong sign or the journal. Two strong
// signs together match without the names, since a meeting abstract's title
// often leaves the drugs out. Without names, it needs
// two populations or outcomes and a strong sign or the journal, and the
// drug class when the claim names one. Without a strong sign, a review never
// matches a claim that is not about one, a paper that is not a meta-analysis
// never matches a claim that is, a paper whose abstract lacks the claim's
// sample size matches only when the claim names its journal, and a meeting
// abstract never matches at all; with one, a meeting abstract still matches
// only a claim about a meeting abstract.
func twoaiHRScoreLead(cl hrClaim, c hrCandidate) hrMatch {
	m := hrMatch{cand: c, kind: twoaiHRKind(c)}
	if m.kind == "skip" {
		m.why = append(m.why, "not a paper, or a secondary source")
		return m
	}
	text := c.title + " " + c.abstract
	hits := 0
	for i, re := range cl.anchorRes {
		if re.MatchString(text) {
			hits++
			m.why = append(m.why, "names "+cl.anchors[i])
		}
	}
	m.score += 3 * hits
	classHit := cl.classRe != nil && cl.classRe.MatchString(text)
	if classHit {
		m.score++
		m.why = append(m.why, "names "+cl.class)
	}
	pop := 0
	for _, k := range cl.concepts {
		if k.re.MatchString(text) {
			pop++
			m.why = append(m.why, "names "+k.name)
		}
	}
	m.score += 2 * pop
	design := 0
	for _, k := range cl.designs {
		if k.re.MatchString(text) {
			design++
			m.why = append(m.why, "design "+k.name)
		}
	}
	m.score += design
	// The claim gives a sample size, the candidate has an abstract, and the
	// abstract does not carry it: evidence against.
	sizeMissing := false
	if len(cl.numbers) > 0 {
		for n := range twoaiHRNumbers(text) {
			if cl.numbers[n] {
				m.strong = true
				m.strongN++
				m.score += 4
				m.why = append(m.why, "sample size "+n)
				break
			}
		}
		sizeMissing = !m.strong && c.abstract != ""
	}
	if twoaiHRVenueNamed(cl.raw, c.venue) {
		m.journal = true
		m.score += 3
		m.why = append(m.why, "journal "+c.venue+" named in the claim")
	}
	if a := twoaiHRAuthorNamed(cl.raw, c.authors); a != "" {
		m.strong = true
		m.strongN++
		m.score += 4
		m.why = append(m.why, "author "+a+" named in the claim")
	}
	for _, code := range cl.codes {
		if strings.Contains(strings.ToUpper(c.title), code) {
			m.strong = true
			m.strongN++
			m.score += 4
			m.why = append(m.why, "abstract number "+code)
			break
		}
	}
	metaWanted, metaHit := false, false
	for _, k := range cl.designs {
		if k.name == "meta-analysis" {
			metaWanted, metaHit = true, k.re.MatchString(text)
		}
	}
	switch m.kind {
	case "journal":
		m.score++
	case "conference":
		if cl.conference {
			m.score++
		} else {
			m.score -= 2
		}
	case "preprint":
		m.score -= 3
	}
	reviewLike := !cl.review && twoaiHRReviewRe.MatchString(c.title)
	if reviewLike {
		m.score -= 3
		m.why = append(m.why, "a review, the claim is not")
	}
	need := len(cl.anchors) - len(cl.anchors)/3
	switch {
	case pop == 0:
	case reviewLike && !m.strong:
	case metaWanted && !metaHit && !m.strong:
		m.why = append(m.why, "not a meta-analysis, the claim is")
	case sizeMissing && !m.strong && !m.journal:
		m.why = append(m.why, "the abstract lacks the claim's sample size")
	case m.kind == "conference" && !(cl.conference && m.strong):
		// A meeting abstract is the primary only for a claim about one, and
		// then only with its number, author or sample size: meetings repeat
		// the same subjects every year.
		m.why = append(m.why, "a meeting abstract without its number, author or sample size")
	case m.strongN >= 2:
		// Two strong signs together, the author and the abstract number say,
		// identify the paper even when its title leaves the drugs out.
		m.ok = true
	case len(cl.anchors) > 0:
		needPop := 2
		if len(cl.concepts) < 2 {
			needPop = len(cl.concepts)
		}
		m.ok = hits >= need && (pop >= needPop || m.strong || m.journal)
	default:
		m.ok = pop >= 2 && (m.strong || m.journal) && (cl.class == "" || classHit)
	}
	return m
}

// twoaiHRPickLead scores every candidate, merging the two sources' copies
// of one DOI, and returns the clear match, if there is one. Two clear
// matches within a point of each other are ambiguous unless the better one
// has the sample size, an author or the journal behind it.
func twoaiHRPickLead(cl hrClaim, cands []hrCandidate) (*hrMatch, []hrMatch, string) {
	byDOI := map[string]int{}
	var uniq []hrCandidate
	for _, c := range cands {
		if c.doi == "" {
			continue
		}
		if i, ok := byDOI[c.doi]; ok {
			u := &uniq[i]
			if u.abstract == "" && c.abstract != "" {
				u.abstract, u.from = c.abstract, c.from
			}
			if u.venue == "" {
				u.venue = c.venue
			}
			if len(u.authors) == 0 {
				u.authors = c.authors
			}
			if u.page == "" {
				u.page = c.page
			}
			continue
		}
		byDOI[c.doi] = len(uniq)
		uniq = append(uniq, c)
	}
	ranked := make([]hrMatch, 0, len(uniq))
	for _, c := range uniq {
		ranked = append(ranked, twoaiHRScoreLead(cl, c))
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.ok != b.ok {
			return a.ok
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.cand.cites != b.cand.cites {
			return a.cand.cites > b.cand.cites
		}
		return a.cand.doi < b.cand.doi
	})
	if len(ranked) == 0 || !ranked[0].ok {
		return nil, ranked, "no clear match"
	}
	top := ranked[0]
	if len(ranked) > 1 && ranked[1].ok && ranked[1].score >= top.score-1 && !top.strong && !top.journal {
		return nil, ranked, fmt.Sprintf("ambiguous: %s and %s score %d and %d", top.cand.doi, ranked[1].cand.doi, top.score, ranked[1].score)
	}
	return &top, ranked, ""
}

// twoaiHRClassifyLead places a lead's paper with the existing rules: the
// health watch's sub-hub rules for diabetes, the Lp(a) classifier for Lp(a).
// One rule comes first for diabetes: the content project files testosterone
// and hypogonadism papers under testosterone, a subtopic the health watch
// does not know, and its leads on that subject would otherwise land on glp1.
func twoaiHRClassifyLead(topic, text string) (string, string) {
	switch topic {
	case "lpa":
		return twoaiHRClassifyLpa(text)
	case "dmed":
		if m := twoaiHRTestoRe.FindString(text); m != "" {
			return "testosterone", m
		}
		hub, m := twoaiHealthSubHub("dmed", text)
		if sub := strings.TrimPrefix(hub, "dmed-"); hub != "dmed" && sub != "" {
			return sub, m
		}
	}
	return "overview", ""
}

// hrLeadResult is what one lead search found.
type hrLeadResult struct {
	best   *hrMatch
	ranked []hrMatch
	note   string
	search string
}

// leadOpenAlex searches OpenAlex for a claim's paper, most relevant first.
func (r *hrRun) leadOpenAlex(cl hrClaim, from time.Time, budget int) ([]hrCandidate, string, error) {
	s := twoaiHRLeadSearch(cl)
	if s == "" {
		return nil, "", nil
	}
	filter := fmt.Sprintf("title_and_abstract.search:%s,from_publication_date:%s,type:article|review|preprint", s, from.Format("2006-01-02"))
	u := fmt.Sprintf("https://api.openalex.org/works?filter=%s&sort=relevance_score:desc&per-page=25&select=%s",
		url.QueryEscape(filter), url.QueryEscape(twoaiHRSelect+",biblio"))
	body, code, err := r.oaGet(u, budget)
	if err != nil {
		return nil, s, err
	}
	if code != 200 {
		return nil, s, fmt.Errorf("openalex http %d", code)
	}
	var out struct {
		Results []twoaiHRWork `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, s, fmt.Errorf("parse: %w", err)
	}
	var cands []hrCandidate
	for _, w := range out.Results {
		title := strings.TrimSpace(w.Title)
		if title == "" {
			title = strings.TrimSpace(w.Display)
		}
		c := hrCandidate{doi: twoaiHRDOI(w.DOI), title: title, abstract: twoaiOAAbstract(w.AbstractII), venue: w.venue(),
			typ: w.Type, from: "openalex", oaID: strings.TrimPrefix(w.ID, "https://openalex.org/"), year: w.PubYear, cites: w.CitedBy}
		if w.Biblio != nil {
			c.page = w.Biblio.FirstPage
		}
		for _, a := range w.Authorships {
			if n := strings.TrimSpace(a.Author.Name); n != "" {
				c.authors = append(c.authors, n)
			}
		}
		cands = append(cands, c)
	}
	time.Sleep(350 * time.Millisecond)
	return cands, s, nil
}

// twoaiHRLeadCrossref asks Crossref's bibliographic search for a claim's
// paper.
func twoaiHRLeadCrossref(client *http.Client, cl hrClaim, from time.Time) ([]hrCandidate, error) {
	u := "https://api.crossref.org/works?query.bibliographic=" + url.QueryEscape(twoaiHRBiblio(cl.raw)) +
		"&filter=" + url.QueryEscape("from-pub-date:"+from.Format("2006-01-02")) +
		"&rows=20&select=" + url.QueryEscape(twoaiHRCrossrefSelect)
	body, err := twoaiHRCrossrefGet(client, u)
	if err != nil || body == nil {
		return nil, err
	}
	var out struct {
		Message struct {
			Items []twoaiHRCRWork `json:"items"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	cands := make([]hrCandidate, 0, len(out.Message.Items))
	for _, w := range out.Message.Items {
		cands = append(cands, w.candidate())
	}
	return cands, nil
}

// findPrimary searches both sources for a lead's paper. It fails only when
// neither source answered, which does not count as a try.
func (r *hrRun) findPrimary(l hrLead, budget int) (hrLeadResult, error) {
	var res hrLeadResult
	cl := twoaiHRParseClaim(l.claim)
	base := l.found
	if base.IsZero() {
		base = time.Now().UTC()
	}
	from := base.AddDate(-twoaiHRLeadLookback, 0, 0)
	var cands []hrCandidate
	answered := 0
	var errs []string
	oa, search, err := r.leadOpenAlex(cl, from, budget)
	res.search = search
	switch {
	case err == nil:
		answered++
		cands = append(cands, oa...)
	case !errors.Is(err, errHRStop):
		errs = append(errs, "openalex: "+err.Error())
	}
	cr, err := twoaiHRLeadCrossref(r.client, cl, from)
	if err == nil {
		answered++
		cands = append(cands, cr...)
	} else {
		errs = append(errs, "crossref: "+err.Error())
	}
	time.Sleep(200 * time.Millisecond)
	if answered == 0 {
		return res, fmt.Errorf("no source answered: %s", strings.Join(errs, "; "))
	}
	res.best, res.ranked, res.note = twoaiHRPickLead(cl, cands)
	return res, nil
}

// twoaiHRLeadNote is the evidence written to resolved_note.
func twoaiHRLeadNote(m hrMatch) string {
	return truncate(fmt.Sprintf("matched by twoai_health_research via %s, score %d, %s %s (%d): %s",
		m.cand.from, m.score, m.kind, firstNonEmpty(m.cand.venue, "no venue"), m.cand.year, strings.Join(m.why, "; ")), 1000)
}

// leads tries each open lead once a day, ten a run.
func (r *hrRun) leads() {
	rows, err := r.db.Query(`SELECT id, topic, claim, status, COALESCE(found_date, added_on), attempts
		FROM twoai_health_leads
		WHERE primary_doi IS NULL
		  AND (status = 'find primary'
		       OR (status = 'not found yet' AND (attempted_at IS NULL OR attempted_at < now() - make_interval(secs => $1))))
		  AND (attempted_at IS NULL OR attempted_at < now() - make_interval(secs => $2))
		ORDER BY attempted_at NULLS FIRST, id
		LIMIT $3`, twoaiHRLeadRetry.Seconds(), twoaiHRLeadEvery.Seconds(), twoaiHRLeadsPerRun)
	if err != nil {
		r.notice("leads queue: %v", err)
		return
	}
	var todo []hrLead
	for rows.Next() {
		var l hrLead
		if rows.Scan(&l.id, &l.topic, &l.claim, &l.status, &l.found, &l.attempts) == nil {
			todo = append(todo, l)
		}
	}
	rows.Close()
	budget := r.calls + twoaiHRLeadCalls
	if budget > twoaiHRCallCap {
		budget = twoaiHRCallCap
	}
	for _, l := range todo {
		if r.late() {
			r.notice("deadline reached, leads left for the next run")
			return
		}
		res, err := r.findPrimary(l, budget)
		if err != nil {
			r.notice("lead %d: %v", l.id, err)
			continue
		}
		r.leadsTried++
		if res.best == nil {
			// The status changes only from 'find primary' to 'not found yet'.
			// No code but this stage reads the leads table (checked on
			// 2026-10-05), and the lead stays in the weekly queue, so the
			// change only slows the asking.
			r.db.Exec(`UPDATE twoai_health_leads SET attempts = attempts + 1, attempted_at = now(),
				status = CASE WHEN status = 'find primary' AND attempts + 1 >= $1 THEN 'not found yet' ELSE status END
				WHERE id = $2 AND primary_doi IS NULL`, twoaiHRLeadGiveUp, l.id)
			continue
		}
		if err := r.resolveLead(l, *res.best); err != nil {
			r.notice("lead %d: %v", l.id, err)
		}
	}
}

// resolveLead stores a lead's paper and links the lead to it. A paper
// already in the library keeps its row, gaining only an abstract it lacked.
func (r *hrRun) resolveLead(l hrLead, m hrMatch) error {
	c := m.cand
	sub, why := twoaiHRClassifyLead(l.topic, c.title+" "+c.abstract)
	match := fmt.Sprintf("lead=%d subtopic=%s", l.id, sub)
	if why != "" {
		match += " matched=" + why
	}
	src := ""
	if c.abstract != "" {
		src = c.from
	}
	var year any
	if c.year > 0 {
		year = c.year
	}
	names := c.authors
	if len(names) > 25 {
		names = names[:25]
	}
	if names == nil {
		names = []string{}
	}
	authors, _ := json.Marshal(names)
	var inserted bool
	err := r.db.QueryRow(`INSERT INTO twoai_health_papers
		(doi, title, year, journal, citations, topic, subtopic, found_via, abstract, abstract_source,
		 status, added_on, authors, openalex_id, harvested_at, discovered_at, harvest_match, abstract_checked_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,'lead',NULLIF($8,''),NULLIF($9,''),
		 'pending',current_date,$10::jsonb,NULLIF($11,''),now(),now(),$12,CASE WHEN $8 = '' THEN NULL ELSE now() END)
		ON CONFLICT (doi) DO UPDATE SET
			abstract_source = CASE WHEN COALESCE(twoai_health_papers.abstract,'') = '' AND EXCLUDED.abstract IS NOT NULL
				THEN EXCLUDED.abstract_source ELSE twoai_health_papers.abstract_source END,
			abstract = CASE WHEN COALESCE(twoai_health_papers.abstract,'') = '' THEN EXCLUDED.abstract
				ELSE twoai_health_papers.abstract END
		RETURNING (xmax = 0)`,
		c.doi, c.title, year, c.venue, c.cites, l.topic, sub, c.abstract, src,
		string(authors), c.oaID, match).Scan(&inserted)
	if err != nil {
		return fmt.Errorf("insert paper %s: %w", c.doi, err)
	}
	note := twoaiHRLeadNote(m)
	if !inserted {
		note += ". The paper was already in twoai_health_papers, its row was kept and gained at most an abstract it lacked."
	}
	res, err := r.db.Exec(`UPDATE twoai_health_leads SET primary_doi = $1, status = 'found', resolved_note = $2,
		resolved_at = now(), attempts = attempts + 1, attempted_at = now()
		WHERE id = $3 AND primary_doi IS NULL`, c.doi, note, l.id)
	if err != nil {
		return fmt.Errorf("update lead: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		r.leadsFound++
	}
	return nil
}

// hrNewPaper is one row for the bridge message.
type hrNewPaper struct {
	title, doi, topic, sub string
	year, cites            int
}

// hrLeadLine is one lead for the bridge message: resolved ones carry the
// DOI, open ones the number of tries.
type hrLeadLine struct {
	claim, doi, status string
	attempts           int
}

// bridge sends one row a calendar day to theworldofai, and only when this
// stage added papers, saved full texts or resolved leads since the last row,
// or when leads are still open.
func (r *hrRun) bridge(leadsReady bool) {
	var sentToday bool
	r.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_health_research_bridge WHERE sent_on = current_date)`).Scan(&sentToday)
	if sentToday {
		return
	}
	var last sql.NullTime
	r.db.QueryRow(`SELECT max(sent_at) FROM twoai_health_research_bridge`).Scan(&last)
	since := time.Time{}
	sinceText := "this stage's first run"
	if last.Valid {
		since = last.Time
		sinceText = "the last bridge row, " + last.Time.UTC().Format("2006-01-02 15:04 UTC")
	}

	rows, err := r.db.Query(`SELECT title, doi, topic, COALESCE(subtopic,''), COALESCE(year,0), COALESCE(citations,0)
		FROM twoai_health_papers WHERE found_via = 'openalex' AND discovered_at > $1
		ORDER BY citations DESC NULLS LAST, doi`, since)
	if err != nil {
		r.notice("bridge: %v", err)
		return
	}
	var papers []hrNewPaper
	for rows.Next() {
		var p hrNewPaper
		if rows.Scan(&p.title, &p.doi, &p.topic, &p.sub, &p.year, &p.cites) == nil {
			papers = append(papers, p)
		}
	}
	rows.Close()
	var saved int
	r.db.QueryRow(`SELECT count(*) FROM twoai_health_papers WHERE full_text_r2 IS NOT NULL AND ft_checked_at > $1`, since).Scan(&saved)
	var resolved, open []hrLeadLine
	if leadsReady {
		resolved = r.leadLines(`SELECT claim, COALESCE(primary_doi,''), status, attempts FROM twoai_health_leads
			WHERE status = 'found' AND primary_doi IS NOT NULL AND resolved_at > $1 ORDER BY id`, since)
		open = r.leadLines(`SELECT claim, '', status, attempts FROM twoai_health_leads
			WHERE primary_doi IS NULL AND status IN ('find primary','not found yet') ORDER BY id`)
	}
	if len(papers) == 0 && saved == 0 && len(resolved) == 0 && len(open) == 0 {
		return
	}
	promising := 0
	for _, p := range papers {
		if twoaiHRCure(p.topic, p.sub) && twoaiHRHighCited(p.year, p.cites, r.nowYear) {
			promising++
		}
	}
	subject := fmt.Sprintf("Health research: %d new papers, %d promising, %d full texts saved", len(papers), promising, saved)
	if len(resolved) > 0 || len(open) > 0 {
		subject += fmt.Sprintf(", %d leads resolved, %d open", len(resolved), len(open))
	}
	body := twoaiHRBridgeBody(papers, promising, saved, sinceText, r.nowYear, resolved, open)
	if _, err := r.db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`, subject, body); err != nil {
		r.notice("bridge row: %v", err)
		return
	}
	r.db.Exec(`INSERT INTO twoai_health_research_bridge (new_papers, promising, full_texts, bridge_topic, leads_found, leads_open)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, len(papers), promising, saved, subject, len(resolved), len(open))
}

// leadLines reads lead rows for the bridge message.
func (r *hrRun) leadLines(query string, args ...any) []hrLeadLine {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		r.notice("bridge leads: %v", err)
		return nil
	}
	defer rows.Close()
	var out []hrLeadLine
	for rows.Next() {
		var l hrLeadLine
		if rows.Scan(&l.claim, &l.doi, &l.status, &l.attempts) == nil {
			out = append(out, l)
		}
	}
	return out
}

// twoaiHRBridgeBody writes the bridge message: counts by topic and
// subtopic, the rule behind "promising", the top new papers, the leads
// resolved since the last row and the leads still open.
func twoaiHRBridgeBody(papers []hrNewPaper, promising, saved int, since string, nowYear int, resolved, open []hrLeadLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "twoai_health_research added %d papers to twoai_health_papers since %s, and saved %d open access full texts.\n\n",
		len(papers), since, saved)
	if len(resolved) > 0 {
		fmt.Fprintf(&b, "Leads resolved to a primary paper (%d), claim then DOI. The paper is in twoai_health_papers with found_via lead, the match evidence is in the lead's resolved_note:\n", len(resolved))
		for _, l := range resolved {
			fmt.Fprintf(&b, "  %s -> %s\n", truncate(l.claim, 160), l.doi)
		}
		b.WriteString("\n")
	}
	if len(open) > 0 {
		fmt.Fprintf(&b, "Leads still without a primary paper (%d), with daily tries so far:\n", len(open))
		for _, l := range open {
			note := ""
			if l.status != "find primary" {
				note = ", " + l.status + ", now tried weekly"
			}
			fmt.Fprintf(&b, "  %s (%d tries%s)\n", truncate(l.claim, 160), l.attempts, note)
		}
		b.WriteString("\n")
	}
	counts := map[string]int{}
	for _, p := range papers {
		counts[p.topic+" / "+p.sub]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		b.WriteString("New papers by topic and subtopic:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %d\n", k, counts[k])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Promising (%d) means a new paper in a cure subtopic (dmed insulin-beta, type1, pipeline, lpa gene, rna) with at least 50 citations, or at least 10 when published in %d or %d. The promising column is yours and was not set.\n\n",
		promising, nowYear-1, nowYear)
	if len(papers) > 0 {
		n := len(papers)
		if n > 15 {
			n = 15
		}
		fmt.Fprintf(&b, "Top %d new papers by citations:\n", n)
		for _, p := range papers[:n] {
			fmt.Fprintf(&b, "  %s (%d), %d citations, %s / %s, doi %s\n", truncate(p.title, 140), p.year, p.cites, p.topic, p.sub, p.doi)
		}
		b.WriteString("\n")
	}
	b.WriteString("New rows carry found_via openalex, or lead for a paper traced from a lead, and status pending. " +
		"Abstracts, including those from Crossref and PubMed (abstract_source crossref or pubmed), and full texts are internal only and never rendered. " +
		"Full text PDFs are in the private srj-uploads bucket under corpus/health-papers/, named in full_text_r2. " +
		"harvest_match records the query and the words that set each subtopic. srj owns the stage, send code needs by bridge.")
	return b.String()
}
