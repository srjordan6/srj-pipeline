package main

// Editor-written section trees and sourced facts. theworldofai rows 457 and
// 458, Stephen 2026-10-05: "have theworldofai write a lot of this".
//
// theworldofai writes the pages, srj renders them. Two tables, filled in SQL
// by theworldofai:
//
//   twoai_section_pages  one row per page of a section tree: hub, sub-hubs and
//                        topics (Life Sciences, section lsc, under Industry Use
//                        Cases; AI in Health Care Delivery, section hcd, under
//                        Healthcare). A topic carries an answer, dated
//                        developments each citing its primary sources, and a
//                        closing paragraph. A row is published when its status
//                        is live.
//   twoai_sourced_facts  a fact for a page anywhere on the site, cited to its
//                        primary source, placed on the page whose URL path is
//                        target_path (row 458's routing by subject).
//
// Pages are written as art-hub and art-topic documents in industries/, so the
// enterprise route renders them with breadcrumbs, child lists and siblings.
// Nothing here writes copy; a page with no answer stays out of search.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type sectionPage struct {
	Slug, Section, Parent, Kind, Name, Blurb, Answer, MeaningHeading, Meaning, WrittenBy string
	Developments                                                                         json.RawMessage
	Sort, Refresh                                                                        int
	Reviewed                                                                             string
	// Row 481: the explainer under "How it works", its questions and, on hubs,
	// the key points listed under the answer.
	Explainer      string
	KeyPoints, Faq json.RawMessage
}

// Where each section's root hangs: its parent page on the site.
// The Latest in Diabetic Medicine (dmed, row 483) and the Lp(a) Research
// Center (lpa, row 484) joined Healthcare on 2026-10-05.
var sectionRootParent = map[string]string{"lsc": "industry-use-cases", "hcd": "industry-healthcare",
	"dmed": "industry-healthcare", "lpa": "industry-healthcare"}

// Sections about a person's own treatment, whose every page says it is not
// medical advice.
var sectionMedical = map[string]bool{"dmed": true, "lpa": true}

// Cross links between trees, both ways (row 484: heart protection in
// diabetes and the future of Lp(a) treatment).
var sectionSeeAlso = map[string][]string{"dmed-heart": {"lpa-future"}, "lpa-future": {"dmed-heart"}}

const sectionBase = "/ai-ecosystem/enterprise-applications-governance-and-tools/"

