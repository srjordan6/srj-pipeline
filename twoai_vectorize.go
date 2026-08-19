package main

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// twoaiVectorize pushes the retrieval index from Postgres to Cloudflare
// Vectorize, so the Worker serving theworldofai.org can query it at the edge.
//
// POSTGRES STAYS AUTHORITATIVE. twoai_embeddings is the index; Vectorize is a
// derived copy that can be dropped and rebuilt from it at any time. Two stores
// that each believe they are the source of truth is how data quietly diverges,
// and this codebase has found that failure three times in a week: a glossary
// held in two tables where only one rendered, a case timeline reading a field
// the pipeline never wrote, and 2,109 audience lenses that existed in SQL and
// nowhere else. One master, one cache, and the cache is disposable.
//
// WHAT GOES IN THE METADATA. url, title and the chunk body itself. Vectorize
// allows 10KiB of metadata per vector and our chunks are ~2KB, so the answer
// text travels with the vector and the Worker never needs the database. A
// vector store that cannot return the text it matched can rank but not cite,
// and an answer without citations is the one thing this site cannot publish.
//
// SYNC IS BY HASH, not by timestamp. Only chunks whose body_hash differs from
// what was last pushed are sent, so a normal day moves a handful of vectors.
// Vector ids must be stable and under 64 bytes; the chunk key hashed is both.
// The SAME expression is used in the SQL that finds stale vectors, so the two
// can never disagree about which id belongs to which chunk.
func md5sum(s string) []byte {
	h := md5.Sum([]byte(s))
	return h[:]
}

const (
	twoaiVectorizeIndex = "twoai-pages"
	twoaiVectorizeBatch = 500
)

func twoaiVectorizeRun(db *sql.DB) error {
	acct := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	tok := strings.TrimSpace(os.Getenv("CLOUDFLARE_VECTORIZE_TOKEN"))
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("CLOUDFLARE_AI_TOKEN"))
	}
	if acct == "" || tok == "" {
		return fmt.Errorf("CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_VECTORIZE_TOKEN (or CLOUDFLARE_AI_TOKEN) must be set")
	}

	// A push ledger, so a re-run costs nothing and a failed batch retries.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_vectorize_pushed (
		vector_id text PRIMARY KEY,
		body_hash text NOT NULL,
		pushed_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	rows, err := db.Query(`SELECT e.path, e.chunk_no, e.url, e.title, e.body, e.body_hash,
			e.embedding::text
		FROM twoai_embeddings e
		LEFT JOIN twoai_vectorize_pushed p
		  ON p.vector_id = md5(e.path || '#' || e.chunk_no)
		WHERE e.embedding IS NOT NULL
		  AND (p.body_hash IS NULL OR p.body_hash <> e.body_hash)`)
	if err != nil {
		return err
	}
	type vec struct {
		ID       string            `json:"id"`
		Values   []float32         `json:"values"`
		Metadata map[string]string `json:"metadata"`
		hash     string
	}
	var todo []vec
	for rows.Next() {
		var path, url, title, body, hash, emb string
		var n int
		if rows.Scan(&path, &n, &url, &title, &body, &hash, &emb) != nil {
			continue
		}
		var vals []float32
		if json.Unmarshal([]byte(emb), &vals) != nil || len(vals) == 0 {
			continue
		}
		// Metadata cap is 10KiB. Chunks are ~2KB, but trim defensively rather
		// than have Vectorize reject a batch for one long row.
		if len(body) > 6000 {
			body = body[:6000]
		}
		todo = append(todo, vec{
			ID:     fmt.Sprintf("%x", md5sum(path+"#"+fmt.Sprint(n))),
			Values: vals,
			Metadata: map[string]string{
				"url": url, "title": title, "body": body,
			},
			hash: hash,
		})
	}
	rows.Close()

	if len(todo) == 0 {
		fmt.Println("twoai_vectorize: nothing changed")
		return nil
	}

	client := &http.Client{Timeout: 120 * time.Second}
	endpoint := "https://api.cloudflare.com/client/v4/accounts/" + acct +
		"/vectorize/v2/indexes/" + twoaiVectorizeIndex + "/upsert"

	pushed, failed := 0, 0
	for i := 0; i < len(todo); i += twoaiVectorizeBatch {
		end := i + twoaiVectorizeBatch
		if end > len(todo) {
			end = len(todo)
		}
		group := todo[i:end]

		// Vectorize takes NDJSON: one vector per line.
		var buf bytes.Buffer
		for _, v := range group {
			line, _ := json.Marshal(map[string]any{
				"id": v.ID, "values": v.Values, "metadata": v.Metadata,
			})
			buf.Write(line)
			buf.WriteByte('\n')
		}
		req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(buf.Bytes()))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err := client.Do(req)
		if err != nil {
			failed += len(group)
			fmt.Fprintf(os.Stderr, "twoai_vectorize: batch failed, retries next run: %v\n", err)
			continue
		}
		var out struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if !out.Success {
			msg := "unknown"
			if len(out.Errors) > 0 {
				msg = out.Errors[0].Message
			}
			failed += len(group)
			// Unmarked, so the next run retries. A vector recorded as pushed but
			// absent from the index would be silently missing from every answer.
			fmt.Fprintf(os.Stderr, "twoai_vectorize: %d rejected (%s)\n", resp.StatusCode, msg)
			continue
		}
		for _, v := range group {
			db.Exec(`INSERT INTO twoai_vectorize_pushed (vector_id, body_hash, pushed_at)
				VALUES ($1,$2,now()) ON CONFLICT (vector_id)
				DO UPDATE SET body_hash=EXCLUDED.body_hash, pushed_at=now()`, v.ID, v.hash)
			pushed++
		}
	}

	// Vectors whose chunk no longer exists are deleted, so the assistant cannot
	// answer from a page a reader can no longer open.
	var stale []string
	if r, err := db.Query(`SELECT p.vector_id FROM twoai_vectorize_pushed p
		WHERE NOT EXISTS (SELECT 1 FROM twoai_embeddings e
			WHERE md5(e.path || '#' || e.chunk_no) = p.vector_id)`); err == nil {
		for r.Next() {
			var id string
			if r.Scan(&id) == nil {
				stale = append(stale, id)
			}
		}
		r.Close()
	}
	removed := 0
	if len(stale) > 0 {
		body, _ := json.Marshal(map[string]any{"ids": stale})
		req, _ := http.NewRequest("POST",
			"https://api.cloudflare.com/client/v4/accounts/"+acct+
				"/vectorize/v2/indexes/"+twoaiVectorizeIndex+"/delete_by_ids",
			bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				for _, id := range stale {
					db.Exec(`DELETE FROM twoai_vectorize_pushed WHERE vector_id=$1`, id)
					removed++
				}
			}
		}
	}

	fmt.Printf("twoai_vectorize: pushed=%d failed=%d removed=%d index=%s\n",
		pushed, failed, removed, twoaiVectorizeIndex)
	return nil
}
