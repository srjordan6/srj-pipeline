package main

// Lobbying firms, lobbyists and AI bills as their own pages, each cross
// referenced with the others, with company pages and with AI people. Stephen,
// 2026-09-21: add lobbyists and lobbying firms to the website, and make
// certain they are cross referenced with AI legislation, AI people and
// companies. This reverses the earlier decision to name them only.
//
// IDENTITY IS THE LDA'S OWN ID, NEVER A NAME. A firm is its LDA registrant
// id; a lobbyist is their LDA lobbyist id. Two lobbyists with the same name
// are two pages. A lobbyist is linked to an AI People page only through a
// row Stephen confirms in twoai_pol_lobbyist_people. A member of Congress is
// linked to an AI People page when the member's Wikidata id from
// congress-legislators equals a Wikidata id this site holds for that person,
// which is a hard identifier.
//
// BILL NUMBERS. A lobbying filing writes "H.R. 1770"; LegiScan files the same
// bill as HB1770. The mapping is HR to HB, S to SB, HRES to HR, SRES to SR,
// and only filings reporting on 2025 or later are mapped, because the same
// number in a 2024 filing is a different bill in the 118th Congress.
//
// WHAT A PAGE SAYS. Only what the filings and records say: counts, names as
// filed, dates, and the prior government positions a filing discloses under
// the Act, quoted as disclosed. No page characterises a person.
//
// ALL PAGES ARE DRAFTS until Stephen publishes the hub. Rebuilt every run.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

const (
	polFirmsUID      = "ef995158"
	polLobbyistsUID  = "b954389e"
	polBillsUID      = "928bd470"
	polAISQL         = `(artificial intelligence|machine learning|\mA\.?I\.?\M|generative|large language model|deepfake|algorithmic|automated decision|facial recognition|data ?cent(er|re)|hyperscale|large load|colocation facilit)`
	polDirectoryList = 300
)

type polPt struct {
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Source string `json:"source"`
}

func polPutPage(db *sql.DB, path, tax string, doc map[string]any) error {
	// tax must be a real twoai_taxonomy slug: twoai_pages.taxonomy_slug has a
	// foreign key to it. The first version passed pol-member and wrote zero
	// member pages without a word, because the error was not printed.
	// PUBLISHED, Stephen 2026-09-22. Every politics page is live. A page
	// whose document is under polThinBytes is kept out of search (noindex,
	// so the postbuild sitemap step drops it) until its data grows, because
	// AdSense flagged the site for low value content the day before.
	doc["noindex"] = false
	b, _ := json.Marshal(doc)
	uid, _ := doc["uid"].(string)
	if polKeepOutOfSearch(db, uid, len(b)) {
		doc["noindex"] = true
		b, _ = json.Marshal(doc)
	}
	_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
		VALUES ($1,'tech-section',$2,$3,now())
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()`,
		path, tax, ldaRaw(b))
	if err != nil {
		fmt.Printf("twoai_politics_directory: write %s: %v\n", path, err)
	}
	return err
}

// polThinBytes is the document size below which a politics page is live but
// not indexed. Entity pages with one or two filings sit around 1.5 to 2.7 kB.
const polThinBytes = 4000

// THE SIZE RULE LET THIN PAGES THROUGH. Stephen, 2026-09-23: the site-wide
// thin audit found 88 member timelines and about 60 lobbyist and firm pages
// indexable with under 300 words of their own, because their documents were
// over 4,000 bytes of structure but rendered little text. So a page is also
// kept out of search when it was MEASURED thin, recorded in twoai_pol_thin by
// uid from the live-page audit. The hold is sticky: a noindex page leaves the
// sitemap and so leaves the audit, and without a sticky record it would flip
// back to indexable days later and fall out again. It lifts when the page's
// document has grown half again past the size it had when it was judged thin,
// the point at which there is plausibly more to read.
func polKeepOutOfSearch(db *sql.DB, uid string, size int) bool {
	if size < polThinBytes {
		return true
	}
	if uid == "" {
		return false
	}
	var bytesAt int
	if db.QueryRow(`SELECT bytes_at FROM twoai_pol_thin WHERE uid = $1`, uid).Scan(&bytesAt) != nil {
		return false
	}
	return size < bytesAt*3/2
}

// polRecordMeasuredThin copies the audit's verdict into the sticky hold for
// every politics page it measured under the floor, before the pages are
// written. Runs once per politics build.
func polRecordMeasuredThin(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_pol_thin (
		uid text PRIMARY KEY, words int NOT NULL, bytes_at int NOT NULL, decided_on date NOT NULL DEFAULT current_date)`)
	res, err := db.Exec(`INSERT INTO twoai_pol_thin (uid, words, bytes_at)
		SELECT substring(a.url from '/([0-9a-f]{8})/$'), a.words, length(p.data::text)
		FROM twoai_page_audit a
		JOIN twoai_pages p ON p.data->>'uid' = substring(a.url from '/([0-9a-f]{8})/$')
		  AND p.taxonomy_slug LIKE 'pol%'
		WHERE a.status = 200 AND a.words < $1 AND a.url LIKE '%/enterprise-applications-governance-and-tools/%'
		ON CONFLICT (uid) DO NOTHING`, thinAuditFloorPolitics)
	if err == nil {
		n, _ := res.RowsAffected()
		if n > 0 {
			fmt.Printf("twoai_politics: %d page(s) measured thin by the audit, held out of search\n", n)
		}
	}
}

