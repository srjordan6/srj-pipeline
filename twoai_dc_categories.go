package main

// Data center metric categories as hub pages, theworldofai row 451 (Stephen,
// 2026-10-04: "these should each be a hub").
//
// The 25 metric pages under Data Centers fall into eight categories in
// twoai_dc_metrics (Power efficiency, Cooling and climate, Sustainability,
// Capacity and server use, Uptime and redundancy, Power and grid, Development
// and land, Financials and supply chain). Until now a category was only a
// heading on b441a27b. Each is now a page: a short answer first, its metric
// pages one line each, then the site's live data that belongs to the group
// where it exists. The answers live in twoai_dc_categories, seeded below the
// first time and edited by theworldofai from then on; nothing here overwrites
// an answer that is already there.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// dcCategorySlug and dcCategoryUID name a category page.
func dcCategorySlug(cat string) string {
	return strings.Trim(nonAlnumDash(strings.ToLower(cat)), "-")
}

func nonAlnumDash(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return b.String()
}

func dcCategoryUID(cat string) string { return twoaiUID("dc-category:" + dcCategorySlug(cat)) }

// The first answer for each category, written 2026-10-04 from the metric
// definitions. theworldofai owns the text from here on.
var dcCategorySeed = map[string]string{
	"Power efficiency":            "These measures show how much of the power a data center draws reaches the computing equipment, and how hard each rack is pushed. Dense GPU racks draw far more power than older server halls, so efficiency and density decide how much AI compute a site can run on the power it has.",
	"Cooling and climate":         "These measures show how a data center removes heat and what that costs in energy and water. AI racks now run hot enough that liquid cooling has replaced air at the high end, so cooling sets a hard limit on how dense a facility can be.",
	"Sustainability":              "These measures show the carbon, water and energy sourcing behind a data center's operation. AI demand has made data centers one of the fastest growing users of electricity, so these figures are where climate commitments meet actual consumption.",
	"Capacity and server use":     "These measures show how much of a facility's power and space servers actually use, and how much sits stranded. For AI buildings the question is whether expensive capacity is earning, because idle accelerators and stranded power are paid for either way.",
	"Uptime and redundancy":       "These measures show how reliably a data center keeps its servers running and connected: network quality, backup power and contractual uptime. An AI training run can last weeks across thousands of chips, so an interruption costs more than in most workloads.",
	"Power and grid":              "These measures show how a data center gets its electricity: its place in the interconnection queue, what capacity costs on the market, power generated on site and long-term power contracts. Power is now the main limit on where and how fast AI data centers can be built.",
	"Development and land":        "These measures show what is being built: the megawatts under construction and planned. For AI data centers the construction pipeline is the earliest sign of where new capacity will come online.",
	"Financials and supply chain": "These measures show the money and equipment behind the buildout: what the largest builders spend, what colocation space rents for, and how long critical equipment takes to arrive. Capital spending by a handful of companies sets the pace of AI data center construction.",
}

type dcCatMetric struct {
	Track, Category, Metric, Definition, UID string
	Sort                                     int
}

type dcCatFacility struct {
	UID, Name string
	Profile   json.RawMessage
}

type dcCatBuilder struct {
	Name, End, UID string
	Latest         float64
}

