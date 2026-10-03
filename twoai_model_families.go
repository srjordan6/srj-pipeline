package main

// twoai_model_families: one page per model family, under each model category.
//
// theworldofai bridge rows 386 to 389 (Stephen, 2026-10-02). A family is a
// developer's named model line with every version and size under it: OpenAI
// GPT, Anthropic Claude, Z.ai GLM, Qwen. Versions and tiers are members, not
// families. Billing aliases (:free, :batch) and rolling pointers (~vendor/...,
// ...-latest) are not members. A family needs two or more members.
//
// One category at a time, starting with Large Language Models (87868942):
// the ten families in it that are newest by their newest member, frozen on
// the day they are chosen (LIFO, no backfill). After that, a model added to
// the catalog joins its family's page on the next run, and a line that gains
// its second member after the freeze becomes a new family. Stephen wants it
// slow: at most three new family pages a run, each with its reading, so the
// Ollama budget stays with the CVE headlines and the story lines. When the
// first ten are live, a bridge row asks theworldofai to have Stephen look
// before the next category (Reasoning Models) starts. That category is not
// built here until he says so.
//
// Family pages are tech/family-<uid>.json, kind tech-section, shape
// model-family, uid twoaiUID("family:"+key), rendered by the technology
// route at /ai-ecosystem/technology-and-core-infrastructure/<uid>/. The LLM
// section row (models/llms.json) gains families[] and family_of{} so the
// category page lists them and each model in its table links its family.

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const famBase = "/ai-ecosystem/technology-and-core-infrastructure/"

// famEnabled: on since twoai-site bf7d821 renders shape model-family.
const famEnabled = true

type famMember struct {
	ID, Name, Released, Cutoff, Expires, HFID, Tokenizer, Instruct string
	Context, MaxOut                                                int64
	PromptPM, CompletionPM                                         float64
	Reasoning, Delisted                                            bool
	In, Out                                                        []string
	Created                                                        int64
	Providers                                                      []map[string]any
}

type famGroup struct {
	Key, Dev, Line, DevName, LineName string
	Members                           []famMember
}

