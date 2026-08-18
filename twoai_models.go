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
// instead of blanking them. The per-source delete happens only after its
// fetch succeeds.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
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

func twoaiModelsEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_catalog (
		source text NOT NULL,
		ext_id text NOT NULL,
		name text NOT NULL,
		section text NOT NULL,
		data jsonb NOT NULL,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (source, ext_id, section))`)
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
				tx.Exec(`DELETE FROM twoai_model_catalog WHERE source='openrouter'`)
				n := 0
				for _, m := range body.Data {
					id, _ := m["id"].(string)
					name, _ := m["name"].(string)
					if id == "" || name == "" {
						continue
					}
					j, _ := json.Marshal(m)
					if _, err := tx.Exec(`INSERT INTO twoai_model_catalog (source, ext_id, name, section, data)
						VALUES ('openrouter', $1, $2, 'api', $3::jsonb)
						ON CONFLICT (source, ext_id, section) DO UPDATE SET name=EXCLUDED.name, data=EXCLUDED.data, fetched_at=now()`,
						id, name, string(j)); err == nil {
						n++
					}
				}
				if err := tx.Commit(); err == nil {
					fmt.Printf("twoai_models: openrouter %d models\n", n)
				}
			}
		}
	}

	// ---- Hugging Face: top models per pipeline tag, by all-time downloads.
	for section, tags := range twoaiModelSections {
		for _, tag := range tags {
			url := "https://huggingface.co/api/models?pipeline_tag=" + tag + "&sort=downloads&direction=-1&limit=50"
			if tag == "__slm__" {
				url = "https://huggingface.co/api/models?pipeline_tag=text-generation&sort=downloads&direction=-1&limit=100&expand[]=safetensors&expand[]=downloads&expand[]=likes&expand[]=createdAt&expand[]=tags&expand[]=pipeline_tag"
			} else if strings.Contains(tag, "|") {
				parts := strings.SplitN(tag, "|", 2)
				url = "https://huggingface.co/api/models?sort=downloads&direction=-1&limit=50&filter=" + parts[1]
				if parts[0] != "" {
					url += "&pipeline_tag=" + parts[0]
				}
			}
			raw, err := twoaiJobsGet(url, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "twoai_models: hf %s fetch failed, rendering last good data: %v\n", tag, err)
				continue
			}
			var models []map[string]any
			if err := json.Unmarshal(raw, &models); err != nil || len(models) == 0 {
				fmt.Fprintf(os.Stderr, "twoai_models: hf %s parse failed or empty, rendering last good data\n", tag)
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
				WHERE source='huggingface' AND section=$1 AND data->>'pipeline_tag'=$2`, section, tag); err == nil {
				for pr.Next() {
					var id string
					if pr.Scan(&id) == nil {
						prior[id] = true
					}
				}
				pr.Close()
			}
			tx.Exec(`DELETE FROM twoai_model_catalog WHERE source='huggingface' AND section=$1 AND data->>'pipeline_tag'=$2`, section, tag)
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
					ON CONFLICT (source, ext_id, section) DO UPDATE SET name=EXCLUDED.name, data=EXCLUDED.data, fetched_at=now()`,
					id, id, section, string(j)); err == nil {
					n++
					seenNow[id] = true
					if !prior[id] {
						added++
					}
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
	}
	var api []orModel
	rows, err := db.Query(`SELECT data FROM twoai_model_catalog WHERE source='openrouter'`)
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
	rows, err = db.Query(`SELECT section, data FROM twoai_model_catalog WHERE source='huggingface'`)
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
		if err := write("api-pricing", doc); err != nil {
			return count, err
		}
		keep = append(keep, "models/api-pricing.json")
		count++
	}

	if len(keep) > 0 {
		if _, err := db.Exec(`DELETE FROM twoai_pages
			WHERE kind='model-section' AND NOT (path = ANY($1))`, pq.Array(keep)); err != nil {
			return count, err
		}
	}
	return count, nil
}
