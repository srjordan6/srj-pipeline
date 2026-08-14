package main

// twoai_hardware.go - AI Infrastructure and Hardware (7 curated sections)
// plus AI Datasets (2 live-stat sections + 1 licensing page).
//
// Hardware rows are curated in twoai_hardware, one verified vendor or
// primary source URL each (the daily re-verification pass in
// twoai_apistatus keeps verified_on honest). Dataset rows are curated ids
// in twoai_dataset_catalog whose downloads, licence, and gating are
// re-fetched from the Hugging Face API every run - the curation says what
// belongs, the Hub says what is current.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var twoaiHardwareSections = []string{
	"gpus", "npus-tpus", "chip-fabs", "memory-and-storage",
	"datacenters", "networking-fabric", "power-and-cooling",
}

func twoaiHardware(db *sql.DB, today string) (int, error) {
	count := 0

	write := func(path, slug string, doc map[string]any) error {
		doc["uid"] = twoaiUID("section:" + slug)
		doc["tax"] = slug
		doc["generated"] = today
		j, _ := json.Marshal(doc)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(j), slug)
		return err
	}
	taxMeta := func(slug string) (name, blurb string) {
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, slug).Scan(&name, &blurb)
		return
	}

	// ---- Hardware sections from the curated table.
	type hwRow struct {
		Slug      string `json:"slug"`
		Name      string `json:"name"`
		Maker     string `json:"maker"`
		Note      string `json:"note"`
		SourceURL string `json:"source_url"`
		EntityUID string `json:"entity_uid,omitempty"`
		Verified  string `json:"verified"`
	}
	for _, sec := range twoaiHardwareSections {
		var items []hwRow
		rows, err := db.Query(`SELECT slug, name, maker, note, source_url, COALESCE(entity_uid,''), verified_on::text
			FROM twoai_hardware WHERE section_slug=$1 ORDER BY slug`, sec)
		if err != nil {
			return count, err
		}
		for rows.Next() {
			var h hwRow
			if rows.Scan(&h.Slug, &h.Name, &h.Maker, &h.Note, &h.SourceURL, &h.EntityUID, &h.Verified) == nil {
				if h.EntityUID == "" {
					db.QueryRow(`SELECT c->>'uid' FROM twoai_pages, jsonb_array_elements(data->'companies') c
						WHERE path='companies/index.json' AND lower(c->>'name')=lower($1) LIMIT 1`,
						h.Maker).Scan(&h.EntityUID)
				}
				items = append(items, h)
			}
		}
		rows.Close()
		if len(items) == 0 {
			continue
		}
		name, blurb := taxMeta(sec)
		if err := write("tech/hw-"+sec+".json", sec, map[string]any{
			"name": name, "blurb": blurb, "shape": "hardware",
			"items": items, "total": len(items),
		}); err != nil {
			return count, err
		}
		count++
	}

	// ---- Datasets: refresh live stats per curated id, then render.
	client := &http.Client{Timeout: 20 * time.Second}
	ids, err := db.Query(`SELECT section_slug, hf_id FROM twoai_dataset_catalog`)
	if err != nil {
		return count, err
	}
	type key struct{ sec, id string }
	var keys []key
	for ids.Next() {
		var k key
		if ids.Scan(&k.sec, &k.id) == nil {
			keys = append(keys, k)
		}
	}
	ids.Close()
	for _, k := range keys {
		req, _ := http.NewRequest("GET", "https://huggingface.co/api/datasets/"+k.id, nil)
		req.Header.Set("User-Agent", "theworldofai.org pipeline (contact: info@srjconsultingservices.com)")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_hardware: dataset %s fetch failed, keeping last stats: %v\n", k.id, err)
			continue
		}
		var d struct {
			Downloads int64    `json:"downloads"`
			Gated     any      `json:"gated"`
			Tags      []string `json:"tags"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		if resp.StatusCode != 200 || derr != nil {
			fmt.Fprintf(os.Stderr, "twoai_hardware: dataset %s status %d, keeping last stats\n", k.id, resp.StatusCode)
			continue
		}
		lic := ""
		for _, t := range d.Tags {
			if strings.HasPrefix(t, "license:") {
				lic = strings.TrimPrefix(t, "license:")
				break
			}
		}
		gated := false
		switch g := d.Gated.(type) {
		case bool:
			gated = g
		case string:
			gated = g != ""
		}
		db.Exec(`UPDATE twoai_dataset_catalog SET downloads=$1, licence=$2, gated=$3, fetched_at=now()
			WHERE section_slug=$4 AND hf_id=$5`, d.Downloads, lic, gated, k.sec, k.id)
		time.Sleep(300 * time.Millisecond)
	}

	type dsRow struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		Note      string `json:"note"`
		Downloads int64  `json:"downloads"`
		Licence   string `json:"licence"`
		Gated     bool   `json:"gated"`
	}
	renderDS := func(sec, path string) error {
		var items []dsRow
		rows, err := db.Query(`SELECT hf_id, note, downloads, licence, gated FROM twoai_dataset_catalog
			WHERE section_slug=$1 ORDER BY downloads DESC`, sec)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r dsRow
			if rows.Scan(&r.ID, &r.Note, &r.Downloads, &r.Licence, &r.Gated) == nil {
				r.URL = "https://huggingface.co/datasets/" + r.ID
				items = append(items, r)
			}
		}
		rows.Close()
		if len(items) == 0 {
			return nil
		}
		name, blurb := taxMeta(sec)
		if err := write(path, sec, map[string]any{
			"name": name, "blurb": blurb, "shape": "datasets",
			"items": items, "total": len(items),
		}); err != nil {
			return err
		}
		count++
		return nil
	}
	if err := renderDS("training-datasets", "tech/ds-training.json"); err != nil {
		return count, err
	}
	if err := renderDS("evaluation-datasets", "tech/ds-evaluation.json"); err != nil {
		return count, err
	}

	// ---- Licensing and provenance: computed licence split across the
	// curated catalogue plus sourced context points.
	type licSplit struct {
		Licence string `json:"licence"`
		Count   int    `json:"count"`
	}
	var splits []licSplit
	if rows, err := db.Query(`SELECT CASE WHEN licence='' THEN 'undeclared' ELSE licence END, count(*)
		FROM twoai_dataset_catalog GROUP BY 1 ORDER BY 2 DESC, 1`); err == nil {
		for rows.Next() {
			var l licSplit
			if rows.Scan(&l.Licence, &l.Count) == nil {
				splits = append(splits, l)
			}
		}
		rows.Close()
	}
	var gatedN, totalN int
	db.QueryRow(`SELECT count(*) FILTER (WHERE gated), count(*) FROM twoai_dataset_catalog`).Scan(&gatedN, &totalN)
	ln, lb := taxMeta("dataset-licensing")
	if err := write("tech/ds-licensing.json", "dataset-licensing", map[string]any{
		"name": ln, "blurb": lb, "shape": "ds-licensing",
		"splits": splits, "gated": gatedN, "total": totalN,
		"points": []map[string]string{
			{"name": "Licence and permission are different questions", "source": "https://huggingface.co/datasets/HuggingFaceFW/fineweb",
				"desc": "A dataset's licence covers the compilation; the underlying web text keeps its own copyrights. FineWeb ships under ODC-By while the crawled pages remain their authors' - which is exactly the distinction being litigated."},
			{"name": "The provenance disputes are on this site's own tracker", "source": "https://theworldofai.org/ai-lawsuits/",
				"desc": "Whether training on copyrighted text is fair use is the live question in the AI copyright docket - NYT v. OpenAI, Authors Guild, Bartz v. Anthropic and the rest are tracked with current status on the lawsuit tracker."},
			{"name": "Gating is provenance control", "source": "https://huggingface.co/datasets/bigcode/the-stack",
				"desc": "The Stack gates access and runs an opt-out process for code authors; GPQA gates to keep benchmark answers out of training corpora. Access control is doing licence work that licences alone cannot."},
			{"name": "Documentation is the differentiator", "source": "https://huggingface.co/datasets/allenai/dolma",
				"desc": "Dolma ships a datasheet documenting sources, filtering, and decisions - the practice the field is converging on, and the reason fully documented corpora anchor this catalogue."},
		},
	}); err != nil {
		return count, err
	}
	count++
	return count, nil
}
