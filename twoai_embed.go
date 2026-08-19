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
		s := strings.TrimSpace(t)
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
func twoaiURLIndex(db *sql.DB) map[string]string {
	idx := map[string]string{}
	if r, err := db.Query(`SELECT slug, live_path FROM twoai_taxonomy
		WHERE status='live' AND live_path IS NOT NULL AND live_path NOT LIKE '%#%'`); err == nil {
		for r.Next() {
			var slug, path string
			if r.Scan(&slug, &path) == nil {
				idx["tax:"+slug] = "https://theworldofai.org" + path
			}
		}
		r.Close()
	}
	return idx
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
	// Anything carrying its own uid renders under the ecosystem routes.
	if uid, ok := doc["uid"].(string); ok && uid != "" {
		for _, cat := range []string{"technology-and-core-infrastructure",
			"ecosystem-entities-market-and-operations", "research-knowledge-and-learning",
			"enterprise-applications-governance-and-tools"} {
			if u, found := idx["uid:"+uid]; found {
				return u
			}
			_ = cat
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

func twoaiEmbedRun(db *sql.DB) error {
	idx := twoaiURLIndex(db)
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

	emit := func(key, url, title, kind string, payload any, start int) int {
		var sb strings.Builder
		twoaiFlatten(payload, &sb, 0)
		text := strings.TrimSpace(sb.String())
		if len(text) < 200 {
			return start
		}
		n := start
		for _, c := range twoaiChunk(text) {
			h := sha256.Sum256([]byte(c))
			want = append(want, chunkRow{key, n, url, title, kind, c, hex.EncodeToString(h[:16])})
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

		// Bundles first: one document, many pages, each cited separately.
		if items := twoaiBundleItems(path, doc); len(items) > 0 {
			for _, it := range items {
				url, _ := it["__url"].(string)
				title, _ := it["__title"].(string)
				emit(path+"::"+url, url, title, kind, it["item"], 0)
			}
			continue
		}

		title, _ := doc["name"].(string)
		if title == "" {
			title, _ = doc["title"].(string)
		}
		if title == "" {
			title = path
		}
		url := twoaiDocURL(path, doc, idx)
		if url == "" {
			skipped++
			continue
		}
		emit(path, url, title, kind, doc, 0)
	}
	rows.Close()

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
