package main

// twoai_lang_pages: a page of our own for every programming language and AI
// framework the site lists. Stephen, 2026-10-02: none of the languages on
// /ai-ecosystem/technology-and-core-infrastructure/6badc1e5/ have their own
// page; each language and framework gets one, and it must answer what a
// reader wants to know about it: what it is used for and where it is
// strongest, how hard it is to learn and what it needs first, its ecosystem
// and community, prototyping speed against production performance, industry
// adoption and careers, and how it fits beside the others.
//
// Subjects: the languages in tech/programming-languages.json and the
// frameworks in repos/ai-frameworks.json. Each has an official site. The
// whole-site rule applies (2026-09-29): the site is crawled and digested
// first, a page is written only from a finished digest plus the facts the
// site already holds (steward, first release, licence, stars, last push),
// and the official link sits at the very bottom. Written once per digest;
// rewritten only when the digest changes.
//
// Pages are tech documents of shape lang-profile under the Technology and
// Core Infrastructure category, uid twoaiUID("lang:"+slug) or
// twoaiUID("framework:"+owner/repo), minted once and never moved.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

const twoaiLangCrawlPerRun = 4
const twoaiLangWritePerRun = 6

// langSiteOverrides names the official site for a repository whose GitHub
// homepage field is wrong or too wide, keyed on owner/repo so it survives
// the daily repo refresh. scope is the crawl key: the host, with a path
// prefix when the host is a whole documentation estate and the subject is
// one part of it (crawlSplitScope).
//
// Semantic Kernel: Stephen, 2026-10-02, via theworldofai row 360. The GitHub
// homepage is https://aka.ms/semantic-kernel, a redirect, and the official
// site is the Semantic Kernel section of learn.microsoft.com.
var langSiteOverrides = map[string]struct{ site, scope string }{
	"microsoft/semantic-kernel": {"https://learn.microsoft.com/en-us/semantic-kernel/overview/", "learn.microsoft.com/en-us/semantic-kernel"},
}

// langScope turns a subject's site into its crawl key. For most sites that
// is the host. On a code host the host is everyone's: the 06:05 run of
// 2026-10-02 crawled sixty pages of github.com for exllamav2 (no homepage,
// so its repository stood in) and would have handed that digest to every
// other repository-only subject. There the key is host plus owner/repo, so
// the crawl stays inside the one repository (crawlSplitScope).
func langScope(site string) string {
	host := crawlHost(site)
	switch host {
	case "github.com", "gitlab.com", "codeberg.org", "huggingface.co", "bitbucket.org":
		if p, err := url.Parse(site); err == nil {
			parts := strings.Split(strings.Trim(p.Path, "/"), "/")
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				return host + "/" + parts[0] + "/" + parts[1]
			}
		}
	}
	return host
}

// langShortLinkHost says whether a host only ever redirects somewhere else.
func langShortLinkHost(h string) bool {
	switch h {
	case "aka.ms", "bit.ly", "t.co", "goo.gl", "tinyurl.com", "git.io", "lnkd.in", "":
		return true
	}
	return false
}

var langResolved = map[string]string{}

// langResolveSite follows redirects and returns the address a browser would
// land on, so the crawl is keyed on the real host. On any failure the
// address is returned as given.
func langResolveSite(u string) string {
	if v, ok := langResolved[u]; ok {
		return v
	}
	final := u
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", u, nil)
	if err == nil {
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org site reader; srj@srjconsultingservices.com)")
		if resp, err := client.Do(req); err == nil {
			if resp.Request != nil && resp.Request.URL != nil && resp.StatusCode < 400 {
				final = resp.Request.URL.String()
			}
			resp.Body.Close()
		}
	}
	langResolved[u] = final
	return final
}

type langSubject struct {
	key, uid, kind, name, slug, siteURL, domain string
	facts                                       map[string]any
	parentPath, parentName                      string
}

