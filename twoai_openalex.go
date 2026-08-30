package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// THE WORKS SPINE, PHASE 1: EVERY AI PAPER OPENALEX KNOWS, AS METADATA.
//
// THE DECISION THIS ENCODES. The goal is to be the source of truth on AI, and
// the one word that had to change to make that reachable was "content":
// full text of books, journals and news is copyrighted, and hoarding it would
// poison the one corpus here with clean provenance. What is buildable and
// defensible is the INDEX - every work identified by hard identifier,
// cross-linked, carrying its own license class - with full text only where a
// license grants it. Decisions recorded 2026-08-25: metadata + abstract only
// (~10x cheaper than full text); AI-core scope, widened on demand; no page
// per work, ever - the corpus serves the API, the site assistant, and the
// export, and pages exist only where editorial value is added. A million thin
// pages would wreck the crawl budget that makes the 9,044 real ones rank.
//
// WHY OPENALEX IS THE BACKBONE. Its metadata is CC0, it already carries the
// identifiers everything else joins on (DOI, arXiv, PMID, ORCID, ROR), it
// tracks citations and open-access status, and it updates daily. Abstracts
// ride along as inverted indexes; they are publisher prose, so they land as
// cite_only, same rule as every other third-party text here. The metadata
// row itself is trainable.
//
// HOW IT HARVESTS WITHOUT EVER LOSING ITS PLACE. OpenAlex cursor paging with
// the cursor PERSISTED in twoai_harvest_cursors after every page, so a
// crashed or budget-capped run resumes exactly where it stopped. Two modes,
// stored with the cursor: "backfill" walks a whole subfield; when the cursor
// exhausts, that subfield flips itself to "delta" and thereafter asks only
// for works updated since its high-water date. The per-run page budget keeps
// a single run polite and bounded; the corpus is built by the daily rhythm,
// not by one heroic pull.
//
// The sandbox IP that developed this was rate-limited by OpenAlex even with
// the polite pool, so 429 handling is not theoretical: exponential backoff,
// and a run that gives up leaves the cursor where it was.

const (
	twoaiOAMailto      = "info@srjconsultingservices.com"
	twoaiOAPagesPerRun = 150 // x200 works = up to 30k rows a run, shared across subfields
	twoaiOASelect      = "id,doi,title,display_name,publication_year,publication_date,language,type,ids,primary_topic,open_access,best_oa_location,authorships,cited_by_count,referenced_works_count,abstract_inverted_index,updated_date"
)

// twoaiOARecentFrom is the boundary between the two backfill phases: works
// published on or after this date are harvested first, everything earlier is
// harvested second. It is a running order, not a cutoff.
var twoaiOARecentFrom = time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)

// AI RESEARCH DOES NOT LIVE IN ONE SUBFIELD, AND ASSUMING IT DID COST US.
//
// The spine originally harvested primary_topic.subfield 1702, Artificial
// Intelligence, on the reasoning that primary-topic-only keeps a medical
// paper that mentions a neural net out of the corpus. That part still holds.
// What did not hold is the assumption that modern AI work is CLASSIFIED as
// Artificial Intelligence. Checked on 2026-08-26 against two papers found
// through Consensus: a 2026 survey on agentic reasoning in LLMs sits under
// Computer Vision and Pattern Recognition, and a benchmark of LLM agents for
// clinical decisions sits under Health Informatics. Both are in OpenAlex.
// Neither was in our corpus, because neither is in subfield 1702.
//
// The symptom that should have prompted this earlier: 103 arXiv-DOI works in
// 180,000, and exactly ONE of them from 2026. A corpus claiming to be the
// source of truth on AI held almost no recent preprints, and the filter was
// why.
//
// EACH SUBFIELD KEEPS ITS OWN CURSOR, keyed openalex:<id> in
// twoai_harvest_cursors. Adding a subfield therefore starts a fresh backfill
// without disturbing the progress of the others, and removing one leaves its
// rows in place. The per-run page budget is shared round-robin so one large
// subfield cannot starve the rest.
var twoaiOASubfields = []struct{ id, name string }{
	{"1702", "Artificial Intelligence"},
	{"1707", "Computer Vision and Pattern Recognition"},
	{"1703", "Computational Theory and Mathematics"},
	{"1706", "Computer Science Applications"},
	{"2718", "Health Informatics"},
}

