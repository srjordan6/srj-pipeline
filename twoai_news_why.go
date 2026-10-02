package main

// twoai_news_why: one line of our own analysis on every news story, saying
// how it connects to a law, case, company or person the site tracks.
//
// theworldofai, bridge row 371 (Stephen, 2026-10-02): the blueprint called
// for it, and it is what separates the site from an aggregator. The model
// writes only from the records the site has already matched to the story:
// twoai_news_links (members of Congress and bills, matched deterministically
// by twoai_news_link), companies and people the clustering extracted that
// have a page here. It may not add a connection that is not in that data,
// and a story with no tracked match gets no line rather than a generic one.
// The line is stored on the story (story.why_it_matters: text, the records
// it cites with their links, the model, the date, and a hash of the matched
// records) so it is written once and rewritten only when the matches change.
// Newest live stories first, a capped number per run, retired stories
// skipped, Ollama only, a bulk stage so it waits for off-peak hours.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type whyMatch struct {
	Kind string `json:"kind"`
	UID  string `json:"uid"`
	Name string `json:"name"`
	Href string `json:"href"`
}

type whyRecord struct {
	Text        string     `json:"text"`
	Cites       []whyMatch `json:"cites"`
	Model       string     `json:"model"`
	WrittenOn   string     `json:"written_on"`
	MatchedHash string     `json:"matched_hash"`
	None        bool       `json:"none,omitempty"`
	Attempts    int        `json:"attempts,omitempty"`
}

