package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// PAPER EXPLANATIONS: the pipeline writes them, because nothing else did.
//
// The research paper pages carry three audience explanations, beginner,
// practitioner, business. They were meant to be written by a scheduled
// desktop task, which ran once on 2026-08-03, produced five papers, and
// never ran again; on 2026-08-29 the other 129 pages were placeholder text
// and AdSense flagged the site for low value content the same day. A
// content job that has to survive a laptop being asleep belongs in the
// cron with everything else, so it now lives here.
//
// THE GROUNDING RULE IS ABSOLUTE. An explanation is written only when a
// real abstract for the paper is in hand: first from the local twoai_works
// mirror (exact title match against 359k OpenAlex rows), then from the
// OpenAlex API by title search. A paper with no abstract found is SKIPPED,
// not summarised from its title, because a summary of a title is fiction
// wearing a citation. The abstract grounds the writing and is never stored
// or displayed: twoai_works marks abstracts cite_only, and the site's own
// rule is that publisher prose is not republished. What ships is this
// site's own words, and the model is told to quote nothing.
//
// Twelve papers per run, most cited first, so the most-read pages fill in
// first and the whole backlog clears in about eleven days. A paper that
// gains its explanations re-enters the sitemap automatically on the next
// build, because the thin-page checks read the same fields this writes.
func twoaiPaperExplain(db *sql.DB) error {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Println("twoai_paper_explain: ANTHROPIC_API_KEY not set, skipped")
		return nil
	}
	rows, err := db.Query(`SELECT uid, title, COALESCE(authors,''), COALESCE(year,0),
			COALESCE(journal,''), COALESCE(our_note,''), COALESCE(doi,'')
		FROM twoai_research_papers
		WHERE COALESCE(explain_beginner,'')='' AND COALESCE(explain_practitioner,'')=''
			AND COALESCE(explain_business,'')=''
		ORDER BY COALESCE(citations,0) DESC, uid
		LIMIT 12`)
	if err != nil {
		return err
	}
	type paper struct {
		uid, title, authors, journal, note, doi string
		year                                    int
	}
	var todo []paper
	for rows.Next() {
		var p paper
		if rows.Scan(&p.uid, &p.title, &p.authors, &p.year, &p.journal, &p.note, &p.doi) == nil {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return nil
	}

	written, local, api, skipped := 0, 0, 0, 0
	// OpenAlex now meters anonymous calls against a small daily budget, and
	// the nightly works backfill spends from the same pot. Six lookups per
	// run is the cap here; everything else waits for the local mirror to
	// catch up, which it does at roughly thirty thousand works a night.
	apiTries := 0
	for _, p := range todo {
		abstract, src := paperAbstract(db, p.title, p.doi, apiTries < 6)
		if src == "api" || (abstract == "" && apiTries < 6) {
			apiTries++
		}
		if abstract == "" {
			skipped++
			continue
		}
		if src == "local" {
			local++
		} else {
			api++
		}
		beg, prac, biz, err := paperExplainCall(p.title, p.authors, p.year, p.journal, p.note, abstract)
		if err != nil {
			fmt.Println("twoai_paper_explain:", p.uid, err)
			skipped++
			continue
		}
		// A model that copied the abstract instead of explaining it does not
		// get published. Twelve consecutive shared words is the line.
		if sharedRun(beg+" "+prac+" "+biz, abstract) >= 12 {
			fmt.Println("twoai_paper_explain:", p.uid, "output too close to abstract, skipped")
			skipped++
			continue
		}
		if _, err := db.Exec(`UPDATE twoai_research_papers
			SET explain_beginner=$1, explain_practitioner=$2, explain_business=$3,
				verified_on=current_date
			WHERE uid=$4`, beg, prac, biz, p.uid); err != nil {
			return err
		}
		written++
		time.Sleep(500 * time.Millisecond)
	}
	var pending int
	db.QueryRow(`SELECT count(*) FROM twoai_research_papers
		WHERE COALESCE(explain_beginner,'')=''`).Scan(&pending)
	fmt.Printf("twoai_paper_explain: written=%d grounded_local=%d grounded_api=%d skipped=%d pending=%d\n",
		written, local, api, skipped, pending)
	return nil
}

