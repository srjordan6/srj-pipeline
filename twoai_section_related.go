package main

// Related records for the section pages, theworldofai row 481 (Stephen,
// 2026-10-05, on Life Sciences: "the content is thin in this whole section").
//
// A section page is written prose. This attaches what the site already holds
// on the same subject, so no page rests on prose alone: the glossary terms and
// the companies its text names, facts checked at the source, news stories,
// the most cited papers, lawsuits and CVEs. Each list is empty when nothing
// matches, and the page shows only the lists that have rows.
//
// Matching is deliberately plain: the page name, less the words every page
// shares, must appear together (stemmed, English) and the record must also
// carry a word of the section's own domain, so "Cloud Provider Deals" finds
// pharma cloud deals and not every cloud deal. Papers must carry the name
// words in their title, because abstracts mention everything.

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Sections whose papers come from twoai_health_papers, not twoai_works.
var sectionHealthLibrary = map[string]bool{"dmed": true, "lpa": true}

// Words of each section's domain, one of which a matched record must carry.
var sectionDomainWords = map[string]string{
	"lsc":  "pharmaceutical | pharma | drug | drugs | biotech | biotechnology | clinical | fda | ema | medical | medicine | patient | health | biology | therapeutic | protein | antibody | gmp | pharmacovigilance | medtech",
	"dmed": "diabetes | diabetic | glucose | insulin | obesity | glp | metformin | a1c | hba1c | glycemic | incretin",
	"lpa":  "lipoprotein | cardiovascular | cholesterol | heart | aortic | atherosclerosis | lipid | pelacarsen | olpasiran | lepodisiran | zerlasiran | muvalaplin",
	"hcd":  "hospital | hospitals | clinical | clinician | patient | patients | health | healthcare | physician | nurse | nursing | medical | medicine",
}

// Words every page name in these trees shares, or that carry no subject.
var sectionStopWords = map[string]bool{
	"ai": true, "and": true, "the": true, "of": true, "for": true, "in": true, "with": true, "its": true,
	"a": true, "an": true, "how": true, "why": true, "using": true, "inside": true, "when": true, "needs": true,
	"itself": true, "what": true, "to": true, "on": true, "by": true, "at": true, "as": true, "from": true,
	"life": true, "sciences": true, "built": true, "latest": true,
	// Words that name a kind of page rather than its subject (row 484: "The
	// Lp(a) Research Center" found papers about research centres).
	"research": true, "center": true, "centre": true, "understanding": true, "current": true, "future": true,
	"treatment": true, "treatments": true, "list": true, "end": true, "s": true,
}

// Two-letter words that are a subject: Lp(a).
var sectionShortWords = map[string]bool{"lp": true}

// sectionNameWords returns the subject words of a page name, longest first.
func sectionNameWords(name string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if sectionStopWords[w] || seen[w] || (len(w) < 3 && !unicode.IsDigit(rune(w[0])) && !sectionShortWords[w]) {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// sectionQueries returns the tsquery strings to try in order: every subject
// word, then the two longest when there are more than two.
func sectionQueries(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	qs := []string{strings.Join(words, " & ")}
	if len(words) > 2 {
		qs = append(qs, words[0]+" & "+words[1])
	}
	return qs
}

type sectionGlossTerm struct{ Term, Slug, Line string }
type sectionCompany struct{ Name, UID string }

// sectionRelatedIndex holds what every page matches its text against.
type sectionRelatedIndex struct {
	terms []sectionGlossTerm
	cos   []sectionCompany
}

func sectionLoadIndex(db *sql.DB) sectionRelatedIndex {
	var ix sectionRelatedIndex
	var raw string
	if db.QueryRow(`SELECT data::text FROM site_content WHERE path = 'resources/glossary.json'`).Scan(&raw) == nil {
		var g struct {
			Terms []struct{ Term, Slug, Definition string } `json:"terms"`
		}
		if json.Unmarshal([]byte(raw), &g) == nil {
			for _, t := range g.Terms {
				if t.Term != "" && t.Slug != "" {
					ix.terms = append(ix.terms, sectionGlossTerm{t.Term, t.Slug, twoaiOneLine(t.Definition)})
				}
			}
		}
	}
	// Multi-word terms first, so "Large Language Model" is found before "Model".
	sort.SliceStable(ix.terms, func(i, j int) bool { return len(ix.terms[i].Term) > len(ix.terms[j].Term) })
	if rows, err := db.Query(`SELECT c.name, c.uid FROM twoai_company_profiles c
		WHERE length(c.name) >= 4 AND EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = 'companies/' || c.uid || '.json')`); err == nil {
		for rows.Next() {
			var c sectionCompany
			if rows.Scan(&c.Name, &c.UID) == nil {
				ix.cos = append(ix.cos, c)
			}
		}
		rows.Close()
	}
	return ix
}

var sectionParenRe = regexp.MustCompile(`\s*\(([^)]*)\)`)

// sectionTermsIn returns the glossary terms the text names, at most max.
func sectionTermsIn(text string, terms []sectionGlossTerm, max int) []map[string]string {
	lower := strings.ToLower(text)
	var out []map[string]string
	for _, t := range terms {
		base := strings.TrimSpace(sectionParenRe.ReplaceAllString(t.Term, ""))
		hit := len(base) > 3 && sectionHasWord(lower, strings.ToLower(base))
		if !hit {
			if m := sectionParenRe.FindStringSubmatch(t.Term); m != nil && len(m[1]) >= 2 && !strings.Contains(m[1], " ") {
				hit = sectionHasWord(text, m[1])
			}
		}
		if hit {
			out = append(out, map[string]string{"term": t.Term, "line": t.Line, "path": "/ai-glossary/" + t.Slug + "/"})
			if len(out) == max {
				break
			}
		}
	}
	return out
}

// sectionCompaniesIn returns the companies the text names, matched by case.
func sectionCompaniesIn(text string, cos []sectionCompany, max int) []map[string]string {
	var out []map[string]string
	seen := map[string]bool{}
	for _, c := range cos {
		if seen[c.UID] || !sectionHasWord(text, c.Name) {
			continue
		}
		seen[c.UID] = true
		out = append(out, map[string]string{"name": c.Name, "path": "/companies/" + c.UID + "/"})
		if len(out) == max {
			break
		}
	}
	return out
}

// sectionHasWord reports whether w appears in s with no letter or digit
// directly either side.
func sectionHasWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(w)
		before := a == 0 || !sectionIsWordByte(s[a-1])
		after := b == len(s) || !sectionIsWordByte(s[b])
		if before && after {
			return true
		}
		i = a + 1
	}
}

func sectionIsWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// sectionRelated gathers the related records for one page. selfPath is the
// page's live path, whose own placed facts the page already shows.
func sectionRelated(db *sql.DB, ix sectionRelatedIndex, p sectionPage, selfPath, text string) map[string]any {
	rel := map[string]any{}
	if t := sectionTermsIn(text, ix.terms, 10); len(t) > 0 {
		rel["terms"] = t
	}
	if c := sectionCompaniesIn(text, ix.cos, 10); len(c) > 0 {
		rel["companies"] = c
	}
	domain, ok := sectionDomainWords[p.Section]
	if !ok {
		return rel
	}
	words := sectionNameWords(p.Name)
	qs := sectionQueries(words)
	if len(qs) == 0 {
		// A root page named only by its section: its domain is its subject.
		qs = []string{domain}
	}
	// first runs q with each subject query in turn until one returns rows.
	// Only facts, which are about this domain already, may fall back to the
	// two longest words; elsewhere a loose match found stock tips for "Cloud
	// Provider Deals", and an empty list is better than a wrong one.
	first := func(q string, scan func(*sql.Rows) map[string]any, loose bool) []map[string]any {
		tries := qs
		if !loose && len(tries) > 1 {
			tries = tries[:1]
		}
		for _, s := range tries {
			rows, err := db.Query(q, s, domain, selfPath)
			if err != nil {
				return nil
			}
			var out []map[string]any
			for rows.Next() {
				if m := scan(rows); m != nil {
					out = append(out, m)
				}
			}
			rows.Close()
			if len(out) > 0 {
				return out
			}
		}
		return nil
	}
	if news := first(`SELECT uid, headline, COALESCE(published_on::text,'') FROM (
			SELECT s.uid, s.headline, s.published_on FROM twoai_news_stories s
			WHERE s.retired_at IS NULL AND s.uid IS NOT NULL AND COALESCE(s.headline,'') <> ''
			  AND to_tsvector('english', s.headline || ' ' || COALESCE(s.story->>'summary', s.story->>'Summary', ''))
			      @@ (to_tsquery('english', $1) && to_tsquery('english', $2))
			UNION
			SELECT s.uid, s.headline, s.published_on FROM twoai_page_news pn JOIN twoai_news_stories s ON s.uid = pn.story_uid
			WHERE pn.active AND pn.page_uid = reverse(split_part(reverse(rtrim($3, '/')), '/', 1)) AND s.retired_at IS NULL) x
		ORDER BY published_on DESC NULLS LAST LIMIT 5`, func(r *sql.Rows) map[string]any {
		var uid, h, d string
		if r.Scan(&uid, &h, &d) != nil {
			return nil
		}
		return map[string]any{"headline": h, "path": "/ai-news/" + uid + "/", "date": d}
	}, false); len(news) > 0 {
		rel["news"] = news
	}
	if facts := first(`SELECT claim, source_url, COALESCE(source_title,''), COALESCE(source_date::text,'') FROM twoai_sourced_facts
		WHERE status = 'live' AND target_path <> $3
		  AND to_tsvector('english', claim) @@ (to_tsquery('english', $1) && to_tsquery('english', $2))
		ORDER BY source_date DESC NULLS LAST, id LIMIT 5`, func(r *sql.Rows) map[string]any {
		var c, u, t, d string
		if r.Scan(&c, &u, &t, &d) != nil {
			return nil
		}
		return map[string]any{"claim": c, "source_url": u, "source_title": t, "date": d}
	}, true); len(facts) > 0 {
		rel["facts"] = facts
	}
	if sectionHealthLibrary[p.Section] {
		// The medical trees draw on the research library theworldofai keeps for
		// them (row 485), by sub-hub, most cited first: titles and DOIs only,
		// the abstracts stay internal. twoai_works is about AI and gave the
		// Lp(a) pages papers on AI in cardiology.
		sub := strings.TrimPrefix(p.Slug, p.Section+"-")
		if p.Kind != "subhub" && p.Kind != "topic" {
			sub = ""
		}
		if rows, err := db.Query(`SELECT title, COALESCE(year,0), COALESCE(citations,0), doi FROM twoai_health_papers
			WHERE topic = $1 AND ($2 = '' OR subtopic = $2) AND COALESCE(title,'') <> '' AND COALESCE(status,'') <> 'rejected'
			ORDER BY citations DESC NULLS LAST, year DESC NULLS LAST LIMIT 5`, p.Section, sub); err == nil {
			var papers []map[string]any
			for rows.Next() {
				var title, doi string
				var year, cited int
				if rows.Scan(&title, &year, &cited, &doi) == nil {
					papers = append(papers, map[string]any{"title": title, "year": year, "cited_by": cited, "url": "https://doi.org/" + doi})
				}
			}
			rows.Close()
			if len(papers) > 0 {
				rel["papers"] = papers
				rel["papers_from_library"] = true
			}
		}
	} else if len(words) > 0 {
		// Title must carry the subject and a word of the domain; the abstract
		// alone found aerospace maintenance for "Predictive Maintenance".
		if papers := first(`SELECT title, COALESCE(pub_year,0), COALESCE(cited_by,0), COALESCE(doi,''), COALESCE(oa_url,''), openalex_id
			FROM twoai_works
			WHERE to_tsvector('english', COALESCE(title,'') || ' ' || COALESCE(abstract,'')) @@ (to_tsquery('english', $1) && to_tsquery('english', $2))
			  AND to_tsvector('english', COALESCE(title,'')) @@ (to_tsquery('english', $1) && to_tsquery('english', $2))
			  AND duplicate_of IS NULL AND excluded_reason IS NULL AND $3 <> ''
			ORDER BY cited_by DESC NULLS LAST LIMIT 5`, func(r *sql.Rows) map[string]any {
			var title, doi, oa, oid string
			var year, cited int
			if r.Scan(&title, &year, &cited, &doi, &oa, &oid) != nil {
				return nil
			}
			u := oa
			if doi != "" {
				u = "https://doi.org/" + doi
			} else if u == "" {
				u = "https://openalex.org/" + strings.TrimPrefix(oid, "https://openalex.org/")
			}
			return map[string]any{"title": title, "year": year, "cited_by": cited, "url": u}
		}, false); len(papers) > 0 {
			rel["papers"] = papers
		}
	}
	if suits := first(`SELECT slug, case_name, COALESCE(status,'') FROM ai_lawsuits
		WHERE is_active AND COALESCE(slug,'') <> '' AND $3 <> ''
		  AND to_tsvector('english', case_name || ' ' || COALESCE(summary,'') || ' ' || COALESCE(executive_summary,''))
		      @@ (to_tsquery('english', $1) && to_tsquery('english', $2))
		ORDER BY filed_date DESC NULLS LAST LIMIT 5`, func(r *sql.Rows) map[string]any {
		var slug, name, st string
		if r.Scan(&slug, &name, &st) != nil {
			return nil
		}
		return map[string]any{"name": name, "path": "/ai-lawsuits/" + slug + "/", "status": st}
	}, false); len(suits) > 0 {
		rel["lawsuits"] = suits
	}
	if cves := first(`SELECT cve_id, COALESCE(NULLIF(headline,''), product, cve_id) FROM twoai_cves
		WHERE status IN ('published','approved') AND $3 <> ''
		  AND to_tsvector('english', COALESCE(description,'') || ' ' || COALESCE(product,'') || ' ' || COALESCE(headline,''))
		      @@ (to_tsquery('english', $1) && to_tsquery('english', $2))
		ORDER BY published DESC NULLS LAST LIMIT 5`, func(r *sql.Rows) map[string]any {
		var id, h string
		if r.Scan(&id, &h) != nil {
			return nil
		}
		return map[string]any{"id": id, "headline": h, "path": "/ai-news/cves/" + id + "/"}
	}, false); len(cves) > 0 {
		rel["cves"] = cves
	}
	return rel
}
