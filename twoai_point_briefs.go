package main

// twoai_point_briefs: a real paragraph on every cited source, kept current.
//
// Stephen, 2026-09-11: an industry point read "Stanford HAI measures the
// macro trends", one sentence, and a link out. "We could have paraphrased the
// report before sending them to the source." It is 151 of 159 points across
// the 23 industry pages - a headline, a line of orientation, and the reader
// doing all the work.
//
// NOTHING NEW IS FETCHED. twoai_industry_hub already harvests every cited
// source URL daily into twoai_source_harvest: the readable text of the page,
// keyed by url, with a content_hash and a fetched_on date. That harvest feeds
// the SECTOR analysis at the bottom of each page and has never fed the points
// it was gathered from. This stage reads the same extracts and writes one
// short brief per source.
//
// HOW IT STAYS CURRENT, which was the other half of the ask. Every brief is
// stored against the content_hash of the extract it was written from. The
// harvest refreshes daily; when a source page's substance changes, its hash
// changes, and this stage rewrites that brief on the next run and no other.
// A source that has not changed is never re-summarised, so cost and churn
// both stay near zero while the pages track their sources automatically.
//
// WHAT THE PROMPT FORBIDS, and why each rule is here:
//   - Only what the extract says. No background knowledge, no filling a gap
//     from what the model remembers about the organisation. This site has
//     quarantined fabricated people before; a plausible paragraph about a
//     report nobody checked is the same failure in a quieter register.
//   - Paraphrase, never reproduce. One quoted phrase at most, under fifteen
//     words. The brief is a reason to open the source, not a replacement.
//   - Name who reported a figure, inside the sentence, so no number floats
//     free of its publisher.
//   - Say less when the extract says less. Two honest sentences beat a
//     padded paragraph, and the model is told to return NOTHING rather than
//     invent.
//
// The site renders the brief under the point, above the outbound link, with
// the date it was written. A point with no brief renders exactly as it does
// today, so nothing regresses while the backlog fills.

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const pointBriefSystem = `You write one short brief about a source page for a sourced AI reference site. A reader sees it under a one-line summary and decides whether to open the source.

ABSOLUTE RULES:
- Use ONLY what the supplied page text says. Never add background knowledge. Never infer a finding the text does not state. Never name an organisation, figure, product or date that is not in the text.
- Paraphrase in your own words. Do NOT copy the source's sentences. At most ONE quoted phrase, under fifteen words, in quotation marks.
- Attribute every figure or finding to whoever reported it, inside the sentence.
- One paragraph, three to five sentences. Plain English. No marketing language, no "in today's rapidly evolving landscape", no closing exhortation, no instruction to visit the source.
- Write about what the source SAYS, not about what the page is.

If the text has no substantive content to summarise - a navigation page, a paywall notice, a cookie banner, an error - output exactly: NOTHING`

var pbSentRe = regexp.MustCompile(`[.!?]`)

