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

const twoaiAmazonTag = "theworldofa0b-20"

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

	// ---- Education: books, courses, certifications from the curated
	// twoai_learning table (all source URLs verified; re-verified daily).
	type learnRow struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Level    string `json:"level"`
		Audience string `json:"audience"`
		Cost     string `json:"cost"`
		Note     string `json:"note"`
		CodeURL  string `json:"code_url,omitempty"`
		Renewal  string `json:"renewal,omitempty"`
		Source   string `json:"source_url"`
		Verified string `json:"verified"`
	}
	for _, sec := range []struct{ slug, path string }{
		{"books", "learn/books.json"},
		{"courses", "learn/courses.json"},
		{"certifications", "learn/certifications.json"},
	} {
		var items []learnRow
		rows, err := db.Query(`SELECT slug, name, provider, level, audience, cost, note,
			COALESCE(code_url,''), COALESCE(renewal,''), source_url, verified_on::text
			FROM twoai_learning WHERE section_slug=$1 ORDER BY slug`, sec.slug)
		if err != nil {
			return count, err
		}
		for rows.Next() {
			var l learnRow
			if rows.Scan(&l.Slug, &l.Name, &l.Provider, &l.Level, &l.Audience, &l.Cost,
				&l.Note, &l.CodeURL, &l.Renewal, &l.Source, &l.Verified) == nil {
				items = append(items, l)
			}
		}
		rows.Close()
		if len(items) == 0 {
			continue
		}
		free := 0
		withCode := 0
		for _, l := range items {
			if strings.HasPrefix(strings.ToLower(l.Cost), "free") {
				free++
			}
			if l.CodeURL != "" {
				withCode++
			}
		}
		name, blurb := taxMeta(sec.slug)
		doc := map[string]any{
			"name": name, "blurb": blurb, "shape": "learning",
			"items": items, "total": len(items), "free": free, "with_code": withCode,
		}
		// The books page also lists the nine SRJ volumes - this site's own
		// publisher - as a clearly labeled separate group, read from the
		// same canonical books data the sources page renders. Labeled, not
		// mixed: the independent list above it stays independent.
		if sec.slug == "books" {
			srjPaths := map[int]string{
				1: "/books/ai-business-services/the-ai-business-enablement-audit/",
				2: "/books/ai-business-services/the-ai-readiness-performance-assessment/",
				3: "/books/ai-business-services/the-ai-risk-governance-review/",
				4: "/books/ai-business-services/the-ai-efficiency-process-optimization/",
				5: "/books/ai-risk-governance-security/the-ai-it-security-audit/",
				6: "/books/ai-risk-governance-security/the-ai-it-security-implementation-strategy/",
				7: "/books/ai-risk-governance-security/the-secure-by-design/",
				8: "/books/ai-risk-governance-security/the-application-security/",
				9: "/books/ai-risk-governance-security/the-cloud-infrastructure-security/",
			}
			var srj []map[string]any
			brows, err := db.Query(`SELECT (b->>'number')::int, b->>'title', b->>'pillar',
				b->>'status', COALESCE(b->>'published',''), COALESCE(b->>'amazon_url','')
				FROM twoai_pages, jsonb_array_elements(data->'books') b
				WHERE path='sources/index.json' ORDER BY (b->>'number')::int`)
			if err == nil {
				for brows.Next() {
					var num int
					var title, pillar, status, published, amazon string
					if brows.Scan(&num, &title, &pillar, &status, &published, &amazon) == nil {
						// Amazon Associates tag for theworldofai.org. Appended only
						// to real Amazon product URLs; forthcoming titles carry no
						// amazon_url and stay untagged. The disclosure renders beside
						// the links, which the operating agreement requires - a policy
						// page on its own does not satisfy it.
						if amazon != "" && strings.Contains(amazon, "amazon.com/") && !strings.Contains(amazon, "tag=") {
							if strings.Contains(amazon, "?") {
								amazon += "&tag=" + twoaiAmazonTag
							} else {
								amazon += "?tag=" + twoaiAmazonTag
							}
						}
						srj = append(srj, map[string]any{
							"number": num, "title": title, "pillar": pillar,
							"status": status, "published": published, "amazon": amazon,
							"url": "https://srjconsultingservices.com" + srjPaths[num],
						})
					}
				}
				brows.Close()
			}
			if len(srj) > 0 {
				doc["srj_books"] = srj
			}
		}
		if err := write(sec.path, sec.slug, doc); err != nil {
			return count, err
		}
		count++
	}

	// ---- Media and Visual Repository: curated videos, podcasts, and
	// openly licensed image/diagram sources, one verified URL each.
	type mediaRow struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Creator  string `json:"creator"`
		Kind     string `json:"kind"`
		Note     string `json:"note"`
		Extra    string `json:"extra"`
		Source   string `json:"source_url"`
		Verified string `json:"verified"`
	}
	for _, sec := range []struct{ slug, path string }{
		{"video-library", "learn/media-video.json"},
		{"podcasts", "learn/media-podcasts.json"},
		{"image-library", "learn/media-images.json"},
	} {
		var items []mediaRow
		rows, err := db.Query(`SELECT slug, name, creator, kind, note, extra, source_url, verified_on::text
			FROM twoai_media WHERE section_slug=$1 ORDER BY slug`, sec.slug)
		if err != nil {
			return count, err
		}
		for rows.Next() {
			var m mediaRow
			if rows.Scan(&m.Slug, &m.Name, &m.Creator, &m.Kind, &m.Note, &m.Extra, &m.Source, &m.Verified) == nil {
				items = append(items, m)
			}
		}
		rows.Close()
		if len(items) == 0 {
			continue
		}
		name, blurb := taxMeta(sec.slug)
		if err := write(sec.path, sec.slug, map[string]any{
			"name": name, "blurb": blurb, "shape": "media",
			"items": items, "total": len(items),
		}); err != nil {
			return count, err
		}
		count++
	}

	// ---- Industry use cases: per-industry sourced points plus honest
	// thinness - a sector with no verifiable primary source beyond the
	// Census adoption series says less rather than inventing case studies.
	type indPoint struct {
		Name   string `json:"name"`
		Desc   string `json:"desc"`
		Source string `json:"source"`
	}
	irows, err := db.Query(`SELECT slug, name, summary, points, verified_on::text FROM twoai_industries ORDER BY slug`)
	if err != nil {
		return count, err
	}
	type ind struct {
		slug, name, summary, verified string
		points                        []indPoint
	}
	var inds []ind
	for irows.Next() {
		var x ind
		var pj string
		if irows.Scan(&x.slug, &x.name, &x.summary, &pj, &x.verified) == nil {
			json.Unmarshal([]byte(pj), &x.points)
			inds = append(inds, x)
		}
	}
	irows.Close()
	for _, x := range inds {
		name, blurb := taxMeta(x.slug)
		if name == "" {
			name = x.name
		}
		if err := write("industries/"+x.slug+".json", x.slug, map[string]any{
			"name": name, "blurb": blurb, "shape": "industry",
			"summary": x.summary, "points": x.points, "verified": x.verified,
			// total lets the ecosystem hub report the depth of the sector
			// page instead of "1" - the one-page-many-items rule that jobs
			// earned. Real estate is 17 sourced points, not one page.
			"total": len(x.points),
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
