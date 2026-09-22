package main

// twoai_art: sectioned reference sections built from a list of pages and
// written by the model. Two run through it today:
//
//   The Art of AI   (section art, ecosystem-entities-market-and-operations)
//   The AI Lawyer   (section law, enterprise-applications-governance-and-tools)
//
// Each is a hub, ten sub-hubs and fifty topics, and every topic uses the same
// five-part frame: scope, what it runs on, how the work is done, rights and
// risk and provenance, where it is going.
//
// WHERE IT COMES FROM. Stephen gave both outlines, on 2026-09-22. The outline
// is held in twoai_art_nodes, one row per page, with section and category on
// each row; the seed columns are optional. That table is the record of what
// each page is about; nothing here invents a topic.
//
// WHAT THE MODEL DOES. Stephen, 2026-09-22: the model writes the seed too. A
// topic row need carry nothing but its name and the sub-hub it belongs to;
// Ollama writes all five sections from that, and the prompt carries what this
// site actually holds on the subject: how many image, video and audio models
// are tracked, how many tools, how many intellectual property lawsuits are
// live, and how many glossary terms exist. That is the difference between a
// page of opinion and a page of the site's own record, which is the standard
// the rest of theworldofai.org is held to. A reading is cached on a hash of
// the seed plus those figures, so it is written once and rewritten only when
// the seed or the data moves.
//
// WHAT IT REFUSES. A topic with no expansion yet publishes with whatever the
// editor row holds and is marked so; it is never padded. The stage never
// writes a figure the query did not return.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// twoaiArtCap bounds the model work in one run; fifty topics fill in over a
// few days rather than in one long stage.
const twoaiArtCap = 12

// A section's pages are written to the content folder its category route
// reads: the ecosystem-entities route sweeps content/ecosystem, the
// enterprise route sweeps content/industries.
var twoaiArtDirFor = map[string]string{
	"ecosystem-entities-market-and-operations":     "ecosystem",
	"enterprise-applications-governance-and-tools": "industries",
}

const twoaiArtDefaultCategory = "ecosystem-entities-market-and-operations"

type twoaiArtNode struct {
	Slug, Kind, Parent, Name           string
	Sort                               int
	Blurb                              string
	Scope, Infra, Method, Gov, Horizon string
	Section, Category                  string
}

// twoaiArtFacts is what the site knows that bears on creative AI. Everything
// here is a count of rows this database holds, read fresh each run.
type twoaiArtFacts struct {
	ImageModels, VideoModels, AudioModels, Tools int
	IPCases, MusicCases                          int
	GlossaryTerms                                int
	AllCases, Compliance, CaseLaw, Bills         int
}

func twoaiArtReadFacts(db *sql.DB) twoaiArtFacts {
	var f twoaiArtFacts
	db.QueryRow(`SELECT
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'image-generation-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'video-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section IN ('audio-speech-models','music-models') AND delisted_at IS NULL),
		(SELECT count(*) FROM synced_tools),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND category = 'intellectual property'),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND (lower(case_name) LIKE '%suno%' OR lower(case_name) LIKE '%uncharted%' OR lower(defendants) LIKE '%suno%')),
		(SELECT jsonb_array_length(data->'terms') FROM site_content WHERE path = 'resources/glossary.json'),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'compliance/%'),
		(SELECT count(*) FROM twoai_caselaw),
		(SELECT count(*) FROM pipeline.documents WHERE source = 'legiscan')`).
		Scan(&f.ImageModels, &f.VideoModels, &f.AudioModels, &f.Tools, &f.IPCases, &f.MusicCases, &f.GlossaryTerms,
			&f.AllCases, &f.Compliance, &f.CaseLaw, &f.Bills)
	return f
}

// line is what the model is told this site holds. Each section gets the
// figures that bear on it, because a number that does not belong on the page
// is a number the model will reach for anyway.
func (f twoaiArtFacts) line(section string) string {
	if section == "law" {
		return fmt.Sprintf("This site currently tracks %d active AI lawsuits (%d of them intellectual property), %d AI case law precedents, %d compliance and regulation pages, %d state AI bills, %d AI tools and %d glossary terms.",
			f.AllCases, f.IPCases, f.CaseLaw, f.Compliance, f.Bills, f.Tools, f.GlossaryTerms)
	}
	return fmt.Sprintf("This site currently tracks %d live image generation models, %d video models, %d audio models, %d AI tools, %d active intellectual property lawsuits (of which %d involve AI music services), and %d glossary terms.",
		f.ImageModels, f.VideoModels, f.AudioModels, f.Tools, f.IPCases, f.MusicCases, f.GlossaryTerms)
}

