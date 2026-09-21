package main

// twoai_politics_lda: federal lobbying on AI, from the Lobbying Disclosure
// Act filings, for The Politics of AI hub (taxonomy politics-of-ai, uid
// d9480073). Stephen approved the neutral design on 2026-09-21: the hub
// records what can be proved (money, votes, bills, filings, statements) and
// never assigns a motive to a named person.
//
// SOURCE. lda.gov, the successor host of lda.senate.gov (which now answers
// 301 to it). Public, no key. Verified 2026-09-21: the filings endpoint takes
// filing_specific_lobbying_issues as a text search, filing_dt_posted_after,
// ordering and page_size=25, and returned 2,631 filings for 2026 matching
// "artificial intelligence".
//
// KEEPING IT CURRENT. Cursor per query, stored as the data itself: the next
// run asks only for filings posted after the newest one already held for
// that query, less three days so a filing posted late in a day is never
// missed. A first run walks from 2023 forward, oldest first, capped at
// ldaMaxPages a query, so the backfill completes over a few daily runs and
// then the stage reads only the day's new filings. Quarterly reports land in
// clusters around the 20th of January, April, July and October; the stage
// needs no schedule change for that, it simply reads more pages that week.
//
// NOTHING IS DELETED. A filing seen again refreshes last_seen and its
// fields; an amendment arrives as a new filing_uuid and is kept beside the
// original. A row that should not display is retired with a reason.
//
// NOT YET DONE, stated: registrants and clients are stored with their LDA
// ids and names, and are resolved to site uids in twoai_entities by the page
// build, not here. Until that exists, nothing from this table is published.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const ldaFilingsURL = "https://lda.gov/api/v1/filings/"

// Two phrases, because the text search is literal and filers write both.
// A bare "AI" is not searched: it matches inside unrelated words and codes.
// "data center" added 2026-09-21: an operator lobbying on power, permitting
// or water rarely writes the word AI, and the data center registry is one of
// the site's core sections.
var ldaQueries = []string{"artificial intelligence", "machine learning", "data center"}

const ldaMaxPages = 40 // per query per run, 25 filings a page

// An activity counts as AI lobbying only if its own description says so.
// The API's text search can match a filing on one activity and bring the
// unrelated ones with it; only the matching activities are kept as the
// filing's AI issue text.
var ldaAIRe = regexp.MustCompile(`(?i)artificial intelligence|machine learning|\bA\.?I\.?\b|generative|large language model|deepfake|algorithmic|automated decision|facial recognition`)

// Data center activity, kept alongside AI activity. Law-shaped, like the
// Federal Register data center pairings: the facility, its power and siting.
var ldaDCRe = regexp.MustCompile(`(?i)data ?cent(?:er|re)s?|hyperscale|large load|colocation facilit`)

// ldaKeep is the one test of whether a lobbying activity belongs on the site.
func ldaKeep(desc string) bool { return ldaAIRe.MatchString(desc) || ldaDCRe.MatchString(desc) }

// Bill numbers written in the activity text: H.R. 7334, S. 4686, H.Res. 12.
var ldaBillRe = regexp.MustCompile(`(?i)\b(H\.?\s?R(?:es)?\.?|S\.?\s?(?:Res\.?)?|S\.)\s?(\d{1,5})\b`)

var ldaUSRe = regexp.MustCompile(`(?i)\bU\.\s?S\.`)

type ldaPage struct {
	Count   int               `json:"count"`
	Next    *string           `json:"next"`
	Results []json.RawMessage `json:"results"`
}

type ldaFiling struct {
	FilingUUID        string   `json:"filing_uuid"`
	FilingYear        int      `json:"filing_year"`
	FilingPeriod      string   `json:"filing_period"`
	FilingType        string   `json:"filing_type"`
	FilingTypeDisplay string   `json:"filing_type_display"`
	DtPosted          string   `json:"dt_posted"`
	DocumentURL       string   `json:"filing_document_url"`
	Income            *string  `json:"income"`
	Expenses          *string  `json:"expenses"`
	Registrant        struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"registrant"`
	Client struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"general_description"`
	} `json:"client"`
	Activities []struct {
		Code        string `json:"general_issue_code"`
		Description string `json:"description"`
		Entities    []struct {
			Name string `json:"name"`
		} `json:"government_entities"`
		Lobbyists []json.RawMessage `json:"lobbyists"`
	} `json:"lobbying_activities"`
}

