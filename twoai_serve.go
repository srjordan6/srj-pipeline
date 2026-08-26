package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// twoaiServe runs the site assistant endpoint.
//
// WHY THIS LIVES IN THE PIPELINE BINARY AND NOT IN A CLOUDFLARE WORKER. The
// retrieval index is in Postgres in Ohio behind a one-IP allow list. A Worker
// cannot reach it, and opening the allow list to Cloudflare's egress range to
// avoid writing 200 lines of Go would be a poor trade. Deployed as a Render Web
// Service from this same repo with `./pipeline serve`, it sits inside the
// database's own environment, reuses the embedding code the indexer already
// uses, and needs no new credentials.
//
// THE RULES THIS ENDPOINT ENFORCES, in order of importance:
//
//  1. It answers ONLY from retrieved chunks. No web access, no model
//     recollection. When retrieval is thin it says the site does not cover the
//     question and logs it to twoai_answer_gaps, because a logged gap gets
//     researched and published once for everyone, while a guess helps one
//     person unverifiably and puts an unsourced claim under our domain name.
//  2. Every answer carries the pages it came from. An answer without citations
//     is worth less than no answer on a site whose entire value is that claims
//     trace to a dated page.
//  3. Llama Guard runs in SHADOW MODE: every question screened, every verdict
//     recorded, nothing blocked. This corpus is ABOUT deepfakes, extremism
//     policy and abuse litigation, so a classifier reading surface terms would
//     refuse the site's own tracker to the audience it was built for. The
//     decision to enforce gets made from our own traffic in twoai_answer_guard.
const (
	// ANSWERING GOES DIRECT TO ANTHROPIC, NOT THROUGH WORKERS AI.
	//
	// This file used to name "@cf/anthropic/claude-haiku-4.5" and build a
	// chat-completions payload with the system prompt inside the messages
	// array. Both were wrong, and the same pair of mistakes cost the live
	// Worker weeks: the partner route answers 2021 Invalid User Credentials
	// until Workers AI unified billing is enabled, and Anthropic's API takes
	// system as a TOP-LEVEL string, not a message. The Worker was fixed by
	// dropping the partner route entirely and calling Anthropic with its own
	// key; this path now matches, so activating it will not re-run that
	// diagnosis. The guard stays on Workers AI because Llama Guard genuinely
	// is a Cloudflare model.
	twoaiAnswerModel = "claude-haiku-4-5"
	twoaiGuardModel  = "@cf/meta/llama-guard-3-8b"
	// Below this cosine similarity the site genuinely does not cover the
	// question. Tuned from the retrieval probe: real hits scored 0.63 to 0.71,
	// and the fifth-best chunk of a good question sat around 0.52.
	twoaiScoreFloor  = 0.45
	twoaiTopK        = 6
	twoaiMaxQuestion = 500
)

type askHit struct {
	Title string  `json:"title"`
	URL   string  `json:"url"`
	Body  string  `json:"-"`
	Score float64 `json:"score"`
}

// Per-IP rate limiting. In memory is enough: a single instance serves this, and
// a limit that resets on deploy is still a limit. Cloudflare rate limiting sits
// in front of it as the real control; this is the backstop that protects the
// answer-model spend if that is ever misconfigured.
type rateLimiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

func (r *rateLimiter) allow(ip string, limit int, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	keep := r.seen[ip][:0]
	for _, t := range r.seen[ip] {
		if now.Sub(t) < window {
			keep = append(keep, t)
		}
	}
	r.seen[ip] = keep
	if len(keep) >= limit {
		return false
	}
	r.seen[ip] = append(r.seen[ip], now)
	return true
}

func cfRun(model string, payload any) (map[string]any, error) {
	acct := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	tok := strings.TrimSpace(os.Getenv("CLOUDFLARE_AI_TOKEN"))
	if acct == "" || tok == "" {
		return nil, fmt.Errorf("CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_AI_TOKEN must be set")
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST",
		"https://api.cloudflare.com/client/v4/accounts/"+acct+"/ai/run/"+model, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if ok, _ := out["success"].(bool); !ok {
		return nil, fmt.Errorf("workers ai %d on %s", resp.StatusCode, model)
	}
	return out, nil
}

// twoaiGuard screens a question and returns the verdict. Errors are treated as
// "safe": a classifier that is down must not take the assistant down with it,
// and in shadow mode the verdict changes nothing anyway.
func twoaiGuard(question string) (string, string) {
	out, err := cfRun(twoaiGuardModel, map[string]any{
		"messages": []map[string]string{{"role": "user", "content": question}},
	})
	if err != nil {
		return "error", err.Error()
	}
	res, _ := out["result"].(map[string]any)
	text, _ := res["response"].(string)
	text = strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(text), "unsafe") {
		parts := strings.SplitN(text, "\n", 2)
		cat := ""
		if len(parts) > 1 {
			cat = strings.TrimSpace(parts[1])
		}
		return "unsafe", cat
	}
	return "safe", ""
}