var famVersionTail = regexp.MustCompile(`[0-9.]+.*$`)
var famParamRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([bt])$`)

// famKey gives the family of a catalog id, or "" for a pointer or alias.
func famKey(id string) (dev, line string) {
	if strings.HasPrefix(id, "~") || strings.Contains(id, ":") || strings.HasSuffix(id, "-latest") || strings.Contains(id, "-latest-") {
		return "", ""
	}
	i := strings.Index(id, "/")
	if i <= 0 {
		return "", ""
	}
	dev = id[:i]
	slug := id[i+1:]
	first := strings.SplitN(slug, "-", 2)[0]
	line = famVersionTail.ReplaceAllString(first, "")
	if line == "" {
		return "", ""
	}
	return dev, line
}

// famLineName is the display name of the line from a member's catalog name:
// "OpenAI: GPT-6.1 Sol" gives GPT, "Xiaomi: MiMo-V2.6-Pro" gives MiMo.
func famLineName(name string) string {
	if i := strings.Index(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	w := strings.Fields(strings.TrimSpace(name))
	if len(w) == 0 {
		return ""
	}
	first := strings.SplitN(w[0], "-", 2)[0]
	return famVersionTail.ReplaceAllString(first, "")
}

func famDevName(name, dev string) string {
	if i := strings.Index(name, ":"); i > 0 {
		return strings.TrimSpace(name[:i])
	}
	if dev == "" {
		return ""
	}
	return strings.ToUpper(dev[:1]) + dev[1:]
}

func (g *famGroup) displayName() string {
	if strings.EqualFold(g.DevName, g.LineName) || g.DevName == "" {
		return g.LineName
	}
	return g.DevName + " " + g.LineName
}

// famLoad reads the OpenRouter catalog into families of text-output models.
func famLoad(db *sql.DB) map[string]*famGroup {
	out := map[string]*famGroup{}
	rows, err := db.Query(`SELECT ext_id, name, data::text, delisted_at IS NOT NULL FROM twoai_model_catalog WHERE source='openrouter'`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_model_families load:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, raw string
		var delisted bool
		if rows.Scan(&id, &name, &raw, &delisted) != nil {
			continue
		}
		dev, line := famKey(id)
		if dev == "" {
			continue
		}
		var d struct {
			Created         int64   `json:"created"`
			ContextLength   int64   `json:"context_length"`
			KnowledgeCutoff *string `json:"knowledge_cutoff"`
			ExpirationDate  *string `json:"expiration_date"`
			HuggingFaceID   *string `json:"hugging_face_id"`
			Reasoning       any     `json:"reasoning"`
			Pricing         struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			TopProvider struct {
				MaxCompletion int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Architecture struct {
				Tokenizer    string   `json:"tokenizer"`
				InstructType *string  `json:"instruct_type"`
				In           []string `json:"input_modalities"`
				Out          []string `json:"output_modalities"`
			} `json:"architecture"`
			Endpoints []map[string]any `json:"endpoints"`
		}
		if json.Unmarshal([]byte(raw), &d) != nil || d.Created == 0 {
			continue
		}
		textOut := false
		for _, o := range d.Architecture.Out {
			if o == "text" {
				textOut = true
			}
		}
		if !textOut {
			continue
		}
		pp, _ := strconv.ParseFloat(d.Pricing.Prompt, 64)
		cp, _ := strconv.ParseFloat(d.Pricing.Completion, 64)
		str := func(p *string) string {
			if p == nil {
				return ""
			}
			return strings.TrimSpace(*p)
		}
		m := famMember{ID: id, Name: name, Created: d.Created,
			Released: time.Unix(d.Created, 0).UTC().Format("2006-01-02"),
			Cutoff:   str(d.KnowledgeCutoff), Expires: str(d.ExpirationDate), HFID: str(d.HuggingFaceID),
			Tokenizer: d.Architecture.Tokenizer, Instruct: str(d.Architecture.InstructType),
			Context: d.ContextLength, MaxOut: d.TopProvider.MaxCompletion,
			PromptPM: math.Round(pp*1e8) / 100, CompletionPM: math.Round(cp*1e8) / 100,
			Reasoning: d.Reasoning != nil && d.Reasoning != false, Delisted: delisted,
			In: d.Architecture.In, Out: d.Architecture.Out, Providers: d.Endpoints}
		key := dev + "/" + line
		g := out[key]
		if g == nil {
			g = &famGroup{Key: key, Dev: dev, Line: line}
			out[key] = g
		}
		g.Members = append(g.Members, m)
	}
	for _, g := range out {
		sort.SliceStable(g.Members, func(i, j int) bool {
			if g.Members[i].Created != g.Members[j].Created {
				return g.Members[i].Created > g.Members[j].Created
			}
			return g.Members[i].ID < g.Members[j].ID
		})
		// Names from the newest live member.
		for _, m := range g.Members {
			if !m.Delisted {
				g.DevName = famDevName(m.Name, g.Dev)
				g.LineName = famLineName(m.Name)
				break
			}
		}
		if g.LineName == "" && len(g.Members) > 0 {
			g.DevName = famDevName(g.Members[0].Name, g.Dev)
			g.LineName = famLineName(g.Members[0].Name)
		}
	}
	return out
}

func (g *famGroup) live() []famMember {
	var l []famMember
	for _, m := range g.Members {
		if !m.Delisted {
			l = append(l, m)
		}
	}
	return l
}

func famEnsure(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_families (
		family_key text PRIMARY KEY, uid text NOT NULL, name text NOT NULL, category text NOT NULL,
		how text NOT NULL, selected_on date NOT NULL DEFAULT current_date, page_built_on date,
		reading text, reading_model text, reading_on date, reading_hash text, reading_attempts int NOT NULL DEFAULT 0,
		notified_on date)`)
}

