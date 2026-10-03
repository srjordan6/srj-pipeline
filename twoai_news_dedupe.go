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
	"strconv"
	"strings"
	"time"
)

var newsDupReasonRe = regexp.MustCompile(`duplicate of ([0-9a-f]{8})`)

// newsStoryUIDRepair gives every story the uid the site addresses it by.
//
// 2026-10-02: the one-outlet writers (accounting, TechGig, solo) minted an
// md5 of the article URL as the story uid, while the archive export and the
// site mint sha256 of story:<slug>. The archive quietly overrode the uid on
// the way out, so the pages existed, but every pin in twoai_page_news and
// every ledger row carried the md5 and linked to /ai-news/<md5>/, which no
// page answers: 158 stories, 112 pins on the accountant and company pages.
// This re-keys the row and everything that points at it, set-based, and is
// a no-op once done. Postgres computes the same hash the Go code does.
func newsStoryUIDRepair(db *sql.DB) {
	const sha = `substr(encode(sha256(convert_to('story:' || slug, 'UTF8')), 'hex'), 1, 8)`
	const mapping = `SELECT uid AS old, ` + sha + ` AS new FROM twoai_news_stories WHERE uid IS DISTINCT FROM ` + sha
	var n int
	db.QueryRow(`SELECT count(*) FROM (` + mapping + `) m`).Scan(&n)
	if n == 0 {
		return
	}
	for _, t := range []string{"twoai_page_news", "twoai_state_news_pins", "twoai_news_links", "twoai_solo_news", "twoai_techgig_news", "twoai_accounting_news", "twoai_news_dedupe_log"} {
		col := "story_uid"
		if t == "twoai_news_dedupe_log" {
			col = "loser_uid"
		}
		if _, err := db.Exec(`UPDATE ` + t + ` x SET ` + col + ` = m.new FROM (` + mapping + `) m WHERE x.` + col + ` = m.old`); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_news_archive uid repair %s: %v\n", t, err)
		}
	}
	db.Exec(`UPDATE twoai_news_dedupe_log x SET survivor_uid = m.new FROM (` + mapping + `) m WHERE x.survivor_uid = m.old`)
	// The story entity: the archive refresh already inserts one under the
	// sha uid, so an md5 twin is dropped where the sha one exists and
	// re-keyed where it does not.
	db.Exec(`DELETE FROM twoai_entities e USING (` + mapping + `) m WHERE e.kind='story' AND e.uid = m.old AND EXISTS (SELECT 1 FROM twoai_entities f WHERE f.uid = m.new)`)
	db.Exec(`UPDATE twoai_entities e SET uid = m.new FROM (` + mapping + `) m WHERE e.kind='story' AND e.uid = m.old`)
	if _, err := db.Exec(`UPDATE twoai_news_stories s SET uid = m.new, story = s.story || jsonb_build_object('uid', m.new) FROM (` + mapping + `) m WHERE s.uid = m.old`); err != nil {
		fmt.Fprintf(os.Stderr, "twoai_news_archive uid repair stories: %v\n", err)
		return
	}
	fmt.Printf("twoai_news_archive: re-keyed %d one-outlet stories to the site's uid, pins and ledgers moved with them\n", n)
}

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
		d := newsDedupeFrom(uid, slug, head, st)
		d.date, d.published = date, t
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

