package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// AI BOOKS YOU CAN ACTUALLY OPEN, FROM OPEN LIBRARY.
//
// WHAT THIS IS NOT. The existing AI Books page is an editorial shortlist:
// eleven titles chosen for significance, each verified by hand. Open Library
// answers "artificial intelligence" with 22,786 works. Pouring that into a
// curated page would destroy the only thing that makes it worth reading, so
// this builds a SEPARATE catalogue with a different promise: not "the books
// that matter" but "the AI books you can read right now, for free".
//
// THE GATES, AND WHY EACH ONE EXISTS. Every one was written after looking at
// what the API actually returns, not from imagination:
//
//  1. FICTION IS NOT A BOOK ABOUT AI. The top results for the artificial
//     intelligence subject are Iain M. Banks' Surface Detail, Stross's
//     Accelerando and Murderbot. They are novels tagged with the subject
//     because AI is in the plot. A reference catalogue listing Murderbot as an
//     AI book is simply wrong.
//
//  2. THE SERVER-SIDE NOT IS A LIE. Adding `NOT subject:fiction` to the query
//     changes nothing: records whose subjects plainly contain "fiction" still
//     come back. Verified 2026-08-25. So the filtering happens HERE, against
//     the subject array of each record, and cannot be moved into the query to
//     save a loop.
//
//  3. CONFERENCE VOLUMES ARE NOT READING. Springer LNCS proceedings dominate
//     by edition count - Neural Information Processing, Advances in Swarm
//     Intelligence, Image Analysis and Recognition. Each conference year adds
//     an edition, so sorting by editions surfaces them above every real book.
//
//  4. READABLE ONLY. The decisive gate. Requiring ebook_access of public or
//     borrowable cuts 300 scanned records to 17, and those 17 are Minsky's
//     Perceptrons, Dreyfus, Penrose, Boden, Turkle, Feigenbaum. It is a harsh
//     filter and that is the point: it turns a directory of things that exist
//     into a shelf a reader can use tonight.
//
// LICENSING. Open Library bibliographic metadata is public domain, published
// by the Internet Archive; titles, authors, years and identifiers are facts.
// No description or review text is copied - only the record and the link back
// to Open Library, where the reader borrows or reads the book.
//
// The API asks callers to identify themselves and to go easy on the request
// rate; both are honoured below.

const twoaiOLAgent = "theworldofai.org book catalogue (info@srjconsultingservices.com)"

// twoaiOLTopics are the shelves this catalogue keeps. The topic label is
// stored on each row so the page can group by it, and the query is what the
// API is asked. Each is scoped to English because the render is English and a
// German edition of a book nobody here can describe is not a service.
var twoaiOLTopics = []struct{ topic, query string }{
	{"Foundations and history", `subject:"artificial intelligence" AND language:eng`},
	{"Machine learning", `subject:"machine learning" AND language:eng`},
	{"Neural networks and deep learning", `subject:"neural networks (computer science)" AND language:eng`},
	{"Language and speech", `subject:"natural language processing (computer science)" AND language:eng`},
	{"Computer vision", `subject:"computer vision" AND language:eng`},
	{"Robotics", `subject:"robotics" AND language:eng`},
	{"Ethics, society and policy", `subject:"artificial intelligence" AND subject:"moral and ethical aspects" AND language:eng`},
	{"Data and statistics", `subject:"data mining" AND language:eng`},
}

var twoaiOLAIRe = regexp.MustCompile(`(?i)\b(artificial intelligence|machine learning|deep learning|neural network|natural language processing|large language model|reinforcement learning|computer vision|data science|data mining|generative ai|expert system|robotic|cybernetic|algorithm|pattern recognition|knowledge representation)`)
var twoaiOLBadSubject = regexp.MustCompile(`(?i)\bfiction\b|science fiction|congresses|conference|proceedings|lecture notes|juvenile|comic|graphic novel`)
var twoaiOLBadTitle = regexp.MustCompile(`(?i)\b(proceedings|transactions|advances in|lecture notes|workshop|symposium|selected papers|revised papers)`)

type twoaiOLDoc struct {
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	Authors      []string `json:"author_name"`
	FirstYear    int      `json:"first_publish_year"`
	Subjects     []string `json:"subject"`
	EbookAccess  string   `json:"ebook_access"`
	IA           []string `json:"ia"`
	CoverID      int      `json:"cover_i"`
	EditionCount int      `json:"edition_count"`
}

