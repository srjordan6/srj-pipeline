package main

// ---- twoai_models: the foundation-model catalog and API pricing ------------
//
// Fills twoai_model_catalog from two free public APIs, then renders one page
// per taxonomy section under Technology and Core Infrastructure:
//
//   OpenRouter  https://openrouter.ai/api/v1/models
//     Commercial API models: per-token pricing, context length, modalities,
//     reasoning flag, release date. One request, no key.
//   Hugging Face Hub  https://huggingface.co/api/models?pipeline_tag=...
//     Open-weight models per modality: downloads, likes, licence (from tags),
//     created date. One request per tag, no key.
//
// Every fact on the rendered pages is a field these APIs publish about the
// model; the pages add computed context (medians, licence splits, price
// spreads) rather than editorial claims. Model links go to the original
// home: the Hugging Face model page for open weights, the provider-published
// model record otherwise. Sections render as single catalog pages, not
// per-model stubs, which is a deliberate thin-content decision.
//
// FAILURE MODE. A fetch that errors leaves the previous catalog rows in
// place and renders from them, so an upstream outage ages the pages by a day
// instead of blanking them. Delisting is marked only after its fetch
// succeeds, and rows are never deleted: a delisted model keeps its
// last-known record with delisted_at set.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// twoaiModelSections maps taxonomy section slugs to the Hugging Face
// pipeline tags that feed them. OpenRouter-fed sections (llms,
// reasoning-models, multimodal-models, api-pricing) are handled separately.
var twoaiModelSections = map[string][]string{
	"llms":                    {"text-generation"},
	"multimodal-models":       {"image-text-to-text", "any-to-any"},
	"vision-models":           {"image-classification", "object-detection", "image-segmentation"},
	"image-generation-models": {"text-to-image"},
	"video-models":            {"text-to-video", "image-to-video"},
	"audio-speech-models":     {"automatic-speech-recognition", "text-to-speech"},
	"embedding-models":        {"sentence-similarity"},
	"robotics-models":         {"robotics"},
	"music-models":            {"text-to-audio"},
	"ocr-translation-models":  {"translation", "image-to-text"},
	// Entries below use "pipeline|filter" form: the part before the pipe is
	// a pipeline_tag (may be empty), the part after is a Hub tag filter.
	// Both are the Hub's own classification, so membership is the model
	// authors' claim, stated as such on the pages.
	"coding-models":         {"text-generation|code"},
	"on-device-edge-models": {"text-generation|gguf"},
	"scientific-models":     {"|chemistry", "|biology"},
	"medical-models":        {"|medical"},
	"legal-models":          {"|legal"},
	"financial-models":      {"|finance"},
	// Small language models are a parameter-count cut, not a tag: the
	// fetch expands safetensors metadata and keeps models at or under
	// four billion parameters.
	"small-language-models": {"__slm__"},
}

// hfGetPage fetches one Hub API page and returns the rel="next" link from the
// Link header, which is the Hub's only pagination mechanism. Deeper coverage
// request, 2026-08-29: single-tag sections were saturated at the 500-per-
// request ceiling and read as totals.
func hfGetPage(rawurl string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "theworldofai.org jobs pipeline (contact: stephen@srjconsultingservices.com)")
	resp, err := twoaiJobsClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("GET %s: %d", rawurl, resp.StatusCode)
	}
	next := ""
	for _, part := range strings.Split(resp.Header.Get("Link"), ",") {
		if strings.Contains(part, `rel="next"`) {
			if i := strings.Index(part, "<"); i >= 0 {
				if j := strings.Index(part, ">"); j > i {
					next = part[i+1 : j]
				}
			}
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return body, next, err
}

func twoaiModelsEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_catalog (
		source text NOT NULL,
		ext_id text NOT NULL,
		name text NOT NULL,
		section text NOT NULL,
		data jsonb NOT NULL,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (source, ext_id, section))`)
	if err != nil {
		return err
	}
	// NEVER-DELETE (standing directive 2026-08-19): catalog rows are marked
	// delisted rather than removed, so a model that leaves OpenRouter or drops
	// out of a Hugging Face top-50 remains queryable with the data it last
	// carried. Every live read filters delisted_at IS NULL, so rendered pages
	// are unchanged; the history simply stops being destroyed.
	if _, err = db.Exec(`ALTER TABLE twoai_model_catalog ADD COLUMN IF NOT EXISTS delisted_at timestamptz`); err != nil {
		return err
	}
	// Daily price snapshots per API model, appended once per day the fetch
	// succeeds. History accumulates forever; the pricing page compares the
	// newest day against ~30 days back to report real price moves.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_prices (
		day date NOT NULL,
		model_id text NOT NULL,
		prompt_pm double precision,
		completion_pm double precision,
		PRIMARY KEY (day, model_id))`)
	return err
}