func twoaiPointBriefs(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_point_briefs (
		url text PRIMARY KEY,
		content_hash text NOT NULL,
		brief text,
		model text,
		words int,
		generated_on date,
		attempts int NOT NULL DEFAULT 0,
		last_note text,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Println("twoai_point_briefs: ANTHROPIC_API_KEY unset, skipping")
		return nil
	}
	model := os.Getenv("TWOAI_BRIEF_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	limit := 40
	if v := strings.TrimSpace(os.Getenv("TWOAI_BRIEF_LIMIT")); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}

	// Sources whose harvested text has changed since the brief was written,
	// or which have no brief at all. Newest harvest first so a source that
	// just changed is refreshed before the backlog.
	rows, err := db.Query(`
		SELECT h.url, h.source_name, h.extract, h.content_hash
		FROM twoai_source_harvest h
		LEFT JOIN twoai_point_briefs b ON b.url = h.url
		WHERE h.extract <> '' AND h.http_status = 200
		  AND (b.url IS NULL OR b.content_hash <> h.content_hash)
		  AND COALESCE(b.attempts, 0) < 3
		ORDER BY h.fetched_on DESC, h.url
		LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type job struct{ url, name, extract, hash string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.url, &j.name, &j.extract, &j.hash) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	if len(jobs) == 0 {
		var have, total int
		db.QueryRow(`SELECT count(*) FILTER (WHERE brief IS NOT NULL AND brief <> ''), count(*) FROM twoai_point_briefs`).Scan(&have, &total)
		fmt.Printf("twoai_point_briefs: nothing changed, %d briefs current\n", have)
		return nil
	}

	written, nothing, failed := 0, 0, 0
	for _, j := range jobs {
		user := "Source: " + j.url + "\nPublisher: " + j.name + "\n\nText harvested from the page:\n\n" + j.extract
		out, err := twoaiClaudeCall(model, pointBriefSystem, user)
		if err != nil {
			db.Exec(`INSERT INTO twoai_point_briefs (url, content_hash, attempts, last_note)
				VALUES ($1,$2,1,$3)
				ON CONFLICT (url) DO UPDATE SET attempts=twoai_point_briefs.attempts+1, last_note=$3, updated_at=now()`,
				j.url, j.hash, err.Error())
			failed++
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		brief := strings.TrimSpace(out)
		// The model is told to return NOTHING for a nav page or a paywall.
		// Storing the hash on that outcome matters: it stops the same dead
		// page being re-summarised every night for the rest of time.
		if brief == "" || strings.HasPrefix(brief, "NOTHING") || len([]rune(brief)) < 120 {
			db.Exec(`INSERT INTO twoai_point_briefs (url, content_hash, attempts, last_note)
				VALUES ($1,$2,1,'no substantive content in the harvested text')
				ON CONFLICT (url) DO UPDATE SET content_hash=$2, attempts=twoai_point_briefs.attempts+1,
					last_note='no substantive content in the harvested text', updated_at=now()`, j.url, j.hash)
			nothing++
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		// House rule, enforced here rather than trusted to the prompt: no
		// paragraph over five sentences anywhere on this site.
		if n := len(pbSentRe.FindAllString(brief, -1)); n > 6 {
			db.Exec(`INSERT INTO twoai_point_briefs (url, content_hash, attempts, last_note)
				VALUES ($1,$2,1,'draft ran long, will retry')
				ON CONFLICT (url) DO UPDATE SET attempts=twoai_point_briefs.attempts+1,
					last_note='draft ran long, will retry', updated_at=now()`, j.url, j.hash)
			failed++
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		if _, err := db.Exec(`INSERT INTO twoai_point_briefs
			(url, content_hash, brief, model, words, generated_on, attempts, last_note)
			VALUES ($1,$2,$3,$4,$5,current_date,0,NULL)
			ON CONFLICT (url) DO UPDATE SET content_hash=EXCLUDED.content_hash, brief=EXCLUDED.brief,
				model=EXCLUDED.model, words=EXCLUDED.words, generated_on=EXCLUDED.generated_on,
				attempts=0, last_note=NULL, updated_at=now()`,
			j.url, j.hash, brief, model, len(strings.Fields(brief))); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_point_briefs store:", err)
			failed++
			continue
		}
		written++
		time.Sleep(1500 * time.Millisecond)
	}

	var have, total, stale int
	db.QueryRow(`SELECT count(*) FILTER (WHERE brief IS NOT NULL AND brief <> ''), count(*) FROM twoai_point_briefs`).Scan(&have, &total)
	db.QueryRow(`SELECT count(*) FROM twoai_source_harvest h LEFT JOIN twoai_point_briefs b ON b.url=h.url
		WHERE h.extract <> '' AND h.http_status=200 AND (b.url IS NULL OR b.content_hash <> h.content_hash)`).Scan(&stale)
	fmt.Printf("twoai_point_briefs: written=%d nothing_to_say=%d failed=%d | %d briefs held, %d sources still to write\n",
		written, nothing, failed, have, stale)
	return nil
}
