package main

import (
	"database/sql"
	"fmt"
)

// twoaiDownloads renders the Downloads and Asset Repository from twoai_downloads.
//
// WHAT THIS SECTION IS. Six categories of working document: white papers,
// checklists, policy templates, governance documents, audit workpapers, and
// procurement scorecards. Unlike every other factory on this site, nothing here
// is harvested. Each item is authored, because there is no public register of
// governance templates to ingest and inventing one would be fabrication.
//
// WHY IT STAYED EMPTY UNTIL NOW. The six sections have sat at status='planned'
// since the taxonomy was built, and were deliberately not filled: the documents
// table is empty and book_assets holds book files only, so there was no backing
// data, and the six categories are the subject matter of the nine books SRJ
// sells. Publishing them was a pricing decision rather than a build decision.
// Stephen settled it on 2026-08-18: publish everything, and link to
// srjconsultingservices.com wherever a template is built on a framework from
// the books.
//
// THE CROSS-LINK RULE. srjconsultingservices.com is never a content source for
// theworldofai.org. It may be linked only to send a reader to an SRJ service or
// book. based_on names the framework a template derives from and based_on_url
// points at the books page, which is exactly that permitted case and nothing
// wider. No book text is reproduced: each template turns a framework already
// public as a glossary definition into a document someone can fill in.
func twoaiDownloads(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type item struct {
		Slug       string `json:"slug"`
		Section    string `json:"section"`
		Title      string `json:"title"`
		Summary    string `json:"summary"`
		Audience   string `json:"audience,omitempty"`
		BasedOn    string `json:"based_on,omitempty"`
		BasedOnURL string `json:"based_on_url,omitempty"`
		Body       string `json:"body_md"`
		Reviewed   string `json:"reviewed_on"`
	}
	type section struct {
		Slug  string `json:"slug"`
		Name  string `json:"name"`
		Blurb string `json:"blurb"`
		Items []item `json:"items"`
	}

	// Section names come from the taxonomy so the two cannot drift apart.
	names := map[string]string{}
	blurbs := map[string]string{}
	order := []string{}
	if rows, err := db.Query(`SELECT slug, name, COALESCE(blurb,'') FROM twoai_taxonomy
		WHERE parent_slug='downloads' ORDER BY sort, name`); err == nil {
		for rows.Next() {
			var s, n, b string
			if rows.Scan(&s, &n, &b) == nil {
				names[s], blurbs[s] = n, b
				order = append(order, s)
			}
		}
		rows.Close()
	}
	if len(order) == 0 {
		fmt.Printf("twoai_build: downloads sections=0 items=0 ok=true (no taxonomy rows)\n")
		return 0, nil
	}

	bySection := map[string][]item{}
	rows, err := db.Query(`SELECT slug, section_slug, title, summary, COALESCE(audience,''),
			COALESCE(based_on,''), COALESCE(based_on_url,''), body_md, reviewed_on::text
		FROM twoai_downloads WHERE status='live' ORDER BY section_slug, sort, title`)
	if err != nil {
		return 0, err
	}
	total := 0
	for rows.Next() {
		var it item
		if rows.Scan(&it.Slug, &it.Section, &it.Title, &it.Summary, &it.Audience,
			&it.BasedOn, &it.BasedOnURL, &it.Body, &it.Reviewed) != nil {
			continue
		}
		bySection[it.Section] = append(bySection[it.Section], it)
		total++
	}
	rows.Close()

	// One page per item, at a stable path that never moves once published.
	pages := 0
	for _, items := range bySection {
		for _, it := range items {
			if err := upsert("downloads/"+it.Slug+".json", "download", map[string]any{
				"uid":       twoaiUID("download:" + it.Slug),
				"item":      it,
				"section":   names[it.Section],
				"generated": today,
			}); err != nil {
				return pages, err
			}
			pages++
		}
	}

	// The hub lists every section, including the ones with nothing in them yet,
	// stated as empty rather than hidden. A section quietly omitted reads as a
	// section that does not exist.
	secs := []section{}
	for _, s := range order {
		secs = append(secs, section{Slug: s, Name: names[s], Blurb: blurbs[s], Items: bySection[s]})
	}
	if err := upsert("downloads/index.json", "downloads-hub", map[string]any{
		"uid":       twoaiUID("section:downloads-index"),
		"sections":  secs,
		"total":     total,
		"generated": today,
	}); err != nil {
		return pages, err
	}
	pages++

	// Taxonomy status follows what is actually published: a section with items
	// goes live, one without stays planned. Marking a section live while it is
	// empty is how the category pages ended up advertising pages that were not
	// there.
	for _, s := range order {
		st := "planned"
		lp := sql.NullString{}
		if len(bySection[s]) > 0 {
			st, lp = "live", sql.NullString{String: "/downloads/#" + s, Valid: true}
		}
		db.Exec(`UPDATE twoai_taxonomy SET status=$2, live_path=$3, updated_at=now()
			WHERE slug=$1`, s, st, lp)
	}
	if total > 0 {
		db.Exec(`UPDATE twoai_taxonomy SET status='live', live_path='/downloads/', updated_at=now()
			WHERE slug='downloads'`)
	}

	fmt.Printf("twoai_build: downloads sections=%d items=%d pages=%d ok=true\n",
		len(order), total, pages)
	return pages, nil
}
