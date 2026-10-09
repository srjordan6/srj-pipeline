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

	"github.com/lib/pq"
)

const famBase = "/ai-ecosystem/technology-and-core-infrastructure/"

// famEnabled: on since twoai-site bf7d821 renders shape model-family.
const famEnabled = true

// famCat is one model category with family pages, in the order rows 386 to
// 389 set: Large Language Models first, then Reasoning Models, then on down
// the Foundation Models hub. member says whether a catalog model belongs to
// the category; a family is in it when one of its live members does.
type famCat struct {
	slug, name, uid, path string
	member                func(m famMember) bool
}

var famCats = []famCat{
	{"llms", "Large Language Models", "87868942", "models/llms.json", func(m famMember) bool { return famHas(m.In, "text") && famHas(m.Out, "text") }},
	{"reasoning-models", "Reasoning Models", "80f6d64d", "models/reasoning-models.json", func(m famMember) bool { return m.Reasoning }},
	// Multimodal: more than one input modality, the rule twoai_models uses
	// for the section's own API table (len(InputMods) > 1).
	{"multimodal-models", "Multimodal Models", "62d0f0ce", "models/multimodal-models.json", func(m famMember) bool { return len(m.In) > 1 }},
	// Vision: the model takes images in. Opened by Stephen on 2026-10-08
	// ("no family of models" on the Vision Models page). Families already
	// paged under Multimodal appear here as "also in this category"; the
	// ten counted here are the image-input lines not yet paged.
	{"vision-models", "Vision Models", "4e095e6f", "models/vision-models.json", func(m famMember) bool { return famHas(m.In, "image") }},
	// The rest opened the same day (Stephen: "every pipeline run is supposed
	// to put models with a new web page"). Each is the rule the catalog can
	// answer from a model's own record: what it takes in, what it gives out,
	// its parameter count, or a coder name.
	{"audio-speech-models", "Audio and Speech Models", "2e3db013", "models/audio-speech-models.json", func(m famMember) bool { return famHas(m.In, "audio") || famHas(m.Out, "audio") }},
	{"image-generation-models", "Image Generation Models", "c8f599cd", "models/image-generation-models.json", func(m famMember) bool { return famHas(m.Out, "image") }},
	{"video-models", "Video Models", "33ad47d7", "models/video-models.json", func(m famMember) bool { return famHas(m.In, "video") || famHas(m.Out, "video") }},
	{"coding-models", "Coding Models", "29375dec", "models/coding-models.json", func(m famMember) bool { return famCoderRe.MatchString(m.ID) || famCoderRe.MatchString(m.Name) }},
	{"small-language-models", "Small Language Models", "71415be9", "models/small-language-models.json", func(m famMember) bool { b := famTotalB(m.ID); return b > 0 && b <= 10 }},
	{"on-device-edge-models", "On-device and Edge Models", "062cdc6f", "models/on-device-edge-models.json", func(m famMember) bool { b := famTotalB(m.ID); return b > 0 && b <= 4 }},
}

func famHas(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var famCoderRe = regexp.MustCompile(`(?i)\bcode[rx]?\b|\bcoder\b|-code-|codestral|devstral`)

// famTotalB is the total parameter count in billions from a catalog id, or 0
// when the id does not say ("qwen3.8-27b" gives 27, "x-2.4t-a95b" gives 2400).
func famTotalB(id string) float64 {
	slug := id[strings.Index(id, "/")+1:]
	for _, t := range strings.Split(slug, "-") {
		if m := famParamRe.FindStringSubmatch(t); m != nil {
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return 0
			}
			if strings.EqualFold(m[2], "t") {
				v *= 1000
			}
			return v
		}
	}
	return 0
}

// famOpen is how many of famCats are open. One category at a time, and the
// next only when Stephen says so: he reviewed the Large Language Models ten
// on 2026-10-03 ("look fine"), which opened Reasoning Models, and approved
// the Reasoning Models ten the same evening (theworldofai row 422), which
// opened Multimodal Models. On 2026-10-08 he asked why Vision Models had no
// families and said every run is supposed to add model pages, so every
// category is open and the one-at-a-time gate is gone.
var famOpen = len(famCats)

func famCatBySlug(slug string) famCat {
	for _, c := range famCats {
		if c.slug == slug {
			return c
		}
	}
	return famCats[0]
}

