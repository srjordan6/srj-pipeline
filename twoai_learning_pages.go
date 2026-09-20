package main

// twoai_learning_pages: a page on this site for every certification and course,
// written by the model from the issuer's own page, and rewritten when that
// page changes.
//
// Stephen, 2026-09-19: every one of these should have its own web page, bury
// the link to the certification page at the very bottom, have Ollama see the
// pages, make certain this content is being updated. And then: make Ollama do
// the work.
//
// WHAT WAS WRONG. The AI Certifications and AI Courses sections were lists
// whose headings linked straight out to the issuer. That breaks a standing
// rule restated on 2026-09-17: no hub or index may only link out to other
// websites; explain it here, then give the source URL at the very bottom.
//
// AND THE LISTS WERE ALREADY WRONG, which is the stronger reason. The daily
// "verified" date on each row came from a link check: the URL answered 200.
// Nobody read what the page said. On the day this was written, three of the
// nine certifications were stale while showing verified 2026-09-19:
//
//   - AWS Machine Learning Specialty: AWS's page said the certification was
//     being retired and the last exam day was March 31, 2026, six months gone.
//   - Microsoft Azure AI Engineer Associate: Microsoft's page said this
//     certification and the renewal assessment are retired. The site called it
//     "the working Azure AI credential".
//   - Azure AI Fundamentals: the required exam had become AI-901 and the page
//     had been updated on April 15, 2026. The site still said AI-900.
//
// A link that resolves is not a fact that holds. Those three rows were
// corrected by hand in SQL the same day. This stage exists so the next one is
// caught by the machine: the model reads the page, and the prompt asks it
// directly whether the page says the credential is retired or has changed.
//
// NOTHING NEW IS FETCHED BY THIS FILE. twoaiHarvestSources already pulls every
// cited source once a day into twoai_source_harvest with an extract and a
// content hash. The learning rows were added to that harvest's query, so the
// issuer pages ride the same polite fetcher, the same identified user agent,
// the same one-request-a-day restraint. See twoai_source_briefs.go for the
// note left by the last person who nearly built a second fetcher.
//
// HOW IT STAYS CURRENT. A reading is stored against the hash of the extract it
// was written from. When the issuer changes the page, the hash moves, and that
// one reading is rewritten on the next run. A page that has not changed is
// never re-read, which is the lesson of twoai_enacted_laws: a stage that
// regenerates without cause rewrites published pages with different words
// every day. If a rewrite fails, the last good reading stays on the page and
// the page says which date it was written from.
//
// WHAT THE MODEL MAY AND MAY NOT SAY. Only what the issuer's page says. Cost,
// exam format, prerequisites and validity are each returned empty when the
// page does not state them, and the site then says "not stated on the issuer's
// page" instead of carrying a number from somewhere else. Third-party guides
// quote prep hours and prices; those are their estimates and are not used.
//
// THE PAGE ADDRESS IS PERMANENT, so it was chosen carefully. Each entry is
// minted in twoai_entities as kind 'product', because that table's CHECK
// constraint has no 'certification' or 'course' kind and 'product' is the
// honest nearest: a credential or a course is something a provider offers.
// The uid is a hash of kind plus normalised name and is the URL. Changing the
// kind later would change every address, so it is not to be changed.
//
// WHY taxonomy_slug IS NULL ON THESE PAGES. A section's displayed count is the
// greater of its declared total and the sum of url_count over pages filed
// under its slug. Filing seventeen pages under 'certifications' would make the
// section read 18: seventeen credentials plus the hub. The count is the data,
// seventeen. book-catalog.json is filed the same way for the same reason.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// twoaiLearningPageSections are the sections whose rows get a page each.
// Books are left out on purpose: that section is merged with the Open Library
// catalogue in the template and needs its own treatment.
var twoaiLearningPageSections = []string{"certifications", "courses"}

func twoaiLearningHasPages(section string) bool {
	for _, s := range twoaiLearningPageSections {
		if s == section {
			return true
		}
	}
	return false
}

// twoaiLearningUID mints the permanent identifier for one entry. See the note
// above on why the kind is 'product' and must stay that way.
func twoaiLearningUID(db *sql.DB, name string) string {
	return twoaiEntityID(db, "product", name)
}