func twoaiRetrieve(db *sql.DB, question string) ([]askHit, error) {
	vecs, err := twoaiEmbedBatch([]string{question})
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT title, url, body, score FROM (
			SELECT title, url, body,
				1 - (embedding <=> $1::vector) AS score,
				row_number() OVER (PARTITION BY url ORDER BY embedding <=> $1::vector) AS rn
			FROM twoai_embeddings
			ORDER BY embedding <=> $1::vector
			LIMIT 40
		) q WHERE rn <= 2 ORDER BY score DESC LIMIT $2`, vecLiteral(vecs[0]), twoaiTopK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []askHit
	for rows.Next() {
		var h askHit
		if rows.Scan(&h.Title, &h.URL, &h.Body, &h.Score) == nil {
			hits = append(hits, h)
		}
	}
	return hits, nil
}

const twoaiAskSystem = `You answer questions about artificial intelligence using ONLY the excerpts provided from theworldofai.org.

RULES, in order:
1. Use only what is in the excerpts. If they do not answer the question, say so plainly: "The World of AI does not cover that yet." Do not fill the gap from your own knowledge, and never guess a date, a number, a case outcome or a legal requirement.
2. Cite the pages you used by their titles, naturally, in the sentence that uses them.
3. Be brief. Two or three short paragraphs at most. Lead with the answer.
4. Where the excerpts disagree or are dated, say so rather than smoothing it over.
5. Plain English. No hype. Commas rather than dashes.
6. You are a reference work, not a salesperson and not a lawyer. Never give legal advice; report what the sources say and note that the primary source should be checked for anything that matters.`

func twoaiServe(db *sql.DB) error {
	rl := &rateLimiter{seen: map[string][]time.Time{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		var n int
		db.QueryRow(`SELECT count(*) FROM twoai_embeddings`).Scan(&n)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": n > 0, "chunks": n})
	})

	mux.HandleFunc("/api/ask", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://theworldofai.org")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		if r.Method != "POST" {
			http.Error(w, `{"error":"POST only"}`, 405)
			return
		}
		ip := r.Header.Get("CF-Connecting-IP")
		if ip == "" {
			ip = strings.Split(r.RemoteAddr, ":")[0]
		}
		if !rl.allow(ip, 10, time.Minute) {
			w.WriteHeader(429)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "Too many questions in a short time. Try again in a minute."})
			return
		}

		var in struct {
			Question string `json:"question"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in) != nil {
			http.Error(w, `{"error":"bad request"}`, 400)
			return
		}
		q := strings.TrimSpace(in.Question)
		if len(q) < 3 {
			http.Error(w, `{"error":"ask a question"}`, 400)
			return
		}
		if len(q) > twoaiMaxQuestion {
			q = q[:twoaiMaxQuestion]
		}

		// Shadow screening: recorded, never blocking. See twoai_answer_guard.
		go func(question string) {
			verdict, cats := twoaiGuard(question)
			db.Exec(`INSERT INTO twoai_answer_guard (question, verdict, categories, enforced, answered)
				VALUES ($1,$2,NULLIF($3,''),false,true)`, question, verdict, cats)
		}(q)

		hits, err := twoaiRetrieve(db, q)
		if err != nil {
			log.Println("retrieve:", err)
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(map[string]any{"error": "Search is unavailable right now."})
			return
		}

		best := 0.0
		if len(hits) > 0 {
			best = hits[0].Score
		}
		norm := strings.ToLower(strings.Join(strings.Fields(q), " "))

		if len(hits) == 0 || best < twoaiScoreFloor {
			topPath := ""
			if len(hits) > 0 {
				topPath = hits[0].URL
			}
			// The gap is the product. Recorded with a count so the recurring
			// ones can be researched and published, after which this question
			// gets a real answer with a citation, permanently.
			db.Exec(`INSERT INTO twoai_answer_gaps (question, question_norm, best_score, top_path)
				VALUES ($1,$2,$3,NULLIF($4,''))
				ON CONFLICT (question_norm) DO UPDATE SET asked_count = twoai_answer_gaps.asked_count + 1,
					last_asked = now(), best_score = GREATEST(twoai_answer_gaps.best_score, EXCLUDED.best_score)`,
				q, norm, best, topPath)
			json.NewEncoder(w).Encode(map[string]any{
				"answered": false,
				"answer": "The World of AI does not cover that yet. The question has been recorded, " +
					"and topics that come up repeatedly get researched and published.",
				"sources": []askHit{},
			})
			return
		}

		var ctx strings.Builder
		for i, h := range hits {
			fmt.Fprintf(&ctx, "[%d] %s (%s)\n%s\n\n", i+1, h.Title, h.URL, h.Body)
		}
		answer, err := anthropicAnswer(twoaiAskSystem,
			"Excerpts from theworldofai.org:\n\n"+ctx.String()+
				"\nQuestion: "+q+"\n\nAnswer using only the excerpts above.")
		if err != nil {
			log.Println("answer:", err)
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(map[string]any{"error": "The assistant is unavailable right now."})
			return
		}

		// Sources are the pages actually retrieved, deduplicated, in rank order.
		seen := map[string]bool{}
		var srcs []askHit
		for _, h := range hits {
			if seen[h.URL] {
				continue
			}
			seen[h.URL] = true
			srcs = append(srcs, askHit{Title: h.Title, URL: h.URL, Score: h.Score})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"answered": true,
			"answer":   strings.TrimSpace(answer),
			"sources":  srcs,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	log.Printf("twoai_serve: listening on :%s, answer model %s", port, twoaiAnswerModel)
	return http.ListenAndServe(":"+port, mux)
}

// anthropicAnswer calls the Anthropic Messages API directly.
//
// The system prompt is a TOP-LEVEL string. Putting it in the messages array,
// which is the Workers AI chat-completions shape, is silently accepted as a
// user turn by some gateways and rejected by others, and it was half of why
// the live assistant sat on a fallback model for weeks.
func anthropicAnswer(system, user string) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY not set")
	}
	body, _ := json.Marshal(map[string]any{
		"model":      twoaiAnswerModel,
		"max_tokens": 700,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	answer := ""
	for _, c := range out.Content {
		answer += c.Text
	}
	return answer, nil
}