// twoaiOLKeep applies the gates above. Returns false and a reason.
func twoaiOLKeep(d twoaiOLDoc) (bool, string) {
	if d.Key == "" || strings.TrimSpace(d.Title) == "" {
		return false, "empty"
	}
	subs := strings.Join(d.Subjects, " ; ")
	if twoaiOLBadSubject.MatchString(subs) {
		return false, "subject"
	}
	if twoaiOLBadTitle.MatchString(d.Title) {
		return false, "title"
	}
	if !twoaiOLAIRe.MatchString(d.Title) && !twoaiOLAIRe.MatchString(subs) {
		return false, "not-ai"
	}
	// The field predates 1950 only in retrospect; a 1910 book tagged
	// artificial intelligence is a cataloguing artefact.
	if d.FirstYear < 1950 || d.FirstYear > time.Now().Year()+1 {
		return false, "year"
	}
	if d.EbookAccess != "public" && d.EbookAccess != "borrowable" {
		return false, "not-readable"
	}
	return true, ""
}

func twoaiOpenLibrary(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_book_catalog (
		work_key text PRIMARY KEY,
		title text NOT NULL,
		authors jsonb NOT NULL DEFAULT '[]'::jsonb,
		first_year int,
		subjects jsonb NOT NULL DEFAULT '[]'::jsonb,
		ebook_access text,
		ia_id text,
		cover_id int,
		edition_count int,
		topic text,
		harvested_query text,
		first_seen date NOT NULL DEFAULT current_date,
		last_seen date NOT NULL DEFAULT current_date,
		active boolean NOT NULL DEFAULT true)`); err != nil {
		return fmt.Errorf("openlibrary create table: %w", err)
	}

	client := &http.Client{Timeout: 45 * time.Second}
	fields := "key,title,author_name,first_publish_year,ebook_access,edition_count,subject,ia,cover_i"
	saved, scanned := 0, 0
	rejects := map[string]int{}

	for _, t := range twoaiOLTopics {
		topicSaved := 0
		// Three pages of 100 per topic per run. The catalogue grows across
		// runs rather than hammering the API in one burst, which is what the
		// Open Library rate guidance asks for.
		for page := 1; page <= 3; page++ {
			u := fmt.Sprintf("https://openlibrary.org/search.json?q=%s&limit=100&page=%d&sort=editions&fields=%s",
				url.QueryEscape(t.query), page, url.QueryEscape(fields))
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("User-Agent", twoaiOLAgent)
			resp, err := client.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "openlibrary: %s p%d: %v\n", t.topic, page, err)
				break
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
			resp.Body.Close()
			if resp.StatusCode != 200 {
				fmt.Fprintf(os.Stderr, "openlibrary: %s p%d http %d\n", t.topic, page, resp.StatusCode)
				break
			}
			var out struct {
				Docs []twoaiOLDoc `json:"docs"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				fmt.Fprintf(os.Stderr, "openlibrary: %s p%d parse: %v\n", t.topic, page, err)
				break
			}
			if len(out.Docs) == 0 {
				break
			}
			for _, d := range out.Docs {
				scanned++
				ok, why := twoaiOLKeep(d)
				if !ok {
					rejects[why]++
					continue
				}
				authors, _ := json.Marshal(d.Authors)
				// Subjects are capped: a record can carry hundreds, and the
				// page shows a handful.
				subs := d.Subjects
				if len(subs) > 12 {
					subs = subs[:12]
				}
				subjects, _ := json.Marshal(subs)
				iaID := ""
				if len(d.IA) > 0 {
					iaID = d.IA[0]
				}
				var cover any
				if d.CoverID > 0 {
					cover = d.CoverID
				}
				// topic is set on first insert and left alone after: a work
				// that matches two shelves keeps the one that found it first,
				// so a book does not migrate between runs.
				if _, err := db.Exec(`INSERT INTO twoai_book_catalog
					(work_key, title, authors, first_year, subjects, ebook_access,
					 ia_id, cover_id, edition_count, topic, harvested_query)
					VALUES ($1,$2,$3::jsonb,$4,$5::jsonb,$6,$7,$8,$9,$10,$11)
					ON CONFLICT (work_key) DO UPDATE SET
						title=EXCLUDED.title, authors=EXCLUDED.authors,
						subjects=EXCLUDED.subjects, ebook_access=EXCLUDED.ebook_access,
						ia_id=COALESCE(EXCLUDED.ia_id, twoai_book_catalog.ia_id),
						cover_id=COALESCE(EXCLUDED.cover_id, twoai_book_catalog.cover_id),
						edition_count=EXCLUDED.edition_count,
						last_seen=current_date, active=true`,
					d.Key, strings.TrimSpace(d.Title), string(authors), d.FirstYear,
					string(subjects), d.EbookAccess, iaID, cover, d.EditionCount,
					t.topic, t.query); err != nil {
					fmt.Fprintln(os.Stderr, "openlibrary upsert:", err)
					continue
				}
				saved++
				topicSaved++
			}
			time.Sleep(700 * time.Millisecond) // courtesy to a free public API
		}
		fmt.Printf("openlibrary: %s kept=%d\n", t.topic, topicSaved)
	}

	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_book_catalog WHERE active`).Scan(&total)
	fmt.Printf("openlibrary: scanned=%d kept=%d catalogue=%d rejects fiction/conf=%d title=%d not-ai=%d year=%d not-readable=%d\n",
		scanned, saved, total, rejects["subject"], rejects["title"], rejects["not-ai"], rejects["year"], rejects["not-readable"])
	return nil
}

// twoaiBookCatalog renders the catalogue into content/learn/. Returns the
// number of section files written, which is 0 when the harvest is empty so a
// page never promises a shelf it cannot fill.
func twoaiBookCatalog(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type book struct {
		Title    string   `json:"title"`
		Authors  []string `json:"authors"`
		Year     int      `json:"year,omitempty"`
		Access   string   `json:"access"`
		URL      string   `json:"url"`
		ReadURL  string   `json:"read_url,omitempty"`
		CoverURL string   `json:"cover_url,omitempty"`
		Topic    string   `json:"topic"`
	}
	rows, err := db.Query(`SELECT work_key, title, authors, COALESCE(first_year,0),
			COALESCE(ebook_access,''), COALESCE(ia_id,''), COALESCE(cover_id,0), COALESCE(topic,'')
		FROM twoai_book_catalog WHERE active
		ORDER BY topic, first_year DESC NULLS LAST, title LIMIT 600`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	byTopic := map[string][]book{}
	var order []string
	free := 0
	total := 0
	for rows.Next() {
		var key, title, authorsJSON, access, iaID, topic string
		var year, cover int
		if rows.Scan(&key, &title, &authorsJSON, &year, &access, &iaID, &cover, &topic) != nil {
			continue
		}
		var authors []string
		json.Unmarshal([]byte(authorsJSON), &authors)
		if len(authors) > 3 {
			authors = authors[:3]
		}
		b := book{
			Title: title, Authors: authors, Year: year, Access: access,
			URL: "https://openlibrary.org" + key, Topic: topic,
		}
		if iaID != "" {
			b.ReadURL = "https://archive.org/details/" + iaID
		}
		if cover > 0 {
			b.CoverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", cover)
		}
		if access == "public" {
			free++
		}
		if _, seen := byTopic[topic]; !seen {
			order = append(order, topic)
		}
		byTopic[topic] = append(byTopic[topic], b)
		total++
	}
	if total == 0 {
		fmt.Println("twoai_build: book catalogue 0 rows, section not rendered")
		return 0, nil
	}

	type group struct {
		Topic string `json:"topic"`
		Books []book `json:"books"`
	}
	var groups []group
	for _, t := range order {
		groups = append(groups, group{Topic: t, Books: byTopic[t]})
	}

	uid := twoaiUID("section:ai-book-catalog")
	if err := upsert("learn/book-catalog.json", "book-catalog", map[string]any{
		"uid": uid, "shape": "book-catalog", "tax": "ai-book-catalog",
		"name":   "AI Book Catalogue",
		"blurb":  "Artificial intelligence books you can read or borrow today, from Open Library.",
		"groups": groups, "total": total, "free": free,
		"topics": len(groups), "generated": today,
	}); err != nil {
		return 0, err
	}
	db.Exec(`UPDATE twoai_taxonomy SET status='live', live_path=$1, updated_at=now()
		WHERE slug='ai-book-catalog'`,
		"/ai-ecosystem/research-knowledge-and-learning/"+uid+"/")

	fmt.Printf("twoai_build: book catalogue=%d topics=%d free=%d\n", total, len(groups), free)
	return 1, nil
}