const twoaiArtSystem = `You write reference pages for The World of AI, an atlas of artificial intelligence.

You are given a topic, the field it sits in, and sometimes short seed lines from the site's editor. Write ONE paragraph of three to five sentences for each of the five sections, in this order: scope, what it runs on, how the work is done, rights and risk and provenance, where it is going.

Rules:
- Where a seed line is given, keep its meaning: do not change the subject or contradict it. Where none is given, write the section yourself from what the topic plainly is.
- Plain English. Commas, not dashes. No em dashes. No marketing language, no "in today's landscape", no exclamation.
- Use a figure from the site facts ONLY where it genuinely belongs. Never invent a number, a company, a product version, a case name or a date.
- Name tools only where the seed names them or where the tool is unambiguous and well known.
- Write for a working professional in the field who is competent but not a machine learning engineer. Nothing here is legal advice, and a page about law describes practice rather than advising a reader.

Answer with one JSON object and nothing else:
{"scope": "", "infra": "", "method": "", "governance": "", "horizon": ""}`

func twoaiArtHash(n twoaiArtNode, facts string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{n.Name, n.Scope, n.Infra, n.Method, n.Gov, n.Horizon, facts}, "|")))
	return hex.EncodeToString(h[:])[:16]
}

func twoaiArtExpand(db *sql.DB, nodes []twoaiArtNode, facts twoaiArtFacts) (int, int) {
	written, failed := 0, 0
	parentName := map[string]string{}
	rootFor := map[string]string{}
	for _, p := range nodes {
		parentName[p.Slug] = p.Name
		if p.Kind == "hub" {
			rootFor[p.Section] = p.Slug
		}
	}
	for _, n := range nodes {
		if n.Kind != "topic" || written >= twoaiArtCap {
			continue
		}
		want := twoaiArtHash(n, facts.line(n.Section))
		var have string
		db.QueryRow(`SELECT data_hash FROM twoai_art_readings WHERE slug = $1 AND block = 'all'`, n.Slug).Scan(&have)
		if have == want {
			continue
		}
		seeds := "(none: write all five sections yourself)"
		if strings.TrimSpace(n.Scope+n.Infra+n.Method+n.Gov+n.Horizon) != "" {
			seeds = fmt.Sprintf("scope: %s\ninfra: %s\nmethod: %s\ngovernance: %s\nhorizon: %s",
				n.Scope, n.Infra, n.Method, n.Gov, n.Horizon)
		}
		user := fmt.Sprintf("Topic: %s\nField: %s, part of %s\nSite facts: %s\n\nEditor seed lines:\n%s\n\nAnswer now.",
			n.Name, parentName[n.Parent], parentName[rootFor[n.Section]], facts.line(n.Section), seeds)
		out, model, err := twoaiGenerate("art", twoaiArtSystem, user)
		if err != nil {
			failed++
			fmt.Printf("twoai_art: %s: %v\n", n.Slug, err)
			continue
		}
		s := strings.TrimSpace(out)
		s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if j := strings.LastIndex(s, "}"); j >= 0 {
			s = s[:j+1]
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &got); err != nil {
			failed++
			fmt.Printf("twoai_art: %s: unreadable answer: %v\n", n.Slug, err)
			continue
		}
		if len(got["scope"]) < 80 || len(got["governance"]) < 80 {
			failed++
			fmt.Printf("twoai_art: %s: answer too short, nothing written\n", n.Slug)
			continue
		}
		b, _ := json.Marshal(got)
		if _, err := db.Exec(`INSERT INTO twoai_art_readings (slug, block, body, data_hash, model, generated_on)
			VALUES ($1,'all',$2,$3,$4,current_date)
			ON CONFLICT (slug, block) DO UPDATE SET body = EXCLUDED.body, data_hash = EXCLUDED.data_hash,
				model = EXCLUDED.model, generated_on = current_date`, n.Slug, string(b), want, model); err != nil {
			failed++
			continue
		}
		written++
	}
	return written, failed
}

