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

func twoaiEmbedRun(db *sql.DB) error {
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
	for rows.Next() {
		var path, kind, raw string
		if rows.Scan(&path, &kind, &raw) != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(raw), &doc) != nil {
			continue
		}
		title, _ := doc["name"].(string)
		if title == "" {
			title, _ = doc["title"].(string)
		}
		if title == "" {
			title = path
		}
		var sb strings.Builder
		twoaiFlatten(doc, &sb, 0)
		text := strings.TrimSpace(sb.String())
		if len(text) < 200 {
			continue
		}
		// The page URL is derived from the document path, which is the same
		// mapping the site build uses. A chunk that cannot name its page is
		// useless for citation, so those are skipped rather than stored.
		url := twoaiPathToURL(path)
		if url == "" {
			continue
		}
		for i, c := range twoaiChunk(text) {
			h := sha256.Sum256([]byte(c))
			want = append(want, chunkRow{path, i, url, title, kind, c, hex.EncodeToString(h[:16])})
		}
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
	for _, c := range want {
		if have[fmt.Sprintf("%s#%d", c.path, c.n)] != c.hash {
			todo = append(todo, c)
		}
	}

	// Chunks whose page no longer exists are removed, so the index cannot answer
	// from a page a reader can no longer open.
	var removed int64
	if res, err := db.Exec(`DELETE FROM twoai_embeddings e
		WHERE NOT EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = e.path)`); err == nil {
		removed, _ = res.RowsAffected()
	}

	if len(todo) == 0 {
		fmt.Printf("twoai_embed: chunks=%d up to date, removed=%d\n", len(want), removed)
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

	fmt.Printf("twoai_embed: chunks=%d changed=%d stored=%d failed=%d removed=%d model=%s\n",
		len(want), len(todo), stored, failed, removed, twoaiEmbedModel)
	return nil
}

// twoaiPathToURL maps a content document path to the public URL of the page it
// renders, so a retrieved chunk can be cited. Anything not mapped returns empty
// and is skipped: a chunk that cannot name its page has no business in an index
// whose only job is attribution.
func twoaiPathToURL(path string) string {
	base := "https://theworldofai.org"
	switch {
	case strings.HasPrefix(path, "laws/"):
		return base + "/ai-laws/" + strings.TrimSuffix(strings.TrimPrefix(path, "laws/"), ".json") + "/"
	case strings.HasPrefix(path, "lawsuits/") && path != "lawsuits/lawsuits.json":
		return base + "/ai-lawsuits/" + strings.TrimSuffix(strings.TrimPrefix(path, "lawsuits/"), ".json") + "/"
	case strings.HasPrefix(path, "companies/") && path != "companies/index.json":
		return base + "/companies/" + strings.TrimSuffix(strings.TrimPrefix(path, "companies/"), ".json") + "/"
	case strings.HasPrefix(path, "downloads/") && path != "downloads/index.json":
		return base + "/downloads/" + strings.TrimSuffix(strings.TrimPrefix(path, "downloads/"), ".json") + "/"
	case strings.HasPrefix(path, "security/"):
		return base + "/ai-ecosystem/enterprise-applications-governance-and-tools/"
	case strings.HasPrefix(path, "ecosystem/"):
		return base + "/ai-ecosystem/"
	case path == "glossary/glossary.json":
		return base + "/ai-glossary/"
	}
	return ""
}