func whyNorm(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func twoaiNewsWhy(db *sql.DB) {
	perRun := 10
	if v := strings.TrimSpace(os.Getenv("TWOAI_WHY_PER_RUN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			perRun = n
		}
	}
	if perRun == 0 {
		return
	}
	// Name indexes for the two kinds the clustering extracts by name.
	companies := map[string]whyMatch{}
	people := map[string]whyMatch{}
	if rows, err := db.Query(`SELECT uid, kind, name, coalesce(aliases,'[]'::jsonb)::text FROM twoai_entities WHERE kind IN ('company','person')`); err == nil {
		for rows.Next() {
			var uid, kind, name, aliasesRaw string
			if rows.Scan(&uid, &kind, &name, &aliasesRaw) != nil {
				continue
			}
			var aliases []string
			json.Unmarshal([]byte(aliasesRaw), &aliases)
			m := whyMatch{Kind: kind, UID: uid, Name: name}
			if kind == "company" {
				m.Href = "/companies/" + uid + "/"
				for _, n := range append([]string{name}, aliases...) {
					if k := whyNorm(n); len(k) >= 3 {
						companies[k] = m
					}
				}
			} else {
				m.Href = "/ai-ecosystem/ecosystem-entities-market-and-operations/" + uid + "/"
				if k := whyNorm(name); len(k) >= 5 {
					people[k] = m
				}
			}
		}
		rows.Close()
	}
	// A company page exists only for entities with a page row; a link to a
	// page that is not built is worse than no link.
	hasPage := map[string]bool{}
	if rows, err := db.Query(`SELECT replace(replace(path,'companies/',''),'.json','') FROM twoai_pages WHERE kind='company'
		UNION SELECT data->>'uid' FROM twoai_pages WHERE path LIKE 'people/%' AND data->>'uid' IS NOT NULL`); err == nil {
		for rows.Next() {
			var u string
			if rows.Scan(&u) == nil {
				hasPage[u] = true
			}
		}
		rows.Close()
	}

	rows, err := db.Query(`SELECT uid, headline, story::text, coalesce(story->'why_it_matters'->>'matched_hash',''), coalesce((story->'why_it_matters'->>'attempts')::int, 0)
		FROM twoai_news_stories WHERE retired_at IS NULL AND published_on > current_date - 60
		ORDER BY published_on DESC NULLS LAST, slug LIMIT 400`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_news_why select:", err)
		return
	}
	type cand struct {
		uid, headline, summary, oldHash string
		attempts                        int
		matches                         []whyMatch
	}
	var todo []cand
	for rows.Next() {
		var uid, head, raw, oldHash string
		var attempts int
		if rows.Scan(&uid, &head, &raw, &oldHash, &attempts) != nil {
			continue
		}
		var st map[string]any
		if json.Unmarshal([]byte(raw), &st) != nil {
			continue
		}
		c := cand{uid: uid, headline: head, oldHash: oldHash, attempts: attempts}
		c.summary, _ = st["Summary"].(string)
		seen := map[string]bool{}
		add := func(m whyMatch) {
			k := m.Kind + ":" + m.UID
			if !seen[k] {
				seen[k] = true
				c.matches = append(c.matches, m)
			}
		}
		for _, o := range newsStrings(st["Orgs"]) {
			if m, ok := companies[whyNorm(o)]; ok && hasPage[m.UID] {
				add(m)
			}
		}
		for _, p := range newsStrings(st["Persons"]) {
			if m, ok := people[whyNorm(p)]; ok && hasPage[m.UID] {
				add(m)
			}
		}
		lr, lerr := db.Query(`SELECT l.target_kind, l.target_uid,
				coalesce((SELECT name FROM twoai_pol_members m WHERE m.uid=l.target_uid), ''),
				coalesce((SELECT b.bill_number || ': ' || b.title FROM twoai_pol_bills b WHERE b.uid=l.target_uid), '')
			FROM twoai_news_links l WHERE l.story_uid=$1 AND l.retired_reason IS NULL`, uid)
		if lerr == nil {
			for lr.Next() {
				var kind, tuid, mname, bname string
				if lr.Scan(&kind, &tuid, &mname, &bname) != nil {
					continue
				}
				name := mname
				if kind == "bill" {
					name = bname
				}
				if name == "" {
					continue
				}
				add(whyMatch{Kind: kind, UID: tuid, Name: name, Href: polBase + tuid + "/"})
			}
			lr.Close()
		}
		todo = append(todo, c)
	}
	rows.Close()

	written, none, held, skipped := 0, 0, 0, 0
	for _, c := range todo {
		if written+none >= perRun {
			break
		}
		keys := []string{}
		for _, m := range c.matches {
			keys = append(keys, m.Kind+":"+m.UID)
		}
		sort.Strings(keys)
		h := sha256.Sum256([]byte(strings.Join(keys, "|")))
		hash := hex.EncodeToString(h[:8])
		if hash == c.oldHash {
			skipped++
			continue
		}
		if c.attempts >= 3 {
			skipped++
			continue
		}
		store := func(rec whyRecord) {
			rec.MatchedHash = hash
			rj, _ := json.Marshal(rec)
			db.Exec(`UPDATE twoai_news_stories SET story = story || jsonb_build_object('why_it_matters', $2::jsonb) WHERE uid=$1`, c.uid, string(rj))
		}
		if len(c.matches) == 0 {
			store(whyRecord{None: true, WrittenOn: time.Now().Format("2006-01-02")})
			none++
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Headline: %s\n\nSummary:\n%s\n\nRecords this site tracks that the story is matched to (use only these, cite by key):\n", c.headline, trunc(c.summary, 3000))
		for i, m := range c.matches {
			fmt.Fprintf(&sb, "k%d: %s, %s (page %s)\n", i+1, m.Name, strings.ReplaceAll(m.Kind, "_", " "), m.Href)
		}
		system := `You write one line of analysis for The World of AI, a reference site that tracks AI laws, bills, lawsuits, companies, people and vulnerabilities. Under the heading "Why it matters here" the site says how a news story connects to records it already tracks, so a reader knows what to follow next on the site.
Write one or two sentences, 25 to 70 words, naming the specific tracked records given to you and saying how the story bears on each: what the record is, what the story changes or adds, why a reader following that record should read on. Name each record exactly as given. Use only the records given and only facts in the summary. No opinion about the people in the story, no prediction, no legal advice, no advice of any kind. Commas rather than dashes, plain English, no markdown.
Return only JSON: {"text": "...", "cites": ["k1", "k2"]} where cites lists the keys of every record the text names.`
		out, model, gerr := twoaiGenerate("news_why", system, sb.String())
		if gerr != nil {
			if strings.Contains(gerr.Error(), "deferred") {
				return
			}
			continue
		}
		text := strings.TrimSpace(out)
		if i := strings.Index(text, "{"); i >= 0 {
			text = text[i:]
		}
		if k := strings.LastIndex(text, "}"); k >= 0 {
			text = text[:k+1]
		}
		var got struct {
			Text  string   `json:"text"`
			Cites []string `json:"cites"`
		}
		why := ""
		if json.Unmarshal([]byte(text), &got) == nil {
			why = strings.TrimSpace(twoaiStripMarkdown(got.Text))
		}
		cites := []whyMatch{}
		for _, k := range got.Cites {
			k = strings.TrimSpace(strings.ToLower(k))
			if n, err := strconv.Atoi(strings.TrimPrefix(k, "k")); err == nil && n >= 1 && n <= len(c.matches) {
				cites = append(cites, c.matches[n-1])
			}
		}
		low := strings.ToLower(why)
		problem := ""
		switch {
		case why == "":
			problem = "no text"
		case len([]rune(why)) < 40 || len([]rune(why)) > 520:
			problem = "length"
		case strings.Contains(why, "?"):
			problem = "question"
		case len(cites) == 0:
			problem = "cites none of the records"
		case strings.Contains(low, "we believe") || strings.Contains(low, "we think") || strings.Contains(low, "will likely") || strings.Contains(low, "should consider"):
			problem = "opinion or prediction"
		}
		if problem != "" {
			store(whyRecord{Attempts: c.attempts + 1, WrittenOn: time.Now().Format("2006-01-02")})
			held++
			fmt.Printf("twoai_news_why: %s held (%s)\n", c.uid, problem)
			continue
		}
		store(whyRecord{Text: why, Cites: cites, Model: model, WrittenOn: time.Now().Format("2006-01-02")})
		written++
		fmt.Printf("twoai_news_why: %s: %s\n", c.uid, trunc(why, 110))
	}
	fmt.Printf("twoai_news_why: candidates=%d written=%d no_match=%d held=%d unchanged=%d ok=true\n", len(todo), written, none, held, skipped)
}
