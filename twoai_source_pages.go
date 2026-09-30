package main

// twoai_source_pages: a page of our own for every source the Industry Use
// Cases section cites. Stephen, 2026-09-25: "i want a web page for each source
// where we have read the contents of the source and do a page of summary of
// the content we find. Put the link to the source at the very bottom of the
// page - let ollama do all the work."
//
// The industry pages already cite 156 outside sources, and twoai_harvest
// fetches each one daily into twoai_source_harvest. Until now the only use
// of that text was a one-paragraph point brief inline on the industry page.
// This stage turns each harvested source into its own page: what the source
// is, what it says, the figures it gives, what it means for AI in that
// industry, and its limits, written by Ollama from the harvested text only,
// with the link to the source at the very bottom. A source that could not be
// fetched, or gave under 500 characters, gets no page, because a summary of
// nothing is the thin page this site does not publish.
//
// Pages are art-topic documents, the same shape the profession and SQL
// sections use, so the template needs nothing new except the source line;
// the uid is twoaiUID("source:" + url), minted once and never moved. A page
// is written once and rewritten only when the harvested content's hash
// changes, so a source that never changes costs one model call ever. After
// the pages are written the industry documents get a reading_path on each
// point, so the industry page links to our summary beside the source link.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const twoaiSourcePagesPerRun = 12

// sourceErrorPage matches the opening of a harvested page that is an error,
// not found, access or JavaScript wall rather than content.
var sourceErrorPage = regexp.MustCompile(`(?i)(page (you (are|were) looking for )?(could|can)(not| ?n.t) be found|page not found|404 (error|not found)|this page (does not|doesn.t) exist|access denied|you don.t have permission|enable javascript|please verify you are a human|are you a robot|request blocked)`)

