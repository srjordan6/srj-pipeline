package main

// twoai_news_mine: read the news intake we already pay for and propose the
// things it names that no tracker knows about yet.
//
// WHY IT EXISTS. On 2026-09-22 Stephen opened a story in our own news archive,
// British Columbia suing OpenAI over the Tumbler Ridge shooting, and asked
// whether we were tracking it. The story was there. The lawsuit was not: not
// in ai_lawsuits, not in ai_lawsuit_candidates, although the case had been on
// CourtListener since the day before. Nothing read the news for new cases.
// Two stages mine the intake today, twoai_dc_announcements for facilities and
// twoai_bill_events for bills already tracked, and both stop at their own
// subject.
//
// WHAT IT DOES. Reads articles from pipeline.documents that talk about a
// lawsuit, asks the model to say who sued whom, in which court, and writes a
// row to ai_lawsuit_candidates when the case is not already known. It does not
// publish. Every candidate goes to the same review queue the CourtListener
// discovery writes to, so a wrong reading is a row someone declines, never a
// page.
//
// WHAT IT REFUSES. An article that only discusses a case already in the
// tracker, one with no named defendant, one about a ruling in an old case
// rather than a filing, and one where the model returns anything but the JSON
// asked for. Each look is logged in twoai_news_mine so the same article is
// never read twice, with the reason.
//
// Lawsuits first because that is the gap Stephen found. The table and the
// shape of this stage are written to take more kinds later, which is what
// kind means on the log row.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// twoaiNewsMineCap bounds a run. The model call is the cost, and the backlog
// drains over a few days rather than in one long stage.
const twoaiNewsMineCap = 25

const twoaiNewsMineSystem = `You read a news article and report whether it says a NEW lawsuit has been FILED involving an artificial intelligence company, product or system.

Answer with one JSON object and nothing else:
{"filed": true|false, "case_name": "", "plaintiffs": "", "defendants": "", "court": "", "docket": "", "filed_on": "YYYY-MM-DD", "what": ""}

Rules:
- filed is true ONLY when the article reports a complaint, petition or suit being filed or announced. A ruling, a settlement, an appeal decision, a subpoena, a criminal charge or commentary on an existing case is false.
- The matter must involve AI: an AI company, an AI product, AI-generated material, AI training data, or an AI system's behaviour. A suit about an ordinary business dispute with no AI element is false.
- Copy names as the article gives them. Do not expand, correct or infer them.
- Leave a field empty when the article does not say it. Never guess a court, a docket number or a date.
- what: one sentence, at most 40 words, on what the case is about, in your own words.
- No markdown, no code fence, no text outside the JSON object.`

type twoaiMinedCase struct {
	Filed      bool   `json:"filed"`
	CaseName   string `json:"case_name"`
	Plaintiffs string `json:"plaintiffs"`
	Defendants string `json:"defendants"`
	Court      string `json:"court"`
	Docket     string `json:"docket"`
	FiledOn    string `json:"filed_on"`
	What       string `json:"what"`
}

// twoaiNewsMineJSON pulls the object out of a model reply that may still carry
// a fence or a sentence around it.
func twoaiNewsMineJSON(raw string) (*twoaiMinedCase, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var m twoaiMinedCase
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// twoaiNewsMineKnown asks whether the tracker or the queue already holds this
// case. Matching is on the most distinctive word of the defendant plus the
// most distinctive word of the plaintiff, searched across the case name AND
// the snippet, because the press and the docket name the same case
// differently. The first run, on 2026-09-22, proposed the British Columbia
// suit twice while the queue already held it as "His Majesty the King in
// Right of the Province of British Columbia v. Altman"; no pair of those
// three shares words in the case name alone, and all three share them once
// the snippet is searched too.
func twoaiNewsMineKnown(db *sql.DB, m *twoaiMinedCase) (bool, string) {
	// pickWord returns the longest word that is not corporate or legal
	// furniture, which is the one worth searching on.
	pickWord := func(s string, skip map[string]bool) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.IndexAny(s, ",;"); i > 0 {
			s = strings.TrimSpace(s[:i])
		}
		out := ""
		for _, w := range strings.Fields(s) {
			w = strings.Trim(w, ".,()'\"")
			if skip[w] {
				continue
			}
			if len(w) > len(out) {
				out = w
			}
		}
		return out
	}
	corporate := map[string]bool{"inc": true, "llc": true, "corp": true, "corporation": true, "co": true,
		"ltd": true, "plc": true, "pbc": true, "the": true, "and": true, "group": true, "holdings": true,
		"sam": true, "other": true, "entities": true}
	public := map[string]bool{"the": true, "of": true, "in": true, "and": true, "government": true,
		"province": true, "state": true, "district": true, "his": true, "her": true, "majesty": true,
		"king": true, "right": true, "people": true, "united": true, "states": true}

	defWord := pickWord(m.Defendants, corporate)
	if len(defWord) < 3 {
		return false, ""
	}
	if m.Docket != "" {
		var slug string
		if db.QueryRow(`SELECT slug FROM ai_lawsuits WHERE replace(docket,' ','') = replace($1,' ','') LIMIT 1`, m.Docket).Scan(&slug) == nil && slug != "" {
			return true, "tracked:" + slug
		}
	}
	plainWord := pickWord(m.Plaintiffs, public)
	if len(plainWord) < 4 {
		return false, ""
	}
	var slug string
	if db.QueryRow(`SELECT slug FROM ai_lawsuits
		WHERE lower(coalesce(case_name,'') || ' ' || coalesce(defendants,'') || ' ' || coalesce(plaintiffs,'')) LIKE '%' || $1 || '%'
		  AND lower(coalesce(case_name,'') || ' ' || coalesce(defendants,'') || ' ' || coalesce(plaintiffs,'')) LIKE '%' || $2 || '%'
		LIMIT 1`, defWord, plainWord).Scan(&slug) == nil && slug != "" {
		return true, "tracked:" + slug
	}
	var cid int64
	if db.QueryRow(`SELECT id FROM ai_lawsuit_candidates
		WHERE lower(coalesce(case_name,'') || ' ' || coalesce(snippet,'')) LIKE '%' || $1 || '%'
		  AND lower(coalesce(case_name,'') || ' ' || coalesce(snippet,'')) LIKE '%' || $2 || '%'
		LIMIT 1`, defWord, plainWord).Scan(&cid) == nil && cid > 0 {
		return true, fmt.Sprintf("queued:%d", cid)
	}
	return false, ""
}