func twoaiArt(db *sql.DB, today string) error {
	rows, err := db.Query(`SELECT slug, kind, COALESCE(parent_slug,''), sort, name, COALESCE(blurb,''),
		COALESCE(scope,''), COALESCE(infra,''), COALESCE(method,''), COALESCE(governance,''), COALESCE(horizon,''),
		COALESCE(section,'art'), COALESCE(category,'')
		FROM twoai_art_nodes WHERE status = 'live' ORDER BY section, kind, sort`)
	if err != nil {
		return err
	}
	var nodes []twoaiArtNode
	for rows.Next() {
		var n twoaiArtNode
		if rows.Scan(&n.Slug, &n.Kind, &n.Parent, &n.Sort, &n.Name, &n.Blurb,
			&n.Scope, &n.Infra, &n.Method, &n.Gov, &n.Horizon, &n.Section, &n.Category) == nil {
			if n.Category == "" {
				n.Category = twoaiArtDefaultCategory
			}
			nodes = append(nodes, n)
		}
	}
	rows.Close()
	if len(nodes) == 0 {
		fmt.Println("twoai_art: no nodes, nothing to build")
		return nil
	}

	facts := twoaiArtReadFacts(db)
	written, failed := twoaiArtExpand(db, nodes, facts)

	readings := map[string]map[string]string{}
	rrows, err := db.Query(`SELECT slug, body FROM twoai_art_readings WHERE block = 'all'`)
	if err == nil {
		for rrows.Next() {
			var slug, body string
			if rrows.Scan(&slug, &body) == nil {
				var m map[string]string
				if json.Unmarshal([]byte(body), &m) == nil {
					readings[slug] = m
				}
			}
		}
		rrows.Close()
	}

	uid := func(slug string) string { return twoaiUID("art:" + slug) }
	// A page's address is its own category's, and the root of a section is the
	// row with no parent in that section.
	catOf := map[string]string{}
	rootOf := map[string]string{}
	for _, n := range nodes {
		catOf[n.Slug] = n.Category
		if n.Kind == "hub" {
			rootOf[n.Section] = n.Slug
		}
	}
	path := func(slug string) string {
		cat := catOf[slug]
		if cat == "" {
			cat = twoaiArtDefaultCategory
		}
		return "/ai-ecosystem/" + cat + "/" + uid(slug) + "/"
	}
	fileFor := func(n twoaiArtNode) string {
		dir := twoaiArtDirFor[n.Category]
		if dir == "" {
			dir = "ecosystem"
		}
		return dir + "/" + n.Section + "-" + n.Slug + ".json"
	}
	type kid struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Blurb string `json:"blurb,omitempty"`
	}

	pages := 0
	write := func(file, tax string, doc map[string]any) error {
		b, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
			VALUES ($1,$2,$3,$4::jsonb,1,now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, taxonomy_slug = EXCLUDED.taxonomy_slug,
				data = EXCLUDED.data, url_count = 1, updated_at = now()`,
			file, doc["shape"], tax, string(b)); err != nil {
			return err
		}
		pages++
		return nil
	}
	// Each section has its own taxonomy row, so the category page lists it and
	// the freshness contract can find its pages.
	taxFor := map[string]string{"art": "ai-art", "law": "ai-lawyer"}

	for _, n := range nodes {
		switch n.Kind {
		case "hub", "subhub":
			var kids []kid
			for _, c := range nodes {
				if c.Parent == n.Slug {
					blurb := c.Blurb
					if blurb == "" {
						blurb = c.Scope
					}
					kids = append(kids, kid{Name: c.Name, Path: path(c.Slug), Blurb: blurb})
				}
			}
			doc := map[string]any{
				"uid": uid(n.Slug), "slug": n.Slug, "shape": "art-hub", "category": n.Category,
				"name": n.Name, "title": n.Name, "blurb": n.Blurb, "answer": n.Blurb,
				"children": kids, "generated": today,
			}
			if n.Kind == "subhub" {
				root := rootOf[n.Section]
				for _, p := range nodes {
					if p.Slug == root {
						doc["parent_name"] = p.Name
					}
				}
				doc["parent_path"] = path(root)
			}
			if err := write(fileFor(n), taxFor[n.Section], doc); err != nil {
				return err
			}
		case "topic":
			r := readings[n.Slug]
			pick := func(key, seed string) string {
				if r != nil && strings.TrimSpace(r[key]) != "" {
					return r[key]
				}
				return seed
			}
			var parentName, parentPath string
			for _, p := range nodes {
				if p.Slug == n.Parent {
					parentName, parentPath = p.Name, path(p.Slug)
				}
			}
			var siblings []kid
			for _, s := range nodes {
				if s.Parent == n.Parent && s.Slug != n.Slug {
					siblings = append(siblings, kid{Name: s.Name, Path: path(s.Slug)})
				}
			}
			doc := map[string]any{
				"uid": uid(n.Slug), "slug": n.Slug, "shape": "art-topic", "category": n.Category,
				"name": n.Name, "title": n.Name, "answer": pick("scope", n.Scope),
				"sections": []map[string]string{
					{"heading": "Scope", "body": pick("scope", n.Scope)},
					{"heading": "What it runs on", "body": pick("infra", n.Infra)},
					{"heading": "How the work is done", "body": pick("method", n.Method)},
					{"heading": "Rights, risk and provenance", "body": pick("governance", n.Gov)},
					{"heading": "Where it is going", "body": pick("horizon", n.Horizon)},
				},
				"expanded": r != nil, "parent_name": parentName, "parent_path": parentPath,
				"siblings": siblings, "hub_path": path(rootOf[n.Section]), "generated": today,
				"refresh_every_days": 90,
			}
			if err := write(fileFor(n), taxFor[n.Section], doc); err != nil {
				return err
			}
		}
	}

	fmt.Printf("twoai_art: pages=%d topics_expanded_this_run=%d failed=%d topics_with_a_reading=%d ok=true\n",
		pages, written, failed, len(readings))
	return nil
}