// newsDedupeFrom builds the comparison view of a story row: its article
// URLs, outlets, named actors and headline words. Shared by the merge pass
// and by the unmerge stage, which re-judges old merges from the same view.
func newsDedupeFrom(uid, slug, head string, st map[string]any) *dedupeStory {
	d := &dedupeStory{uid: uid, slug: slug, headline: head, story: st,
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
	return d
}

// newsOneNewsroom reports two stories that each come from one and the same
// outlet under different headlines. A company newsroom does not publish one
// announcement twice: the 21:05 run of 2026-10-02 folded Kyndryl's Dallas lab
// release into its Singapore lab release, and NTT DATA releases into other
// NTT DATA releases. Two stories from one outlet are the same event only when
// the headline is the same, which is a feed ingesting one release twice.
func newsOneNewsroom(a, b *dedupeStory) bool {
	if len(a.domains) != 1 || len(b.domains) != 1 {
		return false
	}
	for d := range a.domains {
		if !b.domains[d] {
			return false
		}
	}
	return whyNorm(a.headline) != whyNorm(b.headline)
}

// newsSameEvent names the rule two stories match on, or returns "".
func newsSameEvent(a, b *dedupeStory) string {
	if newsOneNewsroom(a, b) {
		return ""
	}
	sharedURL := 0
	for u := range a.urls {
		if b.urls[u] {
			sharedURL++
		}
	}
	smallArts := min(len(a.urls), len(b.urls))
	// MOST OF THE SMALLER SET, NOT TWO. The first pass (15:05 run,
	// 2026-10-02) merged 401 of 932 stories: two shared articles was enough,
	// and the daily clusters share articles freely, so an Anthropic doom story
	// folded into a Connecticut rogue-agents story on eight shared links out
	// of twenty-five. The shared links must now be at least half of the
	// smaller story's articles, which is what the request said.
	if sharedURL >= 1 && sharedURL*2 >= smallArts {
		return fmt.Sprintf("%d shared article URL(s) of %d", sharedURL, smallArts)
	}
	return newsWordRule(a, b)
}

// newsWordRule is the headline test on its own: the rule name when two
// headlines describe one event, or "".
func newsWordRule(a, b *dedupeStory) string {
	// A NAME IS NOT AN EVENT. The 18:05 run of 2026-10-02 merged four NTT
	// DATA announcements into four other NTT DATA announcements (Google Cloud
	// into a cloud-value study, Palo Alto Networks into IP networks, Databricks
	// into ENGIE, a Gartner quadrant into an ISG leader award) on five shared
	// words and one shared actor: the five words were ntt, data, ai and two
	// more. The actor's own name and the word ai say which company and which
	// subject, not which event, so they are taken out before the count is
	// trusted: at least three of the shared words must be something else.
	nameWords := map[string]bool{"ai": true}
	for x := range a.actors {
		for w := range newsDedupeWords(x) {
			nameWords[w] = true
		}
	}
	for x := range b.actors {
		for w := range newsDedupeWords(x) {
			nameWords[w] = true
		}
	}
	shared, core := 0, 0
	for w := range a.words {
		if b.words[w] {
			shared++
			if !nameWords[w] {
				core++
			}
		}
	}
	small := min(len(a.words), len(b.words))
	actors := 0
	for x := range a.actors {
		if b.actors[x] {
			actors++
		}
	}
	// Two thirds of at least five words, or five words outright. Three of
	// five let "What The Tech: A new warning about AI" absorb a parenting
	// column on the same pass. The two-thirds path stands on its own: a
	// short headline is mostly names when the names are the event ("AI
	// superintelligence ban proposed by Casar, Sanders"), and the first
	// cut of this rule reversed five true duplicates by asking it for
	// words beyond them. The outright path is where the NTT DATA merges
	// came from, and it alone asks for the words beyond the names.
	twoThirds := small >= 5 && shared*3 >= small*2
	outright := shared >= 5 && (core >= 3 || (core >= 2 && actors >= 2))
	if !(twoThirds || outright) {
		return ""
	}
	outlets := 0
	for d := range a.domains {
		if b.domains[d] {
			outlets++
		}
	}
	if actors >= 1 {
		return fmt.Sprintf("headlines share %d of %d words (%d beyond names) and %d actor(s)", shared, small, core, actors)
	}
	if outlets >= 2 {
		return fmt.Sprintf("headlines share %d of %d words (%d beyond names) and %d outlets", shared, small, core, outlets)
	}
	return ""
}

var newsRuleURLRe = regexp.MustCompile(`^(\d+) shared article URL\(s\)`)
var newsRuleWordsRe = regexp.MustCompile(`share (\d+) of (\d+) words`)

// newsUnmerge re-judges every merge in the log against the current rules
// and reverses the ones the rules no longer make. Written for the first
// pass of 2026-10-02, which merged on two shared links and three of five
// headline words; the stricter rules above keep 320 of its 401 merges. A
// reversed loser is live again with its own articles intact; the survivor
// keeps the extra coverage links it gained (the page shows two or three
// anyway) and loses the loser from merged_from; pins the merge moved go
// back; a record link the merge copied is dropped and the loser's own is
// restored. The log row is marked reversed, so this is safe to rerun.
func newsUnmerge(db *sql.DB) {
	db.Exec(`ALTER TABLE twoai_news_dedupe_log ADD COLUMN IF NOT EXISTS reversed_at timestamptz`)
	rows, err := db.Query(`SELECT d.loser_uid, d.survivor_uid, d.rule, l.story::text, sv.story::text, l.headline, sv.headline
		FROM twoai_news_dedupe_log d JOIN twoai_news_stories l ON l.uid=d.loser_uid JOIN twoai_news_stories sv ON sv.uid=d.survivor_uid
		WHERE d.reversed_at IS NULL`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_news_unmerge:", err)
		return
	}
	type pair struct{ loser, surv, rule, lraw, sraw, lhead, shead string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.loser, &p.surv, &p.rule, &p.lraw, &p.sraw, &p.lhead, &p.shead) == nil {
			pairs = append(pairs, p)
		}
	}
	rows.Close()
	kept, reversed := 0, 0
	forced := map[string]bool{}
	for _, u := range strings.Split(os.Getenv("TWOAI_UNMERGE_UIDS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			forced[u] = true
		}
	}
	for _, p := range pairs {
		var ls, ss map[string]any
		json.Unmarshal([]byte(p.lraw), &ls)
		json.Unmarshal([]byte(p.sraw), &ss)
		count := func(m map[string]any) int {
			if n, ok := m["ArticleCount"].(float64); ok {
				return int(n)
			}
			if a, ok := m["Articles"].([]any); ok {
				return len(a)
			}
			return 0
		}
		lc, sc := count(ls), count(ss)
		keep := false
		if m := newsRuleURLRe.FindStringSubmatch(p.rule); m != nil {
			n, _ := strconv.Atoi(m[1])
			// The survivor's count today includes what the loser brought, so
			// its size before the merge is taken back out.
			sOrig := sc - (lc - n)
			if sOrig < 1 {
				sOrig = 1
			}
			keep = n >= 1 && n*2 >= min(lc, sOrig)
		} else if newsRuleWordsRe.MatchString(p.rule) {
			// Judged again from the stories themselves, so a rule that
			// counts differently from the one that wrote the log line
			// (names taken out since 2026-10-02 evening) still applies.
			keep = newsWordRule(newsDedupeFrom(p.loser, "", p.lhead, ls), newsDedupeFrom(p.surv, "", p.shead, ss)) != ""
		}
		lv, sv := newsDedupeFrom(p.loser, "", p.lhead, ls), newsDedupeFrom(p.surv, "", p.shead, ss)
		if keep && newsOneNewsroom(lv, sv) {
			keep = false
		}
		// AN EDITOR'S REVERSAL. A merge the rules cannot see is wrong once the
		// survivor has absorbed the loser's links and outlets, so a named loser
		// is reversed on request: TWOAI_UNMERGE_UIDS=uid,uid (one run only).
		if forced[p.loser] {
			keep = false
		}
		if keep {
			kept++
			continue
		}
		// THE SURVIVOR GIVES BACK WHAT IT TOOK. The first version left the
		// loser's articles on the survivor, so the next merge pass found the
		// two sharing every link the loser had and merged them again: three
		// NTT DATA stories restored at 19:20 were merged back at 21:05. The
		// loser's article links now leave the survivor when the merge is
		// reversed; the loser keeps its own copy.
		if arts, ok := ss["Articles"].([]any); ok {
			keepArts := []any{}
			for _, a := range arts {
				if m, ok := a.(map[string]any); ok {
					if u, _ := m["URL"].(string); u != "" && lv.urls[u] {
						continue
					}
				}
				keepArts = append(keepArts, a)
			}
			if len(keepArts) > 0 {
				ss["Articles"] = keepArts
				if _, had := ss["ArticleCount"]; had {
					ss["ArticleCount"] = len(keepArts)
				}
			}
		}
		// Reverse. The loser comes back as it was; its row was only retired.
		db.Exec(`UPDATE twoai_news_stories SET retired_at=NULL, retired_reason=NULL, last_seen=now() WHERE uid=$1 AND retired_reason LIKE 'duplicate of ' || $2 || '%'`, p.loser, p.surv)
		mf := []string{}
		for _, u := range newsStrings(ss["merged_from"]) {
			if u != p.loser {
				mf = append(mf, u)
			}
		}
		ss["merged_from"] = mf
		if raw, err := json.Marshal(ss); err == nil {
			db.Exec(`UPDATE twoai_news_stories SET story=$2::jsonb WHERE uid=$1`, p.surv, string(raw))
		}
		db.Exec(`INSERT INTO twoai_state_news_pins (state_slug, story_uid, note, pinned_on)
			SELECT state_slug, $1, replace(note, ' (moved from duplicate ' || $1 || ')', ''), pinned_on FROM twoai_state_news_pins
			WHERE story_uid=$2 AND note LIKE '%(moved from duplicate ' || $1 || ')%' ON CONFLICT (state_slug, story_uid) DO NOTHING`, p.loser, p.surv)
		db.Exec(`DELETE FROM twoai_state_news_pins WHERE story_uid=$2 AND note LIKE '%(moved from duplicate ' || $1 || ')%'`, p.loser, p.surv)
		db.Exec(`UPDATE twoai_page_news SET active=true, retired_reason=NULL WHERE story_uid=$1 AND retired_reason = 'duplicate story merged into ' || $2`, p.loser, p.surv)
		db.Exec(`DELETE FROM twoai_news_links WHERE story_uid=$2 AND matched_on LIKE '%(via duplicate ' || $1 || ')'`, p.loser, p.surv)
		db.Exec(`UPDATE twoai_news_links SET retired_reason=NULL WHERE story_uid=$1 AND retired_reason = 'duplicate story merged into ' || $2`, p.loser, p.surv)
		db.Exec(`UPDATE twoai_news_dedupe_log SET reversed_at=now(), rule = rule || ' (reversed: below the stricter rule)' WHERE loser_uid=$1`, p.loser)
		reversed++
		fmt.Printf("twoai_news_unmerge: restored %s (%s), was merged into %s on %q <- %q\n", p.loser, p.rule, p.surv, p.shead, p.lhead)
	}
	fmt.Printf("twoai_news_unmerge: judged=%d kept=%d reversed=%d ok=true\n", len(pairs), kept, reversed)
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
	// A pair reversed by the unmerge stage and merged again under a later
	// rule gets its log row back as a live merge, so the log stays the record.
	db.Exec(`INSERT INTO twoai_news_dedupe_log (loser_uid, survivor_uid, rule) VALUES ($1,$2,$3)
		ON CONFLICT (loser_uid) DO UPDATE SET survivor_uid=EXCLUDED.survivor_uid, rule=EXCLUDED.rule, merged_at=now(), reversed_at=NULL`, loser.uid, surv.uid, rule)
	return true
}
