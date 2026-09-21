package main

// The press room and member timelines for The Politics of AI. Stephen,
// 2026-09-21: build /press/ for reporters covering AI policy, with data
// downloads, a daily digest feed and member timelines, and make certain it
// stays updated. He approved the neutral version: no divergence index, no
// labels on any person, and every export carries its sources.
//
// THREE PARTS, ALL REBUILT FROM SQL ON EVERY RUN.
//
// 1. twoaiPoliticsLegislators, run from the bills stage once a day. Reads the
//    public domain congress-legislators file (one request) and matches each
//    member to the LegiScan person by Vote Smart id, then OpenSecrets id,
//    both hard identifiers, never by name. A match gives the member their FEC
//    candidate ids. The FEC stage then maps each candidate id to its campaign
//    committees, which is how a PAC contribution to a committee becomes a
//    contribution beside a member's votes.
//
// 2. twoaiPoliticsExports writes the downloadable documents under politics/:
//    lobbying, money, bills, members, and a digest of the last 72 hours. The
//    site mirrors them to /api/ as JSON and CSV and builds the RSS feed from
//    the digest. Each record keeps its uid and its source URL.
//
// 3. Member timelines: one page per member with any AI event, every dated
//    event in date order, contributions received, bills sponsored, votes
//    cast, each with its source. Facts side by side; the page says in words
//    that placement is not a claim of cause. Drafts until Stephen publishes.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const polLegislatorsURL = "https://unitedstates.github.io/congress-legislators/legislators-current.json"

