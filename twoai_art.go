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

// twoaiArtCap bounds the model work inside the daily build, where the whole
// build has a deadline. Run on its own (pipeline.exe twoai_art) the stage
// takes twoaiArtCapAlone instead, so a new section can be written in one sitting
// rather than over ten days. Stephen, 2026-09-22.
var twoaiArtCap = 18

const twoaiArtCapAlone = 400

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
	SECCos, MAFilings, Tickers                   int
	MedModels, SciModels, PLCases                int
	Papers, Claims, BookTitles                   int
}

func twoaiArtReadFacts(db *sql.DB) twoaiArtFacts {
	var f twoaiArtFacts
	// One row of counts. If any sub-select is wrong the whole row fails, so the
	// error is printed rather than swallowed: a section written against zeroes
	// would read as a site that holds nothing.
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'image-generation-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'video-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section IN ('audio-speech-models','music-models') AND delisted_at IS NULL),
		(SELECT count(*) FROM synced_tools),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND category = 'intellectual property'),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND (lower(case_name) LIKE '%suno%' OR lower(case_name) LIKE '%uncharted%' OR lower(defendants) LIKE '%suno%')),
		(SELECT jsonb_array_length(data->'terms') FROM site_content WHERE path = 'resources/glossary.json'),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'compliance/%'),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'caselaw/%'),
		(SELECT count(*) FROM pipeline.documents d JOIN pipeline.sources s ON s.id = d.source_id WHERE s.name LIKE 'LegiScan%'),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'companies/%'),
		(SELECT count(*) FROM twoai_ma_filings),
		(SELECT count(*) FROM twoai_stock_instruments),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'medical-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'scientific-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND category LIKE 'product liability%'),
		(SELECT count(*) FROM twoai_research_papers),
		(SELECT count(*) FROM twoai_claims),
		(SELECT count(*) FROM twoai_book_catalog)`).
		Scan(&f.ImageModels, &f.VideoModels, &f.AudioModels, &f.Tools, &f.IPCases, &f.MusicCases, &f.GlossaryTerms,
			&f.AllCases, &f.Compliance, &f.CaseLaw, &f.Bills, &f.SECCos, &f.MAFilings, &f.Tickers,
			&f.MedModels, &f.SciModels, &f.PLCases, &f.Papers, &f.Claims, &f.BookTitles); err != nil {
		fmt.Println("twoai_art: facts:", err)
	}
	return f
}

// line is what the model is told this site holds. Each section gets the
// figures that bear on it, because a number that does not belong on the page
// is a number the model will reach for anyway.
func (f twoaiArtFacts) line(section string) string {
	if section == "eco" {
		return fmt.Sprintf("This site currently tracks %d listed AI-related instruments with daily prices, %d merger and acquisition filings, %d company pages, %d active AI lawsuits, %d compliance and regulation pages, %d AI tools and %d glossary terms.",
			f.Tickers, f.MAFilings, f.SECCos, f.AllCases, f.Compliance, f.Tools, f.GlossaryTerms)
	}
	if section == "res" {
		return fmt.Sprintf("This site currently holds %d research papers in its library, %d claims extracted from research works, %d AI books in its catalogue, %d scientific models, %d AI tools and %d glossary terms. Content on this site never links to the Consensus search tool; it links to the original paper.",
			f.Papers, f.Claims, f.BookTitles, f.SciModels, f.Tools, f.GlossaryTerms)
	}
	if section == "med" {
		return fmt.Sprintf("This site currently tracks %d medical AI models, %d scientific models, %d active product liability and wrongful death lawsuits against AI companies, %d compliance and regulation pages, %d AI tools and %d glossary terms.",
			f.MedModels, f.SciModels, f.PLCases, f.Compliance, f.Tools, f.GlossaryTerms)
	}
	if section == "fin" {
		return fmt.Sprintf("This site currently tracks %d company pages, %d merger and acquisition filings, %d listed AI-related instruments, %d compliance and regulation pages, %d active AI lawsuits, %d AI tools and %d glossary terms.",
			f.SECCos, f.MAFilings, f.Tickers, f.Compliance, f.AllCases, f.Tools, f.GlossaryTerms)
	}
	if section == "law" {
		return fmt.Sprintf("This site currently tracks %d active AI lawsuits (%d of them intellectual property), %d AI case law precedents, %d compliance and regulation pages, %d state AI bills, %d AI tools and %d glossary terms.",
			f.AllCases, f.IPCases, f.CaseLaw, f.Compliance, f.Bills, f.Tools, f.GlossaryTerms)
	}
	return fmt.Sprintf("This site currently tracks %d live image generation models, %d video models, %d audio models, %d AI tools, %d active intellectual property lawsuits (of which %d involve AI music services), and %d glossary terms.",
		f.ImageModels, f.VideoModels, f.AudioModels, f.Tools, f.IPCases, f.MusicCases, f.GlossaryTerms)
}

// twoaiArtHubSystem writes the landing pages. A hub that is a heading and ten
// links is a thin page, which is the thing this site refuses to publish, so
// each hub and sub-hub gets an opening that says what the field is, what is
// actually changing in it, and how its pages fit together.
const twoaiArtHubSystem = `You write the opening of a reference section for The World of AI, an atlas of artificial intelligence.

