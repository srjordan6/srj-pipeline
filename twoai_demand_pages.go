package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// PAGES ON DEMAND. Stephen, 2026-09-03: whenever the ask box cites a paper,
// the site should spin up a page on that paper covering the topic in depth.
//
// The Worker records every paper an answer actually names in
// twoai_ask_cited_works: DOI or open-access link, title, the question that
// pulled it. This stage reads those records, and for every cited work that
// has no page on the curated shelf yet, promotes it into twoai_research_papers
// from the row twoai_works already holds - title, authors, year, venue,
// citation count, DOI, and the abstract, which OpenAlex supplies for 964,000
// of 1.38 million works. twoaiPaperExplain then writes the three explanations
// from that abstract on the same run, at no API cost, because the abstract is
// local; and twoaiResearch renders the page. A reader who asked tonight finds
// the page tomorrow; the next reader with the same question finds it linked
// in the answer.
//
// The promoted paper lands under the topic asked-by-readers, which is a real
// topic page like the ten curated ones, so the shelf says plainly where these
// came from. our_note records that a reader's question summoned it, in the
// site's own words rather than the reader's, since a question is a reader's
// text.
//
// Capped per run, and a work is promoted once: the demand row is stamped
// promoted_at and carries the page uid, so the same paper cited a hundred
// times produces one page and a count, never a hundred pages.
func twoaiDemandPages(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT d.url, COALESCE(d.doi,''), d.title, count(*) AS asks, min(d.question_norm)
		FROM twoai_ask_cited_works d
		WHERE d.promoted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM twoai_research_papers p
		                   WHERE (d.doi <> '' AND lower(p.doi) = lower(d.doi)) OR p.url = d.url)
		GROUP BY d.url, d.doi, d.title
		ORDER BY count(*) DESC, min(d.cited_at)
		LIMIT 20`)
	if err != nil {
		return err
	}
	type cand struct {
		url, doi, title, q string
		asks              int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.url, &c.doi, &c.title, &c.asks, &c.q) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	promoted, skipped := 0, 0
	for _, c := range cands {
		// The work itself, from the local mirror. Match on DOI when there is
		// one, else on the open-access link the answer used.
		var title, abstract, journal, doi, oaURL, workType, authorsJSON string
		var year, cited sql.NullInt64
		q := `SELECT COALESCE(title,''), COALESCE(abstract,''), COALESCE(doi,''), COALESCE(oa_url,''),
		             COALESCE(work_type,''), COALESCE(authors::text,'[]'), pub_year, cited_by
		      FROM twoai_works WHERE `
		var arg string
		if c.doi != "" {
			q += `doi = $1`
			arg = strings.ToLower(c.doi)
		} else {
			q += `oa_url = $1`
			arg = c.url
		}
		if err := db.QueryRow(q+` LIMIT 1`, arg).Scan(&title, &abstract, &doi, &oaURL, &workType, &authorsJSON, &year, &cited); err != nil {
			skipped++
			db.Exec(`UPDATE twoai_ask_cited_works SET promoted_at = now(), page_uid = 'not-in-mirror' WHERE url = $1 AND promoted_at IS NULL`, c.url)
			continue
		}
		if title == "" {
			title = c.title
		}
		// Authors as "A, B, C" from the JSON the mirror holds.
		var al []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(authorsJSON), &al)
		var names []string
		for _, a := range al {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		authors := strings.Join(names, ", ")

		uid := "q" + twoaiUID("demand:"+c.url)[:7]
		link := c.url
		if doi != "" {
			link = "https://doi.org/" + doi
		}
		absSrc := ""
		if abstract != "" {
			absSrc = "openalex"
		}
		note := fmt.Sprintf("Added because a reader's question on this site drew on it (%d time%s so far). The page exists so the next reader finds it in the answer.",
			c.asks, map[bool]string{true: "", false: "s"}[c.asks == 1])
		if _, err := db.Exec(`INSERT INTO twoai_research_papers
				(uid, title, authors, year, journal, citations, url, topic, our_note, source, added_on, abstract, abstract_source, doi, paper_type, openalex_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'asked-by-readers',$8,'ask-box demand',current_date,$9,$10,$11,$12,
			        (SELECT openalex_id FROM twoai_works WHERE doi = $11 OR oa_url = $7 LIMIT 1))
			ON CONFLICT (uid) DO NOTHING`,
			uid, title, authors, nullInt(year), journal, nullInt(cited), link, note,
			nullStr(abstract), nullStr(absSrc), nullStr(strings.ToLower(doi)), nullStr(workType)); err != nil {
			fmt.Println("twoai_demand_pages:", uid, err)
			skipped++
			continue
		}
		db.Exec(`UPDATE twoai_ask_cited_works SET promoted_at = now(), page_uid = $2 WHERE url = $1 AND promoted_at IS NULL`, c.url, uid)
		promoted++
	}
	// Demand on papers that already have a page: stamp it so the count is
	// visible and the row stops being a candidate.
	db.Exec(`UPDATE twoai_ask_cited_works d SET promoted_at = now(), page_uid = p.uid
		FROM twoai_research_papers p
		WHERE d.promoted_at IS NULL AND ((d.doi <> '' AND lower(p.doi) = lower(d.doi)) OR p.url = d.url)`)

	var pending int
	db.QueryRow(`SELECT count(DISTINCT url) FROM twoai_ask_cited_works WHERE promoted_at IS NULL`).Scan(&pending)
	fmt.Printf("twoai_demand_pages: promoted=%d skipped=%d pending=%d\n", promoted, skipped, pending)
	return nil
}

func nullInt(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