func twoaiLangSubjects(db *sql.DB) []langSubject {
	const base = "/ai-ecosystem/technology-and-core-infrastructure/"
	var out []langSubject
	var raw string
	if db.QueryRow(`SELECT data::text FROM twoai_pages WHERE path='tech/programming-languages.json'`).Scan(&raw) == nil {
		var d struct {
			UID       string `json:"uid"`
			Name      string `json:"name"`
			Languages []struct {
				Name         string   `json:"name"`
				Slug         string   `json:"slug"`
				AIRole       string   `json:"ai_role"`
				Steward      string   `json:"steward"`
				SourceURL    string   `json:"source_url"`
				FirstRelease string   `json:"first_release"`
				Verified     string   `json:"verified"`
				Repos        []string `json:"repos"`
			} `json:"languages"`
		}
		if json.Unmarshal([]byte(raw), &d) == nil {
			for _, l := range d.Languages {
				if l.Name == "" || l.SourceURL == "" {
					continue
				}
				slug := l.Slug
				if slug == "" {
					slug = strings.ToLower(strings.ReplaceAll(l.Name, " ", "-"))
				}
				out = append(out, langSubject{
					key: "lang:" + slug, uid: twoaiUID("lang:" + slug), kind: "language", name: l.Name, slug: slug,
					siteURL: l.SourceURL, domain: langScope(l.SourceURL),
					facts:      map[string]any{"ai_role": l.AIRole, "steward": l.Steward, "first_release": l.FirstRelease, "verified": l.Verified, "tracked_repos": l.Repos, "official_site": l.SourceURL},
					parentPath: base + d.UID + "/", parentName: d.Name,
				})
			}
		}
	}
	// Frameworks and inference engines: the two repository sections under the
	// same hub. Stephen, 2026-10-02: treat all three sections the same.
	for _, src := range []struct{ path, kind, prefix string }{
		{"repos/ai-frameworks.json", "framework", "framework:"},
		{"repos/inference-engines.json", "inference engine", "engine:"},
	} {
		if db.QueryRow(`SELECT data::text FROM twoai_pages WHERE path=$1`, src.path).Scan(&raw) != nil {
			continue
		}
		var d struct {
			UID   string `json:"uid"`
			Name  string `json:"name"`
			Repos []struct {
				Name        string `json:"name"`
				Repo        string `json:"repo"`
				URL         string `json:"url"`
				Homepage    string `json:"homepage"`
				Licence     string `json:"licence"`
				Language    string `json:"language"`
				Description string `json:"description"`
				Stars       int    `json:"stars"`
				Archived    bool   `json:"archived"`
				PushedAt    string `json:"pushed_at"`
			} `json:"repos"`
		}
		if json.Unmarshal([]byte(raw), &d) == nil {
			for _, r := range d.Repos {
				site := r.Homepage
				if site == "" {
					site = r.URL
				}
				if r.Name == "" || site == "" {
					continue
				}
				// Row 357: Semantic Kernel's homepage is https://aka.ms/semantic-kernel,
				// a redirect, and the crawl keyed the domain aka.ms and fetched
				// nothing. The redirect is followed first, and a homepage that
				// still resolves nowhere useful gives way to the repository.
				site = langResolveSite(site)
				if langShortLinkHost(crawlHost(site)) && r.URL != "" {
					site = r.URL
				}
				key := r.Repo
				if key == "" {
					key = r.Name
				}
				scope := langScope(site)
				if o, ok := langSiteOverrides[r.Repo]; ok {
					site, scope = o.site, o.scope
				}
				out = append(out, langSubject{
					key: src.prefix + key, uid: twoaiUID(src.prefix + key), kind: src.kind, name: r.Name, slug: strings.ToLower(strings.ReplaceAll(r.Name, "_", "-")),
					siteURL: site, domain: scope,
					facts:      map[string]any{"description": r.Description, "repo": r.Repo, "repo_url": r.URL, "licence": r.Licence, "language": r.Language, "stars": r.Stars, "archived": r.Archived, "last_push": r.PushedAt, "official_site": site},
					parentPath: base + d.UID + "/", parentName: d.Name,
				})
			}
		}
	}
	return out
}

