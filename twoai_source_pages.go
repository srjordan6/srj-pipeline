package main

// twoai_source_pages: a page of our own for every source the Industry Use
// Cases section cites. Stephen, 2026-09-25: "i want a web page for each source
// where we have read the contents of the source and do a page of summary of
// the content we find. Put the link to the source at the very bottom of the
// page - let ollama do all the work."
//
// The industry pages already cite 156 outside sources, and twoai_harvest
// fetches each one daily into twoai_source_harvest. Until now the only use
// of that text was a one-paragraph point brief inline on the industry page.
// This stage turns each harvested source into its own page: what the source
// is, what it says, the figures it gives, what it means for AI in that
// industry, and its limits, written by Ollama from the harvested text only,
// with the link to the source at the very bottom. A source that could not be
// fetched, or gave under 500 characters, gets no page, because a summary of
// nothing is the thin page this site does not publish.
//
// Pages are art-topic documents, the same shape the profession and SQL
// sections use, so the template needs nothing new except the source line;
// the uid is twoaiUID("source:" + url), minted once and never moved. A page
// is written once and rewritten only when the harvested content's hash
// changes, so a source that never changes costs one model call ever. After
// the pages are written the industry documents get a reading_path on each
// point, so the industry page links to our summary beside the source link.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const twoaiSourcePagesPerRun = 12