// twoaiModelsFetch refreshes twoai_model_catalog. Errors are reported but the
// caller renders from whatever the table holds.
func twoaiModelsFetch(db *sql.DB) {
	// ---- OpenRouter: one request covers pricing and the API-model set.
	if raw, err := twoaiJobsGet("https://openrouter.ai/api/v1/models", nil); err != nil {
		fmt.Fprintf(os.Stderr, "twoai_models: openrouter fetch failed, rendering last good data: %v\n", err)
	} else {
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Data) == 0 {
			fmt.Fprintf(os.Stderr, "twoai_models: openrouter parse failed or empty, rendering last good data\n")
		} else {
			tx, err := db.Begin()
			if err == nil {
				// Upsert-and-mark, never delete-and-rebuild: a model still listed
				// is refreshed and revived if it had been marked delisted; one no
				// longer listed gains delisted_at and keeps its last-known data.
				n := 0
				current := make([]string, 0, len(body.Data))
				for _, m := range body.Data {
					id, _ := m["id"].(string)
					name, _ := m["name"].(string)
					if id == "" || name == "" {
						continue
					}
					j, _ := json.Marshal(m)
					if _, err := tx.Exec(`INSERT INTO twoai_model_catalog (source, ext_id, name, section, data)
						VALUES ('openrouter', $1, $2, 'api', $3::jsonb)
						ON CONFLICT (source, ext_id, section) DO UPDATE SET name=EXCLUDED.name, data=EXCLUDED.data, fetched_at=now(), delisted_at=NULL`,
						id, name, string(j)); err == nil {
						n++
						current = append(current, id)
					}
				}
				delisted := 0
				if len(current) > 0 {
					if res, err := tx.Exec(`UPDATE twoai_model_catalog SET delisted_at=now()
						WHERE source='openrouter' AND delisted_at IS NULL AND NOT (ext_id = ANY($1))`, pq.Array(current)); err == nil {
						if d, err := res.RowsAffected(); err == nil {
							delisted = int(d)
						}
					}
				}
				if err := tx.Commit(); err == nil {
					fmt.Printf("twoai_models: openrouter %d models, %d newly delisted\n", n, delisted)
					// Today's price snapshot, straight from the rows just
					// written. ON CONFLICT keeps reruns idempotent.
					if _, err := db.Exec(`INSERT INTO twoai_model_prices (day, model_id, prompt_pm, completion_pm)
						SELECT current_date, ext_id,
							NULLIF(data->'pricing'->>'prompt','')::double precision * 1e6,
							NULLIF(data->'pricing'->>'completion','')::double precision * 1e6
						FROM twoai_model_catalog
						WHERE source='openrouter' AND delisted_at IS NULL
						ON CONFLICT (day, model_id) DO NOTHING`); err != nil {
						fmt.Fprintf(os.Stderr, "twoai_models: price snapshot: %v\n", err)
					}
					// Per-provider endpoints for each model: the same model is
					// often served by several operators at different prices and
					// quantizations. One public request per model; failures
					// leave that model's previous endpoints in place.
					got := 0
					for _, id := range current {
						eraw, err := twoaiJobsGet("https://openrouter.ai/api/v1/models/"+id+"/endpoints", nil)
						if err != nil {
							continue
						}
						var eb struct {
							Data struct {
								Endpoints []map[string]any `json:"endpoints"`
							} `json:"data"`
						}
						if json.Unmarshal(eraw, &eb) != nil || len(eb.Data.Endpoints) == 0 {
							continue
						}
						var eps []map[string]any
						for _, e := range eb.Data.Endpoints {
							row := map[string]any{}
							if v, ok := e["provider_name"].(string); ok {
								row["provider"] = v
							}
							if v, ok := e["quantization"].(string); ok && v != "" && v != "unknown" {
								row["quantization"] = v
							}
							if v, ok := e["context_length"].(float64); ok {
								row["context"] = int(v)
							}
							if p, ok := e["pricing"].(map[string]any); ok {
								row["prompt_pm"] = round2(perMillion(p, "prompt"))
								row["completion_pm"] = round2(perMillion(p, "completion"))
							}
							if _, ok := row["provider"]; ok {
								eps = append(eps, row)
							}
						}
						if len(eps) == 0 {
							continue
						}
						ej, _ := json.Marshal(eps)
						if _, err := db.Exec(`UPDATE twoai_model_catalog
							SET data = jsonb_set(data, '{endpoints}', $1::jsonb)
							WHERE source='openrouter' AND ext_id=$2`, string(ej), id); err == nil {
							got++
						}
						time.Sleep(50 * time.Millisecond)
					}
					fmt.Printf("twoai_models: endpoints stored for %d models\n", got)
				}
			}
		}
	}

	// ---- Hugging Face: top models per pipeline tag, by all-time downloads.
	for section, tags := range twoaiModelSections {
		for _, tag := range tags {
			url := "https://huggingface.co/api/models?pipeline_tag=" + tag + "&sort=downloads&direction=-1&limit=500"
			if tag == "__slm__" {
				url = "https://huggingface.co/api/models?pipeline_tag=text-generation&sort=downloads&direction=-1&limit=1000&expand[]=safetensors&expand[]=downloads&expand[]=likes&expand[]=createdAt&expand[]=tags&expand[]=pipeline_tag"
			} else if strings.Contains(tag, "|") {
				parts := strings.SplitN(tag, "|", 2)
				url = "https://huggingface.co/api/models?sort=downloads&direction=-1&limit=500&filter=" + parts[1]
				if parts[0] != "" {
					url += "&pipeline_tag=" + parts[0]
				}
			}
			// Single-tag sections go two pages deep (1,000 models); multi-tag
			// sections already stack to 1,000-1,500 across tags and stay at
			// one page each. The SLM sweep doubles so the four-billion cut
			// draws from a deeper pool. A tag with fewer models than the
			// budget simply runs out of next links and stops.
			pages := 1
			if len(tags) == 1 {
				pages = 2
			}
			var models []map[string]any
			fetchURL := url
			for pg := 0; pg < pages && fetchURL != ""; pg++ {
				raw, next, err := hfGetPage(fetchURL)
				if err != nil {
					fmt.Fprintf(os.Stderr, "twoai_models: hf %s page %d fetch failed: %v\n", tag, pg, err)
					break
				}
				var page []map[string]any
				if err := json.Unmarshal(raw, &page); err != nil || len(page) == 0 {
					break
				}
				models = append(models, page...)
				fetchURL = next
				time.Sleep(400 * time.Millisecond)
			}
			if len(models) == 0 {
				fmt.Fprintf(os.Stderr, "twoai_models: hf %s empty, rendering last good data\n", tag)
				continue
			}
			tx, err := db.Begin()
			if err != nil {
				continue
			}
			// Replace only this tag's rows, keyed on the pipeline_tag we
			// asked for, so one failed tag never wipes a healthy one.
			// What was here before this run, so the log can report what CHANGED
			// rather than how many rows we asked for. Every tag printed "50
			// models" every day because 50 is the API limit, not a finding: the
			// line reported the ceiling and looked identical forever whether the
			// top fifty had churned completely or not moved at all.
			prior := map[string]bool{}
			if pr, err := tx.Query(`SELECT ext_id FROM twoai_model_catalog
				WHERE source='huggingface' AND section=$1 AND data->>'pipeline_tag'=$2 AND delisted_at IS NULL`, section, tag); err == nil {
				for pr.Next() {
					var id string
					if pr.Scan(&id) == nil {
						prior[id] = true
					}
				}
				pr.Close()
			}
			n := 0
			added := 0
			seenNow := map[string]bool{}
			for _, m := range models {
				id, _ := m["id"].(string)
				if id == "" {
					continue
				}
				// The small-language cut keeps only models that declare a
				// parameter count of four billion or fewer; a model that
				// does not publish safetensors metadata cannot qualify.
				if tag == "__slm__" {
					st, _ := m["safetensors"].(map[string]any)
					total, _ := st["total"].(float64)
					if total <= 0 || total > 4e9 {
						continue
					}
					if n >= 50 {
						break
					}
				}
				m["pipeline_tag"] = tag
				j, _ := json.Marshal(m)
				if _, err := tx.Exec(`INSERT INTO twoai_model_catalog (source, ext_id, name, section, data)
					VALUES ('huggingface', $1, $2, $3, $4::jsonb)
					ON CONFLICT (source, ext_id, section) DO UPDATE SET name=EXCLUDED.name, data=EXCLUDED.data, fetched_at=now(), delisted_at=NULL`,
					id, id, section, string(j)); err == nil {
					n++
					seenNow[id] = true
					if !prior[id] {
						added++
					}
				}
			}
			// Rows this run did not see leave the live set but keep their data.
			if len(seenNow) > 0 {
				gone := make([]string, 0, 4)
				for id := range prior {
					if !seenNow[id] {
						gone = append(gone, id)
					}
				}
				if len(gone) > 0 {
					tx.Exec(`UPDATE twoai_model_catalog SET delisted_at=now()
						WHERE source='huggingface' AND section=$1 AND data->>'pipeline_tag'=$2
						  AND delisted_at IS NULL AND ext_id = ANY($3)`, section, tag, pq.Array(gone))
				}
			}
			if err := tx.Commit(); err == nil {
				dropped := 0
				for id := range prior {
					if !seenNow[id] {
						dropped++
					}
				}
				// Silent when the top N has not moved, which on all-time-download
				// rankings is most days. A line now means something happened.
				if added > 0 || dropped > 0 || len(prior) == 0 {
					fmt.Printf("twoai_models: hf %s (%s) %d models, %d new, %d dropped\n",
						tag, section, n, added, dropped)
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// hfLicence pulls "license:x" out of a Hugging Face tag list.
func hfLicence(tags []any) string {
	for _, t := range tags {
		s, _ := t.(string)
		if strings.HasPrefix(s, "license:") {
			return strings.TrimPrefix(s, "license:")
		}
	}
	return ""
}

// perMillion converts OpenRouter's dollars-per-token string to $/1M tokens.
// Returns NaN when the field is absent or non-numeric.
func perMillion(pricing map[string]any, key string) float64 {
	s, _ := pricing[key].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		// OpenRouter marks router pseudo-models with -1: not a price.
		return math.NaN()
	}
	return v * 1e6
}

func medianOf(vals []float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	sort.Float64s(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
}

// round2 keeps JSON small and the page readable; NaN marshals to null via
// the *float64 handling below.
func round2(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return math.Round(v*100) / 100
}

// twoaiModels renders the model-catalog sections. Returns pages written.
func twoaiModels(db *sql.DB, today string) (int, error) {
	if err := twoaiModelsEnsure(db); err != nil {
		return 0, err
	}
	twoaiModelsFetch(db)

	// ---- Load the catalog back out of SQL: SQL is what renders, so a fetch
	// failure above degrades to yesterday's rows rather than empty pages.
	type orModel struct {
		ID, Name, Provider, URL string
		Created                 int64
		Context                 int
		PromptPM, CompletionPM  float64
		Reasoning               bool
		InputMods               []string
		HFID                    string
		Expires, Cutoff         string
	}
	var api []orModel
	endpointsByID := map[string][]map[string]any{}
	rows, err := db.Query(`SELECT data FROM twoai_model_catalog WHERE source='openrouter' AND delisted_at IS NULL`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil {
			continue
		}
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		// openrouter/* entries are routing pseudo-models, not models.
		if strings.HasPrefix(id, "openrouter/") {
			continue
		}
		o := orModel{ID: id, Name: name}
		o.Provider = strings.SplitN(id, "/", 2)[0]
		if c, ok := m["created"].(float64); ok {
			o.Created = int64(c)
		}
		if c, ok := m["context_length"].(float64); ok {
			o.Context = int(c)
		}
		if v, ok := m["expiration_date"].(string); ok {
			o.Expires = v
		}
		if v, ok := m["knowledge_cutoff"].(string); ok {
			o.Cutoff = v
		}
		if eps, ok := m["endpoints"].([]any); ok {
			for _, e := range eps {
				if em, ok := e.(map[string]any); ok {
					endpointsByID[o.ID] = append(endpointsByID[o.ID], em)
				}
			}
		}
		if p, ok := m["pricing"].(map[string]any); ok {
			o.PromptPM = perMillion(p, "prompt")
			o.CompletionPM = perMillion(p, "completion")
		}
		if arch, ok := m["architecture"].(map[string]any); ok {
			if mods, ok := arch["input_modalities"].([]any); ok {
				for _, x := range mods {
					if s, _ := x.(string); s != "" {
						o.InputMods = append(o.InputMods, s)
					}
				}
			}
		}
		// OpenRouter's reasoning field is null for models without the
		// capability and an object (mandatory, default_enabled, efforts) for
		// models with it; some catalog generations used a plain bool. Any
		// object counts as supported.
		switch r := m["reasoning"].(type) {
		case bool:
			o.Reasoning = r
		case map[string]any:
			o.Reasoning = true
			_ = r
		}
		o.HFID, _ = m["hugging_face_id"].(string)
		if links, ok := m["links"].(map[string]any); ok {
			o.URL, _ = links["website"].(string)
		}
		if o.URL == "" && o.HFID != "" {
			o.URL = "https://huggingface.co/" + o.HFID
		}
		if o.URL == "" {
			o.URL = "https://openrouter.ai/" + o.ID
		}
		if o.ID != "" && o.Name != "" {
			api = append(api, o)
		}
	}
	rows.Close()

	type hfModel struct {
		ID, Licence, Tag, Created string
		Downloads, Likes          int
	}
	hfBySection := map[string][]hfModel{}
	rows, err = db.Query(`SELECT section, data FROM twoai_model_catalog WHERE source='huggingface' AND delisted_at IS NULL`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var section, raw string
		if rows.Scan(&section, &raw) != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil {
			continue
		}
		h := hfModel{}
		h.ID, _ = m["id"].(string)
		h.Tag, _ = m["pipeline_tag"].(string)
		if d, ok := m["downloads"].(float64); ok {
			h.Downloads = int(d)
		}
		if l, ok := m["likes"].(float64); ok {
			h.Likes = int(l)
		}
		if c, ok := m["createdAt"].(string); ok && len(c) >= 10 {
			h.Created = c[:10]
		}
		if tags, ok := m["tags"].([]any); ok {
			h.Licence = hfLicence(tags)
		}
		if h.ID != "" {
			hfBySection[section] = append(hfBySection[section], h)
		}
	}
	rows.Close()

	// Section metadata comes from the taxonomy so page copy matches the map.
	taxMeta := func(slug string) (name, blurb string) {
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, slug).Scan(&name, &blurb)
		return
	}
	write := func(slug string, doc map[string]any) error {
		doc["uid"] = twoaiUID("section:" + slug)
		doc["tax"] = slug
		doc["generated"] = today
		j, _ := json.Marshal(doc)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'model-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			"models/"+slug+".json", string(j), slug)
		return err
	}
	count := 0
	var keep []string

	// ---- Model Metadata Standard: the record behind every model row,
	// documented as a page, including what is deliberately not tracked.
	metaName, metaBlurb := taxMeta("model-metadata")
	if err := write("model-metadata", map[string]any{
		"name": metaName, "blurb": metaBlurb, "kind": "standard",
		"open_fields": []map[string]string{
			{"name": "Model ID and page", "source": "Hugging Face Hub", "desc": "The canonical repo id, linking to the model's own page."},
			{"name": "Licence", "source": "Hub tags", "desc": "As declared by the publisher in the model's tags; blank when undeclared."},
			{"name": "Downloads and likes", "source": "Hub", "desc": "All-time downloads and likes at fetch time; ranking key."},
			{"name": "Release date", "source": "Hub createdAt", "desc": "When the repo was created on the Hub, which can trail the announcement."},
			{"name": "Modality", "source": "Hub pipeline tag", "desc": "The task classification the section membership comes from."},
			{"name": "Parameters", "source": "safetensors metadata", "desc": "Only where the publisher ships safetensors with a declared count; used for the Small Language Models cut."},
		},
		"api_fields": []map[string]string{
			{"name": "Provider and model id", "source": "OpenRouter catalogue", "desc": "The serving provider and routed model id."},
			{"name": "Context window", "source": "OpenRouter", "desc": "Maximum context length as listed by the provider."},
			{"name": "Pricing", "source": "OpenRouter", "desc": "Dollars per million input and output tokens at fetch time."},
			{"name": "Reasoning flag and modalities", "source": "OpenRouter", "desc": "Whether the model exposes step-by-step reasoning, and its input modalities."},
		},
		"not_tracked": []map[string]string{
			{"name": "Training data", "why": "Most publishers do not disclose it; this site does not guess."},
			{"name": "Benchmark scores", "why": "Self-reported numbers are not independently verifiable; leaderboards are linked from the research section instead."},
			{"name": "Safety evaluations", "why": "No consistent free primary source exists across publishers yet."},
			{"name": "Valuation-grade adoption claims", "why": "Downloads are the only adoption number a free primary source carries."},
		},
	}); err != nil {
		return count, err
	}
	count++
	keep = append(keep, "models/model-metadata.json")

	// ---- Hugging Face sections: open-weight catalogs per modality.
	hfRow := func(h hfModel) map[string]any {
		return map[string]any{
			"id": h.ID, "url": "https://huggingface.co/" + h.ID,
			"licence": h.Licence, "downloads": h.Downloads, "likes": h.Likes,
			"created": h.Created, "task": h.Tag,
		}
	}
	for slug := range twoaiModelSections {
		list := hfBySection[slug]
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Downloads > list[j].Downloads })
		lic := map[string]int{}
		newest := ""
		totalDl := 0
		for _, h := range list {
			if h.Licence != "" {
				lic[h.Licence]++
			}
			if h.Created > newest {
				newest = h.Created
			}
			totalDl += h.Downloads
		}
		type lc struct {
			Licence string `json:"licence"`
			Count   int    `json:"count"`
		}
		var lics []lc
		for l, n := range lic {
			lics = append(lics, lc{l, n})
		}
		sort.Slice(lics, func(i, j int) bool { return lics[i].Count > lics[j].Count })
		if len(lics) > 6 {
			lics = lics[:6]
		}
		var out []map[string]any
		for _, h := range list {
			out = append(out, hfRow(h))
		}
		name, blurb := taxMeta(slug)
		doc := map[string]any{
			"name": name, "blurb": blurb, "models": out, "total": len(out),
			"downloads_total": totalDl, "licences": lics, "newest": newest,
			"source": "huggingface",
		}
		if err := write(slug, doc); err != nil {
			return count, err
		}
		keep = append(keep, "models/"+slug+".json")
		count++
	}

	// ---- OpenRouter-fed sections.
	if len(api) > 0 {
		sort.Slice(api, func(i, j int) bool { return api[i].Created > api[j].Created })
		apiRow := func(o orModel) map[string]any {
			return map[string]any{
				"id": o.ID, "name": o.Name, "provider": o.Provider, "url": o.URL,
				"context": o.Context, "prompt_pm": round2(o.PromptPM),
				"completion_pm": round2(o.CompletionPM),
				"released":      time.Unix(o.Created, 0).UTC().Format("2006-01-02"),
				"reasoning":     o.Reasoning,
				"multimodal":    len(o.InputMods) > 1,
				"open_weights":  o.HFID != "",
				"knowledge_cutoff": o.Cutoff,
				"expires":          o.Expires,
			}
		}
		stats := func(list []orModel) map[string]any {
			var pIn, pOut []float64
			ctxMax, reason, openW := 0, 0, 0
			providers := map[string]bool{}
			for _, o := range list {
				if !math.IsNaN(o.PromptPM) {
					pIn = append(pIn, o.PromptPM)
				}
				if !math.IsNaN(o.CompletionPM) {
					pOut = append(pOut, o.CompletionPM)
				}
				if o.Context > ctxMax {
					ctxMax = o.Context
				}
				if o.Reasoning {
					reason++
				}
				if o.HFID != "" {
					openW++
				}
				providers[o.Provider] = true
			}
			return map[string]any{
				"median_prompt_pm": round2(medianOf(pIn)), "median_completion_pm": round2(medianOf(pOut)),
				"context_max": ctxMax, "reasoning_count": reason, "open_weight_count": openW,
				"provider_count": len(providers),
			}
		}
		emitAPI := func(slug string, list []orModel) error {
			if len(list) == 0 {
				return nil
			}
			var out []map[string]any
			for _, o := range list {
				out = append(out, apiRow(o))
			}
			name, blurb := taxMeta(slug)
			doc := map[string]any{
				"name": name, "blurb": blurb, "models": out, "total": len(out),
				"stats": stats(list), "source": "openrouter",
			}
			if err := write(slug, doc); err != nil {
				return err
			}
			keep = append(keep, "models/"+slug+".json")
			count++
			return nil
		}
		var textOnly, reasoning, multi []orModel
		for _, o := range api {
			if o.Reasoning {
				reasoning = append(reasoning, o)
			}
			if len(o.InputMods) > 1 {
				multi = append(multi, o)
			} else {
				textOnly = append(textOnly, o)
			}
		}
		// Attach an API-model list to an already-written Hugging Face
		// section page, so one page shows the open weights and the
		// commercial APIs side by side. Falls back to emitting an
		// API-only page when the HF fetch produced nothing for the slug.
		attach := func(slug string, list []orModel) error {
			if _, ok := hfBySection[slug]; !ok {
				return emitAPI(slug, list)
			}
			var out []map[string]any
			for _, o := range list {
				out = append(out, apiRow(o))
			}
			j, _ := json.Marshal(out)
			s, _ := json.Marshal(stats(list))
			_, err := db.Exec(`UPDATE twoai_pages SET
				data = jsonb_set(jsonb_set(data, '{api_models}', $1::jsonb), '{api_stats}', $2::jsonb),
				updated_at = now() WHERE path=$3`,
				string(j), string(s), "models/"+slug+".json")
			return err
		}
		if err := attach("llms", textOnly); err != nil {
			return count, err
		}
		if err := emitAPI("reasoning-models", reasoning); err != nil {
			return count, err
		}
		if err := attach("multimodal-models", multi); err != nil {
			return count, err
		}

		// ---- API pricing: the whole priced catalog, grouped by provider.
		type pr struct {
			Provider string           `json:"provider"`
			Models   []map[string]any `json:"models"`
		}
		byProv := map[string][]orModel{}
		for _, o := range api {
			if math.IsNaN(o.PromptPM) && math.IsNaN(o.CompletionPM) {
				continue
			}
			byProv[o.Provider] = append(byProv[o.Provider], o)
		}
		var provs []pr
		for p, list := range byProv {
			sort.Slice(list, func(i, j int) bool { return list[i].PromptPM < list[j].PromptPM })
			var out []map[string]any
			for _, o := range list {
				out = append(out, apiRow(o))
			}
			provs = append(provs, pr{Provider: p, Models: out})
		}
		sort.Slice(provs, func(i, j int) bool { return provs[i].Provider < provs[j].Provider })
		// Cheapest usable rows the page can lead with: lowest prompt price
		// among models with a large context, and overall.
		var cheapBig, cheapAny *orModel
		freeCount := 0
		for i := range api {
			o := &api[i]
			if math.IsNaN(o.PromptPM) {
				continue
			}
			if o.PromptPM == 0 {
				// Free tiers are real but "cheapest" should name the
				// cheapest PAID model; free is a separate count.
				freeCount++
				continue
			}
			if cheapAny == nil || o.PromptPM < cheapAny.PromptPM {
				cheapAny = o
			}
			if o.Context >= 128000 && (cheapBig == nil || o.PromptPM < cheapBig.PromptPM) {
				cheapBig = o
			}
		}
		name, blurb := taxMeta("api-pricing")
		doc := map[string]any{
			"name": name, "blurb": blurb, "providers": provs, "total": len(api),
			"stats": stats(api), "source": "openrouter", "free_count": freeCount,
		}
		if cheapAny != nil {
			doc["cheapest"] = apiRow(*cheapAny)
		}
		if cheapBig != nil {
			doc["cheapest_128k"] = apiRow(*cheapBig)
		}
		// Scheduled retirements: expiration dates come straight from the
		// routing catalog; nobody else publishes a deprecation calendar.
		// Some providers park a sentinel far-future date (2098-12-31 and the
		// like) in the expiration field to mean "no end date". Those are not
		// retirements, and listing them next to a model shutting down next
		// week would make the calendar useless, so anything more than three
		// years out is treated as the placeholder it is.
		cutoff := time.Now().AddDate(3, 0, 0).Format("2006-01-02")
		var retiring []map[string]any
		for _, o := range api {
			if o.Expires != "" && o.Expires <= cutoff {
				retiring = append(retiring, map[string]any{
					"name": o.Name, "url": o.URL, "provider": o.Provider, "expires": o.Expires,
				})
			}
		}
		sort.Slice(retiring, func(i, j int) bool { return retiring[i]["expires"].(string) < retiring[j]["expires"].(string) })
		if len(retiring) > 0 {
			doc["retiring"] = retiring
		}
		// Same model, different serving prices: models with two or more
		// provider endpoints, ranked by input-price spread. The arbitrage
		// table only OpenRouter's data makes possible.
		type spreadRow struct {
			row    map[string]any
			spread float64
		}
		var spreads []spreadRow
		for i := range api {
			o := &api[i]
			eps := endpointsByID[o.ID]
			if len(eps) < 2 {
				continue
			}
			lo, hi := math.MaxFloat64, 0.0
			for _, e := range eps {
				if v, ok := e["prompt_pm"].(float64); ok && v > 0 {
					if v < lo {
						lo = v
					}
					if v > hi {
						hi = v
					}
				}
			}
			if lo == math.MaxFloat64 || hi <= lo {
				continue
			}
			spreads = append(spreads, spreadRow{map[string]any{
				"name": o.Name, "url": o.URL, "low_pm": round2(lo), "high_pm": round2(hi),
				"endpoints": eps,
			}, hi / lo})
		}
		sort.Slice(spreads, func(i, j int) bool { return spreads[i].spread > spreads[j].spread })
		if len(spreads) > 50 {
			spreads = spreads[:50]
		}
		if len(spreads) > 0 {
			var ms []map[string]any
			for _, r := range spreads {
				ms = append(ms, r.row)
			}
			doc["multi_serving"] = ms
		}
		// Price moves: newest snapshot day against the closest day at least
		// ~30 days back (or the oldest we have). Needs two days of history,
		// so this block appears once the snapshots accumulate.
		if mv, err := db.Query(`
			WITH latest AS (SELECT max(day) d FROM twoai_model_prices),
			base AS (
				SELECT COALESCE(
					(SELECT max(day) FROM twoai_model_prices WHERE day <= (SELECT d FROM latest) - 30),
					(SELECT min(day) FROM twoai_model_prices)) d)
			SELECT a.model_id, a.prompt_pm, b.prompt_pm, (SELECT d FROM base)::text
			FROM twoai_model_prices a
			JOIN twoai_model_prices b ON b.model_id = a.model_id AND b.day = (SELECT d FROM base)
			WHERE a.day = (SELECT d FROM latest) AND (SELECT d FROM base) < (SELECT d FROM latest)
			  AND a.prompt_pm IS NOT NULL AND b.prompt_pm IS NOT NULL
			  AND a.prompt_pm <> b.prompt_pm AND b.prompt_pm > 0`); err == nil {
			byID := map[string]*orModel{}
			for i := range api {
				byID[api[i].ID] = &api[i]
			}
			var moves []map[string]any
			for mv.Next() {
				var id, since string
				var now, then float64
				if mv.Scan(&id, &now, &then, &since) != nil {
					continue
				}
				o := byID[id]
				if o == nil {
					continue
				}
				moves = append(moves, map[string]any{
					"name": o.Name, "url": o.URL, "provider": o.Provider,
					"from_pm": round2(then), "to_pm": round2(now), "since": since,
					"pct": round2((now - then) / then * 100),
				})
			}
			mv.Close()
			sort.Slice(moves, func(i, j int) bool {
				return math.Abs(moves[i]["pct"].(float64)) > math.Abs(moves[j]["pct"].(float64))
			})
			if len(moves) > 60 {
				moves = moves[:60]
			}
			if len(moves) > 0 {
				doc["price_moves"] = moves
			}
		}
		if err := write("api-pricing", doc); err != nil {
			return count, err
		}
		keep = append(keep, "models/api-pricing.json")
		count++

		// ---- Serving providers directory: who actually runs the inference,
		// where they are headquartered, and their policy/status pages. The
		// jurisdiction angle (HQ country, datacenter regions) is governance
		// data no other model catalog joins in.
		if provDoc := twoaiFetchServingProviders(); provDoc != nil {
			name, blurb := taxMeta("serving-providers")
			provDoc["name"], provDoc["blurb"] = name, blurb
			if err := write("serving-providers", provDoc); err != nil {
				return count, err
			}
			keep = append(keep, "models/serving-providers.json")
			count++
		}
	}

	if len(keep) > 0 {
		if _, err := db.Exec(`DELETE FROM twoai_pages
			WHERE kind='model-section' AND NOT (path = ANY($1))`, pq.Array(keep)); err != nil {
			return count, err
		}
	}
	return count, nil
}

// twoaiFetchServingProviders pulls OpenRouter's public providers directory:
// 100+ inference operators with headquarters country, datacenter regions, and
// their terms, privacy, and status URLs. Public endpoint, no key. Returns nil
// on any failure so the page simply keeps yesterday's copy.
func twoaiFetchServingProviders() map[string]any {
	resp, err := http.Get("https://openrouter.ai/api/v1/providers")
	if err != nil {
		fmt.Printf("twoai_models: providers fetch: %v\n", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("twoai_models: providers http %d\n", resp.StatusCode)
		return nil
	}
	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil || len(raw.Data) == 0 {
		return nil
	}
	var provs []map[string]any
	for _, p := range raw.Data {
		row := map[string]any{}
		for src, dst := range map[string]string{
			"name": "name", "slug": "slug", "headquarters": "hq",
			"privacy_policy_url": "privacy", "terms_of_service_url": "terms",
			"status_page_url": "status",
		} {
			if v, ok := p[src].(string); ok && v != "" {
				row[dst] = v
			}
		}
		if dc, ok := p["datacenters"].([]any); ok && len(dc) > 0 {
			var out []string
			for _, d := range dc {
				if s, ok := d.(string); ok {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				row["datacenters"] = out
			}
		}
		if _, ok := row["name"]; ok {
			provs = append(provs, row)
		}
	}
	sort.Slice(provs, func(i, j int) bool {
		return provs[i]["name"].(string) < provs[j]["name"].(string)
	})
	return map[string]any{
		"providers": provs, "total": len(provs),
		"shape": "serving-providers", "source": "openrouter",
	}
}
