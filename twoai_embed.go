package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// twoaiEmbed builds the retrieval index the site assistant answers from.
//
// WHAT IT DOES. Walks every twoai_pages document, flattens it to readable text,
// splits that into ~500-token chunks on paragraph boundaries, and stores each
// chunk with its embedding, its page URL and its title in twoai_embeddings.
//
// THE URL AND TITLE ARE NOT DECORATION. An answer is generated from the
// retrieved TEXT and must cite the page it came from. A vector store that keeps
// only vectors can rank but cannot attribute, and an unattributed answer on
// this site is worth less than no answer: the whole reason a model or a
// journalist can quote us is that every claim traces to a page with a date on
// it.
//
// ONLY CHANGED CHUNKS ARE EMBEDDED. body_hash decides. A full backfill is
// roughly 6,200 chunks and costs under a dollar; after that a normal day
// re-embeds the handful of chunks whose text actually moved, for cents. Without
// this the stage would re-embed 3.1M tokens every night to no purpose.
//
// FAILURE POSTURE. A chunk that fails to embed is left unstored, so the next
// run retries it. Partial indexes are fine - retrieval degrades gracefully by
// missing a chunk - but a chunk stored with a stale or absent vector would be
// silently wrong, which is the failure mode this whole codebase keeps finding.
const (
	twoaiEmbedModel = "@cf/baai/bge-m3"
	twoaiEmbedDims  = 1024
	// Roughly 500 tokens. Chunks that are too small lose the context that makes
	// a passage answerable; too large and the retrieved text drowns the question.
	twoaiChunkChars = 2000
	twoaiChunkOver  = 200
)

var twoaiWS = regexp.MustCompile(`[ \t]+`)

// HTML REACHED THE INDEX. 578 chunks carried raw <p>, <h2 id=...>, <nav class=...>
// markup, visible in the first retrieval probe: the top result for "AI laws in
// Texas" opened with a nav element. Tags are not just noise, they are PAID
// noise - every one is tokens sent to the answer model that cannot help it, and
// they dilute the embedding of the sentence they wrap.
var twoaiTag = regexp.MustCompile(`<[^>]{1,200}>`)
var twoaiEnt = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'",
	"&rsquo;", "\u2019", "&ldquo;", `"`, "&rdquo;", `"`, "&nbsp;", " ", "&mdash;", "-",
)

func twoaiStripHTML(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	// Block tags become paragraph breaks so the chunker still has boundaries to
	// split on; everything else simply goes.
	s = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|section)>`).ReplaceAllString(s, "\n\n")
	return twoaiEnt.Replace(twoaiTag.ReplaceAllString(s, " "))
}

var twoaiBlank = regexp.MustCompile(`\n{3,}`)

// twoaiFlatten turns a page document into readable prose. It deliberately keeps
// only strings a human would read: numbers, ids, hashes and booleans are noise
// in a retrieval index and actively hurt ranking.
func twoaiFlatten(v any, out *strings.Builder, depth int) {
	if depth > 6 {
		return
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(twoaiStripHTML(t))
		// Skip URLs, slugs, dates and other machine strings.
		if len(s) < 25 || strings.HasPrefix(s, "http") || !strings.Contains(s, " ") {
			return
		}
		out.WriteString(s)
		out.WriteString("\n\n")
	case []any:
		for _, x := range t {
			twoaiFlatten(x, out, depth+1)
		}
	case map[string]any:
		// Headings first so a chunk reads in a sensible order.
		for _, k := range []string{"name", "title", "heading", "term", "case_name"} {
			if s, ok := t[k].(string); ok && len(s) > 2 {
				out.WriteString(s)
				out.WriteString("\n")
			}
		}
		for k, x := range t {
			switch k {
			case "uid", "slug", "url", "href", "generated", "updated_at", "data_hash",
				"name", "title", "heading", "term", "case_name":
				continue
			}
			twoaiFlatten(x, out, depth+1)
		}
	}
}

func twoaiChunk(text string) []string {
	text = twoaiBlank.ReplaceAllString(twoaiWS.ReplaceAllString(text, " "), "\n\n")
	paras := strings.Split(text, "\n\n")

	// A paragraph longer than the cap is split on sentence boundaries first.
	// Without this a page with no blank lines - a long docket entry, a dense
	// summary - produced ONE chunk of several thousand characters, which the
	// unit test caught: 6,961 chars in a single chunk against a 2,000 cap. That
	// chunk would have been embedded as one vector, so retrieval could never
	// isolate the sentence that answers the question, and the whole passage
	// would flood the answer model's context.
	var split []string
	for _, p := range paras {
		for len(p) > twoaiChunkChars {
			cut := strings.LastIndex(p[:twoaiChunkChars], ". ")
			if cut < twoaiChunkChars/2 {
				cut = strings.LastIndex(p[:twoaiChunkChars], " ")
			}
			if cut < 1 {
				cut = twoaiChunkChars
			} else {
				cut++
			}
			split = append(split, strings.TrimSpace(p[:cut]))
			p = strings.TrimSpace(p[cut:])
		}
		if p != "" {
			split = append(split, p)
		}
	}
	paras = split

	var chunks []string
	var cur strings.Builder
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if cur.Len()+len(p) > twoaiChunkChars && cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			// Overlap so a sentence split across a boundary is still findable
			// from either side.
			prev := cur.String()
			cur.Reset()
			if len(prev) > twoaiChunkOver {
				cur.WriteString(prev[len(prev)-twoaiChunkOver:])
				cur.WriteString("\n\n")
			}
		}
		cur.WriteString(p)
		cur.WriteString("\n\n")
	}
	if strings.TrimSpace(cur.String()) != "" {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

func twoaiEmbedBatch(texts []string) ([][]float32, error) {
	acct := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	tok := strings.TrimSpace(os.Getenv("CLOUDFLARE_AI_TOKEN"))
	if acct == "" || tok == "" {
		return nil, fmt.Errorf("CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_AI_TOKEN must be set")
	}
	body, _ := json.Marshal(map[string]any{"text": texts})
	url := "https://api.cloudflare.com/client/v4/accounts/" + acct + "/ai/run/" + twoaiEmbedModel
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result struct {
			Data [][]float32 `json:"data"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.Success {
		msg := "unknown"
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return nil, fmt.Errorf("workers ai %d: %s", resp.StatusCode, msg)
	}
	if len(out.Result.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d vectors, got %d", len(texts), len(out.Result.Data))
	}
	return out.Result.Data, nil
}

func vecLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// twoaiURLIndex builds path/uid -> public URL resolution once per run.
//
// WHY A LOOKUP AND NOT A SWITCH. The first version of this stage hardcoded a
// handful of path prefixes and returned empty for everything else, so 2,432 of
// 2,766 documents were silently skipped and the index covered 12% of the site.
// Worse, the flagship lawsuit tracker was absent entirely and every glossary
// chunk cited the hub rather than the term page it came from - an answer would
// have said "see the glossary" instead of naming the entry.
//
// The taxonomy already knows where every section lives, so the uid map is read
// from it rather than guessed. Prefix rules cover the factories whose URLs are
// mechanical.
//
// TWO KEY FAMILIES. "tax:"+slug resolves documents that name their taxonomy
// slug. "uid:"+segment resolves documents that carry only their entity uid:
// the last segment of an entity node's live_path IS that uid, which is why the
// jobs hub document (uid 995676ef) and the taxonomy row for AI Jobs and Market
// Dynamics (live_path ending /995676ef/) can be joined without a guess. The
// first version of twoaiDocURL consulted "uid:" keys that nothing ever wrote,
// so the fallback compiled, ran, and resolved nothing - the same silent-skip
// shape as the 12% index above, one layer down.
type twoaiTaxNode struct {
	Slug, Name, Parent, Blurb, Path string
	Level                           int
}

