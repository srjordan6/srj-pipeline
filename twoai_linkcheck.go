package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// THE LINKS THIS SITE SENDS READERS TO, CHECKED.
//
// On 2026-09-04 Stephen brought three CourtListener links from search
// console with click counts beside them. All three answered 404, and so did
// the other 92: the pipeline had been building courtlistener.com/docket/<id>/
// from a docket id, and CourtListener refuses that form. They were dead from
// the day they were written, and the only reason anybody found out is that a
// reader clicked one and Stephen read the report.
//
// This stage removes that dependency on luck. It takes the external URLs the
// site actually renders - lawsuit dockets, facility operator sites and OSM
// records, research paper links, tool and company sites, source citations -
// and fetches each one, recording what it answered. A URL that fails twice on
// different days is a broken promise to a reader, and the log says so by name.
//
// It is deliberately slow and small: fifty URLs a run, oldest check first,
// one second apart, so a full pass over a few thousand links takes about a
// week and no host sees a burst. Hosts that refuse robots but serve readers
// (Cloudflare challenges, 403 to anything without a browser) are recorded as
// blocked rather than broken, because a 403 to this checker is not a 404 to a
// person.
func twoaiLinkCheck(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_link_health (
		url text PRIMARY KEY,
		kind text NOT NULL,
		status int,
		verdict text,
		checked_at timestamptz,
		first_failed_at timestamptz,
		fail_count int NOT NULL DEFAULT 0,
		note text)`); err != nil {
		return err
	}

	// Collect the reader-facing external links from the tables that render
	// them. Each source names its kind so a failure says what broke.
	if _, err := db.Exec(`INSERT INTO twoai_link_health (url, kind)
		SELECT DISTINCT u, k FROM (
			SELECT courtlistener_url AS u, 'lawsuit-docket' AS k FROM ai_lawsuits WHERE is_active AND coalesce(courtlistener_url,'') <> ''
			UNION ALL
			SELECT website, 'facility-operator' FROM twoai_dc_facilities WHERE coalesce(website,'') <> ''
			UNION ALL
			SELECT website, 'operator-site' FROM twoai_dc_operators WHERE retired_at IS NULL AND coalesce(website,'') <> ''
			UNION ALL
			SELECT url, 'research-paper' FROM twoai_research_papers WHERE coalesce(url,'') <> ''
			UNION ALL
			-- sources is an array on most rows and JSON null on a couple, and
			-- jsonb_array_elements on the null form aborts the whole statement:
			-- "cannot extract elements from a scalar", which is exactly how this
			-- stage collected nothing on its first run at 21:13 on 2026-09-04.
			-- coalesce does not help, because JSON null is not SQL NULL.
			-- Normalise to an array before expanding.
			SELECT s->>'url', 'facility-source' FROM twoai_dc_facilities,
			  jsonb_array_elements(
			    CASE jsonb_typeof(profile->'sources')
			      WHEN 'array'  THEN profile->'sources'
			      WHEN 'object' THEN jsonb_build_array(profile->'sources')
			      ELSE '[]'::jsonb END) s
			  WHERE coalesce(s->>'url','') <> ''
			UNION ALL
			-- Added 2026-09-14. A Screaming Frog crawl flagged dead outbound
			-- links on pages this stage had never looked at: it knew about
			-- facilities, dockets and papers and nothing else. These are the
			-- other places the site sends a reader.
			SELECT website, 'company-site' FROM twoai_company_profiles WHERE coalesce(website,'') <> ''
			UNION ALL
			SELECT p->>'source', 'industry-point' FROM twoai_pages, jsonb_array_elements(
			    CASE jsonb_typeof(data->'points') WHEN 'array' THEN data->'points' ELSE '[]'::jsonb END) p
			  WHERE path LIKE 'industries/%' AND coalesce(p->>'source','') <> ''
			UNION ALL
			SELECT url, 'vendor-post' FROM twoai_vendor_posts WHERE retired_at IS NULL AND coalesce(url,'') <> ''
			UNION ALL
			SELECT url, 'tool-site' FROM twoai_pages, jsonb_array_elements(
			    CASE jsonb_typeof(data->'tools') WHEN 'array' THEN data->'tools' ELSE '[]'::jsonb END) t,
			  LATERAL (SELECT t->>'url' AS url) x
			  WHERE path = 'tools/index.json' AND coalesce(url,'') <> ''
		) x WHERE u LIKE 'http%'
		ON CONFLICT (url) DO NOTHING`); err != nil {
		return err
	}

	budget := 150
	if v, err := strconv.Atoi(os.Getenv("TWOAI_LINKCHECK_BUDGET")); err == nil && v >= 0 {
		budget = v
	}
	if budget == 0 {
		return nil
	}

	// WHAT TO CHECK, IN THE ORDER IT MATTERS. Until 2026-09-14 this was
	// "never checked, or checked over 30 days ago", and the first full pass
	// finished on the 9th - so every night since, the query selected nothing,
	// the log said checked=0, and 24 links that had already failed once sat
	// waiting a month for the second failure that would confirm them. A
	// checker that goes quiet for three weeks after its first pass is not
	// checking.
	//
	// Now: a link that failed once is rechecked after three days, because that
	// is the check that turns a suspicion into a fact. A confirmed-dead link
	// is rechecked weekly, because pages come back. A healthy link is
	// rechecked monthly. Never-checked first, always.
	rows, err := db.Query(`SELECT url, kind FROM twoai_link_health
		WHERE checked_at IS NULL
		   OR (verdict = 'broken' AND fail_count = 1 AND checked_at < now() - interval '3 days')
		   OR (verdict = 'broken' AND fail_count >= 2 AND checked_at < now() - interval '7 days')
		   OR (verdict = 'blocked' AND checked_at < now() - interval '14 days')
		   OR (checked_at < now() - interval '30 days')
		ORDER BY
		  CASE WHEN checked_at IS NULL THEN 0
		       WHEN verdict = 'broken' AND fail_count = 1 THEN 1
		       ELSE 2 END,
		  checked_at NULLS FIRST, url
		LIMIT $1`, budget)
	if err != nil {
		return err
	}
	type job struct{ url, kind string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.url, &j.kind) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	client := &http.Client{Timeout: 25 * time.Second}
	ok, blocked, broken := 0, 0, 0
	var deadList []string
	for _, j := range jobs {
		code, verdict := twoaiLinkProbe(client, j.url)
		switch verdict {
		case "ok":
			ok++
			db.Exec(`UPDATE twoai_link_health SET status=$2, verdict='ok', checked_at=now(),
				first_failed_at=NULL, fail_count=0 WHERE url=$1`, j.url, code)
		case "blocked":
			blocked++
			db.Exec(`UPDATE twoai_link_health SET status=$2, verdict='blocked', checked_at=now(),
				note='host refuses automated fetches; not evidence of a broken page' WHERE url=$1`, j.url, code)
		default:
			broken++
			db.Exec(`UPDATE twoai_link_health SET status=$2, verdict='broken', checked_at=now(),
				first_failed_at=COALESCE(first_failed_at, now()), fail_count=fail_count+1 WHERE url=$1`, j.url, code)
			var fails int
			db.QueryRow(`SELECT fail_count FROM twoai_link_health WHERE url=$1`, j.url).Scan(&fails)
			if fails >= 2 {
				deadList = append(deadList, fmt.Sprintf("%s %s (%d, failed %d times)", j.kind, j.url, code, fails))
			}
		}
		time.Sleep(time.Second)
	}

	var pending, dead int
	db.QueryRow(`SELECT count(*) FROM twoai_link_health WHERE checked_at IS NULL`).Scan(&pending)
	db.QueryRow(`SELECT count(*) FROM twoai_link_health WHERE verdict='broken' AND fail_count >= 2`).Scan(&dead)
	fmt.Printf("twoai_link_check: checked=%d ok=%d blocked=%d broken=%d | confirmed_dead=%d never_checked=%d\n",
		len(jobs), ok, blocked, broken, dead, pending)
	if len(deadList) > 0 {
		fmt.Fprintf(os.Stderr, "twoai_link_check: %d link(s) this site renders have now failed twice. A reader who clicks one gets nothing:\n", len(deadList))
		for i, d := range deadList {
			if i == 20 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(deadList)-20)
				break
			}
			fmt.Fprintln(os.Stderr, "  "+d)
		}
	}

	// PUBLISH THE DEAD LIST SO THE SITE CAN ACT ON IT. Until 2026-09-14 a
	// confirmed-dead link's only consequence was a line in stderr; the page
	// kept rendering the link and a reader kept getting a 404. The site's
	// build now reads meta/dead-links.json and neutralises those anchors in
	// the rendered HTML - the text stays, the click goes nowhere - so the
	// fix reaches every template in one place, the same way the paragraph
	// cap does. Written every run, empty list included, so a recovered link
	// comes back on the next build without anyone touching it.
	drows, derr := db.Query(`SELECT url, kind, status, first_failed_at::date::text
		FROM twoai_link_health WHERE verdict='broken' AND fail_count >= 2 ORDER BY url`)
	if derr == nil {
		type deadLink struct {
			URL, Kind, Since string
			Status            int
		}
		var list []map[string]any
		for drows.Next() {
			var u, k, since string
			var st int
			if drows.Scan(&u, &k, &st, &since) == nil {
				list = append(list, map[string]any{"url": u, "kind": k, "status": st, "since": since})
			}
		}
		drows.Close()
		if list == nil {
			list = []map[string]any{}
		}
		doc, _ := json.Marshal(map[string]any{
			"generated": time.Now().UTC().Format("2006-01-02"), "count": len(list), "links": list,
			"note": "External URLs this site renders that have answered 404/410 on two checks at least three days apart. The build removes the href from these anchors.",
		})
		db.Exec(`INSERT INTO twoai_pages (path, kind, data, updated_at) VALUES ('meta/dead-links.json','meta',$1::jsonb,now())
			ON CONFLICT (path) DO UPDATE SET data=EXCLUDED.data, updated_at=now()`, string(doc))
	}
	return nil
}

// twoaiLinkProbe answers ok, blocked or broken. HEAD first because it is
// cheap; a HEAD refusal is retried as GET, since plenty of hosts answer 405
// or 403 to HEAD and serve the page perfectly to a reader.
func twoaiLinkProbe(client *http.Client, u string) (int, string) {
	try := func(method string) int {
		req, err := http.NewRequest(method, u, nil)
		if err != nil {
			return 0
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org link check; +https://theworldofai.org)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	code := try("HEAD")
	if code == 0 || code == 403 || code == 405 || code == 429 || code >= 500 {
		code = try("GET")
	}
	switch {
	case code >= 200 && code < 400:
		return code, "ok"
	case code == 403 || code == 401 || code == 429 || code >= 500:
		// A host that refuses robots, rate-limits, or is having a bad day is
		// not a broken page. Recording these as broken would bury the 404s
		// that actually matter.
		return code, "blocked"
	case code == 0:
		return 0, "blocked"
	default:
		return code, "broken"
	}
}