// famInCat reports whether a family has a live member in the category, and
// when its newest such member was created.
func famInCat(g *famGroup, c famCat) (bool, int64) {
	for _, m := range g.live() {
		if c.member(m) {
			return true, m.Created
		}
	}
	return false, 0
}

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
	// "ByteDance Seed" already ends with its line "Seed" (2026-10-04, the
	// first Multimodal page read "ByteDance Seed Seed"). Display only: the
	// uid comes from the family key, so no URL moves.
	if strings.HasSuffix(strings.ToLower(g.DevName), " "+strings.ToLower(g.LineName)) {
		return g.DevName
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
	// THE REVIEW GATE (theworldofai row 627, 2026-10-09). Stephen reviews each
	// category's first ten before the next category's first ten is chosen;
	// rolling additions in a category already under way do not wait. His
	// sign-off is a row here, so recording it needs no code change.
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_family_reviews (
		category text PRIMARY KEY, approved_on date NOT NULL, approved_by text NOT NULL, note text)`)
	db.Exec(`INSERT INTO twoai_model_family_reviews (category, approved_on, approved_by, note) VALUES
		('llms', '2026-10-03', 'Stephen', 'look fine'),
		('reasoning-models', '2026-10-03', 'Stephen', 'theworldofai row 422')
		ON CONFLICT DO NOTHING`)
}

// famGateOpen reports whether a category may choose its first ten: every
// category before it that already has a first ten must be signed off. It
// returns the category still waiting when the gate is shut.
func famGateOpen(db *sql.DB, c famCat) (bool, string) {
	for _, prev := range famCats {
		if prev.slug == c.slug {
			return true, ""
		}
		var firsts int
		var approved bool
		db.QueryRow(`SELECT (SELECT count(*) FROM twoai_model_families WHERE category=$1 AND how='first ten'),
			EXISTS (SELECT 1 FROM twoai_model_family_reviews WHERE category=$1)`, prev.slug).Scan(&firsts, &approved)
		if firsts > 0 && !approved {
			return false, prev.name
		}
	}
	return true, ""
}

// famFineTune reports whether a line is a third party's fine-tune of a line
// that already has a family page under another developer (row 627: Sao10K
// publishes fine-tunes of Meta Llama, which is not a family of its own). The
// match is on the line's display name, kept with the family it matched.
func famFineTune(db *sql.DB, g *famGroup, groups map[string]*famGroup) (bool, string) {
	rows, err := db.Query(`SELECT family_key FROM twoai_model_families`)
	if err != nil {
		return false, ""
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if rows.Scan(&k) != nil || k == g.Key {
			continue
		}
		o := groups[k]
		if o == nil || o.Dev == g.Dev {
			continue
		}
		if strings.EqualFold(o.LineName, g.LineName) || strings.EqualFold(o.Line, g.Line) {
			return true, k
		}
	}
	return false, ""
}

// famSelect chooses the category's first ten once and freezes them, then
// adds a family only when its second member arrived after the freeze.
func famSelect(db *sql.DB, c famCat, groups map[string]*famGroup) {
	cat := c.slug
	var n int
	var freeze sql.NullTime
	db.QueryRow(`SELECT count(*), min(selected_on) FROM twoai_model_families WHERE category=$1`, cat).Scan(&n, &freeze)
	type ranked struct {
		g      *famGroup
		newest int64
	}
	var all []ranked
	for _, g := range groups {
		if len(g.live()) < 2 {
			continue
		}
		// Ranked by the newest member that is in this category.
		if in, newest := famInCat(g, c); in {
			all = append(all, ranked{g, newest})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].newest != all[j].newest {
			return all[i].newest > all[j].newest
		}
		return all[i].g.Key < all[j].g.Key
	})
	if n == 0 {
		if ok, waiting := famGateOpen(db, c); !ok {
			fmt.Printf("twoai_model_families: %s first ten waits for Stephen's review of %s\n", cat, waiting)
			return
		}
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
			if ft, of := famFineTune(db, r.g, groups); ft {
				fmt.Printf("twoai_model_families: %s skipped, a fine-tune line of %s\n", r.g.displayName(), of)
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
		if ft, _ := famFineTune(db, r.g, groups); ft {
			continue
		}
		res, _ := db.Exec(`INSERT INTO twoai_model_families (family_key, uid, name, category, how) VALUES ($1,$2,$3,$4,'new arrival') ON CONFLICT DO NOTHING`,
			r.g.Key, twoaiUID("family:"+r.g.Key), r.g.displayName(), cat)
		if k, _ := res.RowsAffected(); k > 0 {
			fmt.Printf("twoai_model_families: new family in %s: %s\n", cat, r.g.displayName())
		}
	}
	// EVERY RUN ADDS PAGES (Stephen, 2026-10-08). The first ten were a
	// review set, not a ceiling: once a category's selected families are all
	// built, the next three lines by newest member join it, so the queue
	// never runs dry while the catalog has families nobody has paged.
	var unbuilt int
	db.QueryRow(`SELECT count(*) FROM twoai_model_families WHERE category=$1 AND page_built_on IS NULL`, cat).Scan(&unbuilt)
	if unbuilt > 0 {
		return
	}
	added := 0
	for _, r := range all {
		if added >= 3 {
			break
		}
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_model_families WHERE family_key=$1)`, r.g.Key).Scan(&exists)
		if exists {
			continue
		}
		if ft, _ := famFineTune(db, r.g, groups); ft {
			continue
		}
		res, _ := db.Exec(`INSERT INTO twoai_model_families (family_key, uid, name, category, how) VALUES ($1,$2,$3,$4,'rolling') ON CONFLICT DO NOTHING`,
			r.g.Key, twoaiUID("family:"+r.g.Key), r.g.displayName(), cat)
		if k, _ := res.RowsAffected(); k > 0 {
			added++
			fmt.Printf("twoai_model_families: %s grows: %s (%d members)\n", cat, r.g.displayName(), len(r.g.live()))
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
	open := famCats[:famOpen]
	openSlugs := []string{}
	catOrder := map[string]int{}
	for i, c := range open {
		famSelect(db, c, groups)
		openSlugs = append(openSlugs, c.slug)
		catOrder[c.slug] = i
	}
	companies := famCompanies(db)

	type famRow struct {
		key, uid, name, how, built, reading, readingModel, readingOn, readingHash, category string
		attempts                                                                            int
	}
	var fams []famRow
	rows, err := db.Query(`SELECT family_key, uid, name, how, coalesce(page_built_on::text,''), coalesce(reading,''), coalesce(reading_model,''),
			coalesce(reading_on::text,''), coalesce(reading_hash,''), reading_attempts, category
		FROM twoai_model_families WHERE category = ANY($1) ORDER BY selected_on, family_key`, pq.Array(openSlugs))
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_model_families:", err)
		return 0
	}
	for rows.Next() {
		var f famRow
		if rows.Scan(&f.key, &f.uid, &f.name, &f.how, &f.built, &f.reading, &f.readingModel, &f.readingOn, &f.readingHash, &f.attempts, &f.category) == nil {
			fams = append(fams, f)
		}
	}
	rows.Close()
	// Category order first, then unbuilt families newest first, so the
	// pages arrive one category at a time in LIFO order.
	sort.SliceStable(fams, func(i, j int) bool {
		if catOrder[fams[i].category] != catOrder[fams[j].category] {
			return catOrder[fams[i].category] < catOrder[fams[j].category]
		}
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
	listedBy := map[string][]map[string]any{}
	built := map[string]bool{}
	familyOf := map[string]map[string]string{}
	// THE OPEN-WEIGHT ROWS TOO. Stephen, 2026-10-03: "nothing has been done"
	// on 87868942, pointing at Qwen/Qwen3-0.6B. The page opens with about a
	// thousand Hugging Face rows and the first version attached families only
	// to the API models, so Qwen3-0.6B still linked out with no tie to the Qwen
	// family. A Hugging Face repo joins a family when its owner is the family's
	// own organisation (the owners its catalog members name, or the developer
	// slug) and its name's line matches. Third-party repackagings (unsloth,
	// bartowski) stay unlinked: a name cannot tell a copy from a fine-tune.
	hfRows := []map[string]any{}
	{
		var raw string
		if db.QueryRow(`SELECT coalesce(data->'models','[]'::jsonb)::text FROM twoai_pages WHERE path='models/llms.json'`).Scan(&raw) == nil {
			json.Unmarshal([]byte(raw), &hfRows)
		}
	}
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
		doc := famDoc(db, g, f.uid, today, companies, famCatBySlug(f.category))
		// The other open categories this family also belongs to.
		also := []map[string]string{}
		for _, c := range open {
			if c.slug == f.category {
				continue
			}
			if in, _ := famInCat(g, c); in {
				also = append(also, map[string]string{"name": c.name, "href": famBase + c.uid + "/"})
			}
		}
		doc["also_in"] = also
		releases := famHFReleases(g, hfRows)
		doc["hf_releases"] = releases
		for _, r := range releases {
			if id, _ := r["id"].(string); id != "" {
				familyOf[id] = map[string]string{"uid": f.uid, "name": g.displayName()}
			}
		}
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
		// EVERY MODEL GETS ITS OWN PAGE (Stephen, 2026-10-09: on the Claude
		// family page "each link goes to Anthropic, each model gets its own web
		// page"). One page per member, listed or retired, so a page once
		// published keeps serving; the family's tables link to them.
		pages := famModelPages(db, g, f.uid, famCatBySlug(f.category), today)
		if ms, ok := doc["members"].([]map[string]any); ok {
			for _, r := range ms {
				if id, _ := r["id"].(string); pages[id] != "" {
					r["page"] = pages[id]
				}
			}
		}
		doc["member_pages"] = pages
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
		built[f.key] = true
		listedBy[f.category] = append(listedBy[f.category], map[string]any{"uid": f.uid, "key": f.key, "name": g.displayName(), "developer": g.DevName,
			"members": len(l), "newest": l[0].Released, "newest_model": l[0].Name, "first": l[len(l)-1].Released,
			"href": famBase + f.uid + "/", "how": f.how})
		for _, m := range l {
			familyOf[m.ID] = map[string]string{"uid": f.uid, "name": g.displayName()}
		}
	}
	fj, _ := json.Marshal(familyOf)
	for ci, c := range open {
		// This category's own families, and the families paged under another
		// category that also belong here (row 388: listed apart, not counted).
		listed := listedBy[c.slug]
		sort.SliceStable(listed, func(i, j int) bool { return listed[i]["newest"].(string) > listed[j]["newest"].(string) })
		alsoHere := []map[string]any{}
		for _, other := range open {
			if other.slug == c.slug {
				continue
			}
			for _, x := range listedBy[other.slug] {
				if g := groups[x["key"].(string)]; g != nil {
					if in, _ := famInCat(g, c); in {
						alsoHere = append(alsoHere, x)
					}
				}
			}
		}
		sort.SliceStable(alsoHere, func(i, j int) bool { return alsoHere[i]["newest"].(string) > alsoHere[j]["newest"].(string) })
		lj, _ := json.Marshal(listed)
		aj, _ := json.Marshal(alsoHere)
		db.Exec(`UPDATE twoai_pages SET data = jsonb_set(jsonb_set(jsonb_set(data, '{families}', $1::jsonb), '{also_families}', $2::jsonb), '{family_of}', $3::jsonb), updated_at=now()
			WHERE path=$4`, string(lj), string(aj), string(fj), c.path)

		// The ten live: ask Stephen, through theworldofai, before the next
		// category opens.
		var total, builtN, notified int
		db.QueryRow(`SELECT count(*), count(page_built_on), count(notified_on) FROM twoai_model_families WHERE category=$1 AND how='first ten'`, c.slug).Scan(&total, &builtN, &notified)
		if total == 10 && builtN == 10 && notified == 0 {
			names := []string{}
			for _, l := range listed {
				names = append(names, fmt.Sprintf("%s theworldofai.org%s", l["name"], l["href"]))
			}
			_ = ci
			next := "The next category's first ten waits for his review (row 627). When he approves, record it with INSERT INTO twoai_model_family_reviews (category, approved_on, approved_by) VALUES ('" + c.slug + "', current_date, 'Stephen'). Rolling additions here continue meanwhile"
			body := "Rows 386 to 389: the ten " + c.name + " family pages are built and go live with this run's deploy. Listed on /ai-ecosystem/technology-and-core-infrastructure/" + c.uid + "/ under Model families. " +
				strings.Join(names, ", ") + ". Please ask Stephen to look at them. " + next + "."
			body = strings.ReplaceAll(body, ";", ",")
			if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`,
				c.name+": ten family pages live, Stephen to review", body); err == nil {
				db.Exec(`UPDATE twoai_model_families SET notified_on=current_date WHERE category=$1 AND how='first ten'`, c.slug)
			}
		}
		fmt.Printf("twoai_model_families: %s families=%d also=%d ok=true\n", c.slug, len(listed), len(alsoHere))
	}
	// THE FOUNDATION MODELS HUB LEADS TO THE FAMILIES (theworldofai row 628).
	// Every built family grouped by the category it was paged under, and the
	// ten newest releases across all of them, each linking its family page.
	// Rewritten every run, so a model the catalog gains today is on the hub
	// today. twoaiEcosystem writes the hub earlier in the build; these keys
	// are set on top of it.
	byCat := []map[string]any{}
	type latest struct {
		m   famMember
		fam map[string]any
	}
	var newest []latest
	for _, c := range open {
		listed := listedBy[c.slug]
		if len(listed) == 0 {
			continue
		}
		fl := []map[string]any{}
		for _, x := range listed {
			fl = append(fl, map[string]any{"name": x["name"], "href": x["href"], "developer": x["developer"],
				"latest": x["newest_model"], "latest_on": x["newest"], "versions": x["members"]})
			if g := groups[x["key"].(string)]; g != nil {
				for _, m := range g.live() {
					newest = append(newest, latest{m, x})
				}
			}
		}
		byCat = append(byCat, map[string]any{"category": c.name, "href": famBase + c.uid + "/", "families": fl})
	}
	// SINGLE MODELS (Stephen, 2026-10-09, row 629): a current model that is
	// the only version of its line gets a page too. Current means first
	// listed in the last 90 days; no backfill. Once tracked it stays tracked,
	// so its page keeps serving, and when a second version arrives the line
	// becomes a family and the same page carries on as that member's page.
	singles := famSingles(db, groups, today)
	singleByCat := map[string][]map[string]any{}
	for _, sm := range singles {
		newest = append(newest, latest{sm.m, map[string]any{"name": "", "href": "", "developer": sm.g.DevName}})
		singleByCat[sm.cat.slug] = append(singleByCat[sm.cat.slug], map[string]any{"name": famShortName(sm.m.Name), "developer": sm.g.DevName,
			"href": famBase + famModelUID(sm.m.ID) + "/", "released": sm.m.Released})
	}
	sort.SliceStable(newest, func(i, j int) bool {
		if newest[i].m.Created != newest[j].m.Created {
			return newest[i].m.Created > newest[j].m.Created
		}
		return newest[i].m.ID < newest[j].m.ID
	})
	latestRows := []map[string]any{}
	for _, l := range newest {
		if len(latestRows) >= 10 {
			break
		}
		latestRows = append(latestRows, map[string]any{"model": l.m.Name, "released": l.m.Released, "page": famBase + famModelUID(l.m.ID) + "/",
			"family": l.fam["name"], "href": l.fam["href"], "developer": l.fam["developer"]})
	}
	for _, c := range open {
		sj := []byte("[]")
		if len(singleByCat[c.slug]) > 0 {
			sj, _ = json.Marshal(singleByCat[c.slug])
		}
		db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{single_models}', $1::jsonb) WHERE path=$2`, string(sj), c.path)
	}
	bj, _ := json.Marshal(byCat)
	lj, _ := json.Marshal(latestRows)
	if _, err := db.Exec(`UPDATE twoai_pages SET data = jsonb_set(jsonb_set(data, '{model_families}', $1::jsonb), '{latest_models}', $2::jsonb), updated_at=now()
		WHERE path='ecosystem/foundation-models.json'`, string(bj), string(lj)); err != nil {
		fmt.Fprintln(os.Stderr, "twoai_model_families foundation hub:", err)
	}
	fmt.Printf("twoai_model_families: rows=%d new_pages=%d refreshed=%d hub_categories=%d ok=true\n", len(fams), newPages, refreshed, len(byCat))
	return newPages
}

