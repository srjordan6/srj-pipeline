package main

import (
	"database/sql"
	"encoding/json"
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
// stored with the cursor: "backfill" walks the whole AI subfield oldest
// cursor first; when the cursor exhausts, the stage flips itself to "delta"
// and thereafter asks only for works updated since the high-water date. The
// per-run page budget keeps a single run polite and bounded; the corpus is
// built by the daily rhythm, not by one heroic pull.
//
// The sandbox IP that developed this was rate-limited by OpenAlex even with
// the polite pool, so 429 handling is not theoretical: exponential backoff,
// and a run that gives up leaves the cursor where it was.

const (
	twoaiOAMailto = "info@srjconsultingservices.com"
	// The Artificial Intelligence subfield in OpenAlex's topic taxonomy.
	// Scope decision: primary_topic only, so a medical paper that mentions a
	// neural net does not flood the spine. Widening later is a filter change.
	twoaiOAFilterBackfill = "primary_topic.subfield.id:subfields/1702"
	twoaiOAPagesPerRun    = 150 // x200 works = up to 30k rows a run
	twoaiOASelect         = "id,doi,title,display_name,publication_year,publication_date,language,type,ids,primary_topic,open_access,best_oa_location,authorships,cited_by_count,referenced_works_count,abstract_inverted_index,updated_date"
)

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

func twoaiOpenAlex(db *sql.DB) error {
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

	var mode, cursor, highWater string
	err := db.QueryRow(`SELECT mode, COALESCE(cursor,''), COALESCE(high_water,'')
		FROM twoai_harvest_cursors WHERE source='openalex'`).Scan(&mode, &cursor, &highWater)
	if err == sql.ErrNoRows {
		mode, cursor = "backfill", "*"
		// Delta high-water starts at harvest birth: anything updated after
		// today is caught by delta mode even if backfill takes weeks.
		highWater = time.Now().UTC().Format("2006-01-02")
		db.Exec(`INSERT INTO twoai_harvest_cursors (source, mode, cursor, high_water)
			VALUES ('openalex',$1,$2,$3)`, mode, cursor, highWater)
	} else if err != nil {
		return fmt.Errorf("openalex cursor read: %w", err)
	}

	filter := twoaiOAFilterBackfill
	if mode == "delta" {
		filter = twoaiOAFilterBackfill + ",from_updated_date:" + highWater
		if cursor == "" {
			cursor = "*"
		}
	}

	client := &http.Client{Timeout: 60 * time.Second}
	saved, pages := 0, 0
	newestUpdate := highWater

	for pages < twoaiOAPagesPerRun && cursor != "" {
		u := fmt.Sprintf("https://api.openalex.org/works?filter=%s&per-page=200&cursor=%s&select=%s&mailto=%s",
			url.QueryEscape(filter), url.QueryEscape(cursor), url.QueryEscape(twoaiOASelect), twoaiOAMailto)
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
			Results []struct {
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
			} `json:"results"`
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
			title := strings.TrimSpace(w.Title)
			if title == "" {
				title = strings.TrimSpace(w.Display)
			}
			if title == "" || w.ID == "" {
				continue
			}
			oid := strings.TrimPrefix(w.ID, "https://openalex.org/")
			doi := strings.TrimPrefix(w.DOI, "https://doi.org/")
			// arXiv id recovered from the OA URL when the work lives there;
			// good enough for the join, refined later by the arXiv enricher.
			arxiv := ""
			if strings.Contains(w.OpenAccess.URL, "arxiv.org/abs/") {
				arxiv = w.OpenAccess.URL[strings.Index(w.OpenAccess.URL, "arxiv.org/abs/")+len("arxiv.org/abs/"):]
			}
			pmid := strings.TrimPrefix(w.IDs.PMID, "https://pubmed.ncbi.nlm.nih.gov/")
			var topic string
			var score float64
			if w.PrimaryTopic != nil {
				topic, score = w.PrimaryTopic.Name, w.PrimaryTopic.Score
			}
			license := ""
			if w.BestOA != nil {
				license = w.BestOA.License
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
			for i, a := range w.Authorships {
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
			if w.PubDate != "" {
				pubDate = w.PubDate
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
				oid, doi, arxiv, pmid, title, twoaiOAAbstract(w.AbstractII), pubDate, w.PubYear,
				w.Type, w.Language, w.OpenAccess.Status, w.OpenAccess.URL, license,
				w.CitedBy, w.Referenced, topic, score, string(aj), string(ij), w.Updated); err != nil {
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
		db.Exec(`UPDATE twoai_harvest_cursors SET cursor=$1, updated_at=now() WHERE source='openalex'`, cursor)
		time.Sleep(350 * time.Millisecond)
	}

	// Backfill exhausted: flip to delta. The high-water date advances only in
	// delta mode, and conservatively (the newest updated_date actually seen).
	if mode == "backfill" && cursor == "" {
		db.Exec(`UPDATE twoai_harvest_cursors SET mode='delta', cursor='', updated_at=now()
			WHERE source='openalex'`)
		fmt.Println("openalex: backfill complete, switching to delta mode")
	}
	if mode == "delta" {
		db.Exec(`UPDATE twoai_harvest_cursors SET cursor='', high_water=$1, updated_at=now()
			WHERE source='openalex'`, newestUpdate)
	}

	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_works`).Scan(&total)
	fmt.Printf("openalex: mode=%s pages=%d saved=%d works_total=%d\n", mode, pages, saved, total)
	return nil
}
