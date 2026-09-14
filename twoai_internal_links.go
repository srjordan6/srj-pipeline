package main

// twoai_internal_links: when a page names something this site covers, link to it.
//
// Stephen, 2026-09-13, reading the Industry Use Cases hub: all of this
// contains links to other pages instead of links to our pages.
//
// He is right, and the measurement is stark. Of 159 industry points, 150 link
// straight off the site and 9 link inward. The prose names things this site
// has its own page for - Fed SR 11-7 model risk, FHFA and CFPB valuation
// rules, training-data litigation, NIST standards work, AI bills of material,
// automated valuation models - and every one of those mentions is either
// plain text or a link to somebody else's website.
//
// DOMAIN MATCHING DOES NOT SOLVE THIS, and trying it first is what showed
// why. Only 1 of the 127 distinct external hosts cited by those points has a
// company page here: the sources are trade bodies, regulators and research
// firms (A3, USDA ERS, AICPA, NAIC, EPRI, Skift), not the companies in the
// directory. The link that belongs on these pages is not "we also have a page
// about your source". It is "we have a page about the THING this point is
// discussing".
//
// So the match is on SUBJECT, drawn from the page inventory this site already
// publishes: compliance frameworks, glossary terms, companies, lawsuits and
// tools. A point whose text names one gets a "More on this site" line
// alongside its source link. The outbound source stays exactly where it is,
// because the point is still sourced to whoever published it.
//
// WHAT THIS DELIBERATELY WILL NOT DO:
//
//   - Guess. A match must be a whole-phrase, case-insensitive hit on a title
//     or a registered alias. No stemming, no fuzzy distance, no "close
//     enough". A wrong internal link is worse than none: it tells a reader
//     this site claims a connection it cannot support.
//   - Match short or generic titles. Anything under 5 characters, and any
//     title that is an ordinary English phrase, is skipped. "AI" and "Media"
//     would otherwise match every page on the site.
//   - Link a page to itself, or a point to the page it already cites.
//   - Bury the prose. Three links per point at most, chosen by title length,
//     because the longest matching title is the most specific one.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Titles that are ordinary words, or so short that a substring hit means
// nothing. Kept explicit rather than clever: a list somebody can read and
// correct beats a heuristic nobody can predict.
var linkStopTitles = map[string]bool{
	"ai": true, "media": true, "energy": true, "legal": true, "banking": true,
	"education": true, "government": true, "insurance": true, "retail": true,
	"mining": true, "defense": true, "sports": true, "agriculture": true,
	"healthcare": true, "manufacturing": true, "construction": true,
	"accounting": true, "hospitality": true, "transportation": true,
	"real estate": true, "nonprofits": true, "training": true, "data": true,
	"model": true, "models": true, "agent": true, "agents": true, "token": true,
	"prompt": true, "context": true, "chat": true, "search": true, "vision": true,
	"safety": true, "alignment": true, "benchmark": true, "inference": true,
}

type linkTarget struct {
	title string
	url   string
	kind  string
	lower string
}

// longerProperName reports whether the match sits inside a longer capitalised
// name in the original text - "AI Index" inside "Evident AI Index". Looks at
// the word immediately before the match: if it is capitalised and is not a
// sentence opener or an ordinary article, the match is part of somebody
// else's name.
func longerProperName(raw, title string) bool {
	idx := strings.Index(strings.ToLower(raw), strings.ToLower(title))
	for idx > 0 {
		before := strings.TrimRight(raw[:idx], " ")
		if before == "" {
			return false
		}
		// Sentence boundary immediately before: not a longer name.
		if strings.HasSuffix(before, ".") || strings.HasSuffix(before, ",") ||
			strings.HasSuffix(before, ":") || strings.HasSuffix(before, ";") {
			return false
		}
		fields := strings.Fields(before)
		if len(fields) == 0 {
			return false
		}
		w := fields[len(fields)-1]
		if w == "" {
			return false
		}
		lw := strings.ToLower(w)
		if lw == "the" || lw == "a" || lw == "an" || lw == "and" || lw == "of" || lw == "in" || lw == "on" {
			return false
		}
		r := rune(w[0])
		return r >= 'A' && r <= 'Z'
	}
	return false
}