// thinAuditFloorPolitics is the page-specific word count below which a
// politics entity page is held out of search.
const thinAuditFloorPolitics = 300

func polPage(tax, uid, name, summary, blurb, today string, points []polPt, children []map[string]any) map[string]any {
	if points == nil {
		points = []polPt{}
	}
	if children == nil {
		children = []map[string]any{}
	}
	return map[string]any{
		"tax": tax, "uid": uid, "page_uid": uid, "slug": tax + "-" + uid, "name": name,
		"shape": "tech-section", "is_hub": true, "draft": false,
		"parent_name": "The Politics of AI", "parent_href": polBase + polHubUID + "/",
		"summary": summary, "blurb": blurb, "points": points, "children": children,
		"total": len(points), "generated": today, "verified": today, "refresh_every_days": 1, "category": polCategory,
	}
}

// polBillKey turns a filing's bill reference into the LegiScan number.
const polBillKeySQL = `CASE split_part(r,' ',1) WHEN 'HR' THEN 'HB' WHEN 'S' THEN 'SB' WHEN 'HRES' THEN 'HR' WHEN 'SRES' THEN 'SR' END || split_part(r,' ',2)`

// polBillLabel writes LegiScan's HB1770 the way Congress does, H.R. 1770.
func polBillLabel(n string) string {
	for _, p := range [][2]string{{"HJR", "H.J.Res. "}, {"SJR", "S.J.Res. "}, {"HCR", "H.Con.Res. "}, {"SCR", "S.Con.Res. "},
		{"HB", "H.R. "}, {"SB", "S. "}, {"HR", "H.Res. "}, {"SR", "S.Res. "}} {
		if strings.HasPrefix(n, p[0]) && len(n) > len(p[0]) && n[len(p[0])] >= '0' && n[len(p[0])] <= '9' {
			return p[1] + n[len(p[0]):]
		}
	}
	return n
}

