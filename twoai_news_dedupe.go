package main

// twoai_news_dedupe: one event is one story.
//
// theworldofai, bridge row 368 (Stephen, 2026-10-02): "California Governor
// Signs Laws to Protect Workers from AI Risks" (3 outlets, 2026-10-01) and
// "Gov. Gavin Newsom Signs Laws To Protect Workers From AI Risks" (5 outlets,
// 2026-10-02) were one event split in two because the headlines differed.
// The first was retired by hand as a duplicate of the second. This stage does
// that going forward, and once over the recent archive.
//
// Two live stories within four days are the same event when they share
// article URLs, or when their headlines share most of their words and they
// also share an actor (a person or organisation the clustering extracted)
// or two outlets. The headline test is the one the state law pages have
// used since 2026-09 to group reports of one event; the actor or outlet
// condition is added here because a retirement is a stronger act than a
// grouping. The survivor is the story with more outlets, then the older one.
// The loser is retired with a reason that names the survivor, its articles
// join the survivor's (capped as the archive caps them), and every pin,
// page placement and record link that pointed at the loser points at the
// survivor. Nothing is deleted. The permalink keeps serving (the site reads
// duplicate_of from the archive and sends the reader to the survivor).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

var newsDupReasonRe = regexp.MustCompile(`duplicate of ([0-9a-f]{8})`)

// newsDuplicateMap says, for every retired duplicate, which story survived.
func newsDuplicateMap(db *sql.DB) map[string]string {
	out := map[string]string{}
	rows, err := db.Query(`SELECT uid, retired_reason FROM twoai_news_stories WHERE retired_at IS NOT NULL AND retired_reason LIKE '%duplicate of %'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var uid, why string
		if rows.Scan(&uid, &why) == nil {
			if m := newsDupReasonRe.FindStringSubmatch(why); m != nil && m[1] != uid {
				out[uid] = m[1]
			}
		}
	}
	// Follow chains: a duplicate of a duplicate points at the final survivor.
	for k, v := range out {
		for i := 0; i < 5; i++ {
			if next, ok := out[v]; ok && next != k {
				v = next
			} else {
				break
			}
		}
		out[k] = v
	}
	return out
}

var newsDedupeStop = map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "on": true, "for": true,
	"and": true, "amid": true, "with": true, "as": true, "by": true, "at": true, "is": true, "new": true, "gov": true, "its": true,
	"from": true, "over": true, "after": true, "into": true, "says": true, "say": true, "said": true, "up": true, "out": true, "vs": true}

func newsDedupeWords(h string) map[string]bool {
	if i := strings.Index(h, " - "); i > 0 {
		h = h[:i]
	}
	h = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(h, "Artificial Intelligence", "AI"), "artificial intelligence", "AI"))
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(h, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
		if !newsDedupeStop[w] && len(w) > 1 {
			out[w] = true
		}
	}
	return out
}

type dedupeStory struct {
	uid, slug, headline, date string
	published                 time.Time
	story                     map[string]any
	urls, domains, actors     map[string]bool
	words                     map[string]bool
	domainCount               int
}

func newsStrings(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// twoaiNewsDedupe merges same-event stories and returns how many it merged.
func twoaiNewsDedupe(db *sql.DB) int {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_dedupe_log (
		loser_uid text PRIMARY KEY, survivor_uid text NOT NULL, rule text NOT NULL, merged_at timestamptz NOT NULL DEFAULT now())`)
	// The first run is the one-off pass over the recent archive; after that
	// a week is enough, since a duplicate is born within days of its twin.
	var done int
	db.QueryRow(`SELECT count(*) FROM twoai_news_dedupe_log`).Scan(&done)
	window := 7
	if done == 0 {
		window = 30
	}
	rows, err := db.Query(`SELECT uid, slug, headline, published_on::text, story::text FROM twoai_news_stories
		WHERE retired_at IS NULL AND published_on > current_date - $1::int ORDER BY published_on, slug`, window)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_news_dedupe select:", err)
		return 0
	}
	var all []*dedupeStory
	for rows.Next() {
		var uid, slug, head, date, raw string
		if rows.Scan(&uid, &slug, &head, &date, &raw) != nil {
			continue
		}
		var st map[string]any
		if json.Unmarshal([]byte(raw), &st) != nil {
			continue
		}
		if len(date) > 10 {
			date = date[:10]
		}
		t, perr := time.Parse("2006-01-02", date)
		if perr != nil {
			continue
		}
		d := &dedupeStory{uid: uid, slug: slug, headline: head, date: date, published: t, story: st,
			urls: map[string]bool{}, domains: map[string]bool{}, actors: map[string]bool{}, words: newsDedupeWords(head)}
		if arts, ok := st["Articles"].([]any); ok {
			for _, a := range arts {
				if m, ok := a.(map[string]any); ok {
					if u, _ := m["URL"].(string); u != "" {
						d.urls[u] = true
					}
					if dom, _ := m["Domain"].(string); dom != "" {
						d.domains[dom] = true
					}
				}
			}
		}
		for _, dom := range newsStrings(st["Domains"]) {
			d.domains[dom] = true
		}
		for _, a := range append(newsStrings(st["Persons"]), newsStrings(st["Orgs"])...) {
			d.actors[strings.ToLower(strings.TrimSpace(a))] = true
		}
		if n, ok := st["DomainCount"].(float64); ok {
			d.domainCount = int(n)
		} else {
			d.domainCount = len(d.domains)
		}
		all = append(all, d)
	}
	rows.Close()

	retired := map[string]string{} // loser -> survivor, this run
	merged := 0
	for i := 0; i < len(all); i++ {
		a := all[i]
		if retired[a.uid] != "" {
			continue
		}
		for j := i + 1; j < len(all); j++ {
			b := all[j]
			if retired[b.uid] != "" || retired[a.uid] != "" {
				continue
			}
			if b.published.Sub(a.published) > 4*24*time.Hour {
				break // sorted by date, nothing later is close enough
			}
			rule := newsSameEvent(a, b)
			if rule == "" {
				continue
			}
			surv, loser := a, b
			if b.domainCount > a.domainCount {
				surv, loser = b, a
			}
			if newsMerge(db, surv, loser, rule) {
				retired[loser.uid] = surv.uid
				merged++
				fmt.Printf("twoai_news_dedupe: merged %s into %s (%s): %q <- %q\n", loser.uid, surv.uid, rule, surv.headline, loser.headline)
			}
			if retired[a.uid] != "" {
				break
			}
		}
	}
	fmt.Printf("twoai_news_dedupe: window=%dd stories=%d merged=%d ok=true\n", window, len(all), merged)
	return merged
}

