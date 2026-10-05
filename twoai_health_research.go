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
// INTERNAL ONLY. Abstracts and full text are for research and never rendered:
// this stage writes no page and adds nothing to any page document.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	twoaiHRStage = "twoai_health_research"
	// OpenAlex calls a run may make, harvest and full text together. List
	// calls cost $0.10 per thousand on the metered API, so 60 a run at eight
	// runs a day is about five cents, well inside the free key's $1 a day
	// and small beside the works spine's 150 pages a run.
	twoaiHRCallCap = 60
	// Calls kept back from the harvest for the full text step.
	twoaiHRFTReserve = 10
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
		ADD COLUMN IF NOT EXISTS ft_error text`); err != nil {
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
	return nil
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

	r.harvest()
	r.fullText()
	r.bridge()

	for _, n := range r.notices {
		// Stdout, not stderr: PowerShell logs stderr as a NativeCommandError.
		fmt.Println("twoai_health_research: notice:", n)
	}
	fmt.Printf("twoai_health_research: queries=%d calls=%d new=%d refreshed=%d ft_checked=%d ft_saved=%d closed=%d off_topic=%d ok=true\n",
		r.queriesRun, r.calls, r.newRows, r.refreshed, r.ftChecked, r.ftSaved, r.closed, r.offTopic)
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
	budget := twoaiHRCallCap - twoaiHRFTReserve
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

// twoaiHRUnpaywall looks a DOI up in Unpaywall. A DOI Unpaywall does not
// know returns nil and no error.
func twoaiHRUnpaywall(client *http.Client, doi string) (*twoaiHRUnpaywallResp, error) {
	parts := strings.Split(doi, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	u := "https://api.unpaywall.org/v2/" + strings.Join(parts, "/") + "?email=" + url.QueryEscape(twoaiOAMailto)
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

// hrNewPaper is one row for the bridge message.
type hrNewPaper struct {
	title, doi, topic, sub string
	year, cites            int
}

// bridge sends one row a calendar day to theworldofai, and only when this
// stage added papers or saved full texts since the last row.
func (r *hrRun) bridge() {
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
	if len(papers) == 0 && saved == 0 {
		return
	}
	promising := 0
	for _, p := range papers {
		if twoaiHRCure(p.topic, p.sub) && twoaiHRHighCited(p.year, p.cites, r.nowYear) {
			promising++
		}
	}
	subject := fmt.Sprintf("Health research: %d new papers, %d promising, %d full texts saved", len(papers), promising, saved)
	body := twoaiHRBridgeBody(papers, promising, saved, sinceText, r.nowYear)
	if _, err := r.db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`, subject, body); err != nil {
		r.notice("bridge row: %v", err)
		return
	}
	r.db.Exec(`INSERT INTO twoai_health_research_bridge (new_papers, promising, full_texts, bridge_topic)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, len(papers), promising, saved, subject)
}

// twoaiHRBridgeBody writes the bridge message: counts by topic and
// subtopic, the rule behind "promising", and the top new papers.
func twoaiHRBridgeBody(papers []hrNewPaper, promising, saved int, since string, nowYear int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "twoai_health_research added %d papers to twoai_health_papers since %s, and saved %d open access full texts.\n\n",
		len(papers), since, saved)
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
	b.WriteString("New rows carry found_via openalex and status pending. Abstracts and full texts are internal only and never rendered. " +
		"Full text PDFs are in the private srj-uploads bucket under corpus/health-papers/, named in full_text_r2. " +
		"harvest_match records the query and the words that set each subtopic. srj owns the stage, send code needs by bridge.")
	return b.String()
}