func ldaClean(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
}

// Postgres jsonb refuses the NUL escape, which is how CELESTICA broke the
// company harvest on 2026-09-20. Strip it before the raw filing is stored.
func ldaRaw(b []byte) []byte {
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "\x00", ""), `\u0000`, ""))
}

func ldaBills(text string) []string {
	seen := map[string]bool{}
	// Never nil: pq sends a nil slice as NULL, and bill_refs is NOT NULL.
	// That was every filing without a bill number on the first live run.
	out := []string{}
	// "U.S. 3" is not Senate bill 3. Go's regexp has no lookbehind, so the
	// country abbreviation is neutralised before matching.
	text = ldaUSRe.ReplaceAllString(text, "US ")
	for _, m := range ldaBillRe.FindAllStringSubmatch(text, -1) {
		p := strings.ToUpper(strings.NewReplacer(".", "", " ", "").Replace(m[1]))
		switch p {
		case "HR", "HRES", "S", "SRES":
		default:
			continue
		}
		k := p + " " + m[2]
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func twoaiPoliticsLDA(db *sql.DB) error {
	client := &http.Client{Timeout: 45 * time.Second}
	// Pace to the access level. Anonymous was cut off after 15 pages at 0.7s
	// apart on the first live run, so without a key the stage slows to one
	// page every four seconds, which trades minutes for a complete backfill.
	pace := 4 * time.Second
	maxPages := ldaMaxPages
	if os.Getenv("LDA_API_KEY") != "" {
		pace = 700 * time.Millisecond
		// 120 requests a minute with a key. 150 pages is about two minutes
		// a query and finishes the 2023 backfill in about four runs.
		maxPages = 150
	}
	stored, skipped, pages := 0, 0, 0
	retried := map[string]bool{}
	for _, q := range ldaQueries {
		var newest sql.NullTime
		_ = db.QueryRow(`SELECT max(dt_posted) FROM twoai_pol_lobbying WHERE $1 = ANY(matched_queries)`, q).Scan(&newest)
		after := "2023-01-01"
		if newest.Valid {
			after = newest.Time.Add(-72 * time.Hour).Format("2006-01-02")
		}
		v := url.Values{}
		v.Set("filing_specific_lobbying_issues", q)
		v.Set("filing_dt_posted_after", after)
		v.Set("ordering", "dt_posted")
		v.Set("page_size", "25")
		next := ldaFilingsURL + "?" + v.Encode()
		total := -1
		for p := 0; next != "" && p < maxPages; p++ {
			req, _ := http.NewRequest("GET", next, nil)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "theworldofai.org pipeline (theworldofai@inkboxmail.com)")
			// Anonymous access is rate limited hard: the first run was cut off
			// after 15 pages. A registered key raises the limit. Optional, so the
			// stage still runs without one, only slower.
			if k := os.Getenv("LDA_API_KEY"); k != "" {
				req.Header.Set("Authorization", "Token "+k)
			}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("%q page %d: %w", q, p, err)
			}
			if resp.StatusCode == 429 {
				resp.Body.Close()
				// One wait, if the server says it is short, then give up for
				// the run; the cursor resumes tomorrow.
				wait := 60 * time.Second
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if d, e := time.ParseDuration(ra + "s"); e == nil {
						wait = d
					}
				}
				if retried[q] || wait > 90*time.Second {
					fmt.Printf("twoai_politics_lda: %q rate limited at page %d, resuming next run from the cursor\n", q, p)
					break
				}
				retried[q] = true
				time.Sleep(wait)
				p--
				continue
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				return fmt.Errorf("%q page %d: http %d", q, p, resp.StatusCode)
			}
			var pg ldaPage
			err = json.NewDecoder(resp.Body).Decode(&pg)
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("%q page %d decode: %w", q, p, err)
			}
			pages++
			if total < 0 {
				total = pg.Count
			}
			for _, raw := range pg.Results {
				var f ldaFiling
				if json.Unmarshal(raw, &f) != nil || f.FilingUUID == "" {
					skipped++
					continue
				}
				issues, codes, ents := []string{}, []string{}, []string{}
				lob := 0
				entSeen := map[string]bool{}
				for _, a := range f.Activities {
					if !ldaKeep(a.Description) {
						continue
					}
					issues = append(issues, ldaClean(a.Description))
					codes = append(codes, ldaClean(a.Code))
					lob += len(a.Lobbyists)
					for _, e := range a.Entities {
						n := ldaClean(e.Name)
						if n != "" && !entSeen[n] {
							entSeen[n] = true
							ents = append(ents, n)
						}
					}
				}
				if len(issues) == 0 {
					skipped++
					continue
				}
				issueText := strings.Join(issues, "\n\n")
				h := sha256.Sum256([]byte("lda:" + f.FilingUUID))
				uid := hex.EncodeToString(h[:4])
				var posted interface{}
				if t, err := time.Parse(time.RFC3339, f.DtPosted); err == nil {
					posted = t
				}
				num := func(s *string) interface{} {
					if s == nil || *s == "" {
						return nil
					}
					return *s
				}
				_, err := db.Exec(`
					INSERT INTO twoai_pol_lobbying (filing_uuid, uid, filing_year, filing_period, filing_type,
						filing_type_display, dt_posted, registrant_id, registrant_name, client_id, client_name,
						client_description, income, expenses, document_url, ai_issue_text, issue_codes,
						government_entities, bill_refs, lobbyist_count, matched_queries, raw)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::numeric,$14::numeric,$15,$16,$17,$18,$19,$20,ARRAY[$21::text],$22)
					ON CONFLICT (filing_uuid) DO UPDATE SET
						dt_posted = EXCLUDED.dt_posted, income = EXCLUDED.income, expenses = EXCLUDED.expenses,
						ai_issue_text = EXCLUDED.ai_issue_text, issue_codes = EXCLUDED.issue_codes,
						government_entities = EXCLUDED.government_entities, bill_refs = EXCLUDED.bill_refs,
						lobbyist_count = EXCLUDED.lobbyist_count, raw = EXCLUDED.raw,
						matched_queries = (SELECT array_agg(DISTINCT x) FROM unnest(twoai_pol_lobbying.matched_queries || EXCLUDED.matched_queries) x),
						last_seen = now()`,
					f.FilingUUID, uid, f.FilingYear, f.FilingPeriod, f.FilingType, f.FilingTypeDisplay, posted,
					f.Registrant.ID, ldaClean(f.Registrant.Name), f.Client.ID, ldaClean(f.Client.Name),
					ldaClean(f.Client.Description), num(f.Income), num(f.Expenses), f.DocumentURL, issueText,
					pq.Array(codes), pq.Array(ents), pq.Array(ldaBills(issueText)), lob, q, ldaRaw(raw))
				if err != nil {
					fmt.Printf("twoai_politics_lda: store %s: %v\n", f.FilingUUID, err)
					skipped++
					continue
				}
				stored++
			}
			next = ""
			if pg.Next != nil {
				next = *pg.Next
			}
			time.Sleep(pace)
		}
		fmt.Printf("twoai_politics_lda: %q after=%s matching=%d more_pages_pending=%v\n", q, after, total, next != "")
	}
	var held, clients int
	_ = db.QueryRow(`SELECT count(*), count(DISTINCT client_id) FROM twoai_pol_lobbying WHERE retired_reason IS NULL`).Scan(&held, &clients)
	fmt.Printf("twoai_politics_lda: pages=%d stored=%d skipped_not_ai=%d held=%d clients=%d ok=true\n",
		pages, stored, skipped, held, clients)
	return nil
}