// famSelect chooses the category's first ten once and freezes them, then
// adds a family only when its second member arrived after the freeze.
func famSelect(db *sql.DB, cat string, groups map[string]*famGroup) {
	var n int
	var freeze sql.NullTime
	db.QueryRow(`SELECT count(*), min(selected_on) FROM twoai_model_families WHERE category=$1`, cat).Scan(&n, &freeze)
	type ranked struct {
		g      *famGroup
		newest int64
	}
	var all []ranked
	for _, g := range groups {
		l := g.live()
		if len(l) >= 2 {
			all = append(all, ranked{g, l[0].Created})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].newest != all[j].newest {
			return all[i].newest > all[j].newest
		}
		return all[i].g.Key < all[j].g.Key
	})
	if n == 0 {
		picked := []string{}
		for _, r := range all {
			if len(picked) >= 10 {
				break
			}
			// A family already paged under an earlier category does not
			// count toward this one's ten (row 388).
			var exists bool
			db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_model_families WHERE family_key=$1)`, r.g.Key).Scan(&exists)
			if exists {
				continue
			}
			db.Exec(`INSERT INTO twoai_model_families (family_key, uid, name, category, how) VALUES ($1,$2,$3,$4,'first ten') ON CONFLICT DO NOTHING`,
				r.g.Key, twoaiUID("family:"+r.g.Key), r.g.displayName(), cat)
			picked = append(picked, fmt.Sprintf("%s (%d members, newest %s)", r.g.displayName(), len(r.g.live()), r.g.live()[0].Released))
		}
		fmt.Printf("twoai_model_families: %s first ten frozen: %s\n", cat, strings.Join(picked, "; "))
		return
	}
	if !freeze.Valid {
		return
	}
	cut := freeze.Time.Unix()
	for _, r := range all {
		l := r.g.live()
		// Oldest-but-one live member is the one that made it a family.
		second := l[len(l)-2].Created
		if second <= cut {
			continue
		}
		res, _ := db.Exec(`INSERT INTO twoai_model_families (family_key, uid, name, category, how) VALUES ($1,$2,$3,$4,'new arrival') ON CONFLICT DO NOTHING`,
			r.g.Key, twoaiUID("family:"+r.g.Key), r.g.displayName(), cat)
		if k, _ := res.RowsAffected(); k > 0 {
			fmt.Printf("twoai_model_families: new family in %s: %s\n", cat, r.g.displayName())
		}
	}
}

// famParams pulls parameter counts out of model ids: "qwen3.8-27b" gives 27B,
// "qwen3.8-2.4t-a95b" gives 2.4T total with 95B active.
func famParams(id string) string {
	slug := id[strings.Index(id, "/")+1:]
	total, active := "", ""
	for _, t := range strings.Split(slug, "-") {
		if m := famParamRe.FindStringSubmatch(t); m != nil && total == "" {
			total = m[1] + strings.ToUpper(m[2])
		} else if strings.HasPrefix(t, "a") {
			if m := famParamRe.FindStringSubmatch(t[1:]); m != nil {
				active = m[1] + strings.ToUpper(m[2])
			}
		}
	}
	if total == "" {
		return ""
	}
	if active != "" {
		return total + " total, " + active + " active"
	}
	return total
}

type famCompany struct{ UID, Name string }

func famCompanies(db *sql.DB) map[string]famCompany {
	out := map[string]famCompany{}
	hasPage := map[string]bool{}
	if rows, err := db.Query(`SELECT replace(replace(path,'companies/',''),'.json','') FROM twoai_pages WHERE kind='company'`); err == nil {
		for rows.Next() {
			var u string
			if rows.Scan(&u) == nil {
				hasPage[u] = true
			}
		}
		rows.Close()
	}
	if rows, err := db.Query(`SELECT uid, name, coalesce(aliases,'[]'::jsonb)::text FROM twoai_entities WHERE kind='company'`); err == nil {
		for rows.Next() {
			var uid, name, araw string
			if rows.Scan(&uid, &name, &araw) != nil || !hasPage[uid] {
				continue
			}
			var al []string
			json.Unmarshal([]byte(araw), &al)
			for _, n := range append([]string{name}, al...) {
				if k := strings.ToLower(strings.TrimSpace(n)); k != "" {
					if _, taken := out[k]; !taken {
						out[k] = famCompany{uid, name}
					}
				}
			}
		}
		rows.Close()
	}
	return out
}

// twoaiModelFamilies runs the whole step for the categories that are open.
// Only Large Language Models is open; the next waits for Stephen (row 389).
func twoaiModelFamilies(db *sql.DB, today string) int {
	// Off until the site's model-family template is live, so no run writes
	// a family page the site cannot render yet.
	if !famEnabled {
		return 0
	}
	famEnsure(db)
	groups := famLoad(db)
	if len(groups) == 0 {
		return 0
	}
	const cat = "llms"
	famSelect(db, cat, groups)
	companies := famCompanies(db)

	type famRow struct {
		key, uid, name, how, built, reading, readingModel, readingOn, readingHash string
		attempts                                                                  int
	}
	var fams []famRow
	rows, err := db.Query(`SELECT family_key, uid, name, how, coalesce(page_built_on::text,''), coalesce(reading,''), coalesce(reading_model,''),
			coalesce(reading_on::text,''), coalesce(reading_hash,''), reading_attempts
		FROM twoai_model_families WHERE category=$1 ORDER BY selected_on, family_key`, cat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_model_families:", err)
		return 0
	}
	for rows.Next() {
		var f famRow
		if rows.Scan(&f.key, &f.uid, &f.name, &f.how, &f.built, &f.reading, &f.readingModel, &f.readingOn, &f.readingHash, &f.attempts) == nil {
			fams = append(fams, f)
		}
	}
	rows.Close()
	// Unbuilt families newest first, so the pages arrive in LIFO order.
	sort.SliceStable(fams, func(i, j int) bool {
		gi, gj := groups[fams[i].key], groups[fams[j].key]
		if gi == nil || gj == nil || len(gi.live()) == 0 || len(gj.live()) == 0 {
			return false
		}
		return gi.live()[0].Created > gj.live()[0].Created
	})
	perRun := 3
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TWOAI_FAMILY_PAGES_PER_RUN"))); err == nil && v >= 0 {
		perRun = v
	}
	newPages, refreshed, readings := 0, 0, 0
	var listed []map[string]any
	familyOf := map[string]map[string]string{}
	for _, f := range fams {
		g := groups[f.key]
		if g == nil || len(g.live()) < 2 {
			continue
		}
		if f.built == "" {
			if newPages >= perRun {
				continue
			}
		}
		doc := famDoc(db, g, f.uid, today, companies)
		// THE READING, from the facts on the page only. Written when the page
		// is first built and rewritten when the member list changes, at most
		// three a run, held for off-peak hours like every bulk stage.
		hashSrc := []string{}
		for _, m := range g.live() {
			hashSrc = append(hashSrc, m.ID)
		}
		h := md5.Sum([]byte(strings.Join(hashSrc, ",")))
		hash := hex.EncodeToString(h[:8])
		if (f.reading == "" || f.readingHash != hash) && f.attempts < 3 && readings < perRun {
			readings++
			if text, model, ok := famReading(doc); ok {
				db.Exec(`UPDATE twoai_model_families SET reading=$2, reading_model=$3, reading_on=current_date, reading_hash=$4, reading_attempts=0 WHERE family_key=$1`, f.key, text, model, hash)
				f.reading, f.readingModel, f.readingOn = text, model, today
			} else if model != "deferred" {
				db.Exec(`UPDATE twoai_model_families SET reading_attempts = reading_attempts + 1 WHERE family_key=$1`, f.key)
			}
		}
		if f.reading != "" {
			doc["family_reading"] = map[string]string{"text": f.reading, "model": f.readingModel, "written_on": f.readingOn}
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count) VALUES ($1,'tech-section',$2::jsonb,NULL,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()`,
			"tech/family-"+f.uid+".json", string(j)); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_model_families write:", err)
			continue
		}
		if f.built == "" {
			db.Exec(`UPDATE twoai_model_families SET page_built_on=current_date WHERE family_key=$1`, f.key)
			newPages++
			fmt.Printf("twoai_model_families: page built for %s (%s), %d members\n", f.name, f.uid, len(g.live()))
		} else {
			refreshed++
		}
		l := g.live()
		listed = append(listed, map[string]any{"uid": f.uid, "name": g.displayName(), "developer": g.DevName,
			"members": len(l), "newest": l[0].Released, "newest_model": l[0].Name, "first": l[len(l)-1].Released,
			"href": famBase + f.uid + "/", "how": f.how})
		for _, m := range l {
			familyOf[m.ID] = map[string]string{"uid": f.uid, "name": g.displayName()}
		}
	}
	sort.SliceStable(listed, func(i, j int) bool { return listed[i]["newest"].(string) > listed[j]["newest"].(string) })
	lj, _ := json.Marshal(listed)
	fj, _ := json.Marshal(familyOf)
	db.Exec(`UPDATE twoai_pages SET data = jsonb_set(jsonb_set(data, '{families}', $1::jsonb), '{family_of}', $2::jsonb), updated_at=now()
		WHERE path='models/llms.json'`, string(lj), string(fj))

	// The ten live: ask Stephen, through theworldofai, before the next category.
	var total, built, notified int
	db.QueryRow(`SELECT count(*), count(page_built_on), count(notified_on) FROM twoai_model_families WHERE category=$1 AND how='first ten'`, cat).Scan(&total, &built, &notified)
	if total == 10 && built == 10 && notified == 0 {
		names := []string{}
		for _, l := range listed {
			names = append(names, fmt.Sprintf("%s %s%s/", l["name"], "theworldofai.org", l["href"]))
		}
		body := "Rows 386 to 389: the ten Large Language Models family pages are built and go live with this run's deploy. Listed on /ai-ecosystem/technology-and-core-infrastructure/87868942/ under Model families. " +
			strings.Join(names, ", ") + ". Please ask Stephen to look at them. Reasoning Models (80f6d64d) does not start until he says so through this bridge."
		body = strings.ReplaceAll(body, ";", ",")
		if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai','Large Language Models: ten family pages live, Stephen to review',$1)`, body); err == nil {
			db.Exec(`UPDATE twoai_model_families SET notified_on=current_date WHERE category=$1 AND how='first ten'`, cat)
		}
	}
	fmt.Printf("twoai_model_families: %s families=%d new_pages=%d refreshed=%d ok=true\n", cat, len(fams), newPages, refreshed)
	return newPages
}