func twoaiLangPages(db *sql.DB, today string) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_lang_pages (key text PRIMARY KEY, uid text NOT NULL, kind text NOT NULL,
		name text NOT NULL, domain text, content_hash text, doc jsonb, model text, written_on date, error text)`); err != nil {
		return 0, err
	}
	subjects := twoaiLangSubjects(db)
	if len(subjects) == 0 {
		return 0, nil
	}
	// Crawl and digest the official sites, a few a run, through the same
	// reader the source pages use. The domain is the unit: Hugging Face's
	// site serves transformers and peft with one reading.
	starts := map[string][]string{}
	topicOf := map[string]string{}
	for _, s := range subjects {
		if s.domain == "" {
			continue
		}
		starts[s.domain] = append(starts[s.domain], s.siteURL)
		if topicOf[s.domain] == "" {
			topicOf[s.domain] = "AI software development (the " + s.name + " " + s.kind + ")"
		} else if !strings.Contains(topicOf[s.domain], s.name) {
			topicOf[s.domain] = strings.TrimSuffix(topicOf[s.domain], ")") + ", " + s.name + ")"
		}
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_site_crawl (domain text PRIMARY KEY, start_url text,
		pages_fetched int, pages_relevant int, crawled_on date, digest jsonb, digested_on date, useful_pages int, model text)`)
	var domains []string
	for d := range starts {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	crawled, digested := 0, 0
	for _, d := range domains {
		if crawled >= twoaiLangCrawlPerRun {
			break
		}
		var last sql.NullTime
		db.QueryRow(`SELECT crawled_on FROM twoai_site_crawl WHERE domain=$1`, d).Scan(&last)
		if last.Valid && time.Since(last.Time) < twoaiCrawlRefreshDays*24*time.Hour {
			continue
		}
		f, r := twoaiCrawlSite(db, d, starts[d], false)
		fmt.Printf("twoai_lang_pages: crawled %s fetched=%d relevant=%d\n", d, f, r)
		crawled++
		// A site that yields nothing is not the subject's site for our
		// purposes. Every subject keyed on it that has a repository falls
		// back to the repository's host, so the profile can still be written.
		if f == 0 {
			for i := range subjects {
				if subjects[i].domain != d {
					continue
				}
				repo, _ := subjects[i].facts["repo_url"].(string)
				if repo == "" || langScope(repo) == d {
					continue
				}
				subjects[i].siteURL, subjects[i].domain = repo, langScope(repo)
				subjects[i].facts["official_site"] = repo
				starts[subjects[i].domain] = append(starts[subjects[i].domain], repo)
				if topicOf[subjects[i].domain] == "" {
					topicOf[subjects[i].domain] = "AI software development (the " + subjects[i].name + " " + subjects[i].kind + ")"
				}
				if !slices.Contains(domains, subjects[i].domain) {
					domains = append(domains, subjects[i].domain)
				}
				fmt.Printf("twoai_lang_pages: %s fetched nothing for %s, falling back to %s\n", d, subjects[i].name, repo)
			}
		}
	}
	for _, d := range domains {
		if digested >= twoaiLangCrawlPerRun {
			break
		}
		var need bool
		db.QueryRow(`SELECT crawled_on IS NOT NULL AND (digested_on IS NULL OR digested_on < crawled_on) FROM twoai_site_crawl WHERE domain=$1`, d).Scan(&need)
		if !need {
			continue
		}
		if u, err := twoaiDigestSite(db, d, topicOf[d]); err == nil {
			fmt.Printf("twoai_lang_pages: digested %s useful_pages=%d\n", d, u)
		}
		digested++
	}

	written, skipped := 0, 0
	for _, s := range subjects {
		if written >= twoaiLangWritePerRun {
			break
		}
		var digestRaw sql.NullString
		var digestedOn sql.NullString
		var pagesRead int
		db.QueryRow(`SELECT digest::text, COALESCE(pages_fetched,0), digested_on::text FROM twoai_site_crawl
			WHERE domain=$1 AND digested_on IS NOT NULL AND digested_on >= crawled_on`, s.domain).Scan(&digestRaw, &pagesRead, &digestedOn)
		if !digestRaw.Valid {
			skipped++
			continue
		}
		factsJSON, _ := json.Marshal(s.facts)
		h := sha256.Sum256(append([]byte(digestRaw.String), factsJSON...))
		hash := hex.EncodeToString(h[:16])
		var have string
		db.QueryRow(`SELECT COALESCE(content_hash,'') FROM twoai_lang_pages WHERE key=$1 AND doc IS NOT NULL AND error IS NULL`, s.key).Scan(&have)
		if have == hash {
			continue
		}
		var fs []siteFinding
		json.Unmarshal([]byte(digestRaw.String), &fs)
		var fb strings.Builder
		var usedPages []map[string]string
		for _, f := range fs {
			if !f.Useful || len(f.Facts) == 0 {
				continue
			}
			fmt.Fprintf(&fb, "PAGE %s: %s\n", f.URL, f.What)
			for _, x := range f.Facts {
				fmt.Fprintf(&fb, "  - %s\n", x)
			}
			usedPages = append(usedPages, map[string]string{"url": f.URL, "date": f.Date})
		}
		if fb.Len() < 400 {
			db.Exec(`INSERT INTO twoai_lang_pages (key, uid, kind, name, domain, error) VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (key) DO UPDATE SET domain=$5, error=$6`, s.key, s.uid, s.kind, s.name, s.domain, "site digest too thin to write from")
			skipped++
			continue
		}
		what := "programming language"
		if s.kind == "framework" {
			what = "AI framework or library"
		} else if s.kind == "inference engine" {
			what = "AI inference engine or model serving runtime"
		}
		system := "You write one reference page for theworldofai.org about a " + what + " used in artificial intelligence work. " +
			"You are given the facts this site holds about it and what a full reading of its official website found, page by page. Ground every claim in those; where they do not settle a question, say so in a sentence rather than invent. " +
			"Write in full, readable editorial prose for a working developer, an engineering manager and a student choosing what to learn, the way a well edited technical publication would. Plain English, commas rather than dashes, no bullet lists, no headings inside bodies, no marketing language, no mention of these instructions. " +
			"Return only JSON with this shape: {\"title\": \"<the page title, naming the subject and its role in AI, under 90 characters>\", " +
			"\"answer\": \"<70 to 100 words: what it is, where it sits in AI work and the one thing a reader deciding whether to use it should know, written to stand alone>\", " +
			"\"sections\": [{\"heading\": \"What it is and where it sits in AI work\", \"body\": \"<who makes it, what kind of thing it is, when it appeared, and the layer of the AI stack it serves>\"}, " +
			"{\"heading\": \"What it is used for and where it is strongest\", \"body\": \"<the concrete AI jobs it does: training, inference, data work, agents, serving, edge; and where it is the best choice>\"}, " +
			"{\"heading\": \"How hard it is to learn and what you need first\", \"body\": \"<the learning curve, prerequisites such as mathematics or systems knowledge, and what the official site offers a beginner>\"}, " +
			"{\"heading\": \"Ecosystem and community\", \"body\": \"<the libraries, models, integrations, documentation and community support around it, as the site and the facts show>\"}, " +
			"{\"heading\": \"Prototyping speed against production performance\", \"body\": \"<whether it is built for quick experiments, for speed and memory control in production, or both, and what that costs>\"}, " +
			"{\"heading\": \"Industry adoption and careers\", \"body\": \"<who uses it, whether it is a current standard or a legacy choice, and what that means for someone choosing it for work; use only what the facts and the site support>\"}, " +
			"{\"heading\": \"How it fits beside the others\", \"body\": \"<the languages and frameworks it is typically used with, and the polyglot pattern it belongs to>\"}, " +
			"{\"heading\": \"Limits and open questions\", \"body\": \"<what it is weak at, what is immature or changing, and what the official material does not say>\"}]}"
		user := fmt.Sprintf("Subject: %s, a %s.\nFacts this site holds: %s\nOfficial website: %s, %d pages read on %s.\n\nWhat the official website says, page by page:\n\n%s",
			s.name, what, string(factsJSON), s.domain, pagesRead, digestedOn.String, trunc(fb.String(), 14000))
		out, model, gerr := twoaiGenerate("twoai_lang_pages", system, user)
		if gerr != nil {
			db.Exec(`INSERT INTO twoai_lang_pages (key, uid, kind, name, domain, error) VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (key) DO UPDATE SET domain=$5, error=$6`, s.key, s.uid, s.kind, s.name, s.domain, gerr.Error())
			continue
		}
		o := out
		if i := strings.Index(o, "{"); i > 0 {
			o = o[i:]
		}
		if k := strings.LastIndex(o, "}"); k >= 0 {
			o = o[:k+1]
		}
		var m struct {
			Title    string `json:"title"`
			Answer   string `json:"answer"`
			Sections []struct {
				Heading string `json:"heading"`
				Body    string `json:"body"`
			} `json:"sections"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(o)), &m); err != nil || strings.TrimSpace(m.Answer) == "" || len(m.Sections) < 5 {
			db.Exec(`INSERT INTO twoai_lang_pages (key, uid, kind, name, domain, error) VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (key) DO UPDATE SET domain=$5, error=$6`, s.key, s.uid, s.kind, s.name, s.domain, "model output was not the expected JSON")
			continue
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = s.name + " in AI work"
		}
		var sections []map[string]string
		for _, sec := range m.Sections {
			if strings.TrimSpace(sec.Body) == "" {
				continue
			}
			sections = append(sections, map[string]string{"heading": strings.TrimSpace(sec.Heading), "body": strings.TrimSpace(sec.Body)})
		}
		doc := map[string]any{
			"uid": s.uid, "page_uid": s.uid, "shape": "lang-profile", "tax": "lang-profile", "slug": s.slug,
			"name": s.name, "title": title, "answer": strings.TrimSpace(m.Answer), "sections": sections,
			"subject_kind": s.kind, "subject_key": s.key, "facts": s.facts,
			"category": "technology-and-core-infrastructure", "hub_name": "Programming Languages and Frameworks",
			"parent_name": s.parentName, "parent_path": s.parentPath,
			"source_url": s.siteURL, "source_read_on": digestedOn.String, "site_domain": s.domain, "site_pages_read": pagesRead, "site_pages": usedPages,
			"generated": today, "built_at": time.Now().Format(time.RFC3339), "refresh_every_days": 90, "model": model,
		}
		raw, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_lang_pages (key, uid, kind, name, domain, content_hash, doc, model, written_on, error)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,current_date,NULL)
			ON CONFLICT (key) DO UPDATE SET domain=$5, content_hash=$6, doc=$7::jsonb, model=$8, written_on=current_date, error=NULL`,
			s.key, s.uid, s.kind, s.name, s.domain, hash, string(raw), model); err != nil {
			return written, err
		}
		written++
		fmt.Printf("twoai_lang_pages: wrote %s (%s)\n", s.name, s.kind)
	}

	// Publish every written page as a tech document. Siblings are filled in
	// fresh each run so new pages appear on old ones.
	type sib struct{ name, uid, kind string }
	var sibs []sib
	if sr, err := db.Query(`SELECT name, uid, kind FROM twoai_lang_pages WHERE doc IS NOT NULL ORDER BY kind, name`); err == nil {
		for sr.Next() {
			var x sib
			if sr.Scan(&x.name, &x.uid, &x.kind) == nil {
				sibs = append(sibs, x)
			}
		}
		sr.Close()
	}
	published := 0
	if pr, err := db.Query(`SELECT key, uid, doc::text FROM twoai_lang_pages WHERE doc IS NOT NULL`); err == nil {
		type pub struct{ key, uid, raw string }
		var pubs []pub
		for pr.Next() {
			var p pub
			if pr.Scan(&p.key, &p.uid, &p.raw) == nil {
				pubs = append(pubs, p)
			}
		}
		pr.Close()
		for _, p := range pubs {
			var doc map[string]any
			if json.Unmarshal([]byte(p.raw), &doc) != nil {
				continue
			}
			var others []map[string]string
			for _, x := range sibs {
				if x.uid != p.uid {
					others = append(others, map[string]string{"name": x.name, "kind": x.kind, "path": "/ai-ecosystem/technology-and-core-infrastructure/" + x.uid + "/"})
				}
			}
			doc["siblings"] = others
			raw, _ := json.Marshal(doc)
			// taxonomy_slug is NULL, as the incident pages use. 'lang-profile'
			// is not a row in twoai_taxonomy, so the foreign key refused every
			// insert on the first run and the log said published=0 (row 357).
			// The shape lives in the document's own "shape" and "tax" fields.
			if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
				VALUES ($1, 'tech-section', NULL, $2::jsonb, 1, now())
				ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()
				WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`, "tech/profile-"+p.uid+".json", string(raw)); err == nil {
				published++
			}
		}
	}
	fmt.Printf("twoai_lang_pages: subjects=%d crawled=%d digested=%d written=%d waiting=%d published=%d ok=true\n", len(subjects), crawled, digested, written, skipped, published)
	return written, nil
}