func twoaiSectionPages(db *sql.DB, today string) error {
	rows, err := db.Query(`SELECT slug, section, COALESCE(parent_slug,''), kind, name, COALESCE(blurb,''), COALESCE(answer,''),
			COALESCE(meaning_heading,''), COALESCE(meaning,''), COALESCE(written_by,''), COALESCE(developments,'[]'::jsonb)::text,
			sort, refresh_days, COALESCE(reviewed_on::text,''),
			COALESCE(explainer,''), COALESCE(key_points,'[]'::jsonb)::text, COALESCE(faq,'[]'::jsonb)::text
		FROM twoai_section_pages WHERE status = 'live' ORDER BY section, sort, name`)
	if err != nil {
		return err
	}
	var pages []sectionPage
	for rows.Next() {
		var p sectionPage
		var dev, kp, faq string
		if rows.Scan(&p.Slug, &p.Section, &p.Parent, &p.Kind, &p.Name, &p.Blurb, &p.Answer, &p.MeaningHeading,
			&p.Meaning, &p.WrittenBy, &dev, &p.Sort, &p.Refresh, &p.Reviewed, &p.Explainer, &kp, &faq) == nil {
			p.Developments = json.RawMessage(dev)
			p.KeyPoints, p.Faq = json.RawMessage(kp), json.RawMessage(faq)
			pages = append(pages, p)
		}
	}
	rows.Close()
	bySlug := map[string]sectionPage{}
	for _, p := range pages {
		bySlug[p.Slug] = p
	}
	uid := func(slug string) string { return twoaiUID("sp:" + slug) }
	path := func(slug string) string { return sectionBase + uid(slug) + "/" }
	type ref struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Blurb string `json:"blurb,omitempty"`
	}
	rootParent := map[string]ref{}
	for sec, tax := range sectionRootParent {
		var name, lp string
		db.QueryRow(`SELECT name, COALESCE(live_path,'') FROM twoai_taxonomy WHERE slug = $1`, tax).Scan(&name, &lp)
		if lp != "" {
			rootParent[sec] = ref{Name: name, Path: lp}
		}
	}
	line := func(p sectionPage) string {
		if p.Blurb != "" {
			return p.Blurb
		}
		return twoaiOneLine(p.Answer)
	}
	crumbs := func(p sectionPage) []ref {
		var chain []ref
		seen := map[string]bool{p.Slug: true}
		cur := p
		for cur.Parent != "" && !seen[cur.Parent] {
			seen[cur.Parent] = true
			par, ok := bySlug[cur.Parent]
			if !ok {
				break
			}
			chain = append([]ref{{Name: par.Name, Path: path(par.Slug)}}, chain...)
			cur = par
		}
		if rp, ok := rootParent[p.Section]; ok {
			chain = append([]ref{rp}, chain...)
		}
		return chain
	}
	ix := sectionLoadIndex(db)
	var drugs []dmedDrug
	if _, ok := bySlug["dmed-pipeline"]; ok {
		drugs = dmedLoadDrugs(db)
	}
	var lpaDrugs []map[string]string
	if _, ok := bySlug["lpa-trials"]; ok {
		lpaDrugs = lpaLoadDrugs(db)
	}
	written := 0
	for _, p := range pages {
		var kids, sibs []ref
		for _, c := range pages {
			if c.Parent == p.Slug {
				kids = append(kids, ref{Name: c.Name, Path: path(c.Slug), Blurb: line(c)})
			}
			if p.Parent != "" && c.Parent == p.Parent && c.Slug != p.Slug {
				sibs = append(sibs, ref{Name: c.Name, Path: path(c.Slug)})
			}
		}
		var sections []map[string]string
		if strings.TrimSpace(p.Meaning) != "" {
			h := p.MeaningHeading
			if h == "" {
				h = "What it means in practice"
			}
			sections = append(sections, map[string]string{"heading": h, "body": p.Meaning})
		}
		answer := p.Answer
		if answer == "" {
			answer = p.Blurb
		}
		gen := today
		if p.Reviewed != "" {
			gen = p.Reviewed
		}
		shape := "art-topic"
		if p.Kind != "topic" {
			shape = "art-hub"
		}
		doc := map[string]any{
			"uid": uid(p.Slug), "page_uid": uid(p.Slug), "slug": "sp-" + p.Slug, "shape": shape,
			"category": "enterprise-applications-governance-and-tools",
			"name":     p.Name, "title": p.Name, "blurb": line(p), "answer": answer,
			"sections": sections, "children": kids, "child_count": len(kids), "siblings": sibs,
			"crumbs": crumbs(p), "hub_name": bySlug[p.Section].Name, "hub_path": path(p.Section),
			"generated": gen, "last_reviewed": p.Reviewed, "refresh_every_days": p.Refresh,
			// Written by a model in the theworldofai session unless an editor
			// signed it; the page then carries the AI disclosure line.
			"drafted":  p.WrittenBy != "editor",
			"noindex":  strings.TrimSpace(p.Answer) == "",
			"expanded": strings.TrimSpace(p.Answer) != "",
		}
		var dev []map[string]any
		if json.Unmarshal(p.Developments, &dev) == nil && len(dev) > 0 {
			doc["developments"] = dev
		}
		if strings.TrimSpace(p.Explainer) != "" {
			doc["explainer"] = p.Explainer
		}
		// Row 483: every diabetes page says plainly that it is not medical
		// advice, and the pipeline page carries the drug table.
		if sectionMedical[p.Section] {
			doc["medical_note"] = true
		}
		if p.Slug == "dmed-pipeline" && len(drugs) > 0 {
			doc["drug_table"] = dmedDrugTable(drugs)
		}
		if p.Slug == "lpa-trials" && len(lpaDrugs) > 0 {
			doc["lpa_table"] = lpaDrugs
		}
		var see []ref
		for _, o := range sectionSeeAlso[p.Slug] {
			if op, ok := bySlug[o]; ok {
				see = append(see, ref{Name: op.Name, Path: path(o), Blurb: line(op)})
			}
		}
		if len(see) > 0 {
			doc["see_also"] = see
		}
		var kp []string
		if json.Unmarshal(p.KeyPoints, &kp) == nil && len(kp) > 0 {
			doc["key_points"] = kp
		}
		var faq []struct {
			Q string `json:"q"`
			A string `json:"a"`
		}
		var faqOut []map[string]string
		if json.Unmarshal(p.Faq, &faq) == nil {
			for _, f := range faq {
				if strings.TrimSpace(f.Q) != "" && strings.TrimSpace(f.A) != "" {
					faqOut = append(faqOut, map[string]string{"q": f.Q, "a": f.A})
				}
			}
		}
		if len(faqOut) > 0 {
			doc["faq"] = faqOut
		}
		// Everything the page says, for finding the terms and companies it names.
		text := strings.Join([]string{p.Answer, p.Explainer, p.Meaning, strings.Join(kp, " ")}, " ")
		for _, f := range faqOut {
			text += " " + f["q"] + " " + f["a"]
		}
		for _, d := range dev {
			if t, ok := d["text"].(string); ok {
				text += " " + t
			}
		}
		if rel := sectionRelated(db, ix, p, path(p.Slug), text); len(rel) > 0 {
			doc["related"] = rel
		}
		if p.Parent != "" {
			if par, ok := bySlug[p.Parent]; ok {
				doc["parent_name"], doc["parent_path"] = par.Name, path(par.Slug)
			}
		} else if rp, ok := rootParent[p.Section]; ok {
			doc["parent_name"], doc["parent_path"] = rp.Name, rp.Path
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
			VALUES ($1, $2, NULL, $3::jsonb, 1, now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, data = EXCLUDED.data, url_count = 1, updated_at = now()
			WHERE (twoai_pages.data - 'built_at') IS DISTINCT FROM (EXCLUDED.data - 'built_at')`,
			"industries/"+p.Section+"-"+p.Slug+".json", shape, string(j)); err != nil {
			return fmt.Errorf("%s: %w", p.Slug, err)
		}
		written++
	}
	// The roots join the site: Life Sciences lists under Industry Use Cases
	// through its taxonomy row, and the care delivery hub is linked from
	// Healthcare.
	if _, ok := bySlug["lsc"]; ok {
		db.Exec(`UPDATE twoai_taxonomy SET live_path = $1, status = 'live', updated_at = now()
			WHERE slug = 'life-sciences' AND (live_path IS DISTINCT FROM $1 OR status <> 'live')`, path("lsc"))
		if b := line(bySlug["lsc"]); b != "" {
			db.Exec(`UPDATE twoai_taxonomy SET blurb = $1 WHERE slug = 'life-sciences' AND COALESCE(blurb,'') = ''`, b)
		}
	}
	if dp, ok := bySlug["dmed-pipeline"]; ok {
		trail := append(crumbs(dp), ref{Name: dp.Name, Path: path(dp.Slug)})
		written += dmedDrugPages(db, drugs, trail, dp.Name, path(dp.Slug), bySlug["dmed"].Name, path("dmed"), today)
	}
	var subpages []ref
	for _, root := range []string{"hcd", "dmed", "lpa"} {
		if h, ok := bySlug[root]; ok {
			subpages = append(subpages, ref{Name: h.Name, Path: path(root), Blurb: line(h)})
		}
	}
	if len(subpages) > 0 {
		v, _ := json.Marshal(subpages)
		db.Exec(`INSERT INTO twoai_page_extras (page_path, key, value) VALUES ('industries/industry-healthcare.json', 'subpages', $1::jsonb)
			ON CONFLICT (page_path, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
			WHERE twoai_page_extras.value IS DISTINCT FROM EXCLUDED.value`, string(v))
	}

	// Sourced facts, placed on the page behind each target path.
	placed := 0
	db.Exec(`DELETE FROM twoai_page_extras WHERE key = 'sourced_facts'`)
	frows, err := db.Query(`SELECT target_path, claim, source_url, COALESCE(source_title,''), COALESCE(source_date::text,''), COALESCE(topic,'')
		FROM twoai_sourced_facts WHERE status = 'live' ORDER BY target_path, sort, id`)
	if err == nil {
		byPath := map[string][]map[string]string{}
		for frows.Next() {
			var tp, c, u, t, d, topic string
			if frows.Scan(&tp, &c, &u, &t, &d, &topic) == nil {
				byPath[tp] = append(byPath[tp], map[string]string{"claim": c, "source_url": u, "source_title": t, "date": d, "topic": topic})
			}
		}
		frows.Close()
		for tp, items := range byPath {
			pp := extPagePath(db, tp)
			if pp == "" {
				fmt.Printf("twoai_section_pages: no page behind %s, %d facts not placed\n", tp, len(items))
				continue
			}
			v, _ := json.Marshal(map[string]any{"items": items, "as_of": time.Now().UTC().Format("2006-01-02")})
			db.Exec(`INSERT INTO twoai_page_extras (page_path, key, value) VALUES ($1, 'sourced_facts', $2::jsonb)
				ON CONFLICT (page_path, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, pp, string(v))
			placed += len(items)
		}
	}
	fmt.Printf("twoai_section_pages: %d pages, %d sourced facts placed ok=true\n", written, placed)
	return nil
}
