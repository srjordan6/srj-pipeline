package main

// twoai_hub_pages: a landing page for every level-2 hub that lacks one.
//
// A level-2 taxonomy row is a hub: a named group of sections under a
// category. The category page links to it, and a reader expects to land on
// something that lists what is inside. Hubs built by the pipeline - AI
// Security and Risk, Industry Use Cases - carry a page and a live_path. Hubs
// added by hand in SQL, or whose page was never generated, carried neither,
// and the category page silently sent the reader to the first child instead.
// On 2026-09-17 eleven hubs were in that state.
//
// This generates the missing page from the taxonomy: the hub's name and
// blurb, and a children list of its sections that have paths. The page is
// rewritten every run so it follows the taxonomy rather than drifting from
// it, and the taxonomy row learns the path once, derived from the slug the
// same way every other generated uid on this site is, so it never changes.
//
// It does not touch a hub that already has a live_path. A hub the pipeline
// or Stephen built deliberately keeps its own page.
//
// WRITTEN WHERE THE ROUTE LOOKS, which the first version was not. The nine
// hub pages generated on 2026-09-17 were emitted to industries/<slug>.json.
// The category page duly linked to /ai-ecosystem/<parent>/<uid>/, and every
// one of those nine links 404'd for two days, because the uid routes build
// from a fixed list of folders each and industries/ is in none of them: the
// technology route reads models, repos, status and tech; the entities route
// reads people, companies, observatory and jobs. The link audit found all
// nine on 2026-09-19 and they were the entire broken-link count for the site.
//
// ecosystem/<slug>.json with kind 'ecosystem-section' is the one shape all
// three category routes already render, through the branch that reads
// content/ecosystem and matches on the document's own 'category'. So the hub
// page now declares its category and renders as a section: a title, an answer
// paragraph, and the children under "What is built". Nothing in the templates
// had to change.
//
// The old industries/<slug>.json rows are left in place. They are not linked,
// they publish to a folder whose route ignores them, and this site does not
// delete data.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

func twoaiEnsureHubPages(db *sql.DB) error {
	// A hub already given a live_path by an earlier run still needs its page
	// written, because the first version wrote it to a folder no route reads.
	// The guard is the ecosystem page's absence, not the live_path's.
	//
	// AND ONLY WHEN NOTHING ELSE ALREADY OWNS THAT UID. Of the thirteen hubs
	// this query first matched, four resolve 200 today: AI Insurance,
	// Calculators, Law and Compliance, AI Litigation. Their uid is served by a
	// page the pipeline builds elsewhere. Writing a second file under the same
	// uid would give getStaticPaths two entries for one route, and Astro breaks
	// that tie by directory order, which is exactly how the research topic
	// files collided in e466cca. So a hub is only generated when no other
	// document carries its uid.
	rows, err := db.Query(`
		SELECT t.slug, t.name, COALESCE(t.blurb,''), COALESCE(t.parent_slug,'')
		FROM twoai_taxonomy t
		WHERE t.level = 2 AND COALESCE(t.status,'') <> 'retired'
		  AND (t.live_path IS NULL
		       OR (t.live_path LIKE '/ai-ecosystem/%'
		           AND NOT EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = 'ecosystem/' || t.slug || '.json')
		           AND EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = 'industries/' || t.slug || '.json' AND p.data->>'is_hub' = 'true')
		           AND NOT EXISTS (
		               SELECT 1 FROM twoai_pages p
		               WHERE p.data->>'uid' = encode(substring(sha256(('section:' || t.slug)::bytea) from 1 for 4), 'hex')
		                 AND p.path <> 'industries/' || t.slug || '.json')))
		  AND EXISTS (SELECT 1 FROM twoai_taxonomy c WHERE c.parent_slug = t.slug AND c.live_path IS NOT NULL)`)
	if err != nil {
		return err
	}
	type hub struct{ slug, name, blurb, parent string }
	var hubs []hub
	for rows.Next() {
		var h hub
		if rows.Scan(&h.slug, &h.name, &h.blurb, &h.parent) == nil {
			hubs = append(hubs, h)
		}
	}
	rows.Close()
	if len(hubs) == 0 {
		return nil
	}
	today := time.Now().UTC().Format("2006-01-02")
	made := 0
	for _, h := range hubs {
		h8 := sha256.Sum256([]byte("section:" + h.slug))
		uid := hex.EncodeToString(h8[:4])
		base := "/ai-ecosystem/" + h.parent + "/"
		path := base + uid + "/"
		summary := h.blurb
		if summary == "" {
			summary = h.name + " on The World of AI: the sections below are the ways in."
		}
		if _, err := db.Exec(`
			INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
			VALUES ($1, 'ecosystem-section', $2, jsonb_build_object(
				'uid', $3::text, 'page_uid', $3::text, 'slug', $2::text,
				'category', $7::text, 'title', $4::text, 'name', $4::text,
				'tax', $2::text, 'is_hub', true,
				'generated', $5::text, 'verified', $5::text,
				'answer', $6::text, 'summary', $6::text, 'blurb', $6::text,
				-- 'built' is what the section template renders as a list, so the
				-- sections inside this hub appear as its contents. 'children'
				-- carries the same rows with their hrefs for anything that wants
				-- the links themselves.
				'built', (SELECT COALESCE(jsonb_agg(jsonb_build_object(
					'name', c.name, 'detail', COALESCE(NULLIF(c.blurb,''), c.name)) ORDER BY c.sort), '[]'::jsonb)
					FROM twoai_taxonomy c WHERE c.parent_slug = $2 AND c.live_path IS NOT NULL AND COALESCE(c.status,'') <> 'retired'),
				'children', (SELECT COALESCE(jsonb_agg(jsonb_build_object(
					'name', c.name, 'href', c.live_path, 'desc', COALESCE(c.blurb,''), 'sort', c.sort) ORDER BY c.sort), '[]'::jsonb)
					FROM twoai_taxonomy c WHERE c.parent_slug = $2 AND c.live_path IS NOT NULL AND COALESCE(c.status,'') <> 'retired'),
				'points', '[]'::jsonb, 'total', 0), now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, data = EXCLUDED.data,
				taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()`,
			"ecosystem/"+h.slug+".json", h.slug, uid, h.name, today, summary, h.parent); err != nil {
			return fmt.Errorf("%s: %w", h.slug, err)
		}
		if _, err := db.Exec(`UPDATE twoai_taxonomy SET live_path = $2, updated_at = now() WHERE slug = $1 AND live_path IS NULL`,
			h.slug, path); err != nil {
			return fmt.Errorf("%s path: %w", h.slug, err)
		}
		made++
		fmt.Printf("twoai_ecosystem: hub page generated for %s at %s\n", h.name, path)
	}
	fmt.Printf("twoai_ecosystem: hub pages generated=%d\n", made)
	return nil
}
