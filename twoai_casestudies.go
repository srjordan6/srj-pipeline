package main

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
)

// The disclosure wordings the covered publishers actually use.
var twoaiSponsoredRe = regexp.MustCompile(`sponsored (by|content)|paid (post|content)|in partnership with|advertorial|promoted by`)

// AI CASE STUDIES: an index, not a library.
//
// WHAT THIS IS AND WHY IT IS SHAPED THIS WAY. Stephen asked for case studies
// on the site. Case studies are the one content type this project cannot
// generate from its own data and must not copy: a case study is a publisher's
// reported account of somebody's deployment, and that account is their
// copyrighted prose. Reproducing it would breach the standing rule that this
// site publishes summaries and facts and never article text.
//
// So this stage builds the thing that is genuinely ours to build and that
// nobody else maintains: a current, deduplicated, sector-tagged INDEX of case
// studies published elsewhere, newest first, every row linking out to the
// original. What is stored is the headline, the publisher, the date, the link,
// the publisher's own feed abstract capped hard by twoaiFeedSummary, and a
// sector tag this pipeline derives itself. A reader gets the map; the
// publisher keeps the traffic and the text.
//
// TWO RULES SET BY STEPHEN, 2026-08-23, AND ENFORCED HERE RATHER THAN LABELLED.
//
// 1. NO SPONSORED CONTENT, AT ALL. Not indexed and labelled: refused. Emerj
// and the trade press both run vendor-funded pieces that read exactly like
// case studies. Carrying one would put somebody else's advertising on this
// site, and the only advertising here is this site's own. A post whose
// disclosure text trips twoaiSponsoredRe is never stored, and any row that
// was stored before this rule existed is deactivated on the next run.
//
// 2. ONLY ACTUAL CASE STUDIES. A case study is a reported account of a named
// organisation deploying AI, with something said about what happened. Product
// launches, funding rounds, opinion columns and market commentary are not
// case studies, whatever feed they arrive on, and a page called AI Case
// Studies that lists "Meta AI now available as a desktop app" is lying about
// its own contents. Every candidate is classified once by Haiku and only
// case_study rows render. Vendor announcements belong on the vendor news
// page, which has its own named-source allowlist, and press coverage belongs
// to the daily briefing; this stage routes nothing there automatically,
// because both of those pages make claims about their sources that an
// automatic import would quietly break.
//
// CLASSIFICATION IS CACHED, NOT REPEATED. classification is written once per
// slug and the model is asked only about rows still marked unclassified, so a
// daily run costs a handful of calls rather than hundreds. An item the model
// cannot be reached for stays unclassified and simply does not render: the
// page under-reports rather than guesses.
//
// SECTOR TAGGING IS DETERMINISTIC ON PURPOSE. The tag comes from matching the
// headline against the industry names already in twoai_industries. No model
// guesses at it, so a mis-tag is a missing tag rather than a confident wrong
// one, and an untagged study still lists. That is the same rule the industry
// pages follow: say less rather than invent.