// twoaiIntentLine turns a section's name and blurb into the question forms a
// reader would type to reach it, so the directory chunk's embedding sits near
// natural queries and not only near topical nouns. It composes, never invents:
// every phrase is built from words already in the name or blurb. Returns "" if
// the section has no recognizable user-intent shape, in which case the chunk
// keeps only its descriptive text.
func twoaiIntentLine(name, blurb string) string {
	hay := strings.ToLower(name + " " + blurb)
	var qs []string
	// Each trigger is a topic the section demonstrably covers (the word is in
	// its own name or blurb); the appended question is the register a person
	// uses for that topic. This is the same "compose from fields" rule the
	// rest of the file follows - the trigger gates on the section's own text.
	add := func(present []string, questions ...string) {
		for _, w := range present {
			if strings.Contains(hay, w) {
				qs = append(qs, questions...)
				return
			}
		}
	}
	add([]string{"job", "hiring", "employ", "career", "roles", "workforce"},
		"How do I get a job in AI? Who is hiring for AI roles and what do they pay?")
	add([]string{"skill", "learn", "training", "course", "upskill"},
		"What skills do I need for AI work and how do I learn them?")
	add([]string{"company", "companies", "vendor", "startup", "lab", "provider"},
		"Which companies work in AI, what do they build, and how do they compare?")
	add([]string{"tool", "product", "platform", "app"},
		"What AI tools are available and which one should I use?")
	add([]string{"law", "regulation", "policy", "compliance", "governance", "act"},
		"What are the AI laws and rules, and how do I comply?")
	add([]string{"lawsuit", "litigation", "court", "case"},
		"Who is being sued over AI and what is the status of the case?")
	add([]string{"model", "benchmark", "leaderboard"},
		"Which AI models exist and which performs best?")
	add([]string{"research", "paper", "arxiv", "study"},
		"What does the latest AI research say?")
	add([]string{"news", "announcement", "launch", "update"},
		"What is the latest AI news?")
	if len(qs) == 0 {
		return ""
	}
	// One line, the strongest match first; the add order above is priority
	// order, and de-duplication keeps a multi-topic section from repeating.
	seen := map[string]bool{}
	var out []string
	for _, q := range qs {
		if !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	return strings.Join(out, " ")
}

func twoaiTaxIndex(db *sql.DB) (map[string]string, []twoaiTaxNode) {
	idx := map[string]string{}
	var nodes []twoaiTaxNode
	r, err := db.Query(`SELECT t.slug, t.name, coalesce(p.name,''), coalesce(t.blurb,''),
			t.live_path, t.level
		FROM twoai_taxonomy t
		LEFT JOIN twoai_taxonomy p ON p.slug = t.parent_slug
		WHERE t.status='live' AND t.live_path IS NOT NULL
		ORDER BY t.level, t.sort, t.slug`)
	if err != nil {
		return idx, nodes
	}
	for r.Next() {
		var n twoaiTaxNode
		if r.Scan(&n.Slug, &n.Name, &n.Parent, &n.Blurb, &n.Path, &n.Level) != nil {
			continue
		}
		nodes = append(nodes, n)
		if strings.Contains(n.Path, "#") {
			continue
		}
		url := "https://theworldofai.org" + n.Path
		if _, dup := idx["tax:"+n.Slug]; !dup {
			idx["tax:"+n.Slug] = url
		}
		// Several taxonomy rows share one live_path (a domain and the section
		// inside it). Level ordering in the query means the broader name claims
		// the uid key and the dup guard keeps it, so the jobs hub is titled
		// "AI Jobs and Market Dynamics" rather than its narrowest alias.
		seg := strings.Trim(n.Path, "/")
		if i := strings.LastIndex(seg, "/"); i >= 0 {
			seg = seg[i+1:]
		}
		if _, dup := idx["uid:"+seg]; !dup {
			idx["uid:"+seg] = url
		}
	}
	r.Close()
	return idx, nodes
}

// twoaiDocURL resolves ONE document to its page URL. Empty means the document
// is a bundle handled by twoaiBundleItems, or has no page of its own.
func twoaiDocURL(path string, doc map[string]any, idx map[string]string) string {
	const base = "https://theworldofai.org"
	name := strings.TrimSuffix(path[strings.Index(path, "/")+1:], ".json")
	prefix := path[:strings.Index(path, "/")]

	// Sections addressed by taxonomy slug: the taxonomy owns the URL, so a
	// section that moves takes its citations with it.
	if tax, ok := doc["tax"].(string); ok {
		if u, found := idx["tax:"+tax]; found {
			return u
		}
	}
	if u, found := idx["tax:"+name]; found {
		return u
	}

	switch prefix {
	case "companies":
		if name == "index" {
			return ""
		}
		return base + "/companies/" + name + "/"
	case "mcp":
		if name == "index" {
			return ""
		}
		return base + "/mcp/" + name + "/"
	case "laws":
		if name == "index" {
			return base + "/ai-laws/"
		}
		return base + "/ai-laws/" + name + "/"
	case "downloads":
		if name == "index" {
			return base + "/downloads/"
		}
		return base + "/downloads/" + name + "/"
	case "people":
		if name == "roster" || name == "index" {
			return ""
		}
		return base + "/ai-ecosystem/ecosystem-entities-market-and-operations/" + name + "/"
	case "tools":
		if strings.HasPrefix(name, "cat-") {
			return base + "/ai-tools/category/" + strings.TrimPrefix(name, "cat-") + "/"
		}
		if name == "tools" || name == "index" {
			return base + "/ai-tools/"
		}
		return base + "/ai-tools/" + name + "/"
	case "research":
		if name == "index" {
			return base + "/research/"
		}
		return base + "/research/" + name + "/"
	case "compliance":
		if name == "index" {
			return base + "/ai-compliance/"
		}
		return base + "/ai-compliance/" + name + "/"
	case "prompts":
		return base + "/ai-prompts/"
	case "skills":
		if name == "index" {
			return base + "/skills/"
		}
		return base + "/skills/" + strings.TrimPrefix(name, "occupation-") + "/"
	case "benchmarks":
		return base + "/benchmarks/"
	case "week":
		return base + "/this-week-in-ai/" + name + "/"
	case "static":
		return base + "/" + name + "/"
	}
	// Anything carrying its own uid renders at the entity node whose live_path
	// ends in that uid. The keys are written by twoaiTaxIndex; the earlier
	// version of this fallback looped over category names while consulting a
	// map nothing populated, so it never resolved a single document.
	if uid, ok := doc["uid"].(string); ok && uid != "" {
		if u, found := idx["uid:"+uid]; found {
			return u
		}
	}
	return ""
}

// twoaiBundleItems splits a bundle document into its constituent pages. A
// bundle embedded whole would cite the hub instead of the page that actually
// answers the question, which defeats the point of the index.
func twoaiBundleItems(path string, doc map[string]any) []map[string]any {
	base := "https://theworldofai.org"
	out := []map[string]any{}
	add := func(item any, url, title string) {
		if url == "" || title == "" {
			return
		}
		out = append(out, map[string]any{"__url": url, "__title": title, "item": item})
	}
	switch path {
	case "lawsuits/lawsuits.json":
		if cases, ok := doc["cases"].([]any); ok {
			for _, c := range cases {
				m, _ := c.(map[string]any)
				slug, _ := m["slug"].(string)
				name, _ := m["case_name"].(string)
				add(m, base+"/ai-lawsuits/"+slug+"/", name)
			}
		}
	case "glossary/glossary.json":
		if terms, ok := doc["terms"].([]any); ok {
			for _, t := range terms {
				m, _ := t.(map[string]any)
				slug, _ := m["slug"].(string)
				name, _ := m["term"].(string)
				add(m, base+"/ai-glossary/"+slug+"/", name)
			}
		}
	case "news/archive.json":
		if st, ok := doc["stories"].([]any); ok {
			for _, x := range st {
				m, _ := x.(map[string]any)
				slug, _ := m["slug"].(string)
				title, _ := m["Title"].(string)
				add(m, base+"/ai-news/"+slug+"/", title)
			}
		}
	case "news/vendor.json":
		if arr, ok := doc["archive"].([]any); ok {
			for _, x := range arr {
				m, _ := x.(map[string]any)
				slug, _ := m["slug"].(string)
				title, _ := m["title"].(string)
				if hp, ok := m["has_page"].(bool); ok && !hp {
					continue
				}
				add(m, base+"/ai-news/vendor/"+slug+"/", title)
			}
		}
	}
	return out
}

// twoaiDocTitle finds the human name of a page, checking where each factory
// actually puts it. Returns empty when there is none, which is a reason to skip
// the document rather than to invent a label.
func twoaiDocTitle(doc map[string]any) string {
	for _, k := range []string{"name", "title", "label", "term", "case_name", "heading"} {
		if s, ok := doc[k].(string); ok && len(strings.TrimSpace(s)) > 1 {
			return strings.TrimSpace(s)
		}
	}
	// The nest list is checked against the ACTUAL documents, not guessed. Adding
	// the title requirement without checking took skipped_docs from 12 to 1,941
	// in one run: every MCP server page nests under "server", which was not on
	// this list, so 1,896 pages left the index silently. They had been present
	// before, titled by filename, which is why the count moved rather than the
	// build failing. A stricter rule is only an improvement if it is also right.
	for _, nest := range []string{
		"company", "person", "tool", "sec", "topic", "item", "week",
		"server", "paper", "case", "profile", "bench", "occupation", "hub",
	} {
		if m, ok := doc[nest].(map[string]any); ok {
			for _, k := range []string{"name", "title", "label", "term"} {
				if s, ok := m[k].(string); ok && len(strings.TrimSpace(s)) > 1 {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// twoaiComposeSparse writes one plain sentence from the fields of a record too
// sparse to flatten into useful retrieval text. Returns empty when there is
// nothing to say, because an empty page should stay out of the index rather
// than be padded into it.
func twoaiComposeSparse(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	// Unwrap the single nested object these documents use.
	for _, k := range []string{"server", "tool", "person", "company", "item"} {
		if inner, ok := m[k].(map[string]any); ok {
			m = inner
			break
		}
	}
	str := func(k string) string {
		v, _ := m[k].(string)
		return strings.TrimSpace(v)
	}
	name := str("title")
	if name == "" {
		name = str("name")
	}
	if name == "" {
		return ""
	}

	var b strings.Builder
	desc := str("description")
	vendor := str("vendor")
	switch {
	case str("namespace") != "" || str("remote_url") != "" || str("package_id") != "":
		// An MCP registry server.
		b.WriteString(name + " is a Model Context Protocol server")
		if vendor != "" {
			b.WriteString(" published by " + vendor)
		}
		b.WriteString(" and listed in the official MCP registry. ")
		if desc != "" {
			b.WriteString(desc + " ")
		}
		if u := str("remote_url"); u != "" {
			b.WriteString("It is hosted by its publisher and connects over " +
				fallback(str("remote_type"), "HTTP") + ", so nothing is installed locally. ")
		} else if pid := str("package_id"); pid != "" {
			b.WriteString("It runs locally, installed from " +
				fallback(str("package_registry"), "a package registry") + " as " + pid +
				", and communicates over " + fallback(str("remote_type"), "stdio") + ". ")
		}
		if v := str("version"); v != "" {
			b.WriteString("The registry lists version " + v + ". ")
		}
	default:
		b.WriteString(name)
		if vendor != "" {
			b.WriteString(", from " + vendor)
		}
		b.WriteString(". ")
		if desc != "" {
			b.WriteString(desc)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) < 60 {
		return ""
	}
	return out
}

func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func twoaiEmbedRun(db *sql.DB) error {
	idx, taxNodes := twoaiTaxIndex(db)
	rows, err := db.Query(`SELECT path, kind, data::text FROM twoai_pages ORDER BY path`)
	if err != nil {
		return err
	}
	type chunkRow struct {
		path            string
		n               int
		url, title, sec string
		body, hash      string
	}
	var want []chunkRow
	skipped := 0

	// twoaiSubject pulls the one sentence that says what a page is ABOUT, from
	// wherever the factory puts it. Prefixed to every chunk of that page.
	subjectOf := func(payload any) string {
		m, ok := payload.(map[string]any)
		if !ok {
			return ""
		}
		for _, k := range []string{"server", "tool", "person", "company", "item", "case"} {
			if inner, ok := m[k].(map[string]any); ok {
				m = inner
				break
			}
		}
		for _, k := range []string{"significance", "summary", "hook", "description",
			"definition", "blurb", "answer", "tagline"} {
			if v, ok := m[k].(string); ok {
				v = strings.TrimSpace(v)
				if len(v) > 40 {
					if len(v) > 400 {
						v = v[:400]
					}
					return v
				}
			}
		}
		return ""
	}

	emit := func(key, url, title, kind string, payload any, start int) int {
		subject := subjectOf(payload)
		var sb strings.Builder
		twoaiFlatten(payload, &sb, 0)
		text := strings.TrimSpace(sb.String())

		// SPARSE RECORDS GET A COMPOSED SENTENCE RATHER THAN BEING DROPPED.
		// An MCP registry document flattens to about 100 characters: a title
		// and one short description, with the endpoint and package identifier
		// skipped as machine strings. Under the 200-character floor, so all
		// 1,900 were silently absent and the assistant could not answer "is
		// there an MCP server for Stripe" about a directory this site is one of
		// the few places to maintain.
		//
		// The sentence is ASSEMBLED FROM FIELDS, never invented: what it is,
		// who published it, how it connects. That is composition, not
		// fabrication - the same facts the page renders, written as prose a
		// retrieval model can match against.
		if len(text) < 200 {
			if composed := twoaiComposeSparse(payload); composed != "" {
				text = strings.TrimSpace(title + "\n\n" + composed + "\n\n" + text)
			}
		}
		if len(text) < 120 {
			return start
		}
		n := start
		for _, c := range twoaiChunk(text) {
			// EVERY CHUNK CARRIES ITS PAGE TITLE. Without this, a chunk of the
			// New York Times case page is a paragraph of docket text that never
			// says "OpenAI" or "lawsuit", so "is OpenAI being sued over training
			// data" ranked the company profile and a weekly digest above 89 case
			// pages and the assistant answered that we do not cover it. We do.
			//
			// The title is what the chunk is ABOUT, and embedding a passage
			// without it throws that away. Cheap, and it fixes the class of
			// question this site exists to answer.
			// EVERY CHUNK CARRIES THE SUBJECT LINE, not just the title.
			//
			// The New York Times case has five chunks. One holds the sentence
			// that says what the case IS - the Times alleges OpenAI copied
			// millions of articles to train GPT models. The other four are raw
			// docket text: motions to seal, amicus deadlines, lists of OpenAI
			// corporate entities. Ask "is OpenAI being sued over training data"
			// and those four match nothing, so the definitive training-data case
			// on the site ranked below a weekly digest.
			//
			// A docket entry read alone is unanswerable. Read under a line
			// saying which case it belongs to and what that case is about, it
			// becomes retrievable, which is how a person reads it too.
			lead := title
			if subject != "" {
				lead = title + "\n" + subject
			}
			body := lead + "\n\n" + c
			h := sha256.Sum256([]byte(body))
			want = append(want, chunkRow{key, n, url, title, kind, body, hex.EncodeToString(h[:16])})
			n++
		}
		return n
	}

	for rows.Next() {
		var path, kind, raw string
		if rows.Scan(&path, &kind, &raw) != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(raw), &doc) != nil {
			continue
		}

		// THE JOBS HUB IS DATA, NOT PROSE, so its chunks are composed from
		// the listings themselves; the generic emit would floor them away.
		if kind == "jobs-hub" {
			if url := twoaiDocURL(path, doc, idx); url != "" {
				for i, body := range twoaiJobsChunks(doc) {
					h := sha256.Sum256([]byte(body))
					want = append(want, chunkRow{path, i, url,
						"AI Job Listings", kind, body, hex.EncodeToString(h[:16])})
				}
			}
			continue
		}

		// Bundles first: one document, many pages, each cited separately.
		if items := twoaiBundleItems(path, doc); len(items) > 0 {
			for _, it := range items {
				url, _ := it["__url"].(string)
				title, _ := it["__title"].(string)
				emit(path+"::"+url, url, title, kind, it["item"], 0)
			}
			continue
		}

		// A CITATION LABELLED "companies/d9ea9793.json" IS NOT A CITATION.
		// 731 chunks across 313 pages carried a filename as their title because
		// the fallback was the path. Company documents nest the name under
		// "company", weekly digests use "label", and several factories use
		// "sec" or "topic". Checked in order, and a document that still has no
		// human title is skipped rather than cited by filename.
		title := twoaiDocTitle(doc)
		if title == "" {
			skipped++
			continue
		}
		url := twoaiDocURL(path, doc, idx)
		if url == "" {
			skipped++
			continue
		}
		emit(path, url, title, kind, doc, 0)
	}
	rows.Close()

	// EVERY LIVE SECTION GETS A DIRECTORY CHUNK, composed from twoai_taxonomy.
	//
	// WHY. Hub and section documents are data structures rather than prose: the
	// jobs hub is arrays of listings and salary rows, the company hub is 261
	// summary records, the skills hub is an O*NET matrix. twoaiFlatten keeps
	// only sentences a human would read, so these documents flattened to under
	// the floor and dropped out of the index without a trace - jobs-hub,
	// company-hub, skills-hub and most other hub kinds sat at zero chunks. The
	// visible failure, found by Stephen on 2026-08-19: asked how to get a job,
	// the assistant never pointed at the AI Jobs and Market Dynamics section or
	// the AI Company Directory, because for the assistant neither existed. It
	// answered from the only job-shaped text it had, vendor news posts and MCP
	// job-board server entries.
	//
	// THE SOURCE IS THE TAXONOMY, NOT INVENTION. Every chunk is the section's
	// own authored name and blurb plus its parent's name, the same words the
	// site renders on its section pages. Composition from fields, the rule
	// twoaiComposeSparse already established. Anchored live_paths (#salary,
	// #skills) are kept: the anchor is a real destination and the citation
	// should land the reader on it.
	//
	// KEYED UNDER taxonomy/{slug}, which no twoai_pages path uses, so these rows
	// ride the same reconciler as everything else: a section that goes dark in
	// the taxonomy leaves the index on the next run.
	for _, n := range taxNodes {
		if strings.TrimSpace(n.Blurb) == "" {
			continue
		}
		var b strings.Builder
		b.WriteString(n.Name)
		b.WriteString(" is a section of The World of AI")
		if n.Parent != "" {
			b.WriteString(", part of ")
			b.WriteString(n.Parent)
		}
		b.WriteString(". ")
		b.WriteString(strings.TrimSpace(n.Blurb))
		// INTENT LINE. The name and blurb describe what a section IS in
		// institutional voice ("AI Jobs and Market Dynamics ... roles,
		// salaries, required skills"). A reader asks in a different register
		// ("how do I get a job in AI", "who is hiring"), and bge-m3 embeds the
		// two far enough apart that the section lost to vendor news on the
		// exact query it exists to answer. This appends the question forms a
		// person types to reach this section, composed ONLY from the section's
		// own name and blurb - the destination it points to is a real page,
		// the words are the taxonomy's own. It nudges the vector toward
		// question-shaped queries without inventing a claim.
		if q := twoaiIntentLine(n.Name, n.Blurb); q != "" {
			b.WriteString(" ")
			b.WriteString(q)
		}
		body := n.Name + "\n\n" + b.String()
		if len(body) < 60 {
			continue
		}
		h := sha256.Sum256([]byte(body))
		want = append(want, chunkRow{"taxonomy/" + n.Slug, 0,
			"https://theworldofai.org" + n.Path, n.Name, "taxonomy",
			body, hex.EncodeToString(h[:16])})
	}

	have := map[string]string{}
	if r, err := db.Query(`SELECT path, chunk_no, body_hash FROM twoai_embeddings WHERE model=$1`,
		twoaiEmbedModel); err == nil {
		for r.Next() {
			var p, h string
			var n int
			if r.Scan(&p, &n, &h) == nil {
				have[fmt.Sprintf("%s#%d", p, n)] = h
			}
		}
		r.Close()
	}

	var todo []chunkRow
	live := map[string]bool{}
	for _, c := range want {
		live[fmt.Sprintf("%s#%d", c.path, c.n)] = true
		if have[fmt.Sprintf("%s#%d", c.path, c.n)] != c.hash {
			todo = append(todo, c)
		}
	}

	// Chunks no longer produced by any document are removed, so the index cannot
	// answer from a page a reader can no longer open.
	var removed int64
	if r, err := db.Query(`SELECT path, chunk_no FROM twoai_embeddings`); err == nil {
		type k struct {
			p string
			n int
		}
		var dead []k
		for r.Next() {
			var p string
			var n int
			if r.Scan(&p, &n) == nil && !live[fmt.Sprintf("%s#%d", p, n)] {
				dead = append(dead, k{p, n})
			}
		}
		r.Close()
		for _, d := range dead {
			if res, err := db.Exec(`DELETE FROM twoai_embeddings WHERE path=$1 AND chunk_no=$2`, d.p, d.n); err == nil {
				a, _ := res.RowsAffected()
				removed += a
			}
		}
	}

	if len(todo) == 0 {
		fmt.Printf("twoai_embed: chunks=%d up to date, skipped_docs=%d removed=%d\n",
			len(want), skipped, removed)
		return nil
	}

	const batch = 20
	var mu sync.Mutex
	stored, failed := 0, 0
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := 0; i < len(todo); i += batch {
		end := i + batch
		if end > len(todo) {
			end = len(todo)
		}
		group := todo[i:end]
		wg.Add(1)
		go func(group []chunkRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			texts := make([]string, len(group))
			for j, c := range group {
				texts[j] = c.body
			}
			vecs, err := twoaiEmbedBatch(texts)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed += len(group)
				fmt.Fprintf(os.Stderr, "twoai_embed: batch failed, will retry next run: %v\n", err)
				return
			}
			for j, c := range group {
				if _, err := db.Exec(`INSERT INTO twoai_embeddings
					(path, chunk_no, url, title, section, body, body_hash, embedding, model, updated_at)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8::vector,$9,now())
					ON CONFLICT (path, chunk_no) DO UPDATE SET url=EXCLUDED.url, title=EXCLUDED.title,
						section=EXCLUDED.section, body=EXCLUDED.body, body_hash=EXCLUDED.body_hash,
						embedding=EXCLUDED.embedding, model=EXCLUDED.model, updated_at=now()`,
					c.path, c.n, c.url, c.title, c.sec, c.body, c.hash,
					vecLiteral(vecs[j]), twoaiEmbedModel); err != nil {
					failed++
					continue
				}
				stored++
			}
		}(group)
	}
	wg.Wait()

	fmt.Printf("twoai_embed: chunks=%d changed=%d stored=%d failed=%d skipped_docs=%d removed=%d model=%s\n",
		len(want), len(todo), stored, failed, skipped, removed, twoaiEmbedModel)
	return nil
}

// twoaiAsk is a retrieval probe: embed a question, print what the index would
// hand an answer model, and stop there. No generation.
//
// It exists because retrieval quality is the whole ballgame and it must be
// judged on real questions before a single line of front end is written. If the
// top chunks for "what are the AI laws in Texas" are not the Texas page, no
// answer model will save it, and a fluent answer over bad retrieval is worse
// than no assistant at all: it would be confidently wrong with a citation
// attached.
//
// Usage: pipeline twoai_ask "is openai being sued over training data"
func twoaiAsk(db *sql.DB, question string) error {
	vecs, err := twoaiEmbedBatch([]string{question})
	if err != nil {
		return err
	}
	// AT MOST TWO CHUNKS PER PAGE. The first probe returned five chunks of the
	// same Texas page: the retrieval was right but the answer model would have
	// seen one source five times and had nothing to corroborate it against. A
	// question usually has one best page and several that qualify it, and the
	// qualifying ones are where an honest answer comes from.
	rows, err := db.Query(`SELECT title, url, body, score FROM (
			SELECT title, url, left(body, 220) AS body,
				1 - (embedding <=> $1::vector) AS score,
				row_number() OVER (PARTITION BY url ORDER BY embedding <=> $1::vector) AS rn
			FROM twoai_embeddings
			ORDER BY embedding <=> $1::vector
			LIMIT 40
		) q WHERE rn <= 2 ORDER BY score DESC LIMIT 6`, vecLiteral(vecs[0]))
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("\nQ: %s\n\n", question)
	i := 0
	for rows.Next() {
		var title, url, body string
		var score float64
		if rows.Scan(&title, &url, &body, &score) != nil {
			continue
		}
		i++
		fmt.Printf("%d. [%.3f] %s\n   %s\n   %s...\n\n", i, score, title, url,
			strings.TrimSpace(strings.ReplaceAll(body, "\n", " ")))
	}
	if i == 0 {
		fmt.Println("no matches at all, which means the index is empty or the vector dimension is wrong")
	}
	return nil
}

// twoaiJobsChunks composes retrieval chunks for the jobs hub from the listing
// data itself.
//
// WHY THIS EXISTS. The jobs hub document is arrays: 2,000+ listing records,
// salary rows, an O*NET skills matrix. twoaiFlatten keeps prose, so the hub
// contributed only its taxonomy directory chunk - a description of what the
// section IS, with not one actual job in the index. Asked for open remote
// jobs on 2026-08-20, the assistant retrieved MCP job-board server entries
// and the directory chunk, and honestly concluded the site lists no
// positions. It lists 2,124.
//
// COMPOSED, NEVER INVENTED. Every line is a listing's own title, company and
// location; every count is computed from the same array the page renders.
// The layout mirrors how people ask: a summary chunk for "who is hiring",
// remote chunks for "remote AI jobs" (the exact query that failed), and one
// chunk per job function so "AI safety roles" or "ML engineering jobs" lands
// on lines of real openings rather than a section blurb.
//
// BOUNDED. Lines are capped per chunk to stay inside twoaiChunkChars, and
// overflow is stated ("and N more on the page") rather than silently cut, the
// same honesty rule the ecosystem counts follow. Listings churn daily, so
// these hashes churn daily; that is the embed reconciler working as designed,
// at ~20 chunks of cost.
func twoaiJobsChunks(doc map[string]any) []string {
	type job struct{ title, company, location, function string }
	var jobs []job
	arr, _ := doc["jobs"].([]any)
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		s := func(k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
		j := job{s("title"), s("company"), s("location"), s("function")}
		if j.title == "" || j.company == "" {
			continue
		}
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return nil
	}
	line := func(j job) string {
		l := j.title + " — " + j.company
		if j.location != "" {
			l += " (" + j.location + ")"
		}
		return l
	}
	var remote []job
	byFn := map[string][]job{}
	var fnOrder []string
	for _, j := range jobs {
		if strings.Contains(strings.ToLower(j.location), "remote") {
			remote = append(remote, j)
		}
		if j.function != "" {
			if _, seen := byFn[j.function]; !seen {
				fnOrder = append(fnOrder, j.function)
			}
			byFn[j.function] = append(byFn[j.function], j)
		}
	}
	sort.SliceStable(fnOrder, func(a, b int) bool { return len(byFn[fnOrder[a]]) > len(byFn[fnOrder[b]]) })

	gen, _ := doc["generated"].(string)
	header := fmt.Sprintf("AI Job Listings\nThe World of AI lists %d open AI and AI-security positions with direct employer application links, refreshed daily", len(jobs))
	if gen != "" {
		header += " (last updated " + gen + ")"
	}
	header += fmt.Sprintf(". %d roles are remote or partly remote. Every listing below is open now; browse, filter and apply from the Job Listings page.", len(remote))

	var chunks []string

	// Summary: the "who is hiring / how many jobs" answer, with the question
	// forms appended in the reader's register, same rule as twoaiIntentLine.
	var sum strings.Builder
	sum.WriteString(header)
	sum.WriteString("\nLargest categories: ")
	max := len(fnOrder)
	if max > 10 {
		max = 10
	}
	for i := 0; i < max; i++ {
		if i > 0 {
			sum.WriteString(", ")
		}
		fmt.Fprintf(&sum, "%s (%d)", fnOrder[i], len(byFn[fnOrder[i]]))
	}
	sum.WriteString(". How do I get a job in AI? Who is hiring for AI roles right now? What AI jobs are open, including remote positions?")
	chunks = append(chunks, sum.String())

	// Remote listings: the query that demonstrably failed gets lines of real
	// openings, not a description.
	const perChunk = 20
	const maxRemoteChunks = 4
	for i := 0; i < len(remote) && len(chunks) <= maxRemoteChunks; i += perChunk {
		end := i + perChunk
		if end > len(remote) {
			end = len(remote)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s\nRemote AI jobs currently listed (%d of %d remote openings):\n", header, end-i, len(remote))
		for _, j := range remote[i:end] {
			b.WriteString(line(j))
			b.WriteString("\n")
		}
		if end < len(remote) && len(chunks) == maxRemoteChunks {
			fmt.Fprintf(&b, "and %d more remote roles on the page.", len(remote)-end)
		}
		chunks = append(chunks, b.String())
	}

	// One chunk per function, largest first, capped so daily churn stays
	// proportionate to what it buys.
	const maxFnChunks = 14
	const fnLines = 16
	for i := 0; i < len(fnOrder) && i < maxFnChunks; i++ {
		fn := fnOrder[i]
		list := byFn[fn]
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n%s roles, %d open:\n", header, fn, len(list))
		n := len(list)
		if n > fnLines {
			n = fnLines
		}
		for _, j := range list[:n] {
			b.WriteString(line(j))
			b.WriteString("\n")
		}
		if len(list) > n {
			fmt.Fprintf(&b, "and %d more %s openings on the page.", len(list)-n, fn)
		}
		chunks = append(chunks, b.String())
	}
	return chunks
}