// famDoc builds the page document for one family from the catalog.
func famDoc(db *sql.DB, g *famGroup, uid, today string, companies map[string]famCompany) map[string]any {
	l := g.live()
	name := g.displayName()
	ins, outs := map[string]bool{}, map[string]bool{}
	open, api := 0, 0
	members := []map[string]any{}
	changelog := []map[string]any{}
	params := []map[string]string{}
	hfIDs := []map[string]string{}
	tokenizers := map[string]bool{}
	provAgg := map[string]map[string]any{}
	for _, m := range g.Members {
		for _, x := range m.In {
			if !m.Delisted {
				ins[x] = true
			}
		}
		for _, x := range m.Out {
			if !m.Delisted {
				outs[x] = true
			}
		}
		row := map[string]any{"id": m.ID, "name": m.Name, "released": m.Released, "context": m.Context, "max_output": m.MaxOut,
			"prompt_pm": m.PromptPM, "completion_pm": m.CompletionPM, "knowledge_cutoff": m.Cutoff, "reasoning": m.Reasoning,
			"input": m.In, "output": m.Out, "open_weights": m.HFID != "", "hf_id": m.HFID, "expires": m.Expires,
			"delisted": m.Delisted, "url": "https://openrouter.ai/" + m.ID}
		members = append(members, row)
		changelog = append(changelog, map[string]any{"date": m.Released, "kind": "released", "text": m.Name + " added"})
		if m.Expires != "" {
			changelog = append(changelog, map[string]any{"date": m.Expires, "kind": "retiring", "text": m.Name + " scheduled for retirement"})
		}
		if m.Delisted {
			continue
		}
		if m.HFID != "" {
			open++
			hfIDs = append(hfIDs, map[string]string{"model": m.Name, "hf_id": m.HFID, "url": "https://huggingface.co/" + m.HFID})
		} else {
			api++
		}
		if p := famParams(m.ID); p != "" {
			params = append(params, map[string]string{"model": m.Name, "params": p})
		}
		if m.Tokenizer != "" {
			tokenizers[m.Tokenizer] = true
		}
		for _, e := range m.Providers {
			pn, _ := e["provider"].(string)
			if pn == "" {
				continue
			}
			a := provAgg[pn]
			if a == nil {
				a = map[string]any{"provider": pn, "models": map[string]bool{}, "min_prompt_pm": 0.0}
				provAgg[pn] = a
			}
			a["models"].(map[string]bool)[m.ID] = true
			if v, ok := e["prompt_pm"].(float64); ok && v > 0 && (a["min_prompt_pm"].(float64) == 0 || v < a["min_prompt_pm"].(float64)) {
				a["min_prompt_pm"] = v
			}
		}
	}
	sort.SliceStable(changelog, func(i, j int) bool { return changelog[i]["date"].(string) > changelog[j]["date"].(string) })
	providers := []map[string]any{}
	for _, a := range provAgg {
		providers = append(providers, map[string]any{"provider": a["provider"], "models": len(a["models"].(map[string]bool)), "min_prompt_pm": round2(a["min_prompt_pm"].(float64))})
	}
	sort.SliceStable(providers, func(i, j int) bool {
		if providers[i]["models"].(int) != providers[j]["models"].(int) {
			return providers[i]["models"].(int) > providers[j]["models"].(int)
		}
		return providers[i]["provider"].(string) < providers[j]["provider"].(string)
	})
	keys := func(m map[string]bool) []string {
		o := []string{}
		for k := range m {
			o = append(o, k)
		}
		sort.Strings(o)
		return o
	}
	licence := "API only"
	switch {
	case open > 0 && api == 0:
		licence = "Open weights"
	case open > 0:
		licence = "Some open weights, some API only"
	}
	doc := map[string]any{
		"shape": "model-family", "uid": uid, "name": name, "title": name + " model family",
		"family_key": g.Key, "developer": g.DevName, "line": g.LineName,
		"parent_path": famBase + "87868942/", "parent_name": "Large Language Models",
		"hub_path": famBase + "70d363c9/", "hub_name": "Foundation Models",
		"first_release": l[len(l)-1].Released, "latest_release": l[0].Released, "latest_model": l[0].Name,
		"member_count": len(l), "licence": licence, "input_modalities": keys(ins), "output_modalities": keys(outs),
		"members": members, "changelog": changelog, "params": params, "hf_models": hfIDs, "tokenizers": keys(tokenizers),
		"providers": providers, "generated": today,
		"source": map[string]string{"name": "OpenRouter model catalog", "url": "https://openrouter.ai/models"},
	}
	if c, ok := companies[strings.ToLower(g.DevName)]; ok {
		doc["developer_company"] = map[string]string{"uid": c.UID, "name": c.Name, "href": "/companies/" + c.UID + "/"}
	} else if c, ok := companies[strings.ToLower(g.LineName)]; ok {
		doc["developer_company"] = map[string]string{"uid": c.UID, "name": c.Name, "href": "/companies/" + c.UID + "/"}
	}
	doc["answer"] = famAnswer(doc)
	// BENCHMARKS: only rows from the site's benchmark records, each a named
	// public source with its date and link, whose system names this family.
	bench := []map[string]string{}
	lineRe := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(g.LineName) + `\b`)
	if rows, err := db.Query(`SELECT name, coalesce(results->>'source',''), coalesce(results->>'source_url', url, ''), coalesce(results->>'as_of',''),
			coalesce(results->>'metric',''), coalesce(results->'rows','[]'::jsonb)::text FROM twoai_benchmarks WHERE results IS NOT NULL`); err == nil {
		for rows.Next() {
			var bname, src, surl, asOf, metric, rraw string
			if rows.Scan(&bname, &src, &surl, &asOf, &metric, &rraw) != nil {
				continue
			}
			var rs []map[string]any
			json.Unmarshal([]byte(rraw), &rs)
			for _, r := range rs {
				sys, _ := r["system"].(string)
				if sys == "" || !lineRe.MatchString(sys) {
					continue
				}
				if g.DevName != "" && !strings.Contains(strings.ToLower(sys), strings.ToLower(g.DevName)) && !strings.EqualFold(g.DevName, g.LineName) {
					continue
				}
				bench = append(bench, map[string]string{"benchmark": bname, "system": sys, "score": fmt.Sprint(r["score"]),
					"metric": metric, "as_of": asOf, "source": src, "source_url": surl})
			}
		}
		rows.Close()
	}
	doc["benchmarks"] = bench
	// CVES AND NEWS that name the family. A line name can be an ordinary word
	// (Command, Solar, Ling), so the text must name the developer as well,
	// unless the developer and the line are the same name.
	needDev := !strings.EqualFold(g.DevName, g.LineName)
	devPat := "%" + strings.ToLower(g.DevName) + "%"
	linePat := `\m` + regexp.QuoteMeta(strings.ToLower(g.LineName)) + `\M`
	cves := []map[string]any{}
	if rows, err := db.Query(`SELECT cve_id, coalesce(headline,''), coalesce(cvss_severity,''), coalesce(published::date::text,'')
		FROM twoai_cves WHERE status IN ('published','approved')
		  AND lower(coalesce(headline,'') || ' ' || coalesce(description,'') || ' ' || coalesce(product,'')) ~ $1
		  AND ($2 = false OR lower(coalesce(headline,'') || ' ' || coalesce(description,'') || ' ' || coalesce(vendor,'')) LIKE $3)
		ORDER BY published DESC NULLS LAST LIMIT 10`, linePat, needDev, devPat); err == nil {
		for rows.Next() {
			var id, head, sev, pub string
			if rows.Scan(&id, &head, &sev, &pub) == nil {
				cves = append(cves, map[string]any{"cve_id": id, "headline": head, "severity": sev, "published": pub})
			}
		}
		rows.Close()
	}
	doc["cves"] = cves
	news := []map[string]any{}
	if rows, err := db.Query(`SELECT uid, headline, coalesce(published_on::text,'') FROM twoai_news_stories
		WHERE retired_at IS NULL AND published_on > current_date - 120
		  AND lower(headline || ' ' || coalesce(story->>'Summary','')) ~ $1
		  AND ($2 = false OR lower(headline || ' ' || coalesce(story->>'Summary','')) LIKE $3)
		ORDER BY published_on DESC NULLS LAST LIMIT 6`, linePat, needDev, devPat); err == nil {
		for rows.Next() {
			var su, head, pub string
			if rows.Scan(&su, &head, &pub) == nil {
				if len(pub) > 10 {
					pub = pub[:10]
				}
				news = append(news, map[string]any{"uid": su, "headline": head, "published": pub})
			}
		}
		rows.Close()
	}
	doc["news"] = news
	return doc
}