You are given the section name, what it covers, and the pages under it. Write three paragraphs of four to six sentences each:
- what: what this field is and what artificial intelligence is actually doing in it now, not in theory.
- state: where the work stands, what is solved, what is not, and what the honest limits are.
- map: how the pages listed below fit together and what a reader would go to each for. Name them inside your own sentences rather than listing them.

Rules:
- Plain English. Commas, not dashes. No em dashes. No marketing language, no "in today's landscape", no exclamation.
- Use a figure from the site facts ONLY where it genuinely belongs. Never invent a number, a company, a product version, a case name or a date.
- Nothing here is advice, legal, financial or medical. Describe the work. On medical topics, never tell a reader what to do about their own health.

Answer with one JSON object and nothing else:
{"what": "", "state": "", "map": ""}`

const twoaiArtSystem = `You write reference pages for The World of AI, an atlas of artificial intelligence.

You are given a topic, the field it sits in, and sometimes short seed lines from the site's editor. Write ONE paragraph of three to five sentences for each of the five sections, in this order: scope, what it runs on, how the work is done, rights and risk and provenance, where it is going.

Rules:
- Where a seed line is given, keep its meaning: do not change the subject or contradict it. Where none is given, write the section yourself from what the topic plainly is.
- Plain English. Commas, not dashes. No em dashes. No marketing language, no "in today's landscape", no exclamation.
- Use a figure from the site facts ONLY where it genuinely belongs. Never invent a number, a company, a product version, a case name or a date.
- Name tools only where the seed names them or where the tool is unambiguous and well known.
- Write for a working professional in the field who is competent but not a machine learning engineer. Nothing here is legal, financial, investment or medical advice: describe practice, never advise a reader, never recommend a security, trade or allocation, and on medical topics never tell a reader what to do about their own health or treatment.

