package main

import (
	"database/sql"
	"regexp"
	"strings"
)

// twoaiGlossaryLinks attaches see_also links and a related list to every
// glossary term in the glossary document, from what the site already holds.
func twoaiGlossaryLinks(db *sql.DB, g map[string]any) {
	terms, ok := g["terms"].([]any)
	if !ok {
		return
	}
	type target struct{ label, name, href string }
	byName := map[string][]target{}
	add := func(name, label, href string) {
		k := strings.ToLower(strings.TrimSpace(name))
		if k == "" || href == "" {
			return
		}
		byName[k] = append(byName[k], target{label, strings.TrimSpace(name), href})
	}
	// Tool pages.
	if rows, err := db.Query(`SELECT p.data->>'name', r.url FROM twoai_pages p JOIN twoai_url_registry r ON r.source_path = p.path
			WHERE p.kind = 'tool' AND COALESCE(p.data->>'name','') <> ''`); err == nil {
		for rows.Next() {
			var n, u string
			if rows.Scan(&n, &u) == nil {
				add(n, "Tool page", u)
			}
		}
		rows.Close()
	}
	// Repositories, listed on their observatory section pages.
	if rows, err := db.Query(`SELECT x->>'name', x->>'repo', p.data->>'name', r.url FROM twoai_pages p
			JOIN twoai_url_registry r ON r.source_path = p.path, jsonb_array_elements(p.data->'repos') x
			WHERE p.kind = 'repo-section'`); err == nil {
		for rows.Next() {
			var n, repo, sec, u string
			if rows.Scan(&n, &repo, &sec, &u) == nil {
				add(n, "Open source repository, in "+sec, u)
				if i := strings.Index(repo, "/"); i > 0 && !strings.EqualFold(repo[i+1:], n) {
					add(repo[i+1:], "Open source repository, in "+sec, u)
				}
			}
		}
		rows.Close()
	}
	// Company pages.
	if rows, err := db.Query(`SELECT name, uid FROM twoai_company_profiles WHERE COALESCE(name,'') <> ''`); err == nil {
		for rows.Next() {
			var n, u string
			if rows.Scan(&n, &u) == nil {
				add(n, "Company page", "/companies/"+u+"/")
			}
		}
		rows.Close()
	}
	// Terms by name, for cross references between definitions.
	type tinfo struct {
		slug, name, cat, text string
		re                    *regexp.Regexp
	}
	infos := make([]tinfo, 0, len(terms))
	for _, raw := range terms {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		name, _ := t["term"].(string)
		slug, _ := t["slug"].(string)
		cat, _ := t["category"].(string)
		def, _ := t["definition"].(string)
		ex, _ := t["example"].(string)
		var re *regexp.Regexp
		if len(name) >= 3 {
			re = regexp.MustCompile(`(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(strings.ToLower(name)) + `([^a-z0-9]|$)`)
		}
		infos = append(infos, tinfo{slug, name, cat, strings.ToLower(def + " " + ex), re})
	}
	mentions := func(text string, o tinfo) bool {
		return o.re != nil && o.re.MatchString(text)
	}
	for _, raw := range terms {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		name, _ := t["term"].(string)
		slug, _ := t["slug"].(string)
		cat, _ := t["category"].(string)
		// see_also: pages on this site for the thing the term names, when
		// the editor has not already set them.
		if _, has := t["see_also"]; !has {
			var sa []map[string]string
			seen := map[string]bool{}
			for _, tg := range byName[strings.ToLower(strings.TrimSpace(name))] {
				if seen[tg.href] {
					continue
				}
				seen[tg.href] = true
				sa = append(sa, map[string]string{"label": tg.label, "name": tg.name, "href": tg.href})
				if len(sa) == 3 {
					break
				}
			}
			if len(sa) > 0 {
				t["see_also"] = sa
			}
		}
		// related: terms that mention this one, terms this one mentions,
		// then category neighbours, six in all.
		var rel []map[string]string
		seen := map[string]bool{slug: true}
		push := func(o tinfo) {
			if seen[o.slug] || len(rel) >= 6 {
				return
			}
			seen[o.slug] = true
			rel = append(rel, map[string]string{"slug": o.slug, "term": o.name})
		}
		var me tinfo
		for _, o := range infos {
			if o.slug == slug {
				me = o
			}
		}
		for _, o := range infos {
			if o.slug != slug && (mentions(o.text, me) || mentions(me.text, o)) {
				push(o)
			}
		}
		for _, o := range infos {
			if o.slug != slug && o.cat == cat {
				push(o)
			}
		}
		t["related"] = rel
	}
}