func twoaiInternalLinks(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_internal_links (
		page_path text NOT NULL,
		point_name text NOT NULL,
		target_url text NOT NULL,
		target_title text NOT NULL,
		target_kind text NOT NULL,
		matched_on text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (page_path, point_name, target_url))`); err != nil {
		return err
	}

	// ---- the inventory of things we can link TO -----------------------------
	var targets []linkTarget
	add := func(title, url, kind string) {
		t := strings.TrimSpace(title)
		l := strings.ToLower(t)
		if len([]rune(t)) < 5 || linkStopTitles[l] || url == "" {
			return
		}
		// A GENERIC GLOSSARY TERM IS NOT A USEFUL LINK. The first run linked
		// "Throughput" from a mining point about mill optimisation,
		// "Streaming" from high-school sports, and "Personalization" from
		// hotel inventory. Each match was technically correct and told the
		// reader nothing they wanted. A glossary link earns its place when the
		// term is specific enough that seeing it named IS the reason to
		// follow it: multi-word terms, or acronyms that are unambiguous.
		// Frameworks, companies and lawsuits are specific by nature and are
		// not subject to this.
		if kind == "glossary" && !strings.Contains(l, " ") && !strings.Contains(l, "-") {
			return
		}
		targets = append(targets, linkTarget{title: t, url: url, kind: kind, lower: l})
	}

	// Compliance frameworks: the highest-value internal link on an industry
	// page, because a sector point about model risk or valuation rules is
	// exactly what these pages explain.
	rows, err := db.Query(`SELECT COALESCE(data->>'title',''), path FROM twoai_pages WHERE kind='compliance'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t, p string
		if rows.Scan(&t, &p) == nil {
			add(t, "/ai-compliance/"+strings.TrimSuffix(strings.TrimPrefix(p, "compliance/"), ".json")+"/", "compliance")
		}
	}
	rows.Close()

	// Glossary terms, from the single glossary document.
	var gl []byte
	db.QueryRow(`SELECT data::text FROM twoai_pages WHERE kind='glossary' LIMIT 1`).Scan(&gl)
	if len(gl) > 0 {
		var g struct {
			Terms []struct {
				Term string `json:"term"`
				Slug string `json:"slug"`
			} `json:"terms"`
		}
		if json.Unmarshal(gl, &g) == nil {
			for _, t := range g.Terms {
				if t.Slug != "" {
					add(t.Term, "/ai-glossary/"+t.Slug+"/", "glossary")
				}
			}
		}
	}

	// Companies, by their registered name and any alias. An alias is how
	// "Alphabet" reaches the Google page.
	crows, err := db.Query(`SELECT p.name, p.uid, COALESCE(e.aliases,'[]'::jsonb)
		FROM twoai_company_profiles p LEFT JOIN twoai_entities e ON e.uid = p.uid`)
	if err == nil {
		for crows.Next() {
			var n, uid string
			var al []byte
			if crows.Scan(&n, &uid, &al) != nil {
				continue
			}
			add(n, "/companies/"+uid+"/", "company")
			var aliases []string
			if json.Unmarshal(al, &aliases) == nil {
				for _, a := range aliases {
					add(a, "/companies/"+uid+"/", "company")
				}
			}
		}
		crows.Close()
	}

	// Lawsuits, by case name.
	lrows, err := db.Query(`SELECT COALESCE(data->>'case_name', data->>'title',''), path
		FROM twoai_pages WHERE kind='lawsuit'`)
	if err == nil {
		for lrows.Next() {
			var t, p string
			if lrows.Scan(&t, &p) == nil {
				add(t, "/ai-lawsuits/"+strings.TrimSuffix(strings.TrimPrefix(p, "lawsuits/"), ".json")+"/", "lawsuit")
			}
		}
		lrows.Close()
	}

	// Longest title first, and within equal lengths the more specific kind
	// first, so a compliance page beats a glossary term describing the same
	// thing - the first run emitted both "EU AI Act" links on one point.
	rank := map[string]int{"compliance": 0, "lawsuit": 1, "company": 2, "glossary": 3}
	sort.Slice(targets, func(i, j int) bool {
		if len(targets[i].title) != len(targets[j].title) {
			return len(targets[i].title) > len(targets[j].title)
		}
		return rank[targets[i].kind] < rank[targets[j].kind]
	})
	fmt.Printf("twoai_internal_links: %d linkable subjects in the inventory\n", len(targets))

	// ---- match them against every industry point ---------------------------
	prows, err := db.Query(`SELECT path, data->'points' FROM twoai_pages WHERE path LIKE 'industries/%'`)
	if err != nil {
		return err
	}
	type page struct {
		path   string
		points []byte
	}
	var pages []page
	for prows.Next() {
		var p page
		if prows.Scan(&p.path, &p.points) == nil {
			pages = append(pages, p)
		}
	}
	prows.Close()

	db.Exec(`DELETE FROM twoai_internal_links WHERE page_path LIKE 'industries/%'`)
	linked, pointsWith := 0, 0
	for _, pg := range pages {
		var pts []struct {
			Name   string `json:"name"`
			Desc   string `json:"desc"`
			Source string `json:"source"`
		}
		if json.Unmarshal(pg.points, &pts) != nil {
			continue
		}
		for _, pt := range pts {
			hay := strings.ToLower(pt.Name + " " + pt.Desc)
			raw := pt.Name + " " + pt.Desc
			var hits []linkTarget
			used := map[string]bool{}
			usedTitle := map[string]bool{}
			for _, t := range targets {
				if len(hits) >= 3 {
					break
				}
				if used[t.url] || usedTitle[t.lower] || strings.Contains(pt.Source, t.url) {
					continue
				}
				// Whole-phrase only: bounded by a non-letter on each side, so
				// "IMA" does not match "primary" and "Candid" does not match
				// "candidate".
				re := regexp.MustCompile(`(^|[^a-z0-9])` + regexp.QuoteMeta(t.lower) + `([^a-z0-9]|$)`)
				if !re.MatchString(hay) {
					continue
				}
				// A MATCH INSIDE A LONGER PROPER NAME IS A DIFFERENT THING.
				// The first run linked "The Evident AI Index benchmarks
				// insurers" to Stanford HAI, because "AI Index" is one of
				// Stanford's aliases and sits inside "Evident AI Index". They
				// are different organisations, and that link would have told a
				// reader otherwise on a page about insurance benchmarking.
				// If the matched phrase is preceded by another capitalised
				// word in the original text, it is part of a longer name and
				// this is not our subject.
				if longerProperName(raw, t.title) {
					continue
				}
				used[t.url] = true
				usedTitle[t.lower] = true
				hits = append(hits, t)
			}
			if len(hits) == 0 {
				continue
			}
			pointsWith++
			for _, h := range hits {
				db.Exec(`INSERT INTO twoai_internal_links
					(page_path, point_name, target_url, target_title, target_kind, matched_on)
					VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
					pg.path, pt.Name, h.url, h.title, h.kind, h.lower)
				linked++
			}
		}
	}
	fmt.Printf("twoai_internal_links: %d links across %d points on %d industry pages\n",
		linked, pointsWith, len(pages))
	if linked == 0 {
		fmt.Println("twoai_internal_links: nothing matched. That is a real answer, not a failure: it means the points do not name subjects this site has pages for, and the honest fix is more pages, not looser matching.")
	}
	return nil
}
