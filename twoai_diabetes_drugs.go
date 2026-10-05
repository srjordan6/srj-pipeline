package main

// The diabetes drug database, theworldofai row 483 (Stephen, 2026-10-05).
//
// One row per drug in twoai_diabetes_drugs, written by theworldofai, shown
// as a searchable table on Research Pipeline and Future Treatments
// (dmed-pipeline) with a page of its own per drug. Only rows with status
// 'live' publish, so a drug can be drafted before its sources are checked.
// The table is created here as well as by hand, so a fresh database builds.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

type dmedDrug struct {
	Slug, Name, Maker, Class, DiabetesType, Route, Frequency, FDAStatus, Phase string
	A1C, Weight, EffectSource, CV, Kidney, Summary, Reviewed                   string
	Brands, Targets                                                            []string
	Trials, Sources                                                            json.RawMessage
}

func dmedEnsureDrugs(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_diabetes_drugs (slug text PRIMARY KEY, name text NOT NULL, brands text[], maker text,
		drug_class text, targets text[], diabetes_type text, route text, frequency text, fda_status text, phase text,
		key_trials jsonb DEFAULT '[]'::jsonb, a1c_effect text, weight_effect text, effect_source_url text, cv_evidence text,
		kidney_evidence text, sources jsonb DEFAULT '[]'::jsonb, summary text, status text NOT NULL DEFAULT 'draft',
		last_reviewed date, created_at timestamptz DEFAULT now(), updated_at timestamptz DEFAULT now())`)
}

func dmedLoadDrugs(db *sql.DB) []dmedDrug {
	dmedEnsureDrugs(db)
	rows, err := db.Query(`SELECT slug, name, COALESCE(maker,''), COALESCE(drug_class,''), COALESCE(diabetes_type,''),
			COALESCE(route,''), COALESCE(frequency,''), COALESCE(fda_status,''), COALESCE(phase,''),
			COALESCE(a1c_effect,''), COALESCE(weight_effect,''), COALESCE(effect_source_url,''), COALESCE(cv_evidence,''),
			COALESCE(kidney_evidence,''), COALESCE(summary,''), COALESCE(last_reviewed::text,''),
			COALESCE(brands,'{}'), COALESCE(targets,'{}'), COALESCE(key_trials,'[]'::jsonb)::text, COALESCE(sources,'[]'::jsonb)::text
		FROM twoai_diabetes_drugs WHERE status = 'live' ORDER BY lower(name)`)
	if err != nil {
		fmt.Println("twoai_diabetes_drugs:", err)
		return nil
	}
	defer rows.Close()
	var out []dmedDrug
	for rows.Next() {
		var d dmedDrug
		var trials, sources string
		if rows.Scan(&d.Slug, &d.Name, &d.Maker, &d.Class, &d.DiabetesType, &d.Route, &d.Frequency, &d.FDAStatus, &d.Phase,
			&d.A1C, &d.Weight, &d.EffectSource, &d.CV, &d.Kidney, &d.Summary, &d.Reviewed,
			pq.Array(&d.Brands), pq.Array(&d.Targets), &trials, &sources) == nil {
			d.Trials, d.Sources = json.RawMessage(trials), json.RawMessage(sources)
			out = append(out, d)
		}
	}
	return out
}

func dmedDrugUID(slug string) string { return twoaiUID("dmed-drug:" + slug) }

// dmedDrugAnswer is the opening line when theworldofai has not written one:
// the drug's own record, said plainly.
func dmedDrugAnswer(d dmedDrug) string {
	if strings.TrimSpace(d.Summary) != "" {
		return d.Summary
	}
	s := d.Name
	if len(d.Brands) > 0 {
		s += " (" + strings.Join(d.Brands, ", ") + ")"
	}
	switch {
	case d.Class != "" && d.Maker != "":
		s += " is a " + d.Class + " made by " + d.Maker + "."
	case d.Class != "":
		s += " is a " + d.Class + "."
	case d.Maker != "":
		s += " is made by " + d.Maker + "."
	default:
		s += "."
	}
	if d.FDAStatus != "" {
		s += " FDA status: " + d.FDAStatus + "."
	}
	return s
}

// dmedDrugTable is the row list for the searchable table on dmed-pipeline.
func dmedDrugTable(drugs []dmedDrug) []map[string]any {
	var out []map[string]any
	for _, d := range drugs {
		out = append(out, map[string]any{
			"name": d.Name, "path": sectionBase + dmedDrugUID(d.Slug) + "/", "brands": strings.Join(d.Brands, ", "),
			"maker": d.Maker, "class": d.Class, "type": d.DiabetesType, "route": d.Route,
			"fda_status": d.FDAStatus, "phase": d.Phase,
		})
	}
	return out
}

// dmedDrugPages writes one page per live drug under dmed-pipeline and removes
// the pages of drugs no longer live. crumbs is the trail down to and
// including dmed-pipeline.
func dmedDrugPages(db *sql.DB, drugs []dmedDrug, crumbs any, parentName, parentPath, hubName, hubPath, today string) int {
	keep := []string{}
	written := 0
	for _, d := range drugs {
		uid := dmedDrugUID(d.Slug)
		var sibs []map[string]string
		for _, o := range drugs {
			if o.Slug != d.Slug {
				sibs = append(sibs, map[string]string{"name": o.Name, "path": sectionBase + dmedDrugUID(o.Slug) + "/"})
			}
		}
		var trials, sources []map[string]any
		json.Unmarshal(d.Trials, &trials)
		json.Unmarshal(d.Sources, &sources)
		gen := today
		if d.Reviewed != "" {
			gen = d.Reviewed
		}
		drug := map[string]any{
			"brands": d.Brands, "maker": d.Maker, "class": d.Class, "targets": d.Targets, "type": d.DiabetesType,
			"route": d.Route, "frequency": d.Frequency, "fda_status": d.FDAStatus, "phase": d.Phase,
			"trials": trials, "a1c": d.A1C, "weight": d.Weight, "effect_source": d.EffectSource,
			"cv": d.CV, "kidney": d.Kidney, "sources": sources,
		}
		doc := map[string]any{
			"uid": uid, "page_uid": uid, "slug": "dmed-drug-" + d.Slug, "shape": "art-topic",
			"category": "enterprise-applications-governance-and-tools",
			"name":     d.Name, "title": d.Name, "blurb": twoaiOneLine(dmedDrugAnswer(d)), "answer": dmedDrugAnswer(d),
			"crumbs": crumbs, "parent_name": parentName, "parent_path": parentPath, "hub_name": hubName, "hub_path": hubPath,
			"siblings": sibs, "generated": gen, "last_reviewed": d.Reviewed, "refresh_every_days": 7,
			"drafted": true, "medical_note": true, "expanded": true, "drug": drug,
		}
		j, _ := json.Marshal(doc)
		p := "industries/dmed-drug-" + d.Slug + ".json"
		keep = append(keep, p)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
			VALUES ($1, 'art-topic', NULL, $2::jsonb, 1, now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, data = EXCLUDED.data, url_count = 1, updated_at = now()
			WHERE (twoai_pages.data - 'built_at') IS DISTINCT FROM (EXCLUDED.data - 'built_at')`, p, string(j)); err == nil {
			written++
		}
	}
	db.Exec(`DELETE FROM twoai_pages WHERE path LIKE 'industries/dmed-drug-%' AND NOT (path = ANY($1))`, pq.Array(keep))
	return written
}
