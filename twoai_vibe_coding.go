package main

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// twoaiCatSlug mirrors the slug rule the tools category pages use, so a link
// built here lands on the page that exists rather than a plausible 404.
var twoaiCatSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func twoaiCatSlug(n string) string {
	s := strings.ToLower(strings.ReplaceAll(n, "&", "and"))
	return strings.Trim(twoaiCatSlugRe.ReplaceAllString(s, "-"), "-")
}

// twoaiVibeCoding renders the Vibe Coding section.
//
// WHAT THIS SECTION IS FOR. Stephen's brief was explicit: the site already
// covers nearly everything a vibe coder needs - coding models, coding and
// browser agents, the MCP directory and its security, agent identity, prompt
// engineering, benchmarks, the tools directory, the lawsuit tracker - and what
// was missing was a page that says WHICH of those to open for a given problem.
// So this is a short authored explainer plus a routing table, not a new silo
// restating material that already exists three clicks away.
//
// THE ROUTING TABLE IS COMPUTED, NOT TYPED. Every destination is a join from
// twoai_vibe_routes to twoai_taxonomy on slug, and a route whose target is not
// live is DROPPED rather than rendered as a dead link. Hand-written cross-links
// rot the moment a section moves or is renamed; this cannot, because the only
// thing stored is which section to point at, and the path comes from the
// taxonomy at render time.
//
// A dropped route is reported on stderr. Silence about a link that vanished is
// how a map stops matching the territory.
func twoaiVibeCoding(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	var uid, name, blurb string
	err := db.QueryRow(`SELECT slug, name, COALESCE(blurb,'') FROM twoai_taxonomy
		WHERE slug='vibe-coding'`).Scan(&uid, &name, &blurb)
	if err != nil {
		return 0, nil // section not defined yet; nothing to render
	}
	sectionUID := twoaiUID("section:vibe-coding")

	type block struct {
		Slug    string `json:"slug"`
		Block   string `json:"block"`
		Heading string `json:"heading"`
		Body    string `json:"body_md"`
	}
	var blocks []block
	if rows, err := db.Query(`SELECT slug, block, heading, body_md FROM twoai_vibe_coding
		WHERE status='live' ORDER BY sort, slug`); err == nil {
		for rows.Next() {
			var b block
			if rows.Scan(&b.Slug, &b.Block, &b.Heading, &b.Body) == nil {
				blocks = append(blocks, b)
			}
		}
		rows.Close()
	}
	if len(blocks) == 0 {
		return 0, nil
	}

	type route struct {
		Need string `json:"need"`
		Href string `json:"href"`
		Name string `json:"name"`
		Why  string `json:"why"`
	}
	routes := []route{}
	dropped := 0

	// Taxonomy-backed routes: the path comes from the live taxonomy row, so it
	// follows the section if the section ever moves.
	if rows, err := db.Query(`SELECT r.need, r.why, t.name, COALESCE(t.live_path,''), COALESCE(t.status,'')
		FROM twoai_vibe_routes r
		LEFT JOIN twoai_taxonomy t ON t.slug = r.target_ref
		WHERE r.target_kind='taxonomy' ORDER BY r.sort`); err == nil {
		for rows.Next() {
			var need, why, tname, path, status string
			if rows.Scan(&need, &why, &tname, &path, &status) != nil {
				continue
			}
			if path == "" || status != "live" {
				dropped++
				fmt.Fprintf(os.Stderr, "twoai_vibe_coding: route %q dropped, target not live\n", need)
				continue
			}
			routes = append(routes, route{need, path, tname, why})
		}
		rows.Close()
	}

	// Tool-category routes point at the tools directory category page, built
	// with the same slug rule the category pages themselves use.
	if rows, err := db.Query(`SELECT need, why, target_ref FROM twoai_vibe_routes
		WHERE target_kind='tools-category' ORDER BY sort`); err == nil {
		for rows.Next() {
			var need, why, cat string
			if rows.Scan(&need, &why, &cat) == nil {
				routes = append(routes, route{need, "/ai-tools/category/" + twoaiCatSlug(cat) + "/", cat, why})
			}
		}
		rows.Close()
	}

	// Glossary routes.
	if rows, err := db.Query(`SELECT need, why, target_ref FROM twoai_vibe_routes
		WHERE target_kind='glossary' ORDER BY sort`); err == nil {
		for rows.Next() {
			var need, why, slug string
			if rows.Scan(&need, &why, &slug) == nil {
				routes = append(routes, route{need, "/ai-glossary/" + slug + "/", "AI glossary", why})
			}
		}
		rows.Close()
	}

	// The tools a vibe coder is actually choosing between, counted from the
	// catalogue rather than listed by hand so the number cannot go stale.
	// Coding-tool count, read from this site's OWN catalog in site_content
	// rather than from synced_tools. synced_tools is a shared table written by
	// another system, and on 2026-08-20 a sync there ingested rendered page
	// headings as tool rows ("Coding & Developer Tools 30 tools" stored as a
	// tool name) and deactivated every real row, which silently turned this
	// count into 0 on a live page. A number this page renders must come from
	// the source this project owns and can verify.
	var codingTools int
	db.QueryRow(`SELECT count(*) FROM site_content,
		jsonb_array_elements(data->'tools') t
		WHERE path='resources/tools.json'
		  AND t->>'category' = 'Coding & Developer Tools'`).Scan(&codingTools)
	// A zero here means the catalog read failed or the category was renamed,
	// never that the site has no coding tools. Say so in the log rather than
	// rendering a confident nothing.
	if codingTools == 0 {
		fmt.Println("twoai_vibe_coding: WARNING coding tool count came back 0 from resources/tools.json; check the category label")
	}

	// Written into content/tech/, which is one of the four directories the
	// Technology and Core Infrastructure route reads at build time. Writing it
	// anywhere else produces a file nothing renders - the failure mode that made
	// the case timelines and the glossary lenses invisible.
	if err := upsert("tech/vibe-coding.json", "vibe-coding", map[string]any{
		"uid": sectionUID, "shape": "vibe-coding", "name": name, "blurb": blurb,
		"blocks": blocks, "routes": routes, "coding_tools": codingTools,
		"generated": today,
	}); err != nil {
		return 0, err
	}

	// Live only now that the page exists, and the path is recorded so every
	// other cross-link on the site resolves to it the same way this page's own
	// routes resolve.
	db.Exec(`UPDATE twoai_taxonomy SET status='live', live_path=$1, updated_at=now()
		WHERE slug='vibe-coding'`,
		"/ai-ecosystem/technology-and-core-infrastructure/"+sectionUID+"/")

	fmt.Printf("twoai_build: vibe coding blocks=%d routes=%d dropped=%d coding_tools=%d\n",
		len(blocks), len(routes), dropped, codingTools)
	return 1, nil
}
