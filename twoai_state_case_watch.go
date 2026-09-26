package main

// twoai_state_case_watch: reporting as the docket for state court cases.
// Stephen, 2026-09-26, after extending the tracker to privacy cases: state
// courts have no PACER, so a case like New Mexico v. Meta has no docket feed.
// For every tracker case without a CourtListener docket, this runs one
// Google News query a day on the case's parties, files each new article, and
// when an article's headline carries a status word (verdict, dismissed,
// settled, appeal, penalty, ruling, injunction) it becomes the case's latest
// development and a timeline entry marked as reporting, so the case page
// moves the way a docketed case does. Nothing here changes a status badge;
// that stays a decision for a person, made with the article in front of them.

import (
	"bytes"
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var stateCaseStatusRe = regexp.MustCompile(`(?i)\b(verdict|jury (finds|found|rules|ruled|awards|awarded)|dismiss(ed|es|al)|settle(s|d|ment)|appeal(s|ed)?|penalt(y|ies)|ruling|rules|ruled|judgment|injunction|fine(s|d)?|damages|liable|trial (begins|opens|starts)|certif(ies|ied) class)\b`)

func twoaiStateCaseWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_state_case_news (
		url text PRIMARY KEY,
		slug text NOT NULL,
		title text NOT NULL,
		domain text,
		published_on date,
		status_words text,
		first_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT slug, case_name, plaintiffs, defendants FROM ai_lawsuits
		WHERE is_active AND courtlistener_url IS NULL ORDER BY slug`)
	if err != nil {
		return err
	}
	type c struct{ slug, name, pl, df string }
	var cases []c
	for rows.Next() {
		var x c
		if rows.Scan(&x.slug, &x.name, &x.pl, &x.df) == nil {
			cases = append(cases, x)
		}
	}
	rows.Close()
	if len(cases) == 0 {
		fmt.Println("twoai_state_case_watch: no state court cases on the tracker")
		return nil
	}
	client := &http.Client{Timeout: 20 * time.Second}
	newArticles, moved := 0, 0
	for _, x := range cases {
		// The query is the two sides of the caption and the word lawsuit:
		// "New Mexico" "Meta" lawsuit finds the reporting without the
		// procedural words that vary between outlets.
		q := stateCaseQuery(x.name)
		u := "https://news.google.com/rss/search?q=" + url.QueryEscape(q) + "&hl=en-US&gl=US&ceid=US:en"
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
		resp, ferr := client.Do(req)
		if ferr != nil {
			fmt.Println("twoai_state_case_watch:", x.slug, ferr)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		var feed struct {
			Items []struct {
				Title   string `xml:"title"`
				Link    string `xml:"link"`
				PubDate string `xml:"pubDate"`
				Source  struct {
					Text string `xml:",chardata"`
					URL  string `xml:"url,attr"`
				} `xml:"source"`
			} `xml:"channel>item"`
		}
		dec := xml.NewDecoder(bytes.NewReader(body))
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		if derr := dec.Decode(&feed); derr != nil {
			fmt.Println("twoai_state_case_watch:", x.slug, "parse:", derr)
			continue
		}
		for i, it := range feed.Items {
			if i >= 20 {
				break
			}
			title := strings.TrimSpace(it.Title)
			link := strings.TrimSpace(it.Link)
			if title == "" || link == "" {
				continue
			}
			var seen bool
			if db.QueryRow(`SELECT EXISTS(SELECT 1 FROM twoai_state_case_news WHERE url = $1)`, link).Scan(&seen); seen {
				continue
			}
			pub := ""
			if t, perr := time.Parse(time.RFC1123, it.PubDate); perr == nil {
				pub = t.Format("2006-01-02")
			} else if t, perr := time.Parse(time.RFC1123Z, it.PubDate); perr == nil {
				pub = t.Format("2006-01-02")
			}
			domain := strings.TrimSpace(it.Source.Text)
			if pu, perr := url.Parse(it.Source.URL); perr == nil && pu.Host != "" {
				domain = strings.TrimPrefix(pu.Host, "www.")
			}
			words := strings.Join(uniqueLower(stateCaseStatusRe.FindAllString(title, -1)), ", ")
			if _, err := db.Exec(`INSERT INTO twoai_state_case_news (url, slug, title, domain, published_on, status_words)
				VALUES ($1,$2,$3,$4,NULLIF($5,'')::date,NULLIF($6,'')) ON CONFLICT (url) DO NOTHING`,
				link, x.slug, title, domain, pub, words); err != nil {
				return err
			}
			newArticles++
			if words == "" || pub == "" {
				continue
			}
			// A status word in a headline moves the case: latest development
			// and a timeline entry, both marked as reporting, never a badge.
			dev := fmt.Sprintf("%s (%s, %s; reporting, not a docket entry)", title, domain, pub)
			res, err := db.Exec(`UPDATE ai_lawsuits SET
					latest_development = $2, latest_development_date = $3::date,
					timeline = COALESCE(timeline, '[]'::jsonb) || jsonb_build_array(jsonb_build_object(
						'date', $3::text, 'title', $4::text, 'url', $5::text, 'doc_no', 'reporting')),
					docket_checked_at = now(), updated_at = now()
				WHERE slug = $1 AND (latest_development_date IS NULL OR latest_development_date <= $3::date)
				  AND NOT COALESCE(timeline, '[]'::jsonb) @> jsonb_build_array(jsonb_build_object('url', $5::text))`,
				x.slug, dev, pub, title+" ("+domain+")", link)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				moved++
				fmt.Printf("twoai_state_case_watch: %s moved by reporting: %s [%s]\n", x.slug, trunc(title, 90), words)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("twoai_state_case_watch: cases=%d new_articles=%d moved=%d ok=true\n", len(cases), newArticles, moved)
	return nil
}

// stateCaseQuery turns "State of New Mexico ex rel. Torrez v. Meta Platforms,
// Inc." into `"New Mexico" "Meta" lawsuit`: the distinctive words of each
// side, quoted, without the procedural noise.
func stateCaseQuery(caseName string) string {
	noise := regexp.MustCompile(`(?i)\b(state of|people of the state of|ex rel\.?|et al\.?|inc\.?|llc|corp\.?|corporation|company|co\.?|platforms|technologies|holdings|the)\b|[,.]`)
	parts := regexp.MustCompile(`(?i)\s+v(s|\.)?\s+`).Split(caseName, 2)
	var q []string
	for _, p := range parts {
		// "State of New Mexico ex rel. Torrez": the relator's name narrows
		// the search to articles that name the attorney general, so it goes.
		if i := regexp.MustCompile(`(?i)\s+ex rel`).FindStringIndex(p); i != nil {
			p = p[:i[0]]
		}
		p = strings.TrimSpace(noise.ReplaceAllString(p, " "))
		p = strings.Join(strings.Fields(p), " ")
		if p != "" {
			// keep at most the first three words of a side
			w := strings.Fields(p)
			if len(w) > 3 {
				w = w[:3]
			}
			q = append(q, `"`+strings.Join(w, " ")+`"`)
		}
	}
	return strings.Join(q, " ") + " lawsuit"
}

func uniqueLower(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
