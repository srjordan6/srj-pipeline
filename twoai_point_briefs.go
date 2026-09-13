package main

// twoai_point_briefs: a real paragraph on every cited source, kept current.
//
// Stephen, 2026-09-11: an industry point read "Stanford HAI measures the
// macro trends", one sentence, and a link out. "We could have paraphrased the
// report before sending them to the source." It is 151 of 159 points across
// the 23 industry pages - a headline, a line of orientation, and the reader
// doing all the work.
//
// NOTHING NEW IS FETCHED. twoai_industry_hub already harvests every cited
// source URL daily into twoai_source_harvest: the readable text of the page,
// keyed by url, with a content_hash and a fetched_on date. That harvest feeds
// the SECTOR analysis at the bottom of each page and has never fed the points
// it was gathered from. This stage reads the same extracts and writes one
// short brief per source.
//
// HOW IT STAYS CURRENT, which was the other half of the ask. Every brief is
// stored against the content_hash of the extract it was written from. The
// harvest refreshes daily; when a source page's substance changes, its hash
// changes, and this stage rewrites that brief on the next run and no other.
// A source that has not changed is never re-summarised, so cost and churn
// both stay near zero while the pages track their sources automatically.
//
// WHAT THE PROMPT FORBIDS, and why each rule is here:
//   - Only what the extract says. No background knowledge, no filling a gap
//     from what the model remembers about the organisation. This site has
//     quarantined fabricated people before; a plausible paragraph about a
//     report nobody checked is the same failure in a quieter register.
//   - Paraphrase, never reproduce. One quoted phrase at most, under fifteen
//     words. The brief is a reason to open the source, not a replacement.
//   - Name who reported a figure, inside the sentence, so no number floats
//     free of its publisher.
//   - Say less when the extract says less. Two honest sentences beat a
//     padded paragraph, and the model is told to return NOTHING rather than
//     invent.
//
// The site renders the brief under the point, above the outbound link, with
// the date it was written. A point with no brief renders exactly as it does
// today, so nothing regresses while the backlog fills.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const pointBriefSystem = `You write one short brief about a source page for a sourced AI reference site. A reader sees it under a one-line summary and decides whether to open the source.

ABSOLUTE RULES:
- Use ONLY what the supplied page text says. Never add background knowledge. Never infer a finding the text does not state. Never name an organisation, figure, product or date that is not in the text.
- Paraphrase in your own words. Do NOT copy the source's sentences. At most ONE quoted phrase, under fifteen words, in quotation marks.
- Attribute every figure or finding to whoever reported it, inside the sentence.
- One paragraph, three to five sentences. Plain English. No marketing language, no "in today's rapidly evolving landscape", no closing exhortation, no instruction to visit the source.
- Write about what the source SAYS, not about what the page is.