// twoaiOADoc is one work as OpenAlex returns it. Named rather than inline
// because the DOI queue resolves single works through the same shape, and
// two copies of this struct would drift apart.
type twoaiOADoc struct {
	ID       string `json:"id"`
	DOI      string `json:"doi"`
	Title    string `json:"title"`
	Display  string `json:"display_name"`
	PubYear  int    `json:"publication_year"`
	PubDate  string `json:"publication_date"`
	Language string `json:"language"`
	Type     string `json:"type"`
	IDs      struct {
		PMID string `json:"pmid"`
	} `json:"ids"`
	PrimaryTopic *struct {
		Name  string  `json:"display_name"`
		Score float64 `json:"score"`
	} `json:"primary_topic"`
	OpenAccess struct {
		Status string `json:"oa_status"`
		URL    string `json:"oa_url"`
	} `json:"open_access"`
	BestOA *struct {
		License string `json:"license"`
		PDFURL  string `json:"pdf_url"`
	} `json:"best_oa_location"`
	Authorships []struct {
		Author struct {
			Name  string `json:"display_name"`
			ORCID string `json:"orcid"`
			ID    string `json:"id"`
		} `json:"author"`
		Institutions []struct {
			Name string `json:"display_name"`
			ROR  string `json:"ror"`
		} `json:"institutions"`
	} `json:"authorships"`
	CitedBy    int              `json:"cited_by_count"`
	Referenced int              `json:"referenced_works_count"`
	AbstractII map[string][]int `json:"abstract_inverted_index"`
	Updated    string           `json:"updated_date"`
}