func twoaiSourcePages(db *sql.DB, today string) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_source_pages (
		url text PRIMARY KEY,
		uid text NOT NULL,
		industry_slug text NOT NULL,
		industry_name text NOT NULL,
		point_name text NOT NULL,
		content_hash text,
		doc jsonb,
		model text,
		written_on date,
		error text)`); err != nil {
		return 0, err
	}
	const base = "/ai-ecosystem/enterprise-applications-governance-and-tools/"

	// Every cited source with a usable harvest, and what we already have.
	rows, err := db.Query(`SELECT i.slug, i.name, p->>'name', COALESCE(p->>'desc',''), p->>'source',
			COALESCE(h.extract,''), COALESCE(h.content_hash,''), COALESCE(h.http_status,0),
			COALESCE(sp.content_hash,''), sp.doc IS NOT NULL
		FROM twoai_industries i, jsonb_array_elements(i.points) p
		LEFT JOIN twoai_source_harvest h ON h.url = p->>'source'
		LEFT JOIN twoai_source_pages sp ON sp.url = p->>'source'
		WHERE p->>'source' LIKE 'http%'
		ORDER BY i.slug, p->>'name'`)
	if err != nil {
		return 0, err
	}
	type job struct {
		slug, industry, name, desc, url, extract, hash, oldHash string
		status                                                  int
		hasDoc                                                  bool
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.slug, &j.industry, &j.name, &j.desc, &j.url, &j.extract, &j.hash, &j.status, &j.oldHash, &j.hasDoc); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()

	// Industry page paths, for the crumb back and the sibling list.
	sectionPath := map[string]string{}
	spr, err := db.Query(`SELECT taxonomy_slug, data->>'uid' FROM twoai_pages WHERE path LIKE 'industries/industry-%' AND data->>'shape' = 'tech-section'`)
	if err == nil {
		for spr.Next() {
			var s, u string
			if spr.Scan(&s, &u) == nil && u != "" {
				sectionPath[s] = base + u + "/"
			}
		}
		spr.Close()
	}

	written, skipped, unusable := 0, 0, 0
	for _, j := range jobs {
		if j.status != 200 || len(j.extract) < 500 {
			unusable++
			continue
		}
		if j.hasDoc && j.oldHash == j.hash {
			skipped++
			continue
		}
		if written >= twoaiSourcePagesPerRun {
			continue
		}
		uid := twoaiUID("source:" + j.url)
		system := "You write a reference page for theworldofai.org that summarises ONE outside source for a reader interested in artificial intelligence in the " + j.industry + " industry. " +
			"Use only the source text supplied. Do not add facts, names or numbers the text does not contain; if the text is a landing page with little substance, say so plainly rather than padding. " +
			"Plain English, commas rather than dashes, no bullet lists, no headings inside bodies, no marketing language, no mention of these instructions. " +
			"Return only JSON with this shape: {\"title\": \"<the page title, naming the publisher and what the source is, under 90 characters>\", " +
			"\"answer\": \"<70 to 100 words: what this source is and the single most useful thing it says, written to stand alone>\", " +
			"\"sections\": [{\"heading\": \"What this source is\", \"body\": \"<who publishes it, what kind of document it is, its scope and date if stated>\"}, " +
			"{\"heading\": \"What it says\", \"body\": \"<the substance, 150 to 250 words, faithful to the text>\"}, " +
			"{\"heading\": \"Figures and claims worth noting\", \"body\": \"<specific numbers, definitions or positions the text gives, each attributed to the source; or one sentence saying the text gives none>\"}, " +
			"{\"heading\": \"What it means for AI in " + j.industry + "\", \"body\": \"<why a reader following AI in this industry would use this source, grounded in what it actually contains>\"}, " +
			"{\"heading\": \"Limits of this source\", \"body\": \"<what it does not cover, whether it is commercial, dated or partial, from the text itself>\"}]}"
		user := fmt.Sprintf("Industry: %s\nThe point on our industry page that cites this source: %s. %s\nSource URL: %s\n\nSource text (harvested %s):\n\n%s",
			j.industry, j.name, j.desc, j.url, today, trunc(j.extract, 9000))
		out, model, gerr := twoaiGenerate("twoai_source_pages", system, user)
		if gerr != nil {
			db.Exec(`INSERT INTO twoai_source_pages (url, uid, industry_slug, industry_name, point_name, error)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (url) DO UPDATE SET error=$6`, j.url, uid, j.slug, j.industry, j.name, gerr.Error())
			continue
		}
		s := out
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if k := strings.LastIndex(s, "}"); k >= 0 {
			s = s[:k+1]
		}
		var m struct {
			Title    string `json:"title"`
			Answer   string `json:"answer"`
			Sections []struct {
				Heading string `json:"heading"`
				Body    string `json:"body"`
			} `json:"sections"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil || strings.TrimSpace(m.Answer) == "" || len(m.Sections) < 3 {
			db.Exec(`INSERT INTO twoai_source_pages (url, uid, industry_slug, industry_name, point_name, error)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (url) DO UPDATE SET error=$6`, j.url, uid, j.slug, j.industry, j.name, "model output was not the expected JSON")
			continue
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = j.name
		}
		var sections []map[string]string
		for _, sec := range m.Sections {
			if strings.TrimSpace(sec.Body) == "" {
				continue
			}
			sections = append(sections, map[string]string{"heading": strings.TrimSpace(sec.Heading), "body": strings.TrimSpace(sec.Body)})
		}
		doc := map[string]any{
			"uid": uid, "page_uid": uid, "shape": "art-topic", "slug": "source-" + uid,
			"name": title, "title": title, "answer": strings.TrimSpace(m.Answer),
			"sections":    sections,
			"category":    "enterprise-applications-governance-and-tools",
			"hub_name":    "Industry Use Cases",
			"parent_name": j.industry, "parent_path": sectionPath[j.slug],
			"crumbs":     []map[string]string{{"name": "Industry Use Cases", "path": base + twoaiUID("section:industry-use-cases") + "/"}, {"name": j.industry, "path": sectionPath[j.slug]}},
			"source_url": j.url, "source_name": j.name, "source_read_on": today,
			"kind": "source-summary", "industry_slug": j.slug,
			"generated": today, "built_at": time.Now().Format(time.RFC3339),
			"refresh_every_days": 90, "noindex": false, "expanded": true,
		}
		raw, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_source_pages (url, uid, industry_slug, industry_name, point_name, content_hash, doc, model, written_on, error)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,current_date,NULL)
			ON CONFLICT (url) DO UPDATE SET content_hash=$6, doc=$7::jsonb, model=$8, written_on=current_date, error=NULL`,
			j.url, uid, j.slug, j.industry, j.name, j.hash, string(raw), model); err != nil {
			return written, err
		}
		written++
		fmt.Printf("twoai_source_pages: wrote %s (%s) for %s via %s\n", uid, trunc(title, 60), j.industry, model)
		time.Sleep(300 * time.Millisecond)
	}

	// Publish every written page, with siblings (the other summarised sources
	// in the same industry) filled in fresh each run so new pages appear on
	// old ones.
	sib, err := db.Query(`SELECT industry_slug, uid, doc->>'name' FROM twoai_source_pages WHERE doc IS NOT NULL ORDER BY industry_slug, doc->>'name'`)
	if err != nil {
		return written, err
	}
	siblings := map[string][]map[string]string{}
	for sib.Next() {
		var s, u, n string
		if sib.Scan(&s, &u, &n) == nil {
			siblings[s] = append(siblings[s], map[string]string{"name": n, "path": base + u + "/"})
		}
	}
	sib.Close()
	pr, err := db.Query(`SELECT url, uid, industry_slug, doc::text FROM twoai_source_pages WHERE doc IS NOT NULL`)
	if err != nil {
		return written, err
	}
	published := 0
	type pub struct{ url, uid, slug, raw string }
	var pubs []pub
	for pr.Next() {
		var p pub
		if pr.Scan(&p.url, &p.uid, &p.slug, &p.raw) == nil {
			pubs = append(pubs, p)
		}
	}
	pr.Close()
	for _, p := range pubs {
		var doc map[string]any
		if json.Unmarshal([]byte(p.raw), &doc) != nil {
			continue
		}
		var sibs []map[string]string
		for _, s := range siblings[p.slug] {
			if s["path"] != base+p.uid+"/" {
				sibs = append(sibs, s)
			}
		}
		doc["siblings"] = sibs
		doc["parent_path"] = sectionPath[p.slug]
		raw, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
			VALUES ($1, 'tech-section', $2, $3::jsonb, 1, now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()
			WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`,
			"industries/source-"+p.uid+".json", p.slug, string(raw)); err != nil {
			return written, err
		}
		published++
		// The industry page links to our summary beside the source link.
		db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{points}', (
				SELECT jsonb_agg(CASE WHEN pt->>'source' = $2 THEN pt || jsonb_build_object('reading_path', $3::text) ELSE pt END)
				FROM jsonb_array_elements(data->'points') pt)), updated_at = now()
			WHERE path = $1 AND EXISTS (SELECT 1 FROM jsonb_array_elements(data->'points') q WHERE q->>'source' = $2 AND COALESCE(q->>'reading_path','') <> $3)`,
			"industries/"+p.slug+".json", p.url, base+p.uid+"/")
	}
	fmt.Printf("twoai_source_pages: written=%d skipped_unchanged=%d unusable=%d published=%d of %d cited sources\n",
		written, skipped, unusable, published, len(jobs))
	return published, nil
}