// twoaiCaseStudyHarvest pulls each publisher feed and upserts what it finds.
// A feed that fails is recorded and skipped; one bad publisher never costs the
// others their run.
func twoaiCaseStudyHarvest(db *sql.DB) error {
	rows, err := db.Query(`SELECT publisher, feed_url FROM twoai_case_study_publishers
		WHERE active ORDER BY publisher`)
	if err != nil {
		return err
	}
	type pub struct{ name, url string }
	var pubs []pub
	for rows.Next() {
		var p pub
		if err := rows.Scan(&p.name, &p.url); err != nil {
			rows.Close()
			return err
		}
		pubs = append(pubs, p)
	}
	rows.Close()

	// The sector vocabulary, read from the industries this site already
	// covers, so a tag always points at a page that exists.
	type sector struct{ slug, name string }
	var sectors []sector
	if r, err := db.Query(`SELECT slug, name FROM twoai_industries ORDER BY slug`); err == nil {
		for r.Next() {
			var s sector
			if r.Scan(&s.slug, &s.name) == nil {
				sectors = append(sectors, s)
			}
		}
		r.Close()
	}

	saved, failed, skippedSponsored := 0, 0, 0
	for _, p := range pubs {
		body, err := twoaiJobsGet(p.url, nil)
		if err != nil {
			failed++
			db.Exec(`UPDATE twoai_case_study_publishers SET last_error=$2,
				consecutive_failures = consecutive_failures + 1 WHERE publisher=$1`,
				p.name, err.Error())
			fmt.Fprintf(os.Stderr, "twoai_case_studies: %s: %v\n", p.name, err)
			continue
		}
		var doc twoaiFeedDoc
		if err := xml.Unmarshal(body, &doc); err != nil {
			failed++
			db.Exec(`UPDATE twoai_case_study_publishers SET last_error=$2,
				consecutive_failures = consecutive_failures + 1 WHERE publisher=$1`,
				p.name, "parse: "+err.Error())
			fmt.Fprintf(os.Stderr, "twoai_case_studies: %s parse: %v\n", p.name, err)
			continue
		}
		items := doc.Channel.Items
		if len(items) == 0 {
			items = doc.Entries
		}
		n := 0
		for _, it := range items {
			link := it.URL()
			title := strings.TrimSpace(html.UnescapeString(twoaiTagStrip.ReplaceAllString(it.Title, "")))
			if title == "" || link == "" {
				continue
			}
			slug := twoaiPostSlug(title, link)
			date := twoaiFeedDate(it.PubDate, it.Published, it.Updated, it.Date)
			summary := twoaiFeedSummary(it.Description, it.Summary, it.Content)

			// Tag by industry name appearing in the headline. Whole-word-ish
			// matching on the lowered title; first hit wins; no hit is fine.
			var sectorSlug any
			lt := strings.ToLower(title)
			for _, s := range sectors {
				first := strings.ToLower(strings.Fields(s.name)[0])
				if len(first) >= 5 && strings.Contains(lt, first) {
					sectorSlug = s.slug
					break
				}
			}
			var posted any
			if date != "" {
				posted = date
			}
			// Rule 1. The disclosure lives in the abstract, so it is detected
			// there. Half of one publisher's recent feed tripped this on the
			// first run, which is exactly why it is a refusal and not a badge.
			if twoaiSponsoredRe.MatchString(strings.ToLower(summary + " " + title)) {
				// Refused at the door. Deactivate it if an earlier run stored
				// it, so the rule applies retroactively without a manual sweep.
				db.Exec(`UPDATE twoai_case_studies SET active=false WHERE slug=$1`, slug)
				skippedSponsored++
				continue
			}
			// posted_on and first_seen are written once: a permalink's date
			// must not move because a feed re-dated an old post.
			if _, err := db.Exec(`INSERT INTO twoai_case_studies
				(slug, publisher, title, url, summary, sector_slug, posted_on)
				VALUES ($1,$2,$3,$4,$5,$6,$7::date)
				ON CONFLICT (slug) DO UPDATE SET
					title=EXCLUDED.title, url=EXCLUDED.url,
					summary=CASE WHEN EXCLUDED.summary <> '' THEN EXCLUDED.summary
					             ELSE twoai_case_studies.summary END,
					sector_slug=COALESCE(EXCLUDED.sector_slug, twoai_case_studies.sector_slug),
					active=true, last_seen=now()`,
				slug, p.name, title, link, summary, sectorSlug, posted); err != nil {
				fmt.Fprintln(os.Stderr, "twoai_case_studies: upsert:", err)
				continue
			}
			n++
			saved++
		}
		db.Exec(`UPDATE twoai_case_study_publishers SET last_ok=now(), last_error=NULL,
			consecutive_failures=0 WHERE publisher=$1`, p.name)
		fmt.Printf("twoai_case_studies: %s items=%d\n", p.name, n)
	}
	fmt.Printf("twoai_case_studies: publishers=%d saved=%d sponsored_refused=%d failed=%d\n",
		len(pubs), saved, skippedSponsored, failed)

	return twoaiCaseStudyClassify(db)
}