// newsSameEvent names the rule two stories match on, or returns "".
func newsSameEvent(a, b *dedupeStory) string {
	sharedURL := 0
	for u := range a.urls {
		if b.urls[u] {
			sharedURL++
		}
	}
	smallArts := min(len(a.urls), len(b.urls))
	if sharedURL >= 2 || (sharedURL >= 1 && smallArts <= 2) {
		return fmt.Sprintf("%d shared article URL(s)", sharedURL)
	}
	shared := 0
	for w := range a.words {
		if b.words[w] {
			shared++
		}
	}
	small := min(len(a.words), len(b.words))
	if small < 4 || shared*2 < small {
		return ""
	}
	actors := 0
	for x := range a.actors {
		if b.actors[x] {
			actors++
		}
	}
	outlets := 0
	for d := range a.domains {
		if b.domains[d] {
			outlets++
		}
	}
	if actors >= 1 {
		return fmt.Sprintf("headlines share %d of %d words and %d actor(s)", shared, small, actors)
	}
	if outlets >= 2 {
		return fmt.Sprintf("headlines share %d of %d words and %d outlets", shared, small, outlets)
	}
	return ""
}

// newsMerge folds loser into surv: articles and outlets union, the loser
// retired naming the survivor, and every placement moved across.
func newsMerge(db *sql.DB, surv, loser *dedupeStory, rule string) bool {
	arts := []any{}
	if cur, ok := surv.story["Articles"].([]any); ok {
		arts = append(arts, cur...)
	}
	if more, ok := loser.story["Articles"].([]any); ok {
		for _, a := range more {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			u, _ := m["URL"].(string)
			if u == "" || surv.urls[u] || len(arts) >= twoaiNewsArchiveMaxArticles {
				continue
			}
			arts = append(arts, m)
			surv.urls[u] = true
		}
	}
	doms := map[string]bool{}
	for d := range surv.domains {
		doms[d] = true
	}
	for d := range loser.domains {
		doms[d] = true
	}
	dl := []string{}
	for d := range doms {
		dl = append(dl, d)
	}
	sort.Strings(dl)
	if len(dl) > 12 {
		dl = dl[:12]
	}
	surv.story["Articles"] = arts
	surv.story["Domains"] = dl
	surv.story["DomainCount"] = len(doms)
	surv.story["ArticleCount"] = len(arts)
	surv.story["merged_from"] = append(newsStrings(surv.story["merged_from"]), loser.uid)
	raw, err := json.Marshal(surv.story)
	if err != nil {
		return false
	}
	if _, err := db.Exec(`UPDATE twoai_news_stories SET story=$2::jsonb, last_seen=now() WHERE uid=$1 AND retired_at IS NULL`, surv.uid, string(raw)); err != nil {
		fmt.Fprintln(os.Stderr, "twoai_news_dedupe survivor:", err)
		return false
	}
	reason := fmt.Sprintf("duplicate of %s (%s), merged automatically on %s: %s", surv.uid, surv.headline, time.Now().Format("2006-01-02"), rule)
	res, err := db.Exec(`UPDATE twoai_news_stories SET retired_at=now(), retired_reason=$2 WHERE uid=$1 AND retired_at IS NULL`, loser.uid, reason)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_news_dedupe retire:", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	surv.domainCount = len(doms)
	// Placements follow the survivor. Each insert tolerates the survivor
	// already being there, then the loser's row goes.
	db.Exec(`INSERT INTO twoai_state_news_pins (state_slug, story_uid, note, pinned_on)
		SELECT state_slug, $2, coalesce(note,'') || ' (moved from duplicate ' || $1 || ')', pinned_on FROM twoai_state_news_pins WHERE story_uid=$1
		ON CONFLICT (state_slug, story_uid) DO NOTHING`, loser.uid, surv.uid)
	db.Exec(`DELETE FROM twoai_state_news_pins WHERE story_uid=$1`, loser.uid)
	db.Exec(`UPDATE twoai_page_news p SET story_uid=$2, story_slug=$3, headline=$4
		WHERE p.story_uid=$1 AND NOT EXISTS (SELECT 1 FROM twoai_page_news q WHERE q.page_uid=p.page_uid AND q.story_uid=$2)`,
		loser.uid, surv.uid, surv.slug, surv.headline)
	db.Exec(`UPDATE twoai_page_news SET active=false, retired_reason='duplicate story merged into ' || $2 WHERE story_uid=$1`, loser.uid, surv.uid)
	db.Exec(`INSERT INTO twoai_news_links (story_uid, target_kind, target_uid, matched_on, headline, published_on)
		SELECT $2, target_kind, target_uid, matched_on || ' (via duplicate ' || $1 || ')', $3, published_on FROM twoai_news_links WHERE story_uid=$1
		ON CONFLICT (story_uid, target_kind, target_uid) DO NOTHING`, loser.uid, surv.uid, surv.headline)
	db.Exec(`UPDATE twoai_news_links SET retired_reason='duplicate story merged into ' || $2 WHERE story_uid=$1 AND retired_reason IS NULL`, loser.uid, surv.uid)
	db.Exec(`INSERT INTO twoai_news_dedupe_log (loser_uid, survivor_uid, rule) VALUES ($1,$2,$3) ON CONFLICT (loser_uid) DO NOTHING`, loser.uid, surv.uid, rule)
	return true
}