// twoaiDCCategoryHubs writes one page per category and returns the list the
// Data Centers page links, in metric order.
func twoaiDCCategoryHubs(db *sql.DB, today, dcName string, metrics []dcCatMetric,
	facs []dcCatFacility, builders []dcCatBuilder, keep map[string]bool) []map[string]any {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_dc_categories (slug text PRIMARY KEY, name text NOT NULL,
		answer text NOT NULL, updated_at timestamptz DEFAULT now())`)
	for name, answer := range dcCategorySeed {
		db.Exec(`INSERT INTO twoai_dc_categories (slug, name, answer) VALUES ($1,$2,$3) ON CONFLICT (slug) DO NOTHING`,
			dcCategorySlug(name), name, answer)
	}
	answers := map[string]string{}
	if rows, err := db.Query(`SELECT slug, answer FROM twoai_dc_categories`); err == nil {
		for rows.Next() {
			var s, a string
			if rows.Scan(&s, &a) == nil {
				answers[s] = a
			}
		}
		rows.Close()
	}

	var order []string
	byCat := map[string][]dcCatMetric{}
	track := map[string]string{}
	for _, m := range metrics {
		if _, ok := byCat[m.Category]; !ok {
			order = append(order, m.Category)
			track[m.Category] = m.Track
		}
		byCat[m.Category] = append(byCat[m.Category], m)
	}
	parent := map[string]any{"uid": twoaiUID("section:data-centers"), "name": dcName}

	// Live data that belongs to a group.
	live := map[string]map[string]any{}

	// Power and grid: the grid operators' latest observations, the two power
	// topic pages, and the facilities with generation on site.
	{
		pg := map[string]any{"grid_uid": twoaiUID("dc-grid")}
		var grid []map[string]any
		if rows, err := db.Query(`SELECT DISTINCT ON (source) source, value, COALESCE(unit,''), as_of::text
			FROM twoai_grid_obs WHERE metric = 'mw_tracked' ORDER BY source, as_of DESC`); err == nil {
			for rows.Next() {
				var s, u, d string
				var v float64
				if rows.Scan(&s, &v, &u, &d) == nil {
					grid = append(grid, map[string]any{"source": strings.ToUpper(s), "mw": v, "as_of": d})
				}
			}
			rows.Close()
		}
		if len(grid) > 0 {
			pg["grid_obs"] = grid
		}
		var topics []map[string]string
		if rows, err := db.Query(`SELECT uid, title FROM twoai_dc_power_topics WHERE status IN ('ready','live') ORDER BY title`); err == nil {
			for rows.Next() {
				var u, t string
				if rows.Scan(&u, &t) == nil {
					topics = append(topics, map[string]string{"uid": u, "title": t})
				}
			}
			rows.Close()
		}
		if len(topics) > 0 {
			pg["topics"] = topics
		}
		var btm []map[string]any
		for _, f := range facs {
			var p map[string]any
			if json.Unmarshal(f.Profile, &p) != nil {
				continue
			}
			st, _ := p["onsite_generation_status"].(string)
			mw, hasMW := p["onsite_generation_mw"].(float64)
			if st == "" && !hasMW {
				continue
			}
			e := map[string]any{"uid": f.UID, "name": f.Name}
			if hasMW {
				e["mw"] = mw
			}
			if t, _ := p["onsite_generation_type"].(string); t != "" {
				e["type"] = t
			}
			if st != "" {
				e["status"] = st
			}
			btm = append(btm, e)
		}
		if len(btm) > 0 {
			pg["behind_the_meter"] = btm
		}
		live["Power and grid"] = pg
	}

	// Development and land: the planned capacity this site's facility
	// profiles record.
	{
		var planned []map[string]any
		total := 0.0
		for _, f := range facs {
			var p map[string]any
			if json.Unmarshal(f.Profile, &p) != nil {
				continue
			}
			mw, ok := p["planned_it_capacity_mw"].(float64)
			if !ok || mw <= 0 {
				continue
			}
			total += mw
			e := map[string]any{"uid": f.UID, "name": f.Name, "mw": mw}
			if s, _ := p["construction_status"].(string); s != "" {
				e["status"] = s
			}
			planned = append(planned, e)
		}
		sort.Slice(planned, func(i, j int) bool { return planned[i]["mw"].(float64) > planned[j]["mw"].(float64) })
		if len(planned) > 0 {
			top := planned
			if len(top) > 15 {
				top = top[:15]
			}
			live["Development and land"] = map[string]any{"planned_total_mw": total, "planned_count": len(planned), "planned_top": top}
		}
	}

	// Financials and supply chain: the builders' latest quarterly capex.
	if len(builders) > 0 {
		var bl []map[string]any
		for _, b := range builders {
			bl = append(bl, map[string]any{"name": b.Name, "latest": b.Latest, "end": b.End, "uid": b.UID})
		}
		live["Financials and supply chain"] = map[string]any{"capex": bl}
	}

	var hubs []map[string]any
	for _, cat := range order {
		slug := dcCategorySlug(cat)
		uid := dcCategoryUID(cat)
		var ms []map[string]string
		for _, m := range byCat[cat] {
			ms = append(ms, map[string]string{"label": m.Metric, "uid": m.UID, "line": twoaiOneLine(m.Definition)})
		}
		var sibs []map[string]string
		for _, o := range order {
			if o != cat {
				sibs = append(sibs, map[string]string{"name": o, "uid": dcCategoryUID(o)})
			}
		}
		answer := answers[slug]
		doc := map[string]any{
			"shape": "dc-category", "uid": uid, "tax": "data-centers", "generated": today,
			"name": cat, "slug": slug, "track": track[cat], "answer": answer,
			"metrics": ms, "siblings": sibs, "parent": parent,
		}
		if l, ok := live[cat]; ok {
			doc["live"] = l
		}
		path := "tech/dc-cat-" + uid + ".json"
		keep[path] = true
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(j)); err != nil {
			fmt.Printf("twoai_dc_categories: %s: %v\n", cat, err)
			continue
		}
		hubs = append(hubs, map[string]any{"name": cat, "uid": uid, "track": track[cat],
			"line": twoaiOneLine(answer), "count": len(ms)})
	}
	fmt.Printf("twoai_dc_categories: %d category pages ok=true\n", len(hubs))
	return hubs
}
