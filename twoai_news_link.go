package main

// twoai_news_link: join the news we already hold to the records we already
// hold.
//
// WHY. Stephen opened a story about two Menendez bills clearing committee and
// asked whether he would find it on the Politics of AI hub. The member was
// there, his AI record page was there, the bill was there, and the story was
// in the news archive, and not one of them linked to another. News comes in
// one door and structured records come in another. twoai_news_mine proposes
// new lawsuits from the news; this does the simpler and more common job of
// saying that a story we published is about a record we already track.
//
// HOW IT MATCHES. Deterministically, never by model. A member matches when the
// story's own extracted person list names them, or when the text carries a
// chamber honorific in front of their surname, or their first and last name
// together. A bill matches on its exact title, when that title is long enough
// to be unambiguous, or on its bill number written the way reporters write it.
// Every match records what matched it, so a wrong one can be found and undone
// rather than argued about.
//
// WHAT IT DOES NOT DO. It does not write prose, rank stories, or assert that a
// story is about a person rather than merely naming them. The page says the
// story mentions the record, which is all the match supports.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/lib/pq"
)

// twoaiNewsLinkWindow bounds the work: stories older than this are already
// matched, since the stage runs daily.
const twoaiNewsLinkWindow = 120

func twoaiNewsLink(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_links (
		story_uid text NOT NULL,
		target_kind text NOT NULL,
		target_uid text NOT NULL,
		matched_on text NOT NULL,
		headline text,
		published_on date,
		created_at timestamptz NOT NULL DEFAULT now(),
		retired_reason text,
		PRIMARY KEY (story_uid, target_kind, target_uid))`); err != nil {
		return err
	}

	type story struct {
		uid, headline, text, published string
		persons                        []string
	}
	var stories []story
	rows, err := db.Query(`SELECT uid, headline,
			COALESCE(story->>'Summary','') || ' ' || COALESCE(headline,''),
			COALESCE(published_on::text,''),
			COALESCE((SELECT array_agg(lower(p #>> '{}')) FROM jsonb_array_elements(story->'Persons') p), '{}')
		FROM twoai_news_stories
		WHERE published_on > current_date - $1::int ORDER BY published_on DESC`, twoaiNewsLinkWindow)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s story
		if rows.Scan(&s.uid, &s.headline, &s.text, &s.published, pq.Array(&s.persons)) == nil {
			stories = append(stories, s)
		}
	}
	rows.Close()
	if len(stories) == 0 {
		fmt.Println("twoai_news_link: no stories in the window")
		return nil
	}

	type member struct{ uid, name, first, last string }
	var members []member
	mrows, err := db.Query(`SELECT uid, name FROM twoai_pol_members
		WHERE retired_reason IS NULL AND name <> '' AND uid <> ''`)
	if err != nil {
		return err
	}
	for mrows.Next() {
		var m member
		if mrows.Scan(&m.uid, &m.name) == nil {
			parts := strings.Fields(m.name)
			if len(parts) < 2 {
				continue
			}
			m.first, m.last = parts[0], parts[len(parts)-1]
			if len(m.last) < 4 {
				continue // too short to match safely
			}
			members = append(members, m)
		}
	}
	mrows.Close()

	type bill struct{ uid, num, title string }
	var bills []bill
	brows, err := db.Query(`SELECT uid, bill_number, COALESCE(title,'') FROM twoai_pol_bills
		WHERE retired_reason IS NULL AND uid <> ''`)
	if err != nil {
		return err
	}
	for brows.Next() {
		var b bill
		if brows.Scan(&b.uid, &b.num, &b.title) == nil {
			bills = append(bills, b)
		}
	}
	brows.Close()

	// A member is named when the story's own person list carries the surname,
	// or the text puts a chamber title in front of it, or both names appear
	// together. Surname alone is never enough.
	honorific := func(last string) *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(rep\.|representative|sen\.|senator|congressman|congresswoman|lawmaker)\s+([a-z'\-]+\s+){0,2}` + regexp.QuoteMeta(last) + `\b`)
	}
	billNumRe := func(num string) *regexp.Regexp {
		// HB7294 is written H.R. 7294, HR 7294 or HB 7294 in the press.
		digits := strings.TrimLeft(num, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
		if len(digits) < 3 {
			return nil
		}
		return regexp.MustCompile(`(?i)\b(h\.?\s?r\.?|hb|s\.?|sb)\s?` + regexp.QuoteMeta(digits) + `\b`)
	}

	link := func(s story, kind, uid, how string) int {
		res, err := db.Exec(`INSERT INTO twoai_news_links (story_uid, target_kind, target_uid, matched_on, headline, published_on)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::date) ON CONFLICT DO NOTHING`,
			s.uid, kind, uid, how, s.headline, s.published)
		if err != nil {
			fmt.Printf("twoai_news_link: %s -> %s: %v\n", s.uid, uid, err)
			return 0
		}
		n, _ := res.RowsAffected()
		return int(n)
	}

	newMembers, newBills := 0, 0
	// A surname shared by two tracked members cannot identify either from a
	// chamber title alone: "Sen. Scott" is Tim Scott or Rick Scott.
	surnameCount := map[string]int{}
	for _, m := range members {
		surnameCount[strings.ToLower(m.last)]++
	}
	// hasWord reports whether a whole word appears in s, so "harris" does not
	// match "harrison" and "amodei" in "dario amodei" is a word, but the
	// person named is still not Mark Amodei unless "mark" is there too.
	hasWord := func(s, w string) bool {
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '.' || r == ',' || r == '-' }) {
			if f == w {
				return true
			}
		}
		return false
	}
	// hasFirst also accepts the short form, "rob" for Robert, when one is a
	// prefix of the other and at least three letters long.
	hasFirst := func(s, first string) bool {
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '.' || r == ',' || r == '-' }) {
			if f == first || (len(f) >= 3 && len(first) >= 3 && (strings.HasPrefix(first, f) || strings.HasPrefix(f, first))) {
				return true
			}
		}
		return false
	}
	for _, s := range stories {
		low := strings.ToLower(s.text)
		for _, m := range members {
			lastLow := strings.ToLower(m.last)
			firstLow := strings.ToLower(m.first)
			how := ""
			// FIXED 2026-09-23. The first version accepted any person whose name
			// merely contained the surname, which linked Dario Amodei's stories
			// to Rep. Mark Amodei and Harrison Keller's to Mark Harris: 636 of
			// 680 person-list links were wrong and were retired. Both the first
			// and the last name must now be whole words of the same person.
			for _, p := range s.persons {
				if hasWord(p, lastLow) && hasFirst(p, firstLow) {
					how = "named in the story's person list as " + p
					break
				}
			}
			if how == "" && hasWord(low, lastLow) {
				if surnameCount[lastLow] == 1 && honorific(m.last).MatchString(s.text) {
					how = "chamber title in front of the surname"
				} else if strings.Contains(low, firstLow+" "+lastLow) {
					how = "first and last name together"
				}
			}
			if how != "" {
				newMembers += link(s, "member", m.uid, how)
			}
		}
		for _, b := range bills {
			how := ""
			if len(b.title) >= 25 && strings.Contains(low, strings.ToLower(b.title)) {
				how = "the bill's exact title"
			} else if re := billNumRe(b.num); re != nil && re.MatchString(s.text) &&
				(strings.Contains(low, "bill") || strings.Contains(low, "act") || strings.Contains(low, "congress")) {
				how = "the bill number as reported"
			}
			if how != "" {
				newBills += link(s, "bill", b.uid, how)
			}
		}
	}

	var totalM, totalB int
	db.QueryRow(`SELECT count(*) FILTER (WHERE target_kind='member'), count(*) FILTER (WHERE target_kind='bill')
		FROM twoai_news_links WHERE retired_reason IS NULL`).Scan(&totalM, &totalB)
	fmt.Printf("twoai_news_link: stories=%d members=%d bills=%d new_member_links=%d new_bill_links=%d total member=%d bill=%d ok=true\n",
		len(stories), len(members), len(bills), newMembers, newBills, totalM, totalB)
	return nil
}

// twoaiNewsLinksFor returns the stories matched to one record, newest first,
// as points a politics page can render.
func twoaiNewsLinksFor(db *sql.DB, kind, uid string, limit int) []polPt {
	rows, err := db.Query(`SELECT story_uid, COALESCE(headline,''), COALESCE(published_on::text,''), matched_on
		FROM twoai_news_links WHERE target_kind = $1 AND target_uid = $2 AND retired_reason IS NULL
		ORDER BY published_on DESC NULLS LAST LIMIT $3`, kind, uid, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []polPt
	for rows.Next() {
		var su, head, pub, how string
		if rows.Scan(&su, &head, &pub, &how) != nil {
			continue
		}
		when := pub
		if when == "" {
			when = "date not stated"
		}
		out = append(out, polPt{
			Name:   when + " \u00b7 " + head,
			Desc:   "A story in this site's news archive that names this record: " + how + ". uid " + su + ".",
			Source: "/ai-news/" + su + "/",
		})
	}
	return out
}
