package main

// twoai_dc_topics: topic pages under the Data Centers section (b441a27b).
//
// theworldofai bridge row 399 (Stephen, 2026-10-03): cover more of the power
// issue, and look closely at Tesla Megapacks. The content is theirs, written
// into twoai_dc_power_topics (answer, sections, sources, related); this step
// renders it. Shape dc-topic, so later power pages (nuclear and small modular
// reactors, gas turbines, interconnection queues) use the same table and the
// same template. Rows with status ready or live are published.
//
// The related field is plain text, "companies: Tesla, xAI, SpaceX". Each name
// is resolved to a page that exists: a company page (name or alias), a
// Data Centers facility page, a glossary term, another topic, or a compliance
// page. A name with no page stays plain text in the doc's unresolved list
// rather than becoming a link to nowhere.
//
// Pages are tech/dc-topic-<uid>.json, rendered at
// /ai-ecosystem/technology-and-core-infrastructure/<uid>/. The Data Centers
// page, the facility pages and the company pages find the topics that link
// them by reading these files, so no other document is patched.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type dcLink struct {
	Label string `json:"label"`
	Href  string `json:"href"`
	Kind  string `json:"kind"`
}

var dcDigitRe = regexp.MustCompile(`\d+`)

func twoaiDCTopics(db *sql.DB, today string) int {
	rows, err := db.Query(`SELECT slug, uid, title, coalesce(answer,''), coalesce(sections,'[]'::jsonb)::text, coalesce(sources,'[]'::jsonb)::text,
			coalesce(related,'[]'::jsonb)::text, coalesce(written_on::text,'')
		FROM twoai_dc_power_topics WHERE status IN ('ready','live') ORDER BY written_on, slug`)
	if err != nil {
		// The table is theirs and may not exist on a fresh database.
		return 0
	}
	type topic struct{ slug, uid, title, answer, sections, sources, related, written string }
	var topics []topic
	for rows.Next() {
		var t topic
		if rows.Scan(&t.slug, &t.uid, &t.title, &t.answer, &t.sections, &t.sources, &t.related, &t.written) == nil {
			topics = append(topics, t)
		}
	}
	rows.Close()
	if len(topics) == 0 {
		return 0
	}
	companies := famCompanies(db)
	// Facilities, glossary terms and compliance pages, loaded once.
	type named struct{ name, href string }
	var facilities, glossary, compliance []named
	if r, err := db.Query(`SELECT data->>'uid', coalesce(data->>'name','') FROM twoai_pages WHERE kind='tech-dc-child' AND data->>'shape'='dc-facility' AND data->>'uid' IS NOT NULL`); err == nil {
		for r.Next() {
			var u, n string
			if r.Scan(&u, &n) == nil {
				facilities = append(facilities, named{n, famBase + u + "/"})
			}
		}
		r.Close()
	}
	if r, err := db.Query(`SELECT t->>'term', t->>'slug' FROM site_content s, jsonb_array_elements(s.data->'terms') t WHERE s.path='resources/glossary.json'`); err == nil {
		for r.Next() {
			var n, s string
			if r.Scan(&n, &s) == nil && s != "" {
				glossary = append(glossary, named{n, "/ai-glossary/" + s + "/"})
			}
		}
		r.Close()
	}
	if r, err := db.Query(`SELECT replace(replace(path,'compliance/',''),'.json',''), coalesce(data->>'name', data->>'title','') FROM twoai_pages WHERE path LIKE 'compliance/%' AND path NOT LIKE 'compliance/law-%'`); err == nil {
		for r.Next() {
			var s, n string
			if r.Scan(&s, &n) == nil && n != "" {
				compliance = append(compliance, named{n, "/ai-compliance/" + s + "/"})
			}
		}
		r.Close()
	}
	norm := func(s string) string {
		s = strings.ToLower(s)
		s = strings.NewReplacer("-", " ", "centre", "center", "(", " ", ")", " ", ",", " ").Replace(s)
		return strings.Join(strings.Fields(s), " ")
	}
	// A facility matches when every distinctive word of the reference is in
	// its name and the numbers agree: "Colossus 1" is the one with no number
	// or a 1, "Colossus 2" the one with a 2.
	facility := func(ref string) *named {
		r := norm(ref)
		num := dcDigitRe.FindString(r)
		var best *named
		for i := range facilities {
			n := norm(facilities[i].name)
			key := dcDigitRe.ReplaceAllString(r, "")
			words := 0
			ok := true
			for _, w := range strings.Fields(key) {
				if len(w) < 4 || w == "memphis" || w == "southaven" {
					continue
				}
				words++
				if !strings.Contains(n, w) {
					ok = false
				}
			}
			if !ok || words == 0 {
				continue
			}
			fnum := dcDigitRe.FindString(n)
			if num != "" && fnum != num && !(num == "1" && fnum == "") {
				continue
			}
			if num == "" && fnum != "" {
				continue
			}
			best = &facilities[i]
		}
		return best
	}
	byPrefix := func(list []named, ref string) *named {
		r := norm(ref)
		for i := range list {
			n := norm(list[i].name)
			if n == r || strings.HasPrefix(n, r+" ") || strings.HasPrefix(r, n+" ") || strings.Contains(n, r) {
				return &list[i]
			}
		}
		return nil
	}
	written := 0
	for _, t := range topics {
		var related []string
		json.Unmarshal([]byte(t.related), &related)
		links, unresolved := []dcLink{}, []string{}
		seen := map[string]bool{}
		add := func(l dcLink) {
			if !seen[l.Href] {
				seen[l.Href] = true
				links = append(links, l)
			}
		}
		for _, line := range related {
			kind, list := "", line
			if i := strings.Index(line, ":"); i > 0 {
				kind, list = strings.ToLower(strings.TrimSpace(line[:i])), line[i+1:]
			}
			list = strings.ReplaceAll(list, " and ", ", ")
			for _, item := range strings.Split(list, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				var hit *dcLink
				switch kind {
				case "companies":
					if c, ok := companies[strings.ToLower(item)]; ok {
						hit = &dcLink{c.Name, "/companies/" + c.UID + "/", "company"}
					}
				case "facilities":
					if f := facility(item); f != nil {
						hit = &dcLink{f.name, f.href, "facility"}
					}
				case "glossary":
					if g := byPrefix(glossary, item); g != nil {
						hit = &dcLink{g.name, g.href, "glossary"}
					}
				case "pages":
					for _, o := range topics {
						if o.uid != t.uid && strings.HasPrefix(norm(o.title), norm(item)) {
							hit = &dcLink{o.title, famBase + o.uid + "/", "topic"}
						}
					}
					if hit == nil {
						if cp := byPrefix(compliance, item); cp != nil {
							hit = &dcLink{cp.name, cp.href, "compliance"}
						}
					}
				}
				if hit != nil {
					add(*hit)
				} else {
					unresolved = append(unresolved, item)
				}
			}
		}
		var sections, sources any
		json.Unmarshal([]byte(t.sections), &sections)
		json.Unmarshal([]byte(t.sources), &sources)
		doc := map[string]any{
			"shape": "dc-topic", "uid": t.uid, "slug": t.slug, "name": t.title, "title": t.title,
			"answer": t.answer, "sections": sections, "sources": sources,
			"related_links": links, "unresolved": unresolved,
			"parent_path": famBase + "b441a27b/", "parent_name": "Data Centers",
			"written_on": t.written, "generated": today, "refresh_days": 30,
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count) VALUES ($1,'tech-section',$2::jsonb,NULL,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()`,
			"tech/dc-topic-"+t.uid+".json", string(j)); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_dc_topics:", err)
			continue
		}
		written++
		fmt.Printf("twoai_dc_topics: %s (%s) links=%d unresolved=%v\n", t.slug, t.uid, len(links), unresolved)
	}
	fmt.Printf("twoai_dc_topics: written=%d of %d ok=true\n", written, len(topics))
	return written
}