func twoaiPoliticsDirectory(db *sql.DB, today string) error {
	// 1. Firms and lobbyists from the filings held.
	if _, err := db.Exec(`
		INSERT INTO twoai_pol_firms (registrant_id, uid, name)
		SELECT registrant_id, encode(substring(sha256(('lda-firm:' || registrant_id)::bytea) from 1 for 4), 'hex'),
		       (array_agg(registrant_name ORDER BY dt_posted DESC NULLS LAST))[1]
		FROM twoai_pol_lobbying WHERE registrant_id IS NOT NULL AND registrant_id > 0 AND retired_reason IS NULL
		GROUP BY registrant_id
		ON CONFLICT (registrant_id) DO UPDATE SET name = EXCLUDED.name, last_seen = now()`); err != nil {
		return fmt.Errorf("firms: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO twoai_pol_lobbyists (lobbyist_id, uid, name)
		SELECT DISTINCT ON (id) id, encode(substring(sha256(('lda-lobbyist:' || id)::bytea) from 1 for 4), 'hex'), nm
		FROM (SELECT (l->'lobbyist'->>'id')::bigint AS id,
		             initcap(trim(coalesce(l->'lobbyist'->>'first_name','') || ' ' || coalesce(l->'lobbyist'->>'last_name',''))) AS nm,
		             f.dt_posted
		      FROM twoai_pol_lobbying f, jsonb_array_elements(f.raw->'lobbying_activities') a, jsonb_array_elements(a->'lobbyists') l
		      WHERE f.retired_reason IS NULL AND a->>'description' ~* '`+polAISQL+`' AND (l->'lobbyist'->>'id') ~ '^[0-9]+$') x
		WHERE nm <> ''
		ORDER BY id, dt_posted DESC NULLS LAST
		ON CONFLICT (lobbyist_id) DO UPDATE SET name = EXCLUDED.name, last_seen = now()`); err != nil {
		return fmt.Errorf("lobbyists: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO twoai_pol_filing_lobbyists (filing_uuid, lobbyist_id, covered_position, is_new)
		SELECT DISTINCT ON (f.filing_uuid, (l->'lobbyist'->>'id')::bigint) f.filing_uuid, (l->'lobbyist'->>'id')::bigint,
		       NULLIF(trim(l->>'covered_position'), ''), (l->>'new')::boolean
		FROM twoai_pol_lobbying f, jsonb_array_elements(f.raw->'lobbying_activities') a, jsonb_array_elements(a->'lobbyists') l
		WHERE f.retired_reason IS NULL AND a->>'description' ~* '`+polAISQL+`' AND (l->'lobbyist'->>'id') ~ '^[0-9]+$'
		  AND EXISTS (SELECT 1 FROM twoai_pol_lobbyists p WHERE p.lobbyist_id = (l->'lobbyist'->>'id')::bigint)
		ON CONFLICT (filing_uuid, lobbyist_id) DO NOTHING`); err != nil {
		return fmt.Errorf("filing lobbyists: %w", err)
	}

	// Lookups for cross references.
	company := map[int64]string{} // client_id -> page href, a company page winning over an operator page
	if r, err := db.Query(`SELECT client_id, company_uid, target_kind FROM twoai_pol_client_matches
		WHERE status='confirmed' ORDER BY (target_kind = 'company')`); err == nil {
		for r.Next() {
			var id int64
			var u, kind string
			if r.Scan(&id, &u, &kind) == nil {
				if kind == "dc_operator" {
					company[id] = "/ai-ecosystem/technology-and-core-infrastructure/" + u + "/"
				} else {
					company[id] = "/companies/" + u + "/"
				}
			}
		}
		r.Close()
	}
	firmUID := map[int64]string{}
	if r, err := db.Query(`SELECT registrant_id, uid FROM twoai_pol_firms`); err == nil {
		for r.Next() {
			var id int64
			var u string
			if r.Scan(&id, &u) == nil {
				firmUID[id] = u
			}
		}
		r.Close()
	}
	billUID := map[string]string{} // LegiScan bill number -> bill uid
	if r, err := db.Query(`SELECT bill_number, uid FROM twoai_pol_bills WHERE session_name = '119th Congress'`); err == nil {
		for r.Next() {
			var n, u string
			if r.Scan(&n, &u) == nil {
				billUID[n] = u
			}
		}
		r.Close()
	}
	memberPage := map[int]string{} // people_id -> member page uid, when a timeline exists
	if r, err := db.Query(`SELECT (regexp_match(path, 'pol-member-([0-9a-f]+)'))[1], m.people_id
		FROM twoai_pages p JOIN twoai_pol_members m ON p.path = 'industries/pol-member-' || m.uid || '.json'`); err == nil {
		for r.Next() {
			var u string
			var pid int
			if r.Scan(&u, &pid) == nil {
				memberPage[pid] = u
			}
		}
		r.Close()
	}
	lobbyistPerson := map[int64]string{}
	if r, err := db.Query(`SELECT lobbyist_id, person_href FROM twoai_pol_lobbyist_people WHERE status='confirmed'`); err == nil {
		for r.Next() {
			var id int64
			var h string
			if r.Scan(&id, &h) == nil {
				lobbyistPerson[id] = h
			}
		}
		r.Close()
	}
	clientHref := func(id int64, fallback string) string {
		if h, ok := company[id]; ok {
			return h
		}
		return fallback
	}

	// 2. Bill pages.
	type bill struct {
		id                         int
		uid, num, title, src, date string
		preempt                    bool
		issues                     []string
	}
	var bills []bill
	if r, err := db.Query(`SELECT bill_id, uid, bill_number, title, COALESCE(NULLIF(state_link,''), url), COALESCE(status_date::text,''), preemption, issues
		FROM twoai_pol_bills WHERE retired_reason IS NULL`); err == nil {
		for r.Next() {
			var b bill
			if r.Scan(&b.id, &b.uid, &b.num, &b.title, &b.src, &b.date, &b.preempt, pq.Array(&b.issues)) == nil {
				bills = append(bills, b)
			}
		}
		r.Close()
	}
	billPages := 0
	var billIndex []polPt
	for _, b := range bills {
		raw := b.num
		b.num = polBillLabel(b.num)
		var pts []polPt
		pts = append(pts, polPt{Name: "The bill on congress.gov", Desc: "Full text, actions and cosponsors from the official record.", Source: b.src})
		// A bill about data centers and power sits beside the state and local law
		// that decides where AI compute may be built.
		for _, is := range b.issues {
			if is == "energy-and-data-centers" {
				pts = append(pts, polPt{Name: "Data center siting and power law", Desc: "The state and local rules on where AI compute may be built and who pays for its power, on this site.", Source: "/ai-compliance/datacenter-siting-and-power/"})
				break
			}
		}
		if r, err := db.Query(`SELECT m.people_id, m.name, COALESCE(m.party,''), s.sponsor_type_id FROM twoai_pol_sponsorships s
			JOIN twoai_pol_members m ON m.people_id = s.people_id WHERE s.bill_id = $1 ORDER BY s.sponsor_type_id, s.sponsor_order`, b.id); err == nil {
			for r.Next() {
				var pid, st int
				var name, party string
				if r.Scan(&pid, &name, &party, &st) == nil {
					role := "Cosponsor"
					if st == 1 {
						role = "Sponsor"
					}
					src := b.src
					if u, ok := memberPage[pid]; ok {
						src = polBase + u + "/"
					}
					pts = append(pts, polPt{Name: role + ": " + name + strings.TrimSuffix(" ("+party+")", " ()"), Desc: role + " of " + b.num + " as recorded by LegiScan.", Source: src})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT COALESCE(vote_date::text,''), COALESCE(description,''), yea, nay, passed, chamber FROM twoai_pol_rollcalls WHERE bill_id=$1 ORDER BY vote_date`, b.id); err == nil {
			for r.Next() {
				var d, desc, ch string
				var y, n int
				var passed bool
				if r.Scan(&d, &desc, &y, &n, &passed, &ch) == nil {
					res := "failed"
					if passed {
						res = "passed"
					}
					pts = append(pts, polPt{Name: d + " · Roll call, " + res, Desc: fmt.Sprintf("%s %d yea, %d nay. %s", ch, y, n, desc), Source: b.src})
				}
			}
			r.Close()
		}
		lobbyN := 0
		if r, err := db.Query(`SELECT l.client_id, (array_agg(l.client_name ORDER BY l.dt_posted DESC))[1], count(*),
				(array_agg(l.document_url ORDER BY l.dt_posted DESC))[1], array_agg(DISTINCT l.registrant_id)
			FROM twoai_pol_lobbying l, unnest(l.bill_refs) r
			WHERE l.retired_reason IS NULL AND l.filing_year >= 2025 AND `+polBillKeySQL+` = $1
			GROUP BY l.client_id ORDER BY count(*) DESC`, raw); err == nil {
			for r.Next() {
				var cid int64
				var cname, url string
				var n int
				var regs []int64
				if r.Scan(&cid, &cname, &n, &url, pq.Array(&regs)) == nil {
					lobbyN += n
					names := []string{}
					for _, rid := range regs {
						if u, ok := firmUID[rid]; ok {
							names = append(names, "firm uid "+u)
						}
					}
					pts = append(pts, polPt{Name: "Lobbied on by " + cname, Desc: fmt.Sprintf("%d filing(s) naming %s. Filed by %s.", n, b.num, strings.Join(names, ", ")), Source: clientHref(cid, url)})
				}
			}
			r.Close()
		}
		issues := strings.ReplaceAll(strings.Join(b.issues, ", "), "-", " ")
		sum := fmt.Sprintf("%s, %s. Latest status date %s. Issues tagged from the bill's own text: %s.", b.num, b.title, b.date, strings.TrimSuffix(issues+"", ""))
		if issues == "" {
			sum = fmt.Sprintf("%s, %s. Latest status date %s.", b.num, b.title, b.date)
		}
		if b.preempt {
			sum += " Its text uses preemption language about state law."
		}
		sum += fmt.Sprintf(" %d lobbying filing(s) for 2025 or later name it.", lobbyN)
		// Stories in this site's news archive that name this bill, matched by
		// twoai_news_link on the bill's exact title or its number as reported.
		if news := twoaiNewsLinksFor(db, "bill", b.uid, 12); len(news) > 0 {
			pts = append(pts, news...)
			sum += fmt.Sprintf(" %d news story or stories in this archive name it.", len(news))
		}
		doc := polPage("pol-bill", b.uid, b.num+": "+b.title, sum,
			"Sponsors, recorded votes and the lobbying filings that name this bill, each linked to its source. A filing that names a bill shows the bill was an issue in the lobbying, not the position taken on it.",
			today, pts, nil)
		if polPutPage(db, "industries/pol-bill-"+b.uid+".json", "pol-bills", doc) == nil {
			billPages++
			billIndex = append(billIndex, polPt{Name: b.num + ": " + b.title, Desc: fmt.Sprintf("Latest status %s. %d lobbying filing(s) name it. uid %s.", b.date, lobbyN, b.uid), Source: polBase + b.uid + "/"})
		}
	}

	billHrefs := func(refs []string, year int) []string {
		out := []string{}
		for _, r := range refs {
			p := strings.SplitN(r, " ", 2)
			if len(p) != 2 || year < 2025 {
				continue
			}
			k := map[string]string{"HR": "HB", "S": "SB", "HRES": "HR", "SRES": "SR"}[p[0]]
			if u, ok := billUID[k+p[1]]; ok {
				out = append(out, u)
			}
		}
		return out
	}

	// 3. Firm pages.
	type firm struct {
		id        int64
		uid, name string
	}
	var firms []firm
	if r, err := db.Query(`SELECT registrant_id, uid, name FROM twoai_pol_firms WHERE retired_reason IS NULL`); err == nil {
		for r.Next() {
			var f firm
			if r.Scan(&f.id, &f.uid, &f.name) == nil {
				firms = append(firms, f)
			}
		}
		r.Close()
	}
	firmPages := 0
	type idxRow struct {
		pt polPt
		n  int
	}
	var firmIdx []idxRow
	for _, f := range firms {
		var pts []polPt
		total := 0
		if r, err := db.Query(`SELECT client_id, (array_agg(client_name ORDER BY dt_posted DESC))[1], count(*), (array_agg(document_url ORDER BY dt_posted DESC))[1]
			FROM twoai_pol_lobbying WHERE registrant_id = $1 AND retired_reason IS NULL GROUP BY client_id ORDER BY count(*) DESC`, f.id); err == nil {
			for r.Next() {
				var cid int64
				var cname, url string
				var n int
				if r.Scan(&cid, &cname, &n, &url) == nil {
					total += n
					pts = append(pts, polPt{Name: "Client: " + cname, Desc: fmt.Sprintf("%d AI filing(s) for this client. LDA client id %d.", n, cid), Source: clientHref(cid, url)})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT p.lobbyist_id, p.uid, p.name, count(*) FROM twoai_pol_filing_lobbyists fl
			JOIN twoai_pol_lobbying l ON l.filing_uuid = fl.filing_uuid JOIN twoai_pol_lobbyists p ON p.lobbyist_id = fl.lobbyist_id
			WHERE l.registrant_id = $1 GROUP BY p.lobbyist_id, p.uid, p.name ORDER BY count(*) DESC`, f.id); err == nil {
			for r.Next() {
				var lid int64
				var u, name string
				var n int
				if r.Scan(&lid, &u, &name, &n) == nil {
					pts = append(pts, polPt{Name: "Lobbyist: " + name, Desc: fmt.Sprintf("Named on %d AI filing(s) by this firm.", n), Source: polBase + u + "/"})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT DISTINCT r, filing_year FROM twoai_pol_lobbying l, unnest(l.bill_refs) r WHERE registrant_id = $1 AND retired_reason IS NULL`, f.id); err == nil {
			seen := map[string]bool{}
			for r.Next() {
				var ref string
				var yr int
				if r.Scan(&ref, &yr) == nil {
					for _, u := range billHrefs([]string{ref}, yr) {
						if !seen[u] {
							seen[u] = true
							pts = append(pts, polPt{Name: "Bill named: " + ref, Desc: "Named in this firm's AI filings for 2025 or later. uid " + u + ".", Source: polBase + u + "/"})
						}
					}
				}
			}
			r.Close()
		}
		if total == 0 {
			continue
		}
		doc := polPage("pol-firm", f.uid, f.name+": AI lobbying",
			fmt.Sprintf("%s filed %d Lobbying Disclosure Act reports on AI held by this site. Its clients, the lobbyists named on those filings and the AI bills they name are below. LDA registrant id %d.", f.name, total, f.id),
			"Everything on this page comes from the firm's own filings. A firm that files for a client reports that it lobbied on an issue, not the position it argued.",
			today, pts, nil)
		if polPutPage(db, "industries/pol-firm-"+f.uid+".json", "pol-firms", doc) == nil {
			firmPages++
			firmIdx = append(firmIdx, idxRow{polPt{Name: f.name, Desc: fmt.Sprintf("%d AI filings. uid %s.", total, f.uid), Source: polBase + f.uid + "/"}, total})
		}
	}

	// 4. Lobbyist pages.
	type lob struct {
		id        int64
		uid, name string
	}
	var lobs []lob
	if r, err := db.Query(`SELECT lobbyist_id, uid, name FROM twoai_pol_lobbyists WHERE retired_reason IS NULL`); err == nil {
		for r.Next() {
			var l lob
			if r.Scan(&l.id, &l.uid, &l.name) == nil {
				lobs = append(lobs, l)
			}
		}
		r.Close()
	}
	lobPages := 0
	var lobIdx []idxRow
	for _, l := range lobs {
		var pts []polPt
		total := 0
		if h, ok := lobbyistPerson[l.id]; ok {
			pts = append(pts, polPt{Name: "Profile in the AI People Directory", Desc: "Identity confirmed by the editor, not matched by name.", Source: h})
		}
		if r, err := db.Query(`SELECT l.registrant_id, count(*) FROM twoai_pol_filing_lobbyists fl JOIN twoai_pol_lobbying l ON l.filing_uuid = fl.filing_uuid
			WHERE fl.lobbyist_id = $1 GROUP BY l.registrant_id ORDER BY count(*) DESC`, l.id); err == nil {
			for r.Next() {
				var rid int64
				var n int
				if r.Scan(&rid, &n) == nil {
					total += n
					if u, ok := firmUID[rid]; ok {
						pts = append(pts, polPt{Name: "Filed through a lobbying firm", Desc: fmt.Sprintf("%d AI filing(s). Firm uid %s.", n, u), Source: polBase + u + "/"})
					}
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT l.client_id, (array_agg(l.client_name ORDER BY l.dt_posted DESC))[1], count(*), (array_agg(l.document_url ORDER BY l.dt_posted DESC))[1]
			FROM twoai_pol_filing_lobbyists fl JOIN twoai_pol_lobbying l ON l.filing_uuid = fl.filing_uuid
			WHERE fl.lobbyist_id = $1 GROUP BY l.client_id ORDER BY count(*) DESC`, l.id); err == nil {
			for r.Next() {
				var cid int64
				var cname, url string
				var n int
				if r.Scan(&cid, &cname, &n, &url) == nil {
					pts = append(pts, polPt{Name: "Client: " + cname, Desc: fmt.Sprintf("Named on %d AI filing(s) for this client.", n), Source: clientHref(cid, url)})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT DISTINCT fl.covered_position, (array_agg(l.document_url ORDER BY l.dt_posted DESC))[1]
			FROM twoai_pol_filing_lobbyists fl JOIN twoai_pol_lobbying l ON l.filing_uuid = fl.filing_uuid
			WHERE fl.lobbyist_id = $1 AND fl.covered_position IS NOT NULL GROUP BY fl.covered_position`, l.id); err == nil {
			for r.Next() {
				var cp, url string
				if r.Scan(&cp, &url) == nil {
					pts = append(pts, polPt{Name: "Prior government position, as disclosed", Desc: cp + ". The Lobbying Disclosure Act requires filers to list covered positions held in the prior twenty years.", Source: url})
				}
			}
			r.Close()
		}
		if r, err := db.Query(`SELECT DISTINCT r, l.filing_year FROM twoai_pol_filing_lobbyists fl JOIN twoai_pol_lobbying l ON l.filing_uuid = fl.filing_uuid, unnest(l.bill_refs) r
			WHERE fl.lobbyist_id = $1`, l.id); err == nil {
			seen := map[string]bool{}
			for r.Next() {
				var ref string
				var yr int
				if r.Scan(&ref, &yr) == nil {
					for _, u := range billHrefs([]string{ref}, yr) {
						if !seen[u] {
							seen[u] = true
							pts = append(pts, polPt{Name: "Bill named: " + ref, Desc: "Named in an AI filing listing this lobbyist, for 2025 or later. uid " + u + ".", Source: polBase + u + "/"})
						}
					}
				}
			}
			r.Close()
		}
		if total == 0 {
			continue
		}
		doc := polPage("pol-lobbyist", l.uid, l.name+": AI lobbying record",
			fmt.Sprintf("%s is named as a lobbyist on %d Lobbying Disclosure Act filing(s) about AI held by this site. The firms, clients and AI bills on those filings are below. LDA lobbyist id %d. A different lobbyist with the same name has a separate page.", l.name, total, l.id),
			"This page lists what the filings disclose and nothing else. It draws no conclusion about the person.",
			today, pts, nil)
		if polPutPage(db, "industries/pol-lobbyist-"+l.uid+".json", "pol-lobbyists", doc) == nil {
			lobPages++
			lobIdx = append(lobIdx, idxRow{polPt{Name: l.name, Desc: fmt.Sprintf("Named on %d AI filings. uid %s.", total, l.uid), Source: polBase + l.uid + "/"}, total})
		}
	}

	// 5. Index sections, largest first, capped so each index stays readable.
	top := func(rows []idxRow) []polPt {
		for i := 1; i < len(rows); i++ {
			for j := i; j > 0 && rows[j].n > rows[j-1].n; j-- {
				rows[j], rows[j-1] = rows[j-1], rows[j]
			}
		}
		out := []polPt{}
		for i, r := range rows {
			if i >= polDirectoryList {
				break
			}
			out = append(out, r.pt)
		}
		return out
	}
	polPutPage(db, "industries/pol-firms.json", "pol-firms", polPage("pol-firms", polFirmsUID, "Lobbying Firms on AI",
		fmt.Sprintf("%d firms and in-house teams have filed AI lobbying reports held by this site. The %d with the most filings are listed; each page lists its clients, lobbyists and the AI bills named.", firmPages, min(firmPages, polDirectoryList)),
		"Firms are identified by their LDA registrant id. A company that lobbies for itself appears here as its own registrant.", today, top(firmIdx), nil))
	polPutPage(db, "industries/pol-lobbyists.json", "pol-lobbyists", polPage("pol-lobbyists", polLobbyistsUID, "Lobbyists on AI",
		fmt.Sprintf("%d registered lobbyists are named on AI lobbying filings held by this site. The %d named most often are listed; every lobbyist has a page reachable from their firm.", lobPages, min(lobPages, polDirectoryList)),
		"Lobbyists are identified by their LDA lobbyist id, never by name. A lobbyist is linked to an AI People profile only when the editor has confirmed the identity.", today, top(lobIdx), nil))
	polPutPage(db, "industries/pol-bills.json", "pol-bills", polPage("pol-bills", polBillsUID, "AI Bills in Congress",
		fmt.Sprintf("%d AI bills in the 119th Congress, each with its sponsors, recorded votes and the lobbying filings that name it.", billPages),
		"Bills are those whose own title or description is about AI, from LegiScan's record of Congress.", today, billIndex, nil))
	for slug, uid := range map[string]string{"pol-firms": polFirmsUID, "pol-lobbyists": polLobbyistsUID, "pol-bills": polBillsUID} {
		db.Exec(`UPDATE twoai_taxonomy SET live_path = $2, updated_at = now() WHERE slug = $1 AND live_path IS NULL`, slug, polBase+uid+"/")
	}
	fmt.Printf("twoai_politics_directory: bills=%d firms=%d lobbyists=%d pages written ok=true\n", billPages, firmPages, lobPages)
	return nil
}