// twoaiCaseStudyClassify asks the model, once per item, whether a stored
// candidate is actually a case study. Only case_study rows ever render, so the
// cost of being wrong here is a missing entry, never a mislabelled one.
//
// THIS WAS THE ONE STAGE STILL WIRED TO ANTHROPIC. Stephen, 2026-09-17: cut all
// ties with the API. twoai_llm.go was changed that day so no switch can route a
// stage there, but this file never went through twoaiGenerate. It built its own
// request to api.anthropic.com and gated on ANTHROPIC_API_KEY, so it was missed.
// It has cost nothing since, because the key is not in pipeline.env. It has
// also classified nothing: the log said "candidates stay unclassified" on every
// run, 104 harvested articles sat unrendered, and the case studies page stopped
// growing the day the key was removed.
//
// Stephen, 2026-09-19, when told to add the key back: I only want ollama. So it
// now goes through twoaiGenerate like every other stage, which means Ollama,
// the per-stage model override OLLAMA_MODEL_CASE_STUDIES, and the same
// behaviour when Ollama is down: nothing is written, and the row stays
// unclassified for the next run rather than getting a guess.
func twoaiCaseStudyClassify(db *sql.DB) error {
	rows, err := db.Query(`SELECT slug, title, summary, publisher FROM twoai_case_studies
		WHERE active AND classification='unclassified' ORDER BY posted_on DESC NULLS LAST LIMIT 120`)
	if err != nil {
		return err
	}
	type cand struct{ slug, title, summary, publisher string }
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.slug, &c.title, &c.summary, &c.publisher) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	counts := map[string]int{}
	for _, c := range cands {
		verdict, err := twoaiClassifyCaseStudy(c.title, c.summary, c.publisher)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_case_studies: classify %s: %v\n", c.slug, err)
			// Ollama down is a whole-run condition, not a per-article one. Once
			// twoaiGenerate has marked it down every further call fails the same
			// way, so stop rather than print the same line 120 times.
			if strings.Contains(err.Error(), "marked down") || strings.Contains(err.Error(), "unreachable") {
				fmt.Fprintf(os.Stderr, "twoai_case_studies: ollama is down, %d candidates left for the next run\n", len(cands)-counts["case_study"]-counts["vendor_news"]-counts["other"])
				break
			}
			continue // stays unclassified, does not render
		}
		db.Exec(`UPDATE twoai_case_studies SET classification=$2, classified_on=current_date
			WHERE slug=$1`, c.slug, verdict)
		counts[verdict]++
	}
	if len(cands) > 0 {
		fmt.Printf("twoai_case_studies: classified=%d case_study=%d vendor_news=%d other=%d\n",
			len(cands), counts["case_study"], counts["vendor_news"], counts["other"])
	}
	return nil
}

// twoaiClassifyCaseStudy returns exactly one of case_study, vendor_news or
// other. The prompt names what each one is, because "is this a case study" on
// its own gets a generous answer and a generous answer is how the page fills
// up with product launches.
func twoaiClassifyCaseStudy(title, summary, publisher string) (string, error) {
	system := "You classify news articles. Answer with exactly one word from the list given and nothing else. No punctuation, no explanation."
	prompt := "Classify this article into exactly one category. Answer with one word and nothing else.\n\n" +
		"case_study: a reported account of one or more named organisations actually using or deploying AI, " +
		"describing what they did and what happened. Includes interview-based accounts of a real deployment.\n" +
		"vendor_news: a product launch, release, funding round, acquisition, partnership or company announcement.\n" +
		"other: opinion, market commentary, survey or forecast writeups, general how-to guides, research summaries, " +
		"and anything where no specific organisation's use of AI is reported.\n\n" +
		"If it is a close call between case_study and anything else, answer with the other category.\n\n" +
		"Publisher: " + publisher + "\nHeadline: " + title + "\nAbstract: " + summary
	v, _, err := twoaiGenerate("case_studies", system, prompt)
	if err != nil {
		return "", err
	}
	// A local model is chattier than the one this was written for. It may wrap
	// the word in quotes or markdown, or lead with "Category:". So the verdict
	// is found anywhere in the reply, AS A WHOLE WORD, which matters because
	// "other" hides inside "another".
	//
	// And a reply that names MORE THAN ONE category is refused. "Not a
	// case_study, this is vendor_news" names case_study first, and taking the
	// first word would publish a product launch as a case study. The rule this
	// stage was built on is that being wrong costs a missing entry, never a
	// mislabelled one, so an ambiguous reply leaves the row unclassified.
	v = strings.ToLower(strings.TrimSpace(v))
	seen := map[string]bool{}
	for _, m := range twoaiCaseVerdictRe.FindAllString(v, -1) {
		seen[m] = true
	}
	if len(seen) == 1 {
		for m := range seen {
			return m, nil
		}
	}
	if len(seen) > 1 {
		return "", fmt.Errorf("ambiguous verdict, names %d categories: %q", len(seen), v)
	}
	return "", fmt.Errorf("unrecognised verdict %q", v)
}

var twoaiCaseVerdictRe = regexp.MustCompile(`\b(case_study|vendor_news|other)\b`)