// famDoc builds the page document for one family from the catalog.
func famDoc(db *sql.DB, g *famGroup, uid, today string, companies map[string]famCompany, parent famCat) map[string]any {
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
		"parent_path": famBase + parent.uid + "/", "parent_name": parent.name,
		"hub_path": famBase + "70d363c9/", "hub_name": "Foundation Models",
		"first_release": l[len(l)-1].Released, "latest_release": l[0].Released, "latest_model": l[0].Name,
		"member_count": len(l), "licence": licence, "input_modalities": keys(ins), "output_modalities": keys(outs),
		"members": members, "changelog": changelog, "params": params, "hf_models": hfIDs, "tokenizers": keys(tokenizers),
		"providers": providers, "generated": today,
		"source": map[string]string{"name": "OpenRouter model catalog", "url": "https://openrouter.ai/models"},
	}
	// The developer's name, then the line's, then the company it belongs to
	// (row 493: "ByteDance Seed" is ByteDance's, and had no company link).
	if c, ok := famFindCompany(companies, g); ok {
		doc["developer_company"] = map[string]string{"uid": c.UID, "name": c.Name, "href": "/companies/" + c.UID + "/"}
	}
	doc["answer"] = famAnswer(doc)
	// BENCHMARKS: only rows from the site's benchmark records, each a named
	// public source with its date and link, whose system names this family.
	bench := []map[string]string{}
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
				det, _ := r["detail"].(string)
				if !famBenchMatch(g.LineName, g.DevName, sys, det) {
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
	// Rows 493 and 494: the developer's own site (twoai_family_sources) and
	// the data blocks built from records the site holds.
	famSrcAttach(db, uid, doc)
	famBlocks(db, g, doc)
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
	// Row 493: "ByteDance Seed is ByteDance Seed's model line". When the
	// family is named after its developer, say whose it is from the company
	// record instead, or nothing, never the same name twice.
	subject := fmt.Sprintf("%s is %s's model line", name, dev)
	if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(dev)) {
		company := ""
		if dc, ok := doc["developer_company"].(map[string]string); ok {
			company = dc["name"]
		}
		switch {
		case company != "" && !strings.EqualFold(company, name):
			subject = fmt.Sprintf("%s is a model family from %s", name, company)
		case company != "":
			subject = fmt.Sprintf("%s is a model family from the company of the same name", name)
		default:
			subject = fmt.Sprintf("%s is a model family", name)
		}
	}
	s := fmt.Sprintf("%s, with %d versions listed in the OpenRouter catalog, the first released %s and the newest, %s, on %s.",
		subject, n, first, strings.TrimSpace(latestModel[strings.Index(latestModel, ":")+1:]), latest)
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
	fmt.Fprintf(&sb, "Family: %s, developer %s. %s\nLicence: %s. Input modalities: %v. Output: %v.\n",
		doc["name"], doc["developer"], doc["answer"], doc["licence"], doc["input_modalities"], doc["output_modalities"])
	// COUNTS FOR THE WHOLE FAMILY. The first GPT reading (2026-10-03) said
	// all listed versions support reasoning, because it saw only the newest
	// twenty of 56; GPT-3.5, GPT-4o and GPT-4.1 do not. Anything said about
	// the family as a whole comes from these counts, not from the list.
	live, reasoning, open := 0, 0, 0
	var minCtx, maxCtx int64
	if ms, ok := doc["members"].([]map[string]any); ok {
		for _, m := range ms {
			if d, _ := m["delisted"].(bool); d {
				continue
			}
			live++
			if r, _ := m["reasoning"].(bool); r {
				reasoning++
			}
			if o, _ := m["open_weights"].(bool); o {
				open++
			}
			if c, _ := m["context"].(int64); c > 0 {
				if minCtx == 0 || c < minCtx {
					minCtx = c
				}
				if c > maxCtx {
					maxCtx = c
				}
			}
		}
	}
	fmt.Fprintf(&sb, "Whole family, %d versions: %d support reasoning, %d publish open weights, context windows from %d to %d tokens.\n", live, reasoning, open, minCtx, maxCtx)
	shown := live
	if shown > 20 {
		shown = 20
	}
	fmt.Fprintf(&sb, "The newest %d of the %d versions, newest first:\n", shown, live)
	if ms, ok := doc["members"].([]map[string]any); ok {
		n := 0
		for _, m := range ms {
			if d, _ := m["delisted"].(bool); d {
				continue
			}
			if n >= 20 {
				break
			}
			n++
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
	system := `You write for The World of AI, a reference site. You are given the facts about one AI model family from a public model catalog. Write a short "Strengths and limits" reading for someone choosing a model, 90 to 160 words, using only the facts given: context window, output length, price, modalities, reasoning support, open weights, the spread of versions and tiers, and any benchmark rows named with their source. Anything said about the family as a whole, such as how many versions support reasoning or publish open weights, must come from the whole-family counts, never from the list, which may show only the newest versions. Say what the facts suggest the family is good for and where it is limited or costly. No vendor marketing, no claims the facts do not support, no predictions, no comparison to families not named in the facts, no questions. Plain English, commas rather than dashes, no markdown, no lists.
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

// famHFReleases returns the family's own open-weight repos among the Hugging
// Face rows of the category page, most downloaded first.
func famHFReleases(g *famGroup, rows []map[string]any) []map[string]any {
	orgs := map[string]bool{strings.ToLower(g.Dev): true, strings.ReplaceAll(strings.ToLower(g.Dev), "-", ""): true}
	for _, m := range g.Members {
		if i := strings.Index(m.HFID, "/"); i > 0 {
			orgs[strings.ToLower(m.HFID[:i])] = true
		}
	}
	out := []map[string]any{}
	for _, r := range rows {
		id, _ := r["id"].(string)
		i := strings.Index(id, "/")
		if i <= 0 || !orgs[strings.ToLower(id[:i])] {
			continue
		}
		first := strings.SplitN(id[i+1:], "-", 2)[0]
		if strings.ToLower(famVersionTail.ReplaceAllString(first, "")) != strings.ToLower(g.Line) {
			continue
		}
		out = append(out, r)
	}
	dl := func(r map[string]any) float64 { v, _ := r["downloads"].(float64); return v }
	sort.SliceStable(out, func(a, b int) bool { return dl(out[a]) > dl(out[b]) })
	return out
}

// famModelUID is the uid of one model's page, from its catalog id.
func famModelUID(id string) string { return twoaiUID("model:" + id) }

func famModsText(list []string) string {
	if len(list) == 0 {
		return "text"
	}
	if len(list) == 1 {
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}

func famTokText(n int64) string {
	switch {
	case n >= 1000000 && n%1000000 == 0:
		return fmt.Sprintf("%d million", n/1000000)
	case n >= 1000000:
		return fmt.Sprintf("%.1f million", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// famModelPages writes tech/model-<uid>.json for every member of a family and
// returns catalog id -> page path. Every sentence is composed from the
// catalog record and the family around it; nothing is generated.
func famModelPages(db *sql.DB, g *famGroup, famUID string, cat famCat, today string) map[string]string {
	out := map[string]string{}
	live := g.live()
	famName := g.displayName()
	famHref := famBase + famUID + "/"
	// famUID "" is a single model with no family yet (row 629).
	var famRef any
	if famUID != "" {
		famRef = map[string]any{"name": famName, "href": famHref, "uid": famUID}
	}
	// Family medians, for the comparison lines.
	var prices []float64
	var ctxMax int64
	for _, m := range live {
		if m.PromptPM > 0 {
			prices = append(prices, m.PromptPM)
		}
		if m.Context > ctxMax {
			ctxMax = m.Context
		}
	}
	sort.Float64s(prices)
	median := 0.0
	if len(prices) > 0 {
		median = prices[len(prices)/2]
	}
	siblings := []map[string]any{}
	for _, m := range g.Members {
		siblings = append(siblings, map[string]any{"name": famShortName(m.Name), "id": m.ID, "href": famBase + famModelUID(m.ID) + "/",
			"released": m.Released, "prompt_pm": m.PromptPM, "completion_pm": m.CompletionPM, "context": m.Context, "delisted": m.Delisted})
	}
	for i, m := range g.Members {
		uid := famModelUID(m.ID)
		href := famBase + uid + "/"
		out[m.ID] = href
		short := famShortName(m.Name)
		sents := []string{}
		if famUID == "" {
			sents = append(sents, fmt.Sprintf("%s is a model from %s, first listed on %s. It is the only version of its line in the catalog so far.", short, g.DevName, m.Released))
		} else {
			sents = append(sents, fmt.Sprintf("%s is a model in %s's %s family, first listed on %s.", short, g.DevName, famName, m.Released))
		}
		io := fmt.Sprintf("It takes %s in and returns %s", famModsText(m.In), famModsText(m.Out))
		if m.Context > 0 {
			io += fmt.Sprintf(", with a context window of %s tokens", famTokText(m.Context))
			if m.MaxOut > 0 {
				io += fmt.Sprintf(" and up to %s tokens of output", famTokText(m.MaxOut))
			}
		}
		sents = append(sents, io+".")
		if m.PromptPM > 0 || m.CompletionPM > 0 {
			sents = append(sents, fmt.Sprintf("On the lowest-cost route in the OpenRouter catalog it costs %s per million input tokens and %s per million output tokens.", famCost(m.PromptPM), famCost(m.CompletionPM)))
		} else {
			sents = append(sents, "The OpenRouter catalog lists it at no charge per token on at least one route.")
		}
		if m.Reasoning {
			sents = append(sents, "It supports a reasoning mode, where the model works through a problem before it answers.")
		}
		if m.HFID != "" {
			sents = append(sents, fmt.Sprintf("Its weights are published on Hugging Face as %s, so it can be run on your own hardware as well as through a provider.", m.HFID))
		} else {
			sents = append(sents, "Its weights are not published, so it is reached through an API.")
		}
		if m.Cutoff != "" {
			sents = append(sents, fmt.Sprintf("Its training data runs to %s.", m.Cutoff))
		}
		answer := strings.Join(sents, " ")
		// Where it sits in the family.
		cmp := []string{}
		if !m.Delisted && len(live) > 1 {
			pos := 0
			for _, x := range live {
				if x.Created > m.Created {
					pos++
				}
			}
			switch pos {
			case 0:
				cmp = append(cmp, fmt.Sprintf("It is the newest of the %d versions of %s listed today.", len(live), famName))
			case len(live) - 1:
				cmp = append(cmp, fmt.Sprintf("It is the oldest of the %d versions of %s still listed.", len(live), famName))
			default:
				cmp = append(cmp, fmt.Sprintf("Of the %d versions of %s listed today, %d are newer.", len(live), famName, pos))
			}
			if median > 0 && m.PromptPM > 0 {
				switch {
				case m.PromptPM < median:
					cmp = append(cmp, fmt.Sprintf("Its input price is below the family median of %s per million tokens.", famUSD(median)))
				case m.PromptPM > median:
					cmp = append(cmp, fmt.Sprintf("Its input price is above the family median of %s per million tokens.", famUSD(median)))
				default:
					cmp = append(cmp, fmt.Sprintf("Its input price is the family median, %s per million tokens.", famUSD(median)))
				}
			}
			if m.Context > 0 && m.Context == ctxMax {
				cmp = append(cmp, "No version in the family has a larger context window.")
			}
		}
		if m.Delisted {
			cmp = append(cmp, "It is no longer listed in the OpenRouter catalog. This page keeps its last recorded details.")
		}
		if m.Expires != "" {
			cmp = append(cmp, fmt.Sprintf("Its developer has scheduled it for retirement on %s.", m.Expires))
		}
		providers := []map[string]any{}
		for _, e := range m.Providers {
			pn, _ := e["provider"].(string)
			if pn == "" {
				continue
			}
			providers = append(providers, map[string]any{"provider": pn, "prompt_pm": e["prompt_pm"], "completion_pm": e["completion_pm"], "context": e["context"]})
		}
		sort.SliceStable(providers, func(a, b int) bool {
			pa, _ := providers[a]["prompt_pm"].(float64)
			pb, _ := providers[b]["prompt_pm"].(float64)
			return pa < pb
		})
		faq := []map[string]string{
			{"q": "How much does " + short + " cost?", "a": sents[2]},
			{"q": "How large is the context window of " + short + "?", "a": func() string {
				if m.Context > 0 {
					return fmt.Sprintf("%s tokens of input, per the OpenRouter catalog.", famTokText(m.Context))
				}
				return "The catalog does not state it."
			}()},
		}
		if famUID != "" {
			faq = append(faq, map[string]string{"q": "Which family does " + short + " belong to?", "a": fmt.Sprintf("%s, %s's model line, with %d versions listed today.", famName, g.DevName, len(live))})
		} else {
			faq = append(faq, map[string]string{"q": "Who makes " + short + "?", "a": g.DevName + ". It is the only version of its line in the catalog so far; when a second arrives, this page joins a family page."})
		}
		_ = i
		doc := map[string]any{
			"uid": uid, "page_uid": uid, "slug": "model-" + uid, "shape": "model", "name": short, "full_name": m.Name,
			"title": short + ": price, context window and specs", "id": m.ID, "developer": g.DevName,
			"family":      famRef,
			"parent_name": cat.name, "parent_path": famBase + cat.uid + "/", "hub_name": "Foundation Models", "hub_path": famBase + "70d363c9/",
			"answer": answer, "compare": cmp, "released": m.Released, "context": m.Context, "max_output": m.MaxOut,
			"prompt_pm": m.PromptPM, "completion_pm": m.CompletionPM, "knowledge_cutoff": m.Cutoff, "reasoning": m.Reasoning,
			"input": m.In, "output": m.Out, "open_weights": m.HFID != "", "hf_id": m.HFID, "expires": m.Expires,
			"delisted": m.Delisted, "providers": providers, "siblings": siblings, "faq": faq,
			"source":    map[string]string{"name": "OpenRouter model catalog", "url": "https://openrouter.ai/" + m.ID},
			"generated": today, "refresh_every_days": 1,
		}
		if famUID == "" {
			doc["siblings"] = []map[string]any{}
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count) VALUES ($1,'tech-section',$2::jsonb,NULL,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()
			WHERE (twoai_pages.data - 'built_at') IS DISTINCT FROM EXCLUDED.data`, "tech/model-"+uid+".json", string(j)); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_model_families model page:", err)
		}
	}
	return out
}