If the text has no substantive content to summarise - a navigation page, a paywall notice, a cookie banner, an error - output exactly: NOTHING`

// LINKING WAS TRIED HERE AND REMOVED, 2026-09-13. The prompt above used to
// offer the model a list of this site's compliance and lawsuit pages and
// invite it to mark, in double brackets, any subject the source genuinely
// discussed. The resolver below still exists and is still safe, so the idea
// can be revived; the prompt instruction is gone because the results did not
// justify it.
//
// Of 36 briefs, 4 carried a link and 1 was right:
//
//   GOOD  artificialintelligenceact.eu/article/50 linked the EU AI Act. The
//         source IS that article. Natural, correct.
//   BAD   ILTA ended "...claims intelligence from EvenUp. Agentic AI and the
//         CFAA" - a link bolted onto the paragraph with no sentence around
//         it, which the prompt explicitly forbade.
//   WORSE CodeX became "prototype Agentic AI and the CFAA applications at the
//         intersection of technology and law." CodeX prototypes legal-tech
//         applications. The model took our page title and used it as a noun
//         phrase, changing what the source said.
//
// That last one is the reason this is off rather than tuned. A wrong link is
// recoverable. A SENTENCE REWRITTEN TO ACCOMMODATE A LINK is a distortion of
// the source wearing our own markup, on pages whose whole claim is that they
// report what the source says. The model is excellent at "summarise only what
// is here" and reaches when told it may link, and reaching is the one thing
// that cannot be allowed here.
//
// The earlier regex approach (twoai_internal_links) produced 9 correct links
// across 23 pages and remains the honest fallback. The real answer to
// Stephen's observation is probably neither: it is that these pages cite
// outside sources because the site has not yet written its own pages on what
// they discuss. More pages, not more linking.

var pbSentRe = regexp.MustCompile(`[.!?]`)
var pbLinkRe = regexp.MustCompile(`\[\[([^\]]{3,80})\]\]`)

// twoaiLinkableSubjects is what this site can honestly link to: its own
// compliance frameworks, tracked companies and lawsuit pages. Built once per
// run and offered to the model that writes each brief.
//
// WHY THE MODEL AND NOT A REGEX. The first attempt matched finished prose
// against these titles afterwards and produced 9 links across 23 pages, one
// of them wrong: "the Evident AI Index" matched Stanford HAI, whose alias is
// "AI Index". Guarding against that - reject a match preceded by a capital -
// then killed a link we wanted, "under Fed SR 11-7 supervision", because the
// two cases are grammatically identical and mean opposite things. No pattern
// separates them. A model reading the source text can tell whether the page
// is ABOUT SR 11-7, which is the only question that matters.
func twoaiLinkableSubjects(db *sql.DB) (map[string]string, error) {
	out := map[string]string{}
	add := func(title, url string) {
		t := strings.TrimSpace(title)
		if len([]rune(t)) < 4 || url == "" {
			return
		}
		if _, seen := out[strings.ToLower(t)]; !seen {
			out[strings.ToLower(t)] = url
		}
	}
	rows, err := db.Query(`SELECT COALESCE(data->>'title',''), path FROM twoai_pages WHERE kind='compliance'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t, p string
		if rows.Scan(&t, &p) == nil {
			add(t, "/ai-compliance/"+strings.TrimSuffix(strings.TrimPrefix(p, "compliance/"), ".json")+"/")
		}
	}
	rows.Close()
	if crows, err := db.Query(`SELECT name, uid FROM twoai_company_profiles`); err == nil {
		for crows.Next() {
			var n, uid string
			if crows.Scan(&n, &uid) == nil {
				add(n, "/companies/"+uid+"/")
			}
		}
		crows.Close()
	}
	// Lawsuits live as 108 cases inside ONE document, not as one page per
	// case. Querying kind='lawsuit' returned nothing and cost the first run
	// every case link it could have made - including the training-data suits,
	// which are exactly what an industry point about media or copyright is
	// usually discussing. The page kind is 'lawsuits', plural, and the cases
	// are an array inside it.
	var lj []byte
	if db.QueryRow(`SELECT data::text FROM twoai_pages WHERE kind='lawsuits' LIMIT 1`).Scan(&lj) == nil && len(lj) > 0 {
		var ld struct {
			Cases []struct {
				Slug string `json:"slug"`
				Name string `json:"case_name"`
				Short string `json:"short_name"`
			} `json:"cases"`
		}
		if json.Unmarshal(lj, &ld) == nil {
			for _, c := range ld.Cases {
				if c.Slug == "" {
					continue
				}
				add(c.Name, "/ai-lawsuits/"+c.Slug+"/")
				add(c.Short, "/ai-lawsuits/"+c.Slug+"/")
			}
		}
	}
	return out, nil
}

// twoaiResolveBriefLinks turns [[Title]] into markup the template can render,
// and is the gate that makes offering links to a model safe.
//
// A bracketed title that is not in the inventory is UNWRAPPED, not linked:
// the words stay in the sentence and no link is emitted. So the worst a
// hallucinated title can do is leave ordinary prose. Nothing the model
// invents can become a URL, which is the property that lets the model make
// the judgement a regex could not.
func twoaiResolveBriefLinks(brief string, subjects map[string]string) (string, int) {
	n := 0
	out := pbLinkRe.ReplaceAllStringFunc(brief, func(m string) string {
		title := strings.TrimSpace(pbLinkRe.FindStringSubmatch(m)[1])
		url, ok := subjects[strings.ToLower(title)]
		if !ok {
			return title
		}
		n++
		return `<a href="` + url + `">` + title + `</a>`
	})
	return out, n
}