const learningReadingSystem = `You read the official web page for one AI certification or course and report what that page says, for a sourced reference site. Your output becomes a public page, so accuracy matters more than completeness.

ABSOLUTE RULES:
- Use ONLY the supplied page text. Never add background knowledge. Never state a price, duration, exam length, passing score, prerequisite or renewal period that the text does not state. If the text does not state something, return an empty string for that field.
- Paraphrase in your own words. Do NOT copy the page's sentences. No marketing language, no superlatives, no exhortation to enrol.
- Plain English. Short sentences. No hyphens used as dashes.
- If the page says the certification, course or exam is retired, being retired, replaced, renamed or no longer offered, or gives a retirement date, say so in "status" with the date exactly as the page gives it. This is the most important field. If the page says nothing of the kind, return an empty string for "status".
- Be exact about WHAT is ending. Say whether it is the whole credential, one exam version, one language version of the exam, or only the preparation material. "The Italian and German versions of the exam will be retired after October 15, 2026" is a correct status. Turning that into "this certification is being retired" is a serious error.
- A bare label with nothing after it, such as "Retirement date:" followed by no date, states nothing. Do not report it.
- The page text may open with a block headed "LINES ON THE PAGE ABOUT RETIREMENT, REPLACEMENT OR PRICE". Those lines were lifted out of the page and placed first so they are not missed. Read them as part of the page. Some will be about other products or training bundles sold on the same page: only report a price as the cost when the page ties it to this credential or course.

Return ONE JSON object and nothing else, with exactly these keys:
{
  "status": "",
  "summary": "two to four sentences on what this credential or course is and what holding or finishing it shows",
  "audience": "one or two sentences on who the page says it is for",
  "covers": ["up to eight short phrases naming the topics or skills the page lists"],
  "format": "how it is assessed or delivered, as the page states",
  "prerequisites": "what the page says is required or recommended beforehand",
  "cost": "the price or pricing model exactly as the page states it",
  "validity": "how long it lasts and how it is renewed, as the page states"
}

If the text has no substantive content about the credential or course, such as a navigation page, a login wall, a cookie notice or an error, output exactly: NOTHING`

type learningReading struct {
	Status        string   `json:"status"`
	Summary       string   `json:"summary"`
	Audience      string   `json:"audience"`
	Covers        []string `json:"covers"`
	Format        string   `json:"format"`
	Prerequisites string   `json:"prerequisites"`
	Cost          string   `json:"cost"`
	Validity      string   `json:"validity"`
}

// twoaiParseLearningReading turns a model reply into a reading, or says why it
// cannot. A reply is refused, never repaired: a page built from a half-parsed
// answer would put guessed structure in front of a reader.
func twoaiParseLearningReading(reply string) (*learningReading, string) {
	s := strings.TrimSpace(reply)
	if s == "" {
		return nil, "empty reply"
	}
	if strings.HasPrefix(strings.ToUpper(s), "NOTHING") {
		return nil, "nothing"
	}
	if isRefusal(s) {
		return nil, "model refused"
	}
	// A local model often wraps JSON in a fence or a sentence of preamble.
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return nil, "no JSON object in reply"
	}
	var r learningReading
	if err := json.Unmarshal([]byte(s[i:j+1]), &r); err != nil {
		return nil, "JSON did not parse: " + err.Error()
	}
	r.Status = strings.TrimSpace(r.Status)
	r.Summary = strings.TrimSpace(r.Summary)
	r.Audience = strings.TrimSpace(r.Audience)
	r.Format = strings.TrimSpace(r.Format)
	r.Prerequisites = strings.TrimSpace(r.Prerequisites)
	r.Cost = strings.TrimSpace(r.Cost)
	r.Validity = strings.TrimSpace(r.Validity)
	var covers []string
	for _, c := range r.Covers {
		if c = strings.TrimSpace(c); c != "" && len(covers) < 8 {
			covers = append(covers, c)
		}
	}
	r.Covers = covers
	if len(strings.Fields(r.Summary)) < 12 {
		return nil, "summary too short to publish"
	}
	if isRefusal(r.Summary) {
		return nil, "model refused inside summary"
	}
	return &r, ""
}