// twoaiOAAbstract rebuilds readable text from OpenAlex's inverted index.
func twoaiOAAbstract(inv map[string][]int) string {
	if len(inv) == 0 {
		return ""
	}
	type pw struct {
		pos int
		w   string
	}
	var all []pw
	for w, positions := range inv {
		for _, p := range positions {
			all = append(all, pw{p, w})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	words := make([]string, 0, len(all))
	for _, x := range all {
		words = append(words, x.w)
	}
	s := strings.Join(words, " ")
	if len(s) > 6000 {
		s = s[:6000]
	}
	return s
}

// twoaiArxivID resolves an arXiv identifier for a work.
//
// THE DOI IS AUTHORITATIVE AND THE URL IS NOT. arXiv mints DOIs under the
// 10.48550 prefix as 10.48550/arXiv.2401.01234, which is exact. Scraping the
// open-access URL, which is what the first version of this stage did, finds an
// id only when arXiv happens to be the best OA location AND the URL happens to
// use the /abs/ form: across the first 29,974 works harvested it produced ONE
// arxiv_id while 16 works carried arXiv DOIs, and 2,082 more sat behind
// /pdf/ URLs the /abs/ test never saw. On a corpus about AI, where arXiv is
// where the field actually publishes, that is not a rounding error. Legacy
// ids keep their category prefix (quant-ph/9707021), which is correct.
func twoaiArxivID(doi, oaURL string) string {
	if doi != "" {
		low := strings.ToLower(doi)
		if strings.HasPrefix(low, "10.48550/arxiv.") {
			return doi[len("10.48550/arXiv."):]
		}
	}
	for _, marker := range []string{"arxiv.org/abs/", "arxiv.org/pdf/"} {
		if i := strings.Index(oaURL, marker); i >= 0 {
			id := oaURL[i+len(marker):]
			id = strings.TrimSuffix(id, ".pdf")
			if j := strings.IndexAny(id, "?#"); j >= 0 {
				id = id[:j]
			}
			if id != "" {
				return id
			}
		}
	}
	return ""
}

// twoaiOAUpsert writes one OpenAlex work into the spine. Shared by the
// subfield backfill and the DOI queue, so a work resolved by hand is
// indistinguishable from one the backfill found - same columns, same
// provenance, same licence class.
func twoaiOAUpsert(db *sql.DB, d twoaiOADoc) (string, error) {
	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = strings.TrimSpace(d.Display)
	}
	if title == "" || d.ID == "" {
		return "", fmt.Errorf("work has no title or id")
	}
	oid := strings.TrimPrefix(d.ID, "https://openalex.org/")
	doi := strings.TrimPrefix(d.DOI, "https://doi.org/")
	arxiv := twoaiArxivID(doi, d.OpenAccess.URL)
	pmid := strings.TrimPrefix(d.IDs.PMID, "https://pubmed.ncbi.nlm.nih.gov/")
	var topic string
	var score float64
	if d.PrimaryTopic != nil {
		topic, score = d.PrimaryTopic.Name, d.PrimaryTopic.Score
	}
	license := ""
	if d.BestOA != nil {
		license = d.BestOA.License
	}
	type auth struct {
		Name  string `json:"name"`
		ORCID string `json:"orcid,omitempty"`
	}
	type inst struct {
		Name string `json:"name"`
		ROR  string `json:"ror,omitempty"`
	}
	var authors []auth
	instSeen := map[string]bool{}
	var insts []inst
	for i, a := range d.Authorships {
		if i >= 25 {
			break
		}
		authors = append(authors, auth{a.Author.Name, strings.TrimPrefix(a.Author.ORCID, "https://orcid.org/")})
		for _, in := range a.Institutions {
			if in.Name != "" && !instSeen[in.Name] {
				instSeen[in.Name] = true
				insts = append(insts, inst{in.Name, strings.TrimPrefix(in.ROR, "https://ror.org/")})
			}
		}
	}
	aj, _ := json.Marshal(authors)
	ij, _ := json.Marshal(insts)
	var pubDate any
	if d.PubDate != "" {
		pubDate = d.PubDate
	}
	if _, err := db.Exec(`INSERT INTO twoai_works
		(openalex_id, doi, arxiv_id, pmid, title, abstract, pub_date, pub_year,
		 work_type, language, oa_status, oa_url, license, cited_by, referenced,
		 topic, topic_score, authors, institutions, source_updated)
		VALUES ($1,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),$5,NULLIF($6,''),$7,$8,
		 $9,$10,$11,$12,NULLIF($13,''),$14,$15,$16,$17,$18::jsonb,$19::jsonb,NULLIF($20,'')::date)
		ON CONFLICT (openalex_id) DO UPDATE SET
			doi=EXCLUDED.doi, arxiv_id=COALESCE(EXCLUDED.arxiv_id, twoai_works.arxiv_id),
			pmid=COALESCE(EXCLUDED.pmid, twoai_works.pmid),
			title=EXCLUDED.title,
			abstract=COALESCE(EXCLUDED.abstract, twoai_works.abstract),
			oa_status=EXCLUDED.oa_status, oa_url=EXCLUDED.oa_url,
			license=EXCLUDED.license, cited_by=EXCLUDED.cited_by,
			referenced=EXCLUDED.referenced, topic=EXCLUDED.topic,
			topic_score=EXCLUDED.topic_score, authors=EXCLUDED.authors,
			institutions=EXCLUDED.institutions,
			source_updated=EXCLUDED.source_updated, last_seen=now()`,
		oid, doi, arxiv, pmid, title, twoaiOAAbstract(d.AbstractII), pubDate, d.PubYear,
		d.Type, d.Language, d.OpenAccess.Status, d.OpenAccess.URL, license,
		d.CitedBy, d.Referenced, topic, score, string(aj), string(ij), d.Updated); err != nil {
		return "", err
	}
	return oid, nil
}

// errOpenAlexBudget marks the one 429 that waiting cannot cure: the daily
// spend on the metered API is gone until midnight UTC. It stops the whole
// stage rather than each subfield burning its backoffs in turn.
var errOpenAlexBudget = errors.New("openalex daily budget spent")

// twoaiOpenAlex harvests every configured subfield, sharing the run's page
// budget between them so one crowded subfield cannot starve the others.
func twoaiOpenAlex(db *sql.DB) error {
	if err := twoaiOAEnsureTables(db); err != nil {
		return err
	}
	per := twoaiOAPagesPerRun / len(twoaiOASubfields)
	if per < 1 {
		per = 1
	}
	total := 0
	for _, sf := range twoaiOASubfields {
		n, err := twoaiOAHarvestSubfield(db, sf.id, sf.name, per)
		if errors.Is(err, errOpenAlexBudget) {
			total += n
			fmt.Printf("openalex: stopping after %s, daily budget spent; cursors are saved and the next run resumes\n", sf.name)
			break
		}
		if err != nil {
			// One subfield failing must not cost the others their turn.
			fmt.Fprintf(os.Stderr, "openalex %s: %v\n", sf.name, err)
			continue
		}
		total += n
	}
	var works int
	db.QueryRow(`SELECT count(*) FROM twoai_works`).Scan(&works)
	fmt.Printf("openalex: subfields=%d saved=%d works_total=%d\n", len(twoaiOASubfields), total, works)
	return nil
}

func twoaiOAEnsureTables(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_works (
		openalex_id text PRIMARY KEY,
		doi text,
		arxiv_id text,
		pmid text,
		title text NOT NULL,
		abstract text,
		pub_date date,
		pub_year int,
		work_type text,
		language text,
		oa_status text,
		oa_url text,
		license text,
		cited_by int,
		referenced int,
		topic text,
		topic_score real,
		authors jsonb NOT NULL DEFAULT '[]'::jsonb,
		institutions jsonb NOT NULL DEFAULT '[]'::jsonb,
		source_updated date,
		provenance text NOT NULL DEFAULT 'openalex',
		license_class text NOT NULL DEFAULT 'metadata_cc0_abstract_cite_only',
		first_seen timestamptz NOT NULL DEFAULT now(),
		last_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("openalex create works: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_works_doi ON twoai_works (doi) WHERE doi IS NOT NULL`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_works_arxiv ON twoai_works (arxiv_id) WHERE arxiv_id IS NOT NULL`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_works_year ON twoai_works (pub_year)`)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_harvest_cursors (
		source text PRIMARY KEY,
		mode text NOT NULL,
		cursor text,
		high_water text,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("openalex create cursors: %w", err)
	}

	return nil
}

// twoaiOAHarvestSubfield walks one subfield, resuming from its own cursor.
func twoaiOAHarvestSubfield(db *sql.DB, subfieldID, subfieldName string, pageBudget int) (int, error) {
	source := "openalex:" + subfieldID
	var mode, cursor, highWater string
	err := db.QueryRow(`SELECT mode, COALESCE(cursor,''), COALESCE(high_water,'')
		FROM twoai_harvest_cursors WHERE source=$1`, source).Scan(&mode, &cursor, &highWater)
	if err == sql.ErrNoRows {
		mode, cursor = "backfill", "*"
		// Delta high-water starts at harvest birth: anything updated after
		// today is caught by delta mode even if backfill takes weeks.
		highWater = time.Now().UTC().Format("2006-01-02")
		db.Exec(`INSERT INTO twoai_harvest_cursors (source, mode, cursor, high_water)
			VALUES ($1,$2,$3,$4)`, source, mode, cursor, highWater)
	} else if err != nil {
		return 0, fmt.Errorf("openalex cursor read: %w", err)
	}

	// BACKFILL IS PHASED BY RECENCY, NOT FILTERED BY IT.
	//
	// The five subfields hold about 8.1 million works and the corpus held
	// 509,374 of them on 2026-08-30, roughly six percent, with every subfield
	// still in backfill. Walking them in OpenAlex's default order means the
	// corpus reaches the present last, which is precisely backwards for a
	// site whose value is being current.
	//
	// The fix is ordering, not exclusion. Phase one takes 2015 and later,
	// about 4.35 million works, which is where the modern field lives. When
	// that exhausts, phase two takes everything before 2015 and the archive
	// arrives anyway. Only then does the subfield flip to delta.
	//
	// Excluding the old work outright was considered and rejected, for the
	// reason already recorded below: the pre-1950 rows are Church on
	// lambda-conversion, Peirce on the algebra of logic, Russell, Bouton on
	// Nim. An encyclopedia of AI that cannot cite Church to save a few weeks
	// of harvesting has traded the wrong thing.
	//
	// Changing the filter invalidates an in-flight cursor, so a subfield
	// entering phase one restarts its walk. Upserts make that safe and the
	// cost is pages, not correctness.
	base := "primary_topic.subfield.id:subfields/" + subfieldID
	filter := base
	switch mode {
	case "delta":
		filter = base + ",from_updated_date:" + highWater
		if cursor == "" {
			cursor = "*"
		}
	case "archive":
		filter = base + ",to_publication_date:" + twoaiOARecentFrom.AddDate(0, 0, -1).Format("2006-01-02")
	default: // backfill, phase one
		filter = base + ",from_publication_date:" + twoaiOARecentFrom.Format("2006-01-02")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	saved, pages, skippedYear := 0, 0, 0
	newestUpdate := highWater

	for pages < pageBudget && cursor != "" {
		// THE BUDGET FAULT, 2026-08-30, and why it was never a bill.
		// OpenAlex went metered in February 2026. A request with NO api_key
		// draws on a $0.10/day anonymous allowance; a FREE key, thirty
		// seconds at openalex.org/settings/api, draws on $1/day. List and
		// filter calls, which is all this stage makes, cost $0.10 per
		// thousand. At 150 pages a run and eight runs a day that is 1,200
		// calls, twelve cents: over the anonymous allowance every day, and
		// an eighth of what a free key allows. The whole outage was a
		// missing free key. Set OPENALEX_API_KEY on the cron. Without it the
		// stage still runs and still stops cleanly when the allowance goes.
		u := fmt.Sprintf("https://api.openalex.org/works?filter=%s&per-page=200&cursor=%s&select=%s&mailto=%s",
			url.QueryEscape(filter), url.QueryEscape(cursor), url.QueryEscape(twoaiOASelect), twoaiOAMailto)
		if k := os.Getenv("OPENALEX_API_KEY"); k != "" {
			u += "&api_key=" + url.QueryEscape(k)
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "theworldofai.org works-spine (mailto:"+twoaiOAMailto+")")

		var body []byte
		ok := false
		for attempt := 0; attempt < 4; attempt++ {
			resp, err := client.Do(req)
			if err != nil {
				fmt.Fprintln(os.Stderr, "openalex fetch:", err)
				break
			}
			body, _ = io.ReadAll(io.LimitReader(resp.Body, 40<<20))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ok = true
				break
			}
			// OpenAlex moved to a metered API: a 429 can mean "too fast",
			// which backoff fixes, or "your daily budget is spent", which it
			// cannot. On 2026-08-30 every subfield burned four backoffs, 75
			// seconds each pass, on a budget that had already reset to zero.
			// Read the body and stop the whole stage on a budget refusal.
			if resp.StatusCode == 429 && strings.Contains(string(body), "Insufficient budget") {
				hint := ""
				if os.Getenv("OPENALEX_API_KEY") == "" {
					hint = " (no OPENALEX_API_KEY set: this run used the $0.10/day anonymous allowance, a free key raises it to $1/day)"
				}
				fmt.Fprintf(os.Stderr, "openalex: daily budget spent, stopping the stage%s: %s\n", hint, truncate(string(body), 200))
				return saved, errOpenAlexBudget
			}
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				wait := time.Duration(5*(1<<attempt)) * time.Second
				fmt.Fprintf(os.Stderr, "openalex http %d, backing off %s\n", resp.StatusCode, wait)
				time.Sleep(wait)
				continue
			}
			fmt.Fprintf(os.Stderr, "openalex http %d: %s\n", resp.StatusCode, truncate(string(body), 200))
			break
		}
		if !ok {
			// Cursor stays where the last saved page left it; next run resumes.
			break
		}

		var out struct {
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"meta"`
			Results []twoaiOADoc `json:"results"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			fmt.Fprintln(os.Stderr, "openalex parse:", err)
			break
		}
		if len(out.Results) == 0 {
			cursor = out.Meta.NextCursor
			break
		}

		for _, w := range out.Results {
			// NO YEAR FLOOR, DELIBERATELY, AND THIS REVERSED AN EARLIER FIX.
			// A 1950 floor looked obviously right - the field does not
			// predate 1950 - until the 153 pre-1950 rows were read: Church on
			// the calculi of lambda-conversion, Peirce on the algebra of
			// logic, Russell, Bouton's Nim. The genuine noise was 14
			// misclassified geology papers, 0.05% of the corpus. Discarding
			// Church to remove conchology is a bad trade. TOPIC, not year, is
			// the discriminator if pre-modern noise ever matters. Only an
			// impossible future date is rejected.
			if w.PubYear > time.Now().Year()+1 {
				skippedYear++
				continue
			}
			if _, err := twoaiOAUpsert(db, w); err != nil {
				fmt.Fprintln(os.Stderr, "openalex upsert:", err)
				continue
			}
			saved++
			if w.Updated > newestUpdate {
				newestUpdate = w.Updated
			}
		}

		cursor = out.Meta.NextCursor
		pages++
		// Persist position after EVERY page: a crash costs one page, not a run.
		db.Exec(`UPDATE twoai_harvest_cursors SET cursor=$1, updated_at=now() WHERE source=$2`, cursor, source)
		time.Sleep(350 * time.Millisecond)
	}

	// Phase one exhausted: walk the archive next, and only after that settle
	// into delta. The high-water date advances only in delta mode, and
	// conservatively (the newest updated_date actually seen).
	if mode == "backfill" && cursor == "" {
		db.Exec(`UPDATE twoai_harvest_cursors SET mode='archive', cursor='*', updated_at=now()
			WHERE source=$1`, source)
		fmt.Printf("openalex %s: recent backfill complete (%s onward), starting archive\n",
			subfieldName, twoaiOARecentFrom.Format("2006"))
	}
	if mode == "archive" && cursor == "" {
		db.Exec(`UPDATE twoai_harvest_cursors SET mode='delta', cursor='', updated_at=now()
			WHERE source=$1`, source)
		fmt.Printf("openalex %s: archive complete, switching to delta mode\n", subfieldName)
	}
	if mode == "delta" {
		db.Exec(`UPDATE twoai_harvest_cursors SET cursor='', high_water=$1, updated_at=now()
			WHERE source=$2`, newestUpdate, source)
	}

	fmt.Printf("openalex %s: mode=%s pages=%d saved=%d skipped_year=%d\n",
		subfieldName, mode, pages, saved, skippedYear)
	return saved, nil
}