Answer with one JSON object and nothing else:
{"scope": "", "infra": "", "method": "", "governance": "", "horizon": ""}`

// twoaiArtJSON pulls the object out of a reply that may still carry a fence or
// a sentence around it.
func twoaiArtJSON(raw string) (map[string]string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		return nil, err
	}
	return m, nil
}

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
	// The landing pages come first. A hub or sub-hub that is a heading and a
	// list of links is exactly the thin page this site refuses to publish, so
	// its opening is written before any more topics are.
	kidsOf := map[string][]string{}
	for _, c := range nodes {
		if c.Parent != "" {
			kidsOf[c.Parent] = append(kidsOf[c.Parent], c.Name)
		}
	}
	for _, n := range nodes {
		if n.Kind == "topic" || written >= twoaiArtCap {
			continue
		}
		want := twoaiArtHash(n, facts.line(n.Section)+strings.Join(kidsOf[n.Slug], ","))
		var have string
		db.QueryRow(`SELECT data_hash FROM twoai_art_readings WHERE slug = $1 AND block = 'hub'`, n.Slug).Scan(&have)
		if have == want {
			continue
		}
		user := fmt.Sprintf("Section: %s\nPart of: %s\nWhat it covers: %s\nSite facts: %s\n\nPages under it:\n%s\n\nAnswer now.",
			n.Name, parentName[rootFor[n.Section]], n.Blurb, facts.line(n.Section),
			"- "+strings.Join(kidsOf[n.Slug], "\n- "))
		out, model, err := twoaiGenerate("art", twoaiArtHubSystem, user)
		if err != nil {
			failed++
			fmt.Printf("twoai_art: %s: %v\n", n.Slug, err)
			continue
		}
		got, perr := twoaiArtJSON(out)
		if perr != nil || len(got["what"]) < 120 || len(got["map"]) < 120 {
			failed++
			fmt.Printf("twoai_art: %s: opening not usable, nothing written\n", n.Slug)
			continue
		}
		b, _ := json.Marshal(got)
		if _, err := db.Exec(`INSERT INTO twoai_art_readings (slug, block, body, data_hash, model, generated_on)
			VALUES ($1,'hub',$2,$3,$4,current_date)
			ON CONFLICT (slug, block) DO UPDATE SET body = EXCLUDED.body, data_hash = EXCLUDED.data_hash,
				model = EXCLUDED.model, generated_on = current_date`, n.Slug, string(b), want, model); err != nil {
			failed++
			continue
		}
		written++
	}
	// Topics are taken one section at a time in turn, so a new section is not
	// left waiting behind every topic of an older one. Before 2026-09-22 they
	// ran in table order and the fifty law and fifty finance topics sat behind
	// the fifty art topics.
	var ordered []twoaiArtNode
	{
		bySection := map[string][]twoaiArtNode{}
		var sections []string
		for _, n := range nodes {
			if n.Kind != "topic" {
				continue
			}
			if _, ok := bySection[n.Section]; !ok {
				sections = append(sections, n.Section)
			}
			bySection[n.Section] = append(bySection[n.Section], n)
		}
		for i := 0; ; i++ {
			added := false
			for _, s := range sections {
				if i < len(bySection[s]) {
					ordered = append(ordered, bySection[s][i])
					added = true
				}
			}
			if !added {
				break
			}
		}
	}
	for _, n := range ordered {
		if written >= twoaiArtCap {
			break
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
		got, jerr := twoaiArtJSON(s)
		if jerr != nil {
			failed++
			fmt.Printf("twoai_art: %s: unreadable answer: %v\n", n.Slug, jerr)
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
	hubReadings := map[string]map[string]string{}
	rrows, err := db.Query(`SELECT slug, block, body FROM twoai_art_readings WHERE block IN ('all','hub')`)
	if err == nil {
		for rrows.Next() {
			var slug, block, body string
			if rrows.Scan(&slug, &block, &body) == nil {
				var m map[string]string
				if json.Unmarshal([]byte(body), &m) == nil {
					if block == "hub" {
						hubReadings[slug] = m
					} else {
						readings[slug] = m
					}
				}
			}
		}
		rrows.Close()
	}
	// A child's line in a list is the first sentence of its own scope, so a
	// landing page says what each page under it is about instead of listing
	// names. Written once the child has been written, never invented here.
	firstSentence := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		if i := strings.Index(s, ". "); i > 40 {
			return s[:i+1]
		}
		if len(s) > 220 {
			return s[:220] + "..."
		}
		return s
	}

	uid := func(slug string) string { return twoaiUID("art:" + slug) }
	// A page's address is its own category's, and the root of a section is the
	// row with no parent in that section.
	catOf := map[string]string{}
	rootOf := map[string]string{}
	rootName := map[string]string{}
	for _, n := range nodes {
		catOf[n.Slug] = n.Category
		if n.Kind == "hub" {
			rootOf[n.Section] = n.Slug
			rootName[n.Section] = n.Name
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
	taxFor := map[string]string{"art": "ai-art", "law": "ai-lawyer", "fin": "ai-accountant", "med": "ai-physician", "res": "ai-researcher", "eco": "ai-economist"}

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
					if blurb == "" {
						if r := readings[c.Slug]; r != nil {
							blurb = firstSentence(r["scope"])
						} else if h := hubReadings[c.Slug]; h != nil {
							blurb = firstSentence(h["what"])
						}
					}
					kids = append(kids, kid{Name: c.Name, Path: path(c.Slug), Blurb: blurb})
				}
			}
			h := hubReadings[n.Slug]
			var opening []map[string]string
			if h != nil {
				if h["what"] != "" {
					opening = append(opening, map[string]string{"heading": "What this covers", "body": h["what"]})
				}
				if h["state"] != "" {
					opening = append(opening, map[string]string{"heading": "Where the work stands", "body": h["state"]})
				}
				if h["map"] != "" {
					opening = append(opening, map[string]string{"heading": "How these pages fit together", "body": h["map"]})
				}
			}
			doc := map[string]any{
				"uid": uid(n.Slug), "slug": n.Slug, "shape": "art-hub", "category": n.Category,
				"name": n.Name, "title": n.Name, "blurb": n.Blurb, "answer": n.Blurb,
				"children": kids, "sections": opening, "expanded": h != nil,
				// A page still waiting for its opening is live and linked, and out
				// of the search index until it has something to say.
				"noindex":     h == nil,
				"child_count": len(kids), "generated": today, "refresh_every_days": 90,
				"hub_name": rootName[n.Section],
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
				"expanded": r != nil, "noindex": r == nil,
				"parent_name": parentName, "parent_path": parentPath,
				"siblings": siblings, "hub_path": path(rootOf[n.Section]),
				"hub_name": rootName[n.Section], "generated": today,
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