// famCost is a per-token price for a sentence: "$0.25", or "nothing".
func famCost(v float64) string {
	if v == 0 {
		return "nothing"
	}
	return famUSD(v)
}

type famSingle struct {
	g   *famGroup
	m   famMember
	cat famCat
}

// famSingles finds current single models, records them, and writes their
// pages. A line is single while it has one live member and no family row.
func famSingles(db *sql.DB, groups map[string]*famGroup, today string) []famSingle {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_singles (
		ext_id text PRIMARY KEY, uid text NOT NULL, family_key text NOT NULL, category text NOT NULL, added_on date NOT NULL DEFAULT current_date)`)
	cut := time.Now().AddDate(0, 0, -90).Unix()
	for _, g := range groups {
		l := g.live()
		if len(l) != 1 || l[0].Created < cut {
			continue
		}
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_model_families WHERE family_key=$1)`, g.Key).Scan(&exists)
		if exists {
			continue
		}
		if ft, of := famFineTune(db, g, groups); ft {
			fmt.Printf("twoai_model_families: single %s skipped, a fine-tune line of %s\n", l[0].Name, of)
			continue
		}
		cat := ""
		for _, c := range famCats {
			if c.member(l[0]) {
				cat = c.slug
				break
			}
		}
		if cat == "" {
			continue
		}
		res, _ := db.Exec(`INSERT INTO twoai_model_singles (ext_id, uid, family_key, category) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			l[0].ID, famModelUID(l[0].ID), g.Key, cat)
		if k, _ := res.RowsAffected(); k > 0 {
			fmt.Printf("twoai_model_families: single model tracked: %s (%s)\n", l[0].Name, cat)
		}
	}
	var out []famSingle
	rows, err := db.Query(`SELECT ext_id, family_key, category FROM twoai_model_singles ORDER BY added_on, ext_id`)
	if err != nil {
		return out
	}
	type row struct{ id, key, cat string }
	var rs []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.key, &r.cat) == nil {
			rs = append(rs, r)
		}
	}
	rows.Close()
	for _, r := range rs {
		g := groups[r.key]
		if g == nil || len(g.live()) > 1 {
			// A family now: its member page, at the same uid, is written by
			// the family step once the family is built.
			continue
		}
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_model_families WHERE family_key=$1)`, r.key).Scan(&exists)
		if exists {
			continue
		}
		famModelPages(db, g, "", famCatBySlug(r.cat), today)
		for _, m := range g.Members {
			if m.ID == r.id && !m.Delisted {
				out = append(out, famSingle{g, m, famCatBySlug(r.cat)})
			}
		}
	}
	return out
}