func twoaiPoliticsLegislators(db *sql.DB) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(polLegislatorsURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("legislators: http %d", resp.StatusCode)
	}
	var people []struct {
		ID struct {
			Bioguide    string   `json:"bioguide"`
			FEC         []string `json:"fec"`
			OpenSecrets string   `json:"opensecrets"`
			VoteSmart   int      `json:"votesmart"`
			Wikidata    string   `json:"wikidata"`
		} `json:"id"`
		Name struct {
			Official string `json:"official_full"`
			First    string `json:"first"`
			Last     string `json:"last"`
		} `json:"name"`
		Terms []struct {
			Type     string `json:"type"`
			State    string `json:"state"`
			District any    `json:"district"`
			Party    string `json:"party"`
		} `json:"terms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&people); err != nil {
		return err
	}
	byVS := map[int]int{}
	byOS := map[string]int{}
	if r, err := db.Query(`SELECT people_id, COALESCE(votesmart_id,0), COALESCE(opensecrets_id,'') FROM twoai_pol_members`); err == nil {
		for r.Next() {
			var pid, vs int
			var os string
			if r.Scan(&pid, &vs, &os) == nil {
				if vs > 0 {
					byVS[vs] = pid
				}
				if os != "" {
					byOS[os] = pid
				}
			}
		}
		r.Close()
	}
	stored, matched := 0, 0
	for _, p := range people {
		if p.ID.Bioguide == "" || len(p.Terms) == 0 {
			continue
		}
		t := p.Terms[len(p.Terms)-1]
		name := p.Name.Official
		if name == "" {
			name = strings.TrimSpace(p.Name.First + " " + p.Name.Last)
		}
		fec := p.ID.FEC
		if fec == nil {
			fec = []string{}
		}
		var pid interface{}
		on := ""
		if id, ok := byVS[p.ID.VoteSmart]; ok && p.ID.VoteSmart > 0 {
			pid, on = id, "votesmart"
		} else if id, ok := byOS[p.ID.OpenSecrets]; ok && p.ID.OpenSecrets != "" {
			pid, on = id, "opensecrets"
		}
		dist := ""
		if t.District != nil {
			dist = fmt.Sprint(t.District)
		}
		if _, err := db.Exec(`
			INSERT INTO twoai_pol_legislators (bioguide, name, chamber, state, district, party, fec_ids,
				opensecrets_id, votesmart_id, wikidata, people_id, matched_on)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,NULLIF($8,''),NULLIF($9,0),NULLIF($10,''),$11,NULLIF($12,''))
			ON CONFLICT (bioguide) DO UPDATE SET name = EXCLUDED.name, chamber = EXCLUDED.chamber, state = EXCLUDED.state,
				district = EXCLUDED.district, party = EXCLUDED.party, fec_ids = EXCLUDED.fec_ids,
				opensecrets_id = EXCLUDED.opensecrets_id, votesmart_id = EXCLUDED.votesmart_id, wikidata = EXCLUDED.wikidata,
				people_id = COALESCE(EXCLUDED.people_id, twoai_pol_legislators.people_id),
				matched_on = COALESCE(EXCLUDED.matched_on, twoai_pol_legislators.matched_on), last_seen = now()`,
			p.ID.Bioguide, name, t.Type, t.State, dist, t.Party, pq.Array(fec), p.ID.OpenSecrets,
			p.ID.VoteSmart, p.ID.Wikidata, pid, on); err != nil {
			fmt.Printf("twoai_politics_legislators: %s: %v\n", p.ID.Bioguide, err)
			continue
		}
		stored++
		if pid != nil {
			matched++
		}
	}
	db.Exec(`UPDATE twoai_pol_members m SET bioguide = l.bioguide, fec_candidate_ids = l.fec_ids
		FROM twoai_pol_legislators l WHERE l.people_id = m.people_id`)
	fmt.Printf("twoai_politics_legislators: legislators=%d matched_to_legiscan=%d ok=true\n", stored, matched)
	return nil
}

// polFECCandidateCommittees maps member candidate ids to their campaign
// committees, a few calls a run, until every member is mapped. Called from
// the FEC stage with its call counter so the budget is shared.
func polFECCandidateCommittees(db *sql.DB, client *http.Client, key string, calls *int) (int, error) {
	rows, err := db.Query(`
		SELECT DISTINCT c FROM twoai_pol_legislators l, unnest(l.fec_ids) c
		WHERE l.people_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM twoai_pol_candidate_committees cc WHERE cc.candidate_id = c)
		LIMIT 150`)
	if err != nil {
		return 0, err
	}
	var cands []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	mapped := 0
	for _, cid := range cands {
		v := url.Values{"per_page": {"50"}}
		pg, err := polFECGet(client, key, "/candidate/"+cid+"/committees/", v, calls)
		if err != nil {
			break
		}
		for _, raw := range pg.Results {
			var c struct {
				ID          string `json:"committee_id"`
				Name        string `json:"name"`
				Designation string `json:"designation"`
			}
			if json.Unmarshal(raw, &c) != nil || c.ID == "" {
				continue
			}
			if _, err := db.Exec(`INSERT INTO twoai_pol_candidate_committees (committee_id, candidate_id, designation, name)
				VALUES ($1,$2,$3,$4) ON CONFLICT (committee_id, candidate_id) DO UPDATE SET designation = EXCLUDED.designation,
				name = EXCLUDED.name, last_seen = now()`, c.ID, cid, c.Designation, ldaClean(c.Name)); err == nil {
				mapped++
			}
		}
		// A candidate with no committees still gets a row, so it is not asked
		// for again every run.
		if len(pg.Results) == 0 {
			db.Exec(`INSERT INTO twoai_pol_candidate_committees (committee_id, candidate_id, designation, name)
				VALUES ('none', $1, NULL, NULL) ON CONFLICT DO NOTHING`, cid)
		}
	}
	return mapped, nil
}

type polEvent struct {
	Date   string `json:"date"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Source string `json:"source"`
	UID    string `json:"uid"`
}