func twoaiPointBriefs(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_point_briefs (
		url text PRIMARY KEY,
		content_hash text NOT NULL,
		brief text,
		model text,
		words int,
		generated_on date,
		attempts int NOT NULL DEFAULT 0,
		last_note text,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" && twoaiLLMFor("point_briefs") != "ollama" {
		fmt.Println("twoai_point_briefs: no model configured (ANTHROPIC_API_KEY unset, TWOAI_LLM not ollama), skipping")
		return nil
	}
	limit := 40
	if v := strings.TrimSpace(os.Getenv("TWOAI_BRIEF_LIMIT")); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}

	// Sources whose harvested text has changed since the brief was written,
	// or which have no brief at all. Newest harvest first so a source that
	// just changed is refreshed before the backlog.
	rows, err := db.Query(`
		SELECT h.url, h.source_name, h.extract, h.content_hash, COALESCE(h.sector_slug,'')
		FROM twoai_source_harvest h
		LEFT JOIN twoai_point_briefs b ON b.url = h.url
		WHERE h.extract <> '' AND h.http_status = 200
		  AND (b.url IS NULL OR b.content_hash <> h.content_hash)
		  AND COALESCE(b.attempts, 0) < 3
		ORDER BY h.fetched_on DESC, h.url
		LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type job struct{ url, name, extract, hash, sector string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.url, &j.name, &j.extract, &j.hash, &j.sector) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	// The subjects this site can link to. Built once; offered to the model as
	// a list it may draw from, never as a list it must use.
	subjects, serr := twoaiLinkableSubjects(db)
	if serr != nil {
		return serr
	}
	// The whole inventory is over a thousand titles, which is both expensive
	// to send on every call and an invitation to reach. The model gets the
	// compliance frameworks and lawsuits - the subjects an industry point is
	// most often actually about - plus nothing else, and it is told these are
	// optional. Companies stay resolvable in twoaiResolveBriefLinks, so a
	// model that names one from the source text still gets a working link.
	var offerLines []string
	if orows, oerr := db.Query(`SELECT COALESCE(data->>'title','') FROM twoai_pages
		WHERE kind='compliance' AND COALESCE(data->>'title','') <> '' ORDER BY 1`); oerr == nil {
		for orows.Next() {
			var t string
			if orows.Scan(&t) == nil {
				offerLines = append(offerLines, t)
			}
		}
		orows.Close()
	}
	// The lawsuit case names, from the single lawsuits document. Offered by
	// their short name where they have one, because that is what a source
	// actually calls them.
	var rawCases []byte
	if db.QueryRow(`SELECT data::text FROM twoai_pages WHERE kind='lawsuits' LIMIT 1`).Scan(&rawCases) == nil && len(rawCases) > 0 {
		var ld struct {
			Cases []struct {
				Name  string `json:"case_name"`
				Short string `json:"short_name"`
			} `json:"cases"`
		}
		if json.Unmarshal(rawCases, &ld) == nil {
			for _, c := range ld.Cases {
				if c.Short != "" {
					offerLines = append(offerLines, c.Short)
				} else if c.Name != "" {
					offerLines = append(offerLines, c.Name)
				}
			}
		}
	}
	fmt.Printf("twoai_point_briefs: offering %d linkable subjects (%d resolvable)\n", len(offerLines), len(subjects))
	offer := ""
	// The offer is built but not sent: see the note under pointBriefSystem.
	// Kept assembled rather than deleted because the inventory queries are the
	// expensive part of reviving this, and because the count in the log is a
	// useful measure of how much this site could link to if it linked at all.
	_ = offerLines
	if len(jobs) == 0 {
		var have, total int
		db.QueryRow(`SELECT count(*) FILTER (WHERE brief IS NOT NULL AND brief <> ''), count(*) FROM twoai_point_briefs`).Scan(&have, &total)
		fmt.Printf("twoai_point_briefs: nothing changed, %d briefs current\n", have)
		return nil
	}

	written, nothing, failed, linked := 0, 0, 0, 0
	for _, j := range jobs {
		user := "Source: " + j.url + "\nPublisher: " + j.name + "\n\nText harvested from the page:\n\n" + j.extract + offer
		out, usedModel, err := twoaiGenerate("point_briefs", pointBriefSystem, user)
		if err != nil {
			db.Exec(`INSERT INTO twoai_point_briefs (url, content_hash, attempts, last_note)
				VALUES ($1,$2,1,$3)
				ON CONFLICT (url) DO UPDATE SET attempts=twoai_point_briefs.attempts+1, last_note=$3, updated_at=now()`,
				j.url, j.hash, err.Error())
			failed++
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		brief := strings.TrimSpace(out)
		// The model is told to return NOTHING for a nav page or a paywall.
		// Storing the hash on that outcome matters: it stops the same dead
		// page being re-summarised every night for the rest of time.
		if brief == "" || strings.HasPrefix(brief, "NOTHING") || len([]rune(brief)) < 120 {
			db.Exec(`INSERT INTO twoai_point_briefs (url, content_hash, attempts, last_note)
				VALUES ($1,$2,1,'no substantive content in the harvested text')
				ON CONFLICT (url) DO UPDATE SET content_hash=$2, attempts=twoai_point_briefs.attempts+1,
					last_note='no substantive content in the harvested text', updated_at=now()`, j.url, j.hash)
			nothing++
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		// Resolve the model's [[Title]] marks against the real inventory. An
		// unknown title is unwrapped to plain words, so nothing invented can
		// become a URL.
		resolved, nlinks := twoaiResolveBriefLinks(brief, subjects)
		brief = resolved
		linked += nlinks
		// House rule, enforced here rather than trusted to the prompt: no
		// paragraph over five sentences anywhere on this site.
		if n := len(pbSentRe.FindAllString(brief, -1)); n > 6 {
			db.Exec(`INSERT INTO twoai_point_briefs (url, content_hash, attempts, last_note)
				VALUES ($1,$2,1,'draft ran long, will retry')
				ON CONFLICT (url) DO UPDATE SET attempts=twoai_point_briefs.attempts+1,
					last_note='draft ran long, will retry', updated_at=now()`, j.url, j.hash)
			failed++
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		if _, err := db.Exec(`INSERT INTO twoai_point_briefs
			(url, content_hash, brief, model, words, generated_on, attempts, last_note)
			VALUES ($1,$2,$3,$4,$5,current_date,0,NULL)
			ON CONFLICT (url) DO UPDATE SET content_hash=EXCLUDED.content_hash, brief=EXCLUDED.brief,
				model=EXCLUDED.model, words=EXCLUDED.words, generated_on=EXCLUDED.generated_on,
				attempts=0, last_note=NULL, updated_at=now()`,
			j.url, j.hash, brief, usedModel, len(strings.Fields(brief))); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_point_briefs store:", err)
			failed++
			continue
		}
		written++
		time.Sleep(1500 * time.Millisecond)
	}

	var have, total, stale int
	db.QueryRow(`SELECT count(*) FILTER (WHERE brief IS NOT NULL AND brief <> ''), count(*) FROM twoai_point_briefs`).Scan(&have, &total)
	db.QueryRow(`SELECT count(*) FROM twoai_source_harvest h LEFT JOIN twoai_point_briefs b ON b.url=h.url
		WHERE h.extract <> '' AND h.http_status=200 AND (b.url IS NULL OR b.content_hash <> h.content_hash)`).Scan(&stale)
	fmt.Printf("twoai_point_briefs: written=%d internal_links=%d nothing_to_say=%d failed=%d | %d briefs held, %d sources still to write\n",
		written, linked, nothing, failed, have, stale)
	return nil
}