// paperAbstract finds a real abstract for the paper: the local OpenAlex
// mirror first, the OpenAlex API second, nothing third.
func paperAbstract(db *sql.DB, title, doi string, allowAPI bool) (string, string) {
	var a sql.NullString
	if doi != "" {
		if db.QueryRow(`SELECT abstract FROM twoai_works
			WHERE lower(doi)=lower($1) AND COALESCE(abstract,'')<>'' LIMIT 1`,
			strings.ToLower(doi)).Scan(&a) == nil && a.Valid {
			return strings.TrimSpace(a.String), "local"
		}
	}
	// Titles are matched with punctuation and spacing stripped, because the
	// mirror and the reading list disagree on colons, hyphens and case far
	// more often than they disagree on the paper. Indexed on the same
	// expression, so this stays a lookup rather than a scan of 618k rows.
	if db.QueryRow(`SELECT abstract FROM twoai_works
		WHERE lower(regexp_replace(title, '[^a-zA-Z0-9]', '', 'g')) = lower(regexp_replace($1, '[^a-zA-Z0-9]', '', 'g'))
		  AND COALESCE(abstract,'')<>''
		ORDER BY COALESCE(cited_by,0) DESC LIMIT 1`, title).Scan(&a) == nil && a.Valid {
		return strings.TrimSpace(a.String), "local"
	}
	if !allowAPI {
		return "", ""
	}
	// OpenAlex title search, abstract rebuilt from the inverted index they
	// publish. THE KEY MATTERS HERE TOO. OpenAlex went metered in February
	// 2026: an anonymous call draws on a $0.10 daily allowance and a free key
	// raises it to $1. The nightly works backfill spends the anonymous pot
	// long before this stage runs, so every call from here was refused and
	// paperAbstract returned nothing, which is why the log read
	// written=0 local=0 api=0 skipped=12 on run after run. The backfill was
	// given the key on 2026-08-30 and this call was missed.
	u := "https://api.openalex.org/works?per-page=1&mailto=contact@theworldofai.org&filter=title.search:" +
		url.QueryEscape(sanitizeSearch(title))
	if k := os.Getenv("OPENALEX_API_KEY"); k != "" {
		u += "&api_key=" + url.QueryEscape(k)
	}
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", ""
	}
	var out struct {
		Results []struct {
			Title                 string           `json:"title"`
			AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
		} `json:"results"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out) != nil || len(out.Results) == 0 {
		return "", ""
	}
	r := out.Results[0]
	// The hit must be the same paper, not merely the best hit for the words.
	if !strings.EqualFold(strings.TrimSpace(r.Title), strings.TrimSpace(title)) {
		return "", ""
	}
	if len(r.AbstractInvertedIndex) == 0 {
		return "", ""
	}
	max := 0
	for _, ps := range r.AbstractInvertedIndex {
		for _, i := range ps {
			if i > max {
				max = i
			}
		}
	}
	words := make([]string, max+1)
	for w, ps := range r.AbstractInvertedIndex {
		for _, i := range ps {
			words[i] = w
		}
	}
	return strings.TrimSpace(strings.Join(words, " ")), "api"
}

var searchStripRe = regexp.MustCompile(`[,:;|()]+`)

func sanitizeSearch(s string) string { return searchStripRe.ReplaceAllString(s, " ") }

// sharedRun returns the longest run of consecutive shared words between the
// generated text and the abstract, lowercased.
func sharedRun(gen, abstract string) int {
	g := strings.Fields(strings.ToLower(gen))
	a := strings.ToLower(abstract)
	best := 0
	for i := range g {
		for j := i + best + 1; j <= len(g); j++ {
			if !strings.Contains(a, strings.Join(g[i:j], " ")) {
				break
			}
			if j-i > best {
				best = j - i
			}
		}
	}
	return best
}

// paperExplainCall asks for the three audience explanations as strict JSON.
func paperExplainCall(title, authors string, year int, journal, note, abstract string) (string, string, string, error) {
	prompt := "You are writing for theworldofai.org, a reference site. Below are the verified details and the published abstract of a research paper. " +
		"Write three explanations of this paper, each 70 to 110 words, entirely in your own words, grounded ONLY in the information provided. " +
		"Do not copy phrases from the abstract. Do not add claims the abstract does not support. Plain English, commas rather than dashes.\n" +
		"1. beginner: for a curious reader with no technical background, explain what the paper found and why it matters.\n" +
		"2. practitioner: for an engineer or researcher, what was done, how, and what the key result was.\n" +
		"3. business: for an executive, what this means in practice and what decision it informs.\n" +
		"Output ONLY a JSON object: {\"beginner\":\"...\",\"practitioner\":\"...\",\"business\":\"...\"}\n\n" +
		fmt.Sprintf("Title: %s\nAuthors: %s\nYear: %d\nVenue: %s\nEditor's note: %s\n\nAbstract:\n%s", title, authors, year, journal, note, abstract)
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 800,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("x-api-key", os.Getenv("ANTHROPIC_API_KEY"))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", "", "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	raw := strings.TrimSpace(sb.String())
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	var ex struct {
		Beginner     string `json:"beginner"`
		Practitioner string `json:"practitioner"`
		Business     string `json:"business"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ex); err != nil {
		return "", "", "", fmt.Errorf("unparseable model output")
	}
	wc := func(s string) int { return len(strings.Fields(s)) }
	if wc(ex.Beginner) < 30 || wc(ex.Practitioner) < 30 || wc(ex.Business) < 30 {
		return "", "", "", fmt.Errorf("explanation too short")
	}
	return strings.TrimSpace(ex.Beginner), strings.TrimSpace(ex.Practitioner), strings.TrimSpace(ex.Business), nil
}