// famAnswer is the short answer at the top of the page, built from facts.
func famAnswer(doc map[string]any) string {
	name, _ := doc["name"].(string)
	dev, _ := doc["developer"].(string)
	n, _ := doc["member_count"].(int)
	first, _ := doc["first_release"].(string)
	latest, _ := doc["latest_release"].(string)
	latestModel, _ := doc["latest_model"].(string)
	lic, _ := doc["licence"].(string)
	s := fmt.Sprintf("%s is %s's model line, with %d versions listed in the OpenRouter catalog, the first released %s and the newest, %s, on %s.",
		name, dev, n, first, strings.TrimSpace(latestModel[strings.Index(latestModel, ":")+1:]), latest)
	switch lic {
	case "Open weights":
		s += " Every version publishes open weights."
	case "API only":
		s += " All of them are served over APIs only, with no open weights."
	default:
		s += " Some versions publish open weights and others are API only."
	}
	return s
}

// famReading writes "Strengths and limits" from the page's facts only.
func famReading(doc map[string]any) (string, string, bool) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Family: %s, developer %s. %s\nLicence: %s. Input modalities: %v. Output: %v.\nVersions, newest first:\n",
		doc["name"], doc["developer"], doc["answer"], doc["licence"], doc["input_modalities"], doc["output_modalities"])
	if ms, ok := doc["members"].([]map[string]any); ok {
		for i, m := range ms {
			if i >= 20 {
				break
			}
			if d, _ := m["delisted"].(bool); d {
				continue
			}
			fmt.Fprintf(&sb, "- %s, released %s, context %v tokens, max output %v, $%v per million input, $%v output, reasoning %v, open weights %v\n",
				m["name"], m["released"], m["context"], m["max_output"], m["prompt_pm"], m["completion_pm"], m["reasoning"], m["open_weights"])
		}
	}
	if bs, ok := doc["benchmarks"].([]map[string]string); ok && len(bs) > 0 {
		sb.WriteString("Benchmark rows from named public sources:\n")
		for _, b := range bs {
			fmt.Fprintf(&sb, "- %s: %s scored %s (%s, as of %s)\n", b["benchmark"], b["system"], b["score"], b["source"], b["as_of"])
		}
	}
	system := `You write for The World of AI, a reference site. You are given the facts about one AI model family from a public model catalog. Write a short "Strengths and limits" reading for someone choosing a model, 90 to 160 words, using only the facts given: context window, output length, price, modalities, reasoning support, open weights, the spread of versions and tiers, and any benchmark rows named with their source. Say what the facts suggest the family is good for and where it is limited or costly. No vendor marketing, no claims the facts do not support, no predictions, no comparison to families not named in the facts, no questions. Plain English, commas rather than dashes, no markdown, no lists.
Return only JSON: {"text": "..."}`
	out, model, err := twoaiGenerate("model_family_reading", system, sb.String())
	if err != nil {
		if strings.Contains(err.Error(), "deferred") {
			return "", "deferred", false
		}
		return "", "", false
	}
	text := strings.TrimSpace(out)
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	}
	if k := strings.LastIndex(text, "}"); k >= 0 {
		text = text[:k+1]
	}
	var got struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(text), &got) != nil {
		return "", model, false
	}
	para := strings.TrimSpace(twoaiStripMarkdown(got.Text))
	if n := len([]rune(para)); n < 300 || n > 1300 || strings.Contains(para, "?") || strings.Contains(para, "—") {
		fmt.Printf("twoai_model_families: reading for %v held (length or form)\n", doc["name"])
		return "", model, false
	}
	return para, model, true
}