func twoaiSourcePages(db *sql.DB, today string) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_source_pages (
		url text PRIMARY KEY,
		uid text NOT NULL,
		industry_slug text NOT NULL,
		industry_name text NOT NULL,
		point_name text NOT NULL,
		content_hash text,
		doc jsonb,
		model text,
		written_on date,
		error text)`); err != nil {
		return 0, err
	}
	const base = "/ai-ecosystem/enterprise-applications-governance-and-tools/"

	// Every cited source with a usable harvest, and what we already have.
	rows, err := db.Query(`SELECT i.slug, i.name, p->>'name', COALESCE(p->>'desc',''), p->>'source',
			COALESCE(h.extract,''), COALESCE(h.content_hash,''), COALESCE(h.http_status,0),
			COALESCE(sp.content_hash,''), sp.doc IS NOT NULL
		FROM twoai_industries i, jsonb_array_elements(i.points) p
		LEFT JOIN twoai_source_harvest h ON h.url = p->>'source'
		LEFT JOIN twoai_source_pages sp ON sp.url = p->>'source'
		WHERE p->>'source' LIKE 'http%'
		ORDER BY i.slug, p->>'name'`)
	if err != nil {
		return 0, err
	}
	type job struct {
		slug, industry, name, desc, url, extract, hash, oldHash string
		status                                                  int
		hasDoc                                                  bool
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.slug, &j.industry, &j.name, &j.desc, &j.url, &j.extract, &j.hash, &j.status, &j.oldHash, &j.hasDoc); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()

	// WITHDRAWALS, 2026-09-29. Stephen found a published page headed "AICPA &
	// CIMA AI topic page: an error page, not guidance". The prompt had told
	// the model to say plainly when a source was a thin landing page, and the
	// result was published: 15 pages about error pages, wrong sites, pages on
	// this site, and homepages with no AI content, and about 90 more
	// describing landing pages. A summary is now published only when the
	// source has substance a reader following AI in the industry would learn
	// from; everything else is withdrawn, with the reason kept here.
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_source_withdrawals (uid text PRIMARY KEY, url text,
		reason text NOT NULL, withdrawn_on date NOT NULL DEFAULT current_date, withdrawn_by text NOT NULL)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_source_reviews (uid text PRIMARY KEY, publishable boolean NOT NULL,
		reason text, reviewed_on date NOT NULL DEFAULT current_date, model text)`)
	withdrawn := map[string]bool{}
	// A site-crawl verdict is not a permanent withdrawal: the site is read
	// again every 90 days and the digest decides afresh each time.
	if wr, werr := db.Query(`SELECT uid FROM twoai_source_withdrawals WHERE withdrawn_by <> 'site-crawl'`); werr == nil {
		for wr.Next() {
			var u string
			if wr.Scan(&u) == nil {
				withdrawn[u] = true
			}
		}
		wr.Close()
	}
	withdraw := func(uid, url, reason, by string) {
		db.Exec(`INSERT INTO twoai_source_withdrawals (uid, url, reason, withdrawn_by) VALUES ($1,$2,$3,$4) ON CONFLICT (uid) DO NOTHING`, uid, url, reason, by)
		withdrawn[uid] = true
	}

	// Industry page paths, for the crumb back and the sibling list.
	sectionPath := map[string]string{}
	spr, err := db.Query(`SELECT taxonomy_slug, data->>'uid' FROM twoai_pages WHERE path LIKE 'industries/industry-%' AND data->>'shape' = 'tech-section'`)
	if err == nil {
		for spr.Next() {
			var s, u string
			if spr.Scan(&s, &u) == nil && u != "" {
				sectionPath[s] = base + u + "/"
			}
		}
		spr.Close()
	}

	// READ THE WHOLE SITE FIRST, 2026-09-29. Stephen: crawl it, digest all
	// it has to offer, and only then make a page. Crawling and digesting run
	// here, a few sites a run; a page is written only from a finished digest.
	cited := map[string][]string{}
	industryOf := map[string]string{}
	for _, j := range jobs {
		d := crawlHost(j.url)
		if d == "" {
			continue
		}
		cited[d] = append(cited[d], j.url)
		if industryOf[d] == "" {
			industryOf[d] = j.industry
		}
	}
	twoaiSiteCrawlStep(db, cited, industryOf)

	written, skipped, unusable := 0, 0, 0
	for _, j := range jobs {
		// The cited page alone no longer decides anything: a broken or thin
		// cited address on a site with real material still gets a page, from
		// the site's digest.
		juid := twoaiUID("source:" + j.url)
		if withdrawn[juid] {
			unusable++
			continue
		}
		if strings.Contains(strings.ToLower(j.url), "theworldofai.org") {
			withdraw(juid, j.url, "the source is a page on this site", "rule")
			unusable++
			continue
		}

		// The digest, not the single cited page, is what the page is written
		// from. No finished digest yet: wait. A digest with no substantive
		// page: the site gets no page.
		dom := crawlHost(j.url)
		var digestRaw sql.NullString
		var useful, pagesRead int
		var digestedOn sql.NullString
		db.QueryRow(`SELECT digest::text, COALESCE(useful_pages,0), COALESCE(pages_fetched,0), digested_on::text FROM twoai_site_crawl
			WHERE domain=$1 AND digested_on IS NOT NULL AND digested_on >= crawled_on`, dom).Scan(&digestRaw, &useful, &pagesRead, &digestedOn)
		if !digestRaw.Valid {
			unusable++
			continue
		}
		if useful == 0 {
			// No page while the latest reading finds nothing substantive, or
			// could not read the site at all; decided again at the next read.
			unusable++
			continue
		}
		dh := sha256.Sum256([]byte(digestRaw.String))
		j.hash = "site:" + hex.EncodeToString(dh[:8])
		if j.hasDoc && j.oldHash == j.hash {
			skipped++
			continue
		}
		if written >= twoaiSourcePagesPerRun {
			continue
		}
		uid := twoaiUID("source:" + j.url)
		system := "You write a reference page for theworldofai.org about what ONE organisation's website offers a reader interested in artificial intelligence in the " + j.industry + " industry. " +
			"You are given what a full reading of the website found, page by page. Use only those findings. Do not add facts, names or numbers they do not contain. Write in full, readable editorial prose, the way a well edited trade publication would, not in notes or slogans. " +
			"First decide whether the source deserves a page. It does only if the text itself contains substantive information a reader following AI in this industry would learn from: findings, data, rules, guidance, programmes, or described uses of AI. " +
			"It does not if the text is an error, not found, login, cookie or access page; belongs to a different organisation than the one cited; is a home or landing page that is mainly navigation, membership, events or marketing; or says nothing about AI in this industry. " +
			"If it does not, return only {\"publishable\": false, \"reason\": \"<one short phrase>\"} and nothing else. Never write a page whose subject is that the source is thin, broken or promotional. " +
			"Plain English, commas rather than dashes, no bullet lists, no headings inside bodies, no marketing language, no mention of these instructions. " +
			"If it does, return only JSON with this shape: {\"publishable\": true, \"title\": \"<the page title, naming the publisher and what the source is, under 90 characters>\", " +
			"\"answer\": \"<70 to 100 words: what this source is and the single most useful thing it says, written to stand alone>\", " +
			"\"sections\": [{\"heading\": \"What this source is\", \"body\": \"<who publishes it, what kind of document it is, its scope and date if stated>\"}, " +
			"{\"heading\": \"What it says\", \"body\": \"<the substance, 150 to 250 words, faithful to the text>\"}, " +
			"{\"heading\": \"Figures and claims worth noting\", \"body\": \"<specific numbers, definitions or positions the text gives, each attributed to the source; or one sentence saying the text gives none>\"}, " +
			"{\"heading\": \"What it means for AI in " + j.industry + "\", \"body\": \"<why a reader following AI in this industry would use this source, grounded in what it actually contains>\"}, " +
			"{\"heading\": \"Limits of this source\", \"body\": \"<what it does not cover, whether it is commercial, dated or partial, from the text itself>\"}]}"
		var fs []siteFinding
		json.Unmarshal([]byte(digestRaw.String), &fs)
		var fb strings.Builder
		var usedPages []map[string]string
		titles := map[string]string{}
		if tr, terr := db.Query(`SELECT url, COALESCE(title,'') FROM twoai_site_crawl_pages WHERE domain=$1`, dom); terr == nil {
			for tr.Next() {
				var u, t string
				if tr.Scan(&u, &t) == nil {
					titles[u] = t
				}
			}
			tr.Close()
		}
		for _, f := range fs {
			if !f.Useful || len(f.Facts) == 0 {
				continue
			}
			fmt.Fprintf(&fb, "PAGE %s (%s%s): %s\n", f.URL, f.Kind, map[bool]string{true: ", " + f.Date, false: ""}[f.Date != ""], f.What)
			for _, x := range f.Facts {
				fmt.Fprintf(&fb, "  - %s\n", x)
			}
			usedPages = append(usedPages, map[string]string{"url": f.URL, "title": titles[f.URL], "date": f.Date})
		}
		user := fmt.Sprintf("Industry: %s\nThe point on our industry page that cites this organisation: %s. %s\nWebsite: %s, %d pages read on %s.\n\nWhat the website's pages say about AI, page by page:\n\n%s",
			j.industry, j.name, j.desc, dom, pagesRead, digestedOn.String, trunc(fb.String(), 14000))
		out, model, gerr := twoaiGenerate("twoai_source_pages", system, user)
		if gerr != nil {
			db.Exec(`INSERT INTO twoai_source_pages (url, uid, industry_slug, industry_name, point_name, error)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (url) DO UPDATE SET error=$6`, j.url, uid, j.slug, j.industry, j.name, gerr.Error())
			continue
		}
		s := out
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if k := strings.LastIndex(s, "}"); k >= 0 {
			s = s[:k+1]
		}
		var m struct {
			Publishable *bool  `json:"publishable"`
			Reason      string `json:"reason"`
			Title       string `json:"title"`
			Answer      string `json:"answer"`
			Sections    []struct {
				Heading string `json:"heading"`
				Body    string `json:"body"`
			} `json:"sections"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(s)), &m) == nil && m.Publishable != nil && !*m.Publishable {
			withdraw(uid, j.url, "not substantive: "+strings.TrimSpace(m.Reason), "model")
			db.Exec(`INSERT INTO twoai_source_reviews (uid, publishable, reason, model) VALUES ($1,false,$2,$3)
				ON CONFLICT (uid) DO UPDATE SET publishable=false, reason=$2, reviewed_on=current_date, model=$3`, uid, m.Reason, model)
			unusable++
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil || strings.TrimSpace(m.Answer) == "" || len(m.Sections) < 3 {
			db.Exec(`INSERT INTO twoai_source_pages (url, uid, industry_slug, industry_name, point_name, error)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (url) DO UPDATE SET error=$6`, j.url, uid, j.slug, j.industry, j.name, "model output was not the expected JSON")
			continue
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = j.name
		}
		var sections []map[string]string
		for _, sec := range m.Sections {
			if strings.TrimSpace(sec.Body) == "" {
				continue
			}
			sections = append(sections, map[string]string{"heading": strings.TrimSpace(sec.Heading), "body": strings.TrimSpace(sec.Body)})
		}
		doc := map[string]any{
			"uid": uid, "page_uid": uid, "shape": "art-topic", "slug": "source-" + uid,
			"name": title, "title": title, "answer": strings.TrimSpace(m.Answer),
			"sections":    sections,
			"category":    "enterprise-applications-governance-and-tools",
			"hub_name":    "Industry Use Cases",
			"parent_name": j.industry, "parent_path": sectionPath[j.slug],
			"crumbs":     []map[string]string{{"name": "Industry Use Cases", "path": base + twoaiUID("section:industry-use-cases") + "/"}, {"name": j.industry, "path": sectionPath[j.slug]}},
			"source_url": j.url, "source_name": j.name, "source_read_on": digestedOn.String,
			"built_from": "site-crawl", "site_domain": dom, "site_pages_read": pagesRead, "site_pages": usedPages,
			"kind": "source-summary", "industry_slug": j.slug,
			"generated": today, "built_at": time.Now().Format(time.RFC3339),
			"refresh_every_days": 90, "noindex": false, "expanded": true,
		}
		raw, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_source_pages (url, uid, industry_slug, industry_name, point_name, content_hash, doc, model, written_on, error)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,current_date,NULL)
			ON CONFLICT (url) DO UPDATE SET content_hash=$6, doc=$7::jsonb, model=$8, written_on=current_date, error=NULL`,
			j.url, uid, j.slug, j.industry, j.name, j.hash, string(raw), model); err != nil {
			return written, err
		}
		db.Exec(`INSERT INTO twoai_source_reviews (uid, publishable, reason, model) VALUES ($1,true,'written under the substance test',$2)
			ON CONFLICT (uid) DO UPDATE SET publishable=true, reason='written under the substance test', reviewed_on=current_date, model=$2`, uid, model)
		written++
		fmt.Printf("twoai_source_pages: wrote %s (%s) for %s via %s\n", uid, trunc(title, 60), j.industry, model)
		time.Sleep(300 * time.Millisecond)
	}

	// REVIEW OF PAGES WRITTEN BEFORE THE SUBSTANCE TEST, 20 a run. Each
	// existing page's source text is put to the same test; a page that fails
	// is withdrawn.
	type rv struct{ uid, url, industry, extract string }
	var rvs []rv
	if rr, rerr := db.Query(`SELECT sp.uid, sp.url, sp.industry_name, COALESCE(h.extract,'')
			FROM twoai_source_pages sp LEFT JOIN twoai_source_harvest h ON h.url = sp.url
			WHERE sp.doc IS NOT NULL AND sp.doc->>'built_from' = 'site-crawl'
			  AND NOT EXISTS (SELECT 1 FROM twoai_source_reviews r WHERE r.uid = sp.uid)
			  AND NOT EXISTS (SELECT 1 FROM twoai_source_withdrawals w WHERE w.uid = sp.uid)
			ORDER BY sp.uid LIMIT 20`); rerr == nil {
		for rr.Next() {
			var r rv
			if rr.Scan(&r.uid, &r.url, &r.industry, &r.extract) == nil {
				rvs = append(rvs, r)
			}
		}
		rr.Close()
	}
	reviewedOut := 0
	for _, r := range rvs {
		sys := "You decide whether an outside web page deserves its own summary page on theworldofai.org, for a reader following AI in the " + r.industry + " industry. " +
			"It does only if the text itself contains substantive information that reader would learn from: findings, data, rules, guidance, programmes, or described uses of AI. " +
			"It does not if the text is an error, not found, login, cookie or access page; belongs to a different organisation than the address suggests; is a home or landing page that is mainly navigation, membership, events or marketing; or says nothing about AI in this industry. " +
			`Return only JSON: {"publishable": true or false, "reason": "<one short phrase>"}`
		out, model, gerr := twoaiGenerate("twoai_source_pages", sys, "Address: "+r.url+"\n\nText:\n"+trunc(r.extract, 6000))
		if gerr != nil {
			continue
		}
		o := out
		if i := strings.Index(o, "{"); i >= 0 {
			o = o[i:]
		}
		if k := strings.LastIndex(o, "}"); k >= 0 {
			o = o[:k+1]
		}
		var v struct {
			Publishable *bool  `json:"publishable"`
			Reason      string `json:"reason"`
		}
		if json.Unmarshal([]byte(o), &v) != nil || v.Publishable == nil {
			continue
		}
		db.Exec(`INSERT INTO twoai_source_reviews (uid, publishable, reason, model) VALUES ($1,$2,$3,$4) ON CONFLICT (uid) DO NOTHING`, r.uid, *v.Publishable, v.Reason, model)
		if !*v.Publishable {
			withdraw(r.uid, r.url, "not substantive: "+strings.TrimSpace(v.Reason), "review")
			reviewedOut++
		}
	}
	if len(rvs) > 0 {
		fmt.Printf("twoai_source_pages: reviewed=%d withdrawn=%d\n", len(rvs), reviewedOut)
	}

	// Publish every written page, with siblings (the other summarised sources
	// in the same industry) filled in fresh each run so new pages appear on
	// old ones.
	sib, err := db.Query(`SELECT industry_slug, uid, doc->>'name' FROM twoai_source_pages sp WHERE doc IS NOT NULL
		AND doc->>'built_from' = 'site-crawl'
		AND NOT EXISTS (SELECT 1 FROM twoai_source_withdrawals w WHERE w.uid = sp.uid) ORDER BY industry_slug, doc->>'name'`)
	if err != nil {
		return written, err
	}
	siblings := map[string][]map[string]string{}
	for sib.Next() {
		var s, u, n string
		if sib.Scan(&s, &u, &n) == nil {
			siblings[s] = append(siblings[s], map[string]string{"name": n, "path": base + u + "/"})
		}
	}
	sib.Close()
	pr, err := db.Query(`SELECT url, uid, industry_slug, doc::text FROM twoai_source_pages WHERE doc IS NOT NULL`)
	if err != nil {
		return written, err
	}
	published := 0
	type pub struct{ url, uid, slug, raw string }
	var pubs []pub
	for pr.Next() {
		var p pub
		if pr.Scan(&p.url, &p.uid, &p.slug, &p.raw) == nil {
			pubs = append(pubs, p)
		}
	}
	pr.Close()
	for _, p := range pubs {
		var doc map[string]any
		if json.Unmarshal([]byte(p.raw), &doc) != nil {
			continue
		}
		if bf, _ := doc["built_from"].(string); bf != "site-crawl" && !withdrawn[p.uid] {
			// Written from one page before the whole-site rule; held back
			// until its site has been read, then rewritten.
			doc["noindex"] = true
			doc["redirect_to"] = sectionPath[p.slug]
			doc["held_until_site_read"] = true
			raw, _ := json.Marshal(doc)
			db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
				VALUES ($1, 'tech-section', $2, $3::jsonb, 1, now())
				ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()
				WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`,
				"industries/source-"+p.uid+".json", p.slug, string(raw))
			db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{points}', (
					SELECT jsonb_agg(CASE WHEN pt->>'reading_path' = $2 THEN pt - 'reading_path' ELSE pt END)
					FROM jsonb_array_elements(data->'points') pt)), updated_at = now()
				WHERE path = $1 AND EXISTS (SELECT 1 FROM jsonb_array_elements(data->'points') q WHERE q->>'reading_path' = $2)`,
				"industries/"+p.slug+".json", base+p.uid+"/")
			continue
		}
		if withdrawn[p.uid] {
			// The URL stays (published URLs never disappear), but it no longer
			// carries the summary, is kept out of search, and sends the reader
			// to the industry page; the industry point stops linking to it.
			ind, _ := doc["parent_name"].(string)
			wdoc := map[string]any{
				"uid": p.uid, "page_uid": p.uid, "shape": "art-topic", "slug": "source-" + p.uid,
				"name": ind + ": source reference", "title": ind + ": source reference",
				"answer":   "This page is no longer maintained. The sources we currently rely on for AI in " + ind + " are listed on the " + ind + " page.",
				"sections": []map[string]string{}, "category": "enterprise-applications-governance-and-tools",
				"hub_name": "Industry Use Cases", "parent_name": ind, "parent_path": sectionPath[p.slug],
				"crumbs": doc["crumbs"], "kind": "source-summary", "industry_slug": p.slug, "withdrawn": true,
				"redirect_to": sectionPath[p.slug], "noindex": true,
				"generated": today, "built_at": time.Now().Format(time.RFC3339), "refresh_every_days": 365, "archived": true,
			}
			raw, _ := json.Marshal(wdoc)
			db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
				VALUES ($1, 'tech-section', $2, $3::jsonb, 1, now())
				ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()
				WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`,
				"industries/source-"+p.uid+".json", p.slug, string(raw))
			db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{points}', (
					SELECT jsonb_agg(CASE WHEN pt->>'reading_path' = $2 THEN pt - 'reading_path' ELSE pt END)
					FROM jsonb_array_elements(data->'points') pt)), updated_at = now()
				WHERE path = $1 AND EXISTS (SELECT 1 FROM jsonb_array_elements(data->'points') q WHERE q->>'reading_path' = $2)`,
				"industries/"+p.slug+".json", base+p.uid+"/")
			continue
		}
		var sibs []map[string]string
		for _, s := range siblings[p.slug] {
			if s["path"] != base+p.uid+"/" {
				sibs = append(sibs, s)
			}
		}
		doc["siblings"] = sibs
		doc["parent_path"] = sectionPath[p.slug]
		raw, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
			VALUES ($1, 'tech-section', $2, $3::jsonb, 1, now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()
			WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`,
			"industries/source-"+p.uid+".json", p.slug, string(raw)); err != nil {
			return written, err
		}
		published++
		// The industry page links to our summary beside the source link.
		db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{points}', (
				SELECT jsonb_agg(CASE WHEN pt->>'source' = $2 THEN pt || jsonb_build_object('reading_path', $3::text) ELSE pt END)
				FROM jsonb_array_elements(data->'points') pt)), updated_at = now()
			WHERE path = $1 AND EXISTS (SELECT 1 FROM jsonb_array_elements(data->'points') q WHERE q->>'source' = $2 AND COALESCE(q->>'reading_path','') <> $3)`,
			"industries/"+p.slug+".json", p.url, base+p.uid+"/")
	}
	fmt.Printf("twoai_source_pages: written=%d skipped_unchanged=%d unusable=%d published=%d of %d cited sources\n",
		written, skipped, unusable, published, len(jobs))
	return published, nil
}