// twoaiLearningReadings is the stage: read each changed issuer page and write
// its reading.
func twoaiLearningReadings(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_learning_readings (
		section_slug text NOT NULL,
		slug text NOT NULL,
		url text NOT NULL,
		reading jsonb,
		reading_hash text,
		source_changed_on timestamptz,
		model text,
		generated_on date,
		try_hash text,
		attempts int NOT NULL DEFAULT 0,
		last_note text,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (section_slug, slug))`); err != nil {
		return err
	}
	if twoaiLLMFor("learning_readings") != "ollama" {
		fmt.Println("twoai_learning_readings: stage is not routed to ollama, skipping")
		return nil
	}
	limit := 40
	if v := strings.TrimSpace(os.Getenv("TWOAI_LEARNING_LIMIT")); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}

	// Rows whose harvested page differs from the one their reading was written
	// from, or which have no reading. Three tries per version of a page, then
	// it waits for the page to change again.
	rows, err := db.Query(`
		SELECT l.section_slug, l.slug, l.name, l.provider, h.url, h.extract, h.content_hash,
		       COALESCE(h.content_changed_on, now())
		FROM twoai_learning l
		JOIN twoai_source_harvest h ON h.url = l.source_url
		LEFT JOIN twoai_learning_readings r ON r.section_slug = l.section_slug AND r.slug = l.slug
		WHERE l.section_slug = ANY($1) AND h.extract <> '' AND h.http_status = 200
		  AND r.reading_hash IS DISTINCT FROM h.content_hash
		  AND NOT (r.try_hash IS NOT DISTINCT FROM h.content_hash AND COALESCE(r.attempts,0) >= 3)
		ORDER BY (r.reading IS NULL) DESC, h.content_changed_on DESC NULLS LAST, l.slug
		LIMIT $2`, "{"+strings.Join(twoaiLearningPageSections, ",")+"}", limit)
	if err != nil {
		return err
	}
	type job struct {
		section, slug, name, provider, url, extract, hash string
		changed                                           time.Time
	}
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.section, &j.slug, &j.name, &j.provider, &j.url, &j.extract, &j.hash, &j.changed) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	written, nothing, failed, flagged := 0, 0, 0, []string{}
	fail := func(j job, note string) {
		db.Exec(`INSERT INTO twoai_learning_readings (section_slug, slug, url, try_hash, attempts, last_note)
			VALUES ($1,$2,$3,$4,1,$5)
			ON CONFLICT (section_slug, slug) DO UPDATE SET url=$3,
				attempts = CASE WHEN twoai_learning_readings.try_hash IS NOT DISTINCT FROM $4
				                THEN twoai_learning_readings.attempts + 1 ELSE 1 END,
				try_hash=$4, last_note=$5, updated_at=now()`,
			j.section, j.slug, j.url, j.hash, note)
	}
	for _, j := range jobs {
		user := "Name as this site lists it: " + j.name + "\nProvider: " + j.provider +
			"\nOfficial page: " + j.url + "\n\nPAGE TEXT:\n" + j.extract
		reply, model, gerr := twoaiGenerate("learning_readings", learningReadingSystem, user)
		if gerr != nil {
			fmt.Fprintf(os.Stderr, "twoai_learning_readings: %s/%s: %v\n", j.section, j.slug, gerr)
			// Ollama down is a whole-run condition. Stop, and do not spend an
			// attempt on a page the model never saw.
			if strings.Contains(gerr.Error(), "marked down") || strings.Contains(gerr.Error(), "unreachable") {
				fmt.Fprintf(os.Stderr, "twoai_learning_readings: ollama is down, %d page(s) left for the next run\n", len(jobs)-written-nothing-failed)
				break
			}
			failed++
			fail(j, "generate: "+gerr.Error())
			continue
		}
		r, why := twoaiParseLearningReading(reply)
		if r == nil {
			if why == "nothing" {
				nothing++
			} else {
				failed++
			}
			fail(j, why)
			continue
		}
		rj, _ := json.Marshal(r)
		if _, err := db.Exec(`INSERT INTO twoai_learning_readings
			(section_slug, slug, url, reading, reading_hash, source_changed_on, model, generated_on, try_hash, attempts, last_note)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7,current_date,$5,0,NULL)
			ON CONFLICT (section_slug, slug) DO UPDATE SET url=$3, reading=$4::jsonb, reading_hash=$5,
				source_changed_on=$6, model=$7, generated_on=current_date, try_hash=$5, attempts=0,
				last_note=NULL, updated_at=now()`,
			j.section, j.slug, j.url, string(rj), j.hash, j.changed, model); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_learning_readings store:", err)
			failed++
			continue
		}
		written++
		if r.Status != "" {
			flagged = append(flagged, j.slug)
		}
		fmt.Printf("twoai_learning_readings: %s/%s <- %s (%d chars of page)\n", j.section, j.slug, model, len(j.extract))
	}

	// THE LOUD LINE. An issuer page that says retired, replaced or renamed is
	// the thing this stage was built to notice. The curated row's level, note
	// and renewal are typed by a person, so the model's finding is a prompt
	// for that person, named here, and also shown on the page itself.
	if len(flagged) > 0 {
		fmt.Printf("twoai_learning_readings: ISSUER PAGE REPORTS A STATUS CHANGE for %d entr(ies), check the curated row: %s\n",
			len(flagged), strings.Join(flagged, " "))
	}
	var have, total, unseen int
	db.QueryRow(`SELECT count(*) FROM twoai_learning WHERE section_slug = ANY($1)`,
		"{"+strings.Join(twoaiLearningPageSections, ",")+"}").Scan(&total)
	db.QueryRow(`SELECT count(*) FROM twoai_learning_readings WHERE reading IS NOT NULL`).Scan(&have)
	db.QueryRow(`SELECT count(*) FROM twoai_learning l LEFT JOIN twoai_source_harvest h ON h.url = l.source_url
		WHERE l.section_slug = ANY($1) AND (h.url IS NULL OR h.http_status <> 200 OR COALESCE(h.extract,'') = '')`,
		"{"+strings.Join(twoaiLearningPageSections, ",")+"}").Scan(&unseen)
	fmt.Printf("twoai_learning_readings: written=%d nothing_to_say=%d failed=%d | %d of %d entries have a reading, %d issuer page(s) not yet readable\n",
		written, nothing, failed, have, total, unseen)
	return nil
}

// twoaiLearningEmitPage writes the page document for one entry. Called from
// twoaiHardware's learning loop, which owns the curated row. An entry with no
// reading yet still gets its page, built from the curated row alone, and the
// page says the issuer's page has not been read yet. It never waits on the
// model to exist.
func twoaiLearningEmitPage(db *sql.DB, today, section, sectionName, sectionUID string, l map[string]any) error {
	slug, _ := l["slug"].(string)
	uid, _ := l["uid"].(string)
	if slug == "" || uid == "" {
		return nil
	}
	doc := map[string]any{
		"uid": uid, "shape": "learning-entry", "section": section,
		"section_name": sectionName, "section_uid": sectionUID,
		"generated": today, "entry": l,
		// A top-level name, because twoaiDocTitle titles a document from its
		// own name or from a fixed list of nested objects, and "entry" is not
		// on that list. Without it these pages would drop out of the Ask index
		// silently, which is how 1,896 MCP pages once did.
		"name": l["name"],
	}
	var reading sql.NullString
	var model, genOn sql.NullString
	var changed sql.NullTime
	var fetched sql.NullString
	var status sql.NullInt64
	db.QueryRow(`SELECT r.reading::text, r.model, r.generated_on::text, r.source_changed_on
		FROM twoai_learning_readings r WHERE r.section_slug=$1 AND r.slug=$2 AND r.reading IS NOT NULL`,
		section, slug).Scan(&reading, &model, &genOn, &changed)
	if src, _ := l["source_url"].(string); src != "" {
		db.QueryRow(`SELECT fetched_on::text, http_status FROM twoai_source_harvest WHERE url=$1`, src).Scan(&fetched, &status)
	}
	if reading.Valid && reading.String != "" {
		var r learningReading
		if json.Unmarshal([]byte(reading.String), &r) == nil {
			doc["reading"] = r
			doc["reading_model"] = model.String
			doc["reading_written_on"] = genOn.String
			if changed.Valid {
				doc["source_changed_on"] = changed.Time.UTC().Format("2006-01-02")
			}
		}
	}
	if fetched.Valid {
		doc["source_read_on"] = fetched.String
		doc["source_http_status"] = status.Int64
	}
	j, _ := json.Marshal(doc)
	_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ($1,'learning-entry',$2::jsonb,NULL,1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=NULL, url_count=1, updated_at=now()`,
		"learn/entry-"+section+"-"+slug+".json", string(j))
	return err
}