// twoaiPoliticsExports writes politics/*.json and the member timelines.
func twoaiPoliticsExports(db *sql.DB, today string) error {
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	put := func(path, kind string, doc map[string]any) error {
		b, _ := json.Marshal(doc)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
			VALUES ($1,$2,'politics-of-ai',$3,now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, data = EXCLUDED.data, updated_at = now()`,
			path, kind, ldaRaw(b))
		return err
	}
	jsonRows := func(q string, args ...any) ([]map[string]any, error) {
		r, err := db.Query(`SELECT row_to_json(x)::text FROM (`+q+`) x`, args...)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		out := []map[string]any{}
		for r.Next() {
			var s string
			if r.Scan(&s) == nil {
				var m map[string]any
				if json.Unmarshal([]byte(s), &m) == nil {
					out = append(out, m)
				}
			}
		}
		return out, nil
	}
	cite := "The World of AI, theworldofai.org, The Politics of AI. Retrieved " + today + "."

	lobbying, err := jsonRows(`SELECT uid, filing_uuid, filing_year, filing_period, dt_posted, client_id, client_name,
			registrant_name, income, expenses, bill_refs, government_entities, document_url AS source
		FROM twoai_pol_lobbying WHERE retired_reason IS NULL ORDER BY dt_posted DESC`)
	if err != nil {
		return fmt.Errorf("lobbying export: %w", err)
	}
	money, err := jsonRows(`SELECT m.uid, m.schedule, m.committee_id, c.name AS committee_name, m.counterparty_name,
			m.counterparty_committee_id, m.candidate_id, m.support_oppose, m.amount, m.txn_date,
			(SELECT l.bioguide FROM twoai_pol_candidate_committees cc JOIN twoai_pol_legislators l ON cc.candidate_id = ANY(l.fec_ids)
			  WHERE cc.committee_id = m.counterparty_committee_id LIMIT 1) AS member_bioguide,
			COALESCE(m.pdf_url, 'https://www.fec.gov/data/committee/' || m.committee_id || '/') AS source
		FROM twoai_pol_money m JOIN twoai_pol_committees c ON c.committee_id = m.committee_id
		WHERE m.retired_reason IS NULL ORDER BY m.txn_date DESC NULLS LAST`)
	if err != nil {
		return fmt.Errorf("money export: %w", err)
	}
	bills, err := jsonRows(`SELECT uid, bill_id, bill_number, session_name, title, status, status_date, issues, preemption,
			(SELECT count(*) FROM twoai_pol_sponsorships s WHERE s.bill_id = b.bill_id) AS sponsors,
			COALESCE(NULLIF(state_link,''), url) AS source
		FROM twoai_pol_bills b WHERE retired_reason IS NULL ORDER BY status_date DESC NULLS LAST`)
	if err != nil {
		return fmt.Errorf("bills export: %w", err)
	}
	members, err := jsonRows(`SELECT m.uid, m.people_id, m.name, m.party, m.role, m.district, l.bioguide, l.state, l.fec_ids,
			(SELECT count(*) FROM twoai_pol_sponsorships s WHERE s.people_id = m.people_id) AS ai_bills_sponsored,
			(SELECT count(*) FROM twoai_pol_votes v WHERE v.people_id = m.people_id) AS ai_votes_cast
		FROM twoai_pol_members m LEFT JOIN twoai_pol_legislators l ON l.people_id = m.people_id
		WHERE m.retired_reason IS NULL ORDER BY m.name`)
	if err != nil {
		return fmt.Errorf("members export: %w", err)
	}
	for _, e := range []struct {
		path, title string
		rows        []map[string]any
	}{
		{"politics/lobbying.json", "AI lobbying filings", lobbying},
		{"politics/money.json", "Money from AI company and AI-focused political committees", money},
		{"politics/bills.json", "AI bills in Congress", bills},
		{"politics/members.json", "Members of Congress with AI activity", members},
	} {
		if err := put(e.path, "politics-export", map[string]any{
			"title": e.title, "generated": now, "count": len(e.rows), "citation": cite,
			"note": "Each record carries its uid and the URL of its source filing or record. Placement of records beside one another is not a claim that one caused another.",
			"rows": e.rows,
		}); err != nil {
			return err
		}
	}

	// The digest: everything new in the last 72 hours, newest first.
	digest, _ := jsonRows(`
		SELECT * FROM (
		  SELECT dt_posted AS at, 'lobbying' AS kind, client_name || ' lobbying filing, by ' || COALESCE(registrant_name,'') AS title,
		         uid, document_url AS source FROM twoai_pol_lobbying WHERE first_seen > now() - interval '72 hours'
		  UNION ALL
		  SELECT m.txn_date::timestamptz, 'contribution', c.name || ' to ' || COALESCE(m.counterparty_name,'') || ', $' || to_char(m.amount,'FM999,999,990'),
		         m.uid, COALESCE(m.pdf_url, 'https://www.fec.gov/data/committee/' || m.committee_id || '/')
		  FROM twoai_pol_money m JOIN twoai_pol_committees c ON c.committee_id = m.committee_id WHERE m.first_seen > now() - interval '72 hours'
		  UNION ALL
		  SELECT b.last_seen, 'bill', b.bill_number || ': ' || b.title, b.uid, COALESCE(NULLIF(b.state_link,''), b.url)
		  FROM twoai_pol_bills b WHERE b.last_seen > now() - interval '72 hours'
		  UNION ALL
		  SELECT r.vote_date::timestamptz, 'vote', b.bill_number || ' roll call: ' || COALESCE(r.description,'') || ', ' || r.yea || ' yea, ' || r.nay || ' nay',
		         r.uid, COALESCE(NULLIF(b.state_link,''), b.url)
		  FROM twoai_pol_rollcalls r JOIN twoai_pol_bills b ON b.bill_id = r.bill_id WHERE r.first_seen > now() - interval '72 hours'
		) d ORDER BY at DESC NULLS LAST LIMIT 200`)
	if err := put("politics/digest.json", "politics-export", map[string]any{
		"title": "The Politics of AI, last 72 hours", "generated": now, "count": len(digest), "citation": cite, "rows": digest,
	}); err != nil {
		return err
	}
	if err := put("politics/press.json", "politics-press", map[string]any{
		"generated": now, "lobbying": len(lobbying), "money": len(money), "bills": len(bills), "members": len(members),
		"digest": len(digest), "hub_href": polBase + polHubUID + "/", "lobbying_href": polBase + polLobbyUID + "/",
	}); err != nil {
		return err
	}

	// Member timelines.
	type mem struct {
		pid                  int
		uid, name, party, st string
	}
	mr, err := db.Query(`SELECT m.people_id, m.uid, m.name, COALESCE(m.party,''), COALESCE(l.state,'')
		FROM twoai_pol_members m LEFT JOIN twoai_pol_legislators l ON l.people_id = m.people_id
		WHERE m.retired_reason IS NULL`)
	if err != nil {
		return err
	}
	var mems []mem
	for mr.Next() {
		var m mem
		if mr.Scan(&m.pid, &m.uid, &m.name, &m.party, &m.st) == nil {
			mems = append(mems, m)
		}
	}
	mr.Close()
	pages := 0
	for _, m := range mems {
		var ev []polEvent
		if r, err := db.Query(`SELECT COALESCE(b.status_date::text,''), b.bill_number, b.title, b.uid,
				COALESCE(NULLIF(b.state_link,''), b.url), s.sponsor_type_id
			FROM twoai_pol_sponsorships s JOIN twoai_pol_bills b ON b.bill_id = s.bill_id WHERE s.people_id = $1`, m.pid); err == nil {
			for r.Next() {
				var d, num, title, uid, src string
				var st int
				if r.Scan(&d, &num, &title, &uid, &src, &st) == nil {
					role := "Cosponsor"
					if st == 1 {
						role = "Sponsor"
					}
					ev = append(ev, polEvent{Date: d, Kind: "bill", Title: role + " of " + num, Detail: title + ". Date shown is the bill's latest status date.", Source: src, UID: uid})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT COALESCE(rc.vote_date::text,''), v.vote_text, b.bill_number, COALESCE(rc.description,''), rc.uid,
				COALESCE(NULLIF(b.state_link,''), b.url)
			FROM twoai_pol_votes v JOIN twoai_pol_rollcalls rc ON rc.roll_call_id = v.roll_call_id
			JOIN twoai_pol_bills b ON b.bill_id = rc.bill_id WHERE v.people_id = $1`, m.pid); err == nil {
			for r.Next() {
				var d, vt, num, desc, uid, src string
				if r.Scan(&d, &vt, &num, &desc, &uid, &src) == nil {
					ev = append(ev, polEvent{Date: d, Kind: "vote", Title: "Voted " + vt + " on " + num, Detail: desc, Source: src, UID: uid})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT COALESCE(mo.txn_date::text,''), c.name, mo.amount, mo.uid,
				COALESCE(mo.pdf_url, 'https://www.fec.gov/data/committee/' || mo.committee_id || '/'), mo.schedule, COALESCE(mo.support_oppose,'')
			FROM twoai_pol_money mo JOIN twoai_pol_committees c ON c.committee_id = mo.committee_id
			JOIN twoai_pol_legislators l ON l.people_id = $1
			WHERE (mo.schedule = 'B' AND mo.counterparty_committee_id IN
			        (SELECT cc.committee_id FROM twoai_pol_candidate_committees cc WHERE cc.candidate_id = ANY(l.fec_ids)))
			   OR (mo.schedule = 'E' AND mo.candidate_id = ANY(l.fec_ids))`, m.pid); err == nil {
			for r.Next() {
				var d, cname, uid, src, sch, so string
				var amt float64
				if r.Scan(&d, &cname, &amt, &uid, &src, &sch, &so) == nil {
					t := fmt.Sprintf("Contribution of $%s from %s", polMoney(amt), cname)
					if sch == "E" {
						dir := "supporting"
						if so == "O" {
							dir = "opposing"
						}
						t = fmt.Sprintf("Independent expenditure of $%s by %s, %s", polMoney(amt), cname, dir)
					}
					ev = append(ev, polEvent{Date: d, Kind: "money", Title: t, Detail: "From the committee's filing with the Federal Election Commission.", Source: src, UID: uid})
				}
			}
			r.Close()
		}
		if len(ev) == 0 {
			continue
		}
		sort.SliceStable(ev, func(i, j int) bool { return ev[i].Date > ev[j].Date })
		type point struct {
			Name   string `json:"name"`
			Desc   string `json:"desc"`
			Source string `json:"source"`
		}
		pts := make([]point, 0, len(ev))
		for _, e := range ev {
			d := e.Date
			if d == "" {
				d = "date not stated"
			}
			pts = append(pts, point{Name: d + " · " + e.Title, Desc: e.Detail + " uid " + e.UID + ".", Source: e.Source})
		}
		who := m.name
		if m.party != "" || m.st != "" {
			who += " (" + strings.Trim(m.party+", "+m.st, ", ") + ")"
		}
		doc := map[string]any{
			"tax": "pol-member", "uid": m.uid, "page_uid": m.uid, "slug": "pol-member-" + m.uid,
			"name": who + ": AI record", "shape": "tech-section", "is_hub": true, "draft": true,
			"parent_name": "The Politics of AI", "parent_href": polBase + polHubUID + "/",
			"summary": fmt.Sprintf("Every dated AI event this site holds for %s: bills sponsored or cosponsored, recorded votes on AI bills, and money from AI company and AI-focused political committees, newest first. %d events.", m.name, len(ev)),
			"blurb": "Events are placed in date order so they can be read together. Placement is not a claim that one event caused another, and this page draws no conclusion about any person's motives. Each event links to the filing or record it comes from.",
			"points": pts, "children": []any{}, "total": len(ev), "generated": today, "verified": today,
			"refresh_every_days": 1, "category": polCategory,
		}
		b, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
			VALUES ($1,'tech-section','pol-member',$2,now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`,
			"industries/pol-member-"+m.uid+".json", ldaRaw(b)); err == nil {
			pages++
		}
	}
	fmt.Printf("twoai_politics_exports: lobbying=%d money=%d bills=%d members=%d digest=%d member_pages=%d ok=true\n",
		len(lobbying), len(money), len(bills), len(members), len(digest), pages)
	return nil
}

func polMoney(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