// twoaiCaseStudies renders the section page from whatever the harvest holds.
// An empty table renders nothing at all rather than an empty page promising
// case studies it does not have.
func twoaiCaseStudies(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	var name, blurb string
	if err := db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy
		WHERE slug='ai-case-studies'`).Scan(&name, &blurb); err != nil {
		return 0, nil // section not defined yet
	}
	sectionUID := twoaiUID("section:ai-case-studies")

	type study struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		Publisher string `json:"publisher"`
		Summary   string `json:"summary,omitempty"`
		Sector    string `json:"sector,omitempty"`
		SectorURL string `json:"sector_url,omitempty"`
		Posted    string `json:"posted_on,omitempty"`
	}
	var studies []study
	rows, err := db.Query(`SELECT c.title, c.url, c.publisher, c.summary,
			COALESCE(i.name,''), COALESCE(c.sector_slug,''),
			COALESCE(to_char(c.posted_on,'YYYY-MM-DD'),'')
		FROM twoai_case_studies c
		LEFT JOIN twoai_industries i ON i.slug = c.sector_slug
		WHERE c.active AND NOT c.sponsored AND c.classification='case_study'
		ORDER BY c.posted_on DESC NULLS LAST, c.first_seen DESC
		LIMIT 400`)
	if err != nil {
		return 0, err
	}
	sectorSeen := map[string]bool{}
	for rows.Next() {
		var s study
		var sectorSlug string
		if rows.Scan(&s.Title, &s.URL, &s.Publisher, &s.Summary, &s.Sector, &sectorSlug, &s.Posted) != nil {
			continue
		}
		// The sector link points at the industry page only when that industry
		// is live; a tag with no page renders as plain text, never a 404.
		if sectorSlug != "" && s.Sector != "" {
			s.SectorURL = "/industries/" + strings.TrimPrefix(sectorSlug, "industry-") + "/"
			sectorSeen[s.Sector] = true
		}
		studies = append(studies, s)
	}
	rows.Close()
	if len(studies) == 0 {
		fmt.Println("twoai_build: case studies 0 rows, section not rendered")
		return 0, nil
	}

	type publisher struct {
		Name     string `json:"name"`
		Home     string `json:"home_url"`
		What     string `json:"what_it_is"`
		Terms    string `json:"terms_note"`
		Count    int    `json:"count"`
		LastOK   string `json:"last_ok,omitempty"`
		Failures int    `json:"failures"`
	}
	var pubs []publisher
	if r, err := db.Query(`SELECT p.publisher, p.home_url, p.what_it_is, p.terms_note,
			COALESCE(to_char(p.last_ok,'YYYY-MM-DD'),''), p.consecutive_failures,
			(SELECT count(*) FROM twoai_case_studies c WHERE c.publisher=p.publisher
				AND c.active AND NOT c.sponsored AND c.classification='case_study')
		FROM twoai_case_study_publishers p WHERE p.active ORDER BY p.publisher`); err == nil {
		// A publisher whose every item was filtered out contributes nothing to
		// this page and is not credited on it as a source.
		for r.Next() {
			var pb publisher
			if r.Scan(&pb.Name, &pb.Home, &pb.What, &pb.Terms, &pb.LastOK, &pb.Failures, &pb.Count) == nil && pb.Count > 0 {
				pubs = append(pubs, pb)
			}
		}
		r.Close()
	}

	// Written into content/industries/, which the Enterprise Applications route
	// already reads. Writing it anywhere else produces a file nothing renders -
	// the failure mode that made the case timelines and glossary lenses
	// invisible.
	if err := upsert("industries/case-studies.json", "case-studies", map[string]any{
		"uid": sectionUID, "shape": "case-studies", "tax": "ai-case-studies",
		"name": name, "blurb": blurb, "studies": studies, "publishers": pubs,
		"total": len(studies), "sectors_tagged": len(sectorSeen),
		"generated": today,
	}); err != nil {
		return 0, err
	}

	db.Exec(`UPDATE twoai_taxonomy SET status='live', live_path=$1, updated_at=now()
		WHERE slug='ai-case-studies'`,
		"/ai-ecosystem/enterprise-applications-governance-and-tools/"+sectionUID+"/")

	fmt.Printf("twoai_build: case studies=%d publishers=%d sectors=%d\n",
		len(studies), len(pubs), len(sectorSeen))
	return 1, nil
}