func twoaiNewsMine(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_mine (
		document_id bigint NOT NULL,
		kind text NOT NULL,
		url text,
		title text,
		outcome text NOT NULL,
		detail text,
		mined_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (document_id, kind))`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT d.id, d.url, d.title, COALESCE(d.fulltext,''), COALESCE(d.published_at::date::text,'')
		FROM pipeline.documents d
		WHERE d.fetched_at > now() - interval '21 days'
		  AND COALESCE(d.fulltext,'') <> ''
		  AND (d.title ~* '\m(sues?|sued|lawsuit|suit|complaint|class action|litigation)\M'
		       OR d.fulltext ~* '\m(filed a (lawsuit|complaint|suit)|has sued|filed suit|class[- ]action complaint)\M')
		  AND (d.title ~* '\m(AI|artificial intelligence|chatbot|OpenAI|Anthropic|Google|Meta|xAI|Grok|ChatGPT|Copilot|Midjourney|Stability|Perplexity|deepfake)\M'
		       OR d.fulltext ~* '\martificial intelligence\M')
		  AND NOT EXISTS (SELECT 1 FROM twoai_news_mine m WHERE m.document_id = d.id AND m.kind = 'lawsuit')
		ORDER BY d.published_at DESC NULLS LAST LIMIT $1`, twoaiNewsMineCap)
	if err != nil {
		return err
	}
	type doc struct {
		id                        int64
		url, title, text, pubDate string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if rows.Scan(&d.id, &d.url, &d.title, &d.text, &d.pubDate) == nil {
			docs = append(docs, d)
		}
	}
	rows.Close()

	logLook := func(d doc, outcome, detail string) {
		db.Exec(`INSERT INTO twoai_news_mine (document_id, kind, url, title, outcome, detail)
			VALUES ($1,'lawsuit',$2,$3,$4,$5) ON CONFLICT (document_id, kind) DO NOTHING`,
			d.id, d.url, d.title, outcome, detail)
	}

	proposed, known, notFiling, failed := 0, 0, 0, 0
	for _, d := range docs {
		body := d.text
		if len(body) > 6000 {
			body = body[:6000]
		}
		out, _, gerr := twoaiGenerate("news_mine", twoaiNewsMineSystem,
			"Headline: "+d.title+"\nPublished: "+d.pubDate+"\nArticle:\n"+body+"\n\nAnswer now.")
		if gerr != nil {
			failed++
			fmt.Printf("twoai_news_mine: %s: %v\n", d.url, gerr)
			continue // no log row: the article was never read, so read it next run
		}
		m, perr := twoaiNewsMineJSON(out)
		if perr != nil {
			failed++
			logLook(d, "unreadable", perr.Error())
			continue
		}
		if !m.Filed || strings.TrimSpace(m.Defendants) == "" {
			notFiling++
			logLook(d, "not-a-filing", "")
			continue
		}
		if ok, what := twoaiNewsMineKnown(db, m); ok {
			known++
			logLook(d, "already-known", what)
			continue
		}
		name := strings.TrimSpace(m.CaseName)
		if name == "" {
			name = strings.TrimSpace(m.Plaintiffs) + " v. " + strings.TrimSpace(m.Defendants)
		}
		snippet := strings.TrimSpace(m.What)
		if snippet == "" {
			snippet = d.title
		}
		snippet += fmt.Sprintf(" Proposed by twoai_news_mine from %s, %s. Verify the docket before promoting; the article is the only source.",
			publisherFromURL(d.url), d.pubDate)
		var filedOn any
		if len(m.FiledOn) == 10 {
			filedOn = m.FiledOn
		} else if d.pubDate != "" {
			filedOn = d.pubDate
		}
		if _, ierr := db.Exec(`INSERT INTO ai_lawsuit_candidates
			(source, source_id, case_name, court, docket, filed_date, url, snippet, score, status)
			VALUES ('news', $1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6, $7, 1, 'new')
			ON CONFLICT DO NOTHING`,
			fmt.Sprintf("news:%d", d.id), name, strings.TrimSpace(m.Court), strings.TrimSpace(m.Docket),
			filedOn, d.url, snippet); ierr != nil {
			failed++
			logLook(d, "insert-failed", ierr.Error())
			continue
		}
		proposed++
		logLook(d, "proposed", name)
		fmt.Printf("twoai_news_mine: proposed %s | %s | %s\n", name, strings.TrimSpace(m.Court), publisherFromURL(d.url))
	}
	fmt.Printf("twoai_news_mine: read=%d proposed=%d already_known=%d not_a_filing=%d failed=%d ok=true\n",
		len(docs), proposed, known, notFiling, failed)
	return nil
}
