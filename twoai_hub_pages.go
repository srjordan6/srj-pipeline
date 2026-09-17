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

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

func twoaiEnsureHubPages(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT t.slug, t.name, COALESCE(t.blurb,''), COALESCE(t.parent_slug,'')
		FROM twoai_taxonomy t
		WHERE t.level = 2 AND t.live_path IS NULL AND COALESCE(t.status,'') <> 'retired'
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
			VALUES ($1, 'tech-section', $2, jsonb_build_object(
				'uid', $3::text, 'page_uid', $3::text, 'shape', 'tech-section',
				'name', $4::text, 'tax', $2::text, 'is_hub', true,
				'generated', $5::text, 'verified', $5::text,
				'summary', $6::text, 'blurb', $6::text,
				'children', (SELECT COALESCE(jsonb_agg(jsonb_build_object(
					'name', c.name, 'href', c.live_path, 'desc', COALESCE(c.blurb,''), 'sort', c.sort) ORDER BY c.sort), '[]'::jsonb)
					FROM twoai_taxonomy c WHERE c.parent_slug = $2 AND c.live_path IS NOT NULL AND COALESCE(c.status,'') <> 'retired'),
				'points', '[]'::jsonb, 'total', 0), now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()`,
			"industries/"+h.slug+".json", h.slug, uid, h.name, today, summary); err != nil {
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
