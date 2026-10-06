package main

// AI Incident Database: the harm ledger under the daily briefing.
//
// The AIID (incidentdatabase.ai), run by the Responsible AI Collaborative,
// catalogs real-world harms from deployed AI systems. Its incident
// collections are CC BY-SA, which is share-alike, and this site is not
// CC BY-SA. So we take the same care taken with OpenStreetMap and with
// Wikipedia prose, and publish only what is safe to publish:
//
//	WE PUBLISH  the incident number, the report headline as the label on a
//	            link, the publisher's own URL and domain, the date, and a
//	            link back to the AIID incident page. Facts and links.
//	WE DO NOT   reproduce the AIID's editorial descriptions or narrative,
//	            and we do not reproduce the publisher article excerpt the
//	            feed carries. Neither is ours to republish.
//
// A headline in another language is shown in English, labelled with the
// language it was translated from, and the publisher's wording is kept in
// title_original (Stephen's rule, bridge row 508, 2026-10-06).
//
// AIID is credited by name and link wherever these rows render, with its
// licence stated. The rows are marked cite_only so they never enter the
// training corpus, exactly like the ODbL facility registry.
//
// Source: the public RSS feed, which carries the newest reports across all
// incidents and, critically, links to the ORIGINAL PUBLISHER rather than to
// an aggregator. That is what makes this publishable on the daily briefing
// at all: every row sends the reader to the outlet that did the reporting.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const twoaiAIIDFeed = "https://incidentdatabase.ai/rss.xml"

// The feed hides the incident and report numbers in a cite link appended to
// the description, e.g. (https://incidentdatabase.ai/cite/1661#7853).
var aiidCiteRe = regexp.MustCompile(`incidentdatabase\.ai/cite/(\d+)(?:#(\d+))?`)

type aiidItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

type aiidFeed struct {
	Items []aiidItem `xml:"channel>item"`
}

func twoaiIncidentsHarvest(db *sql.DB) {
	b, err := twoaiGridGet(twoaiAIIDFeed)
	if err != nil {
		fmt.Println("twoai_incidents: feed fetch failed:", err, "(keeping prior rows)")
		return
	}
	var f aiidFeed
	if err := xml.Unmarshal(b, &f); err != nil || len(f.Items) == 0 {
		fmt.Printf("twoai_incidents: feed unparsed: %v items=%d\n", err, len(f.Items))
		return
	}
	twoaiEnglishSchema(db)
	stored, skipped, translated := 0, 0, 0
	for _, it := range f.Items {
		link := strings.TrimSpace(it.Link)
		title := strings.TrimSpace(it.Title)
		if link == "" || title == "" {
			skipped++
			continue
		}
		// The publisher's own domain. A row without one is not useful to a
		// reader and is dropped rather than shown as a bare link.
		host := ""
		if u, err := url.Parse(link); err == nil {
			host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		}
		if host == "" || strings.Contains(host, "incidentdatabase.ai") {
			skipped++
			continue
		}
		var incID, repID int
		if m := aiidCiteRe.FindStringSubmatch(it.Description); m != nil {
			incID, _ = strconv.Atoi(m[1])
			if len(m) > 2 && m[2] != "" {
				repID, _ = strconv.Atoi(m[2])
			}
		}
		if incID == 0 {
			skipped++ // without an incident number we cannot attribute it properly
			continue
		}
		var pub any
		if t, err := time.Parse(time.RFC1123, it.PubDate); err == nil {
			pub = t.UTC().Format("2006-01-02")
		} else if t, err := time.Parse(time.RFC1123Z, it.PubDate); err == nil {
			pub = t.UTC().Format("2006-01-02")
		}
		guid := strings.TrimSpace(it.GUID)
		if guid == "" {
			guid = fmt.Sprintf("aiid:%d:%d", incID, repID)
		}
		// THE TITLE IN ENGLISH, AND NEVER BACK. Stephen's rule, bridge row
		// 508, 2026-10-06: everything visitors see is in English. A foreign
		// headline is translated here, the publisher's own wording kept in
		// title_original, and a title that is already an English rendering,
		// the pipeline's or one the content project wrote by hand (guids
		// 82754709 and 1ee8bf63 on 2026-10-06), is never overwritten by the
		// next harvest: only title_original follows the feed.
		var cur incidentTitle
		var curOrig, curLang sql.NullString
		exists := db.QueryRow(`SELECT title, title_original, title_lang FROM twoai_incidents WHERE guid=$1`, guid).
			Scan(&cur.Title, &curOrig, &curLang) == nil
		cur.Original, cur.Lang = curOrig.String, curLang.String
		feedLang, feedEN := twoaiEnglishLang(title), ""
		if feedLang != "" {
			if incidentTitleKept(exists, cur, title, true) {
				if _, l, ok := twoaiEnglishCached(db, title); ok && l != "en" {
					feedLang = l
				}
			} else if en, l, err := twoaiEnglish(db, "translate_title", title); err == nil {
				if l == "en" {
					feedLang = ""
				} else {
					feedLang, feedEN = l, en
				}
			} else {
				fmt.Fprintf(os.Stderr, "twoai_incidents: %s left untranslated for now: %v\n", guid, err)
			}
		}
		m := incidentTitleMerge(exists, cur, title, feedLang, feedEN)
		if m.Original != "" && m.Title != m.Original {
			translated++
			if !exists || cur.Title != m.Title {
				twoaiEnglishLogged(db, "twoai_incidents", guid, "title", m.Lang, m.Original, m.Title, "translate_title", "srj")
			}
		}
		// NOTE: it.Description is deliberately never stored. It holds the
		// publisher's article excerpt, which is not ours to republish.
		if _, err := db.Exec(`INSERT INTO twoai_incidents
			(guid, incident_id, report_id, title, url, domain, published, title_original, title_lang)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''))
			ON CONFLICT (guid) DO UPDATE SET incident_id=EXCLUDED.incident_id,
				report_id=EXCLUDED.report_id, title=EXCLUDED.title, url=EXCLUDED.url,
				domain=EXCLUDED.domain, published=EXCLUDED.published,
				title_original=EXCLUDED.title_original, title_lang=EXCLUDED.title_lang, last_seen=now()`,
			guid, incID, repID, m.Title, link, host, pub, m.Original, m.Lang); err == nil {
			stored++
		} else {
			fmt.Fprintln(os.Stderr, "twoai_incidents: upsert:", err)
		}
	}
	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_incidents`).Scan(&total)
	fmt.Printf("twoai_incidents: feed items=%d stored=%d skipped=%d translated=%d total=%d\n",
		len(f.Items), stored, skipped, translated, total)
}

// incidentTitle is a stored incident headline: the title shown, the
// publisher's original when the title is a translation, and its language.
// A row whose title equals its original is a foreign headline still waiting
// for its translation.
type incidentTitle struct {
	Title, Original, Lang string
}

// incidentTitleKept reports whether the stored title is already an English
// rendering of a foreign headline, which a re-harvest must leave alone.
// Either the row says so (an original that differs from the title), or the
// row predates title_original and holds English where the feed now carries a
// foreign headline: a hand translation.
func incidentTitleKept(exists bool, cur incidentTitle, feed string, foreign bool) bool {
	if !exists {
		return false
	}
	if cur.Original != "" && cur.Title != cur.Original {
		return true
	}
	return cur.Original == "" && foreign && cur.Title != feed && twoaiEnglishLang(cur.Title) == ""
}

// incidentTitleMerge decides what a harvest writes. feedLang is "" when the
// feed's headline is English, feedEN "" when no translation could be had.
func incidentTitleMerge(exists bool, cur incidentTitle, feed, feedLang, feedEN string) incidentTitle {
	foreign := feedLang != ""
	if incidentTitleKept(exists, cur, feed, foreign) {
		lang := cur.Lang
		if foreign {
			lang = feedLang
		}
		return incidentTitle{Title: cur.Title, Original: feed, Lang: lang}
	}
	if !foreign {
		return incidentTitle{Title: feed}
	}
	if feedEN != "" && feedEN != feed {
		return incidentTitle{Title: feedEN, Original: feed, Lang: feedLang}
	}
	return incidentTitle{Title: feed, Original: feed, Lang: feedLang}
}

// twoaiIncidentEnglish puts a stored incident title into English if it is not
// yet, a row left waiting by the harvest or one stored before 2026-10-06,
// writes the translation back to every row carrying that headline and
// returns title, original and language. An English title passes through.
func twoaiIncidentEnglish(db *sql.DB, id int, title, orig, lang string) (string, string, string) {
	pending := orig != "" && title == orig
	legacy := orig == "" && twoaiEnglishLang(title) != ""
	if !pending && !legacy {
		return title, orig, lang
	}
	en, l := twoaiTranslateTitle(db, title)
	if l == "" || l == "en" || en == title {
		return title, orig, lang
	}
	if res, err := db.Exec(`UPDATE twoai_incidents SET title=$1, title_original=$2, title_lang=$3
		WHERE incident_id=$4 AND title=$2 AND (title_original IS NULL OR title_original=$2)`, en, title, l, id); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			twoaiEnglishLogged(db, "twoai_incidents", fmt.Sprintf("incident %d", id), "title", l, title, en, "translate_title", "srj")
		}
	}
	return en, title, l
}

// twoaiIncidentReports reads every report of an incident, newest first, with
// each outlet's headline in English and the original kept beside it.
func twoaiIncidentReports(db *sql.DB, id int) []incidentReport {
	rows, err := db.Query(`SELECT title, url, domain, COALESCE(published::text,''),
			COALESCE(title_original,''), COALESCE(title_lang,'')
		FROM twoai_incidents WHERE incident_id=$1
		ORDER BY published DESC NULLS LAST, report_id`, id)
	if err != nil {
		return nil
	}
	var reps []incidentReport
	for rows.Next() {
		var r incidentReport
		if rows.Scan(&r.Title, &r.URL, &r.Domain, &r.Published, &r.TitleOriginal, &r.TitleLang) == nil {
			reps = append(reps, r)
		}
	}
	rows.Close()
	for i := range reps {
		r := &reps[i]
		r.Title, r.TitleOriginal, r.TitleLang = twoaiIncidentEnglish(db, id, r.Title, r.TitleOriginal, r.TitleLang)
		if r.TitleOriginal == "" || r.TitleOriginal == r.Title {
			r.TitleOriginal, r.TitleLang = "", ""
		}
	}
	return reps
}

// twoaiIncidentEnglishPatch is what an archived incident page needs to read
// in English: its reports re-read from twoai_incidents, and its headline,
// which is one of those reports' titles in whichever language it was stored
// in. Empty when nothing on the page was ever translated, so a page that was
// always English is not rewritten.
func twoaiIncidentEnglishPatch(db *sql.DB, id int, title string) map[string]any {
	reps := twoaiIncidentReports(db, id)
	patch := map[string]any{}
	for _, r := range reps {
		if r.TitleOriginal != "" {
			patch["reports"] = reps
			break
		}
	}
	for _, r := range reps {
		if r.TitleOriginal != "" && (title == r.TitleOriginal || title == r.Title) {
			patch["title"], patch["title_en"] = r.Title, r.Title
			patch["title_original"], patch["title_lang"] = r.TitleOriginal, r.TitleLang
			break
		}
	}
	return patch
}

type incidentReport struct {
	Title string `json:"title"`
	// The outlet's own headline and its language when Title is our English
	// rendering of it. The page shows Title with "Translated from <language>"
	// beside it (bridge row 508: the source list is visitor text too).
	TitleOriginal string `json:"title_original,omitempty"`
	TitleLang     string `json:"title_lang,omitempty"`
	URL           string `json:"url"`
	Domain        string `json:"domain"`
	Published     string `json:"published"`
}

type incidentOut struct {
	IncidentID int    `json:"incident_id"`
	Title      string `json:"title"`
	// TitleEN and TitleLang: the headline in English, and the language the
	// original is in. Stephen, 2026-09-11, after seeing incident 1684 titled
	// "Motie van het lid Beckerman over een bevoegde instantie aanwijzen..."
	// above an English summary: does the pipeline translate? It did not. The
	// summary and reading are written by Claude, which writes English from
	// whatever it reads, so they were always English; the title is copied
	// verbatim from the AIID feed and was never touched. TitleEN is ours, a
	// translation, and is rendered as such; Title stays the publisher's own
	// headline, still the label on the link, which is the AIID rule above.
	//
	// Since 2026-10-06 (bridge row 508) Title itself is English: the
	// translation is made at harvest and stored in twoai_incidents.title,
	// with the publisher's headline in TitleOriginal. TitleEN repeats Title
	// for pages built by older templates.
	TitleEN       string `json:"title_en,omitempty"`
	TitleLang     string `json:"title_lang,omitempty"`
	TitleOriginal string `json:"title_original,omitempty"`
	URL           string `json:"url"`
	Domain        string `json:"domain"`
	Published     string `json:"published"`
	CiteURL       string `json:"cite_url"`
	// An incident is a story, not a link. These carry the same treatment the
	// daily briefing gives a news story: an original summary written from the
	// reporting, and every outlet that carried it.
	Summary       string           `json:"summary,omitempty"`
	SummaryDomain string           `json:"summary_domain,omitempty"`
	SummaryURL    string           `json:"summary_url,omitempty"`
	Reports       []incidentReport `json:"reports,omitempty"`
	OutletCount   int              `json:"outlet_count"`
}

// twoaiIncidentsRecent returns the newest reports for the briefing, one per
// incident so a heavily covered incident cannot crowd out the rest.
func twoaiIncidentsRecent(db *sql.DB, n int) []incidentOut {
	out := []incidentOut{}
	rows, err := db.Query(`SELECT DISTINCT ON (incident_id)
			incident_id, title, url, domain, COALESCE(published::text,''),
			COALESCE(title_original,''), COALESCE(title_lang,'')
		FROM twoai_incidents
		WHERE published IS NOT NULL
		ORDER BY incident_id, published DESC, first_seen DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var o incidentOut
		if rows.Scan(&o.IncidentID, &o.Title, &o.URL, &o.Domain, &o.Published, &o.TitleOriginal, &o.TitleLang) != nil {
			continue
		}
		o.CiteURL = fmt.Sprintf("https://incidentdatabase.ai/cite/%d", o.IncidentID)
		out = append(out, o)
	}
	// Newest first, then cap.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Published > out[i].Published {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	// ROOT CAUSE OF INCIDENT 1722, 2026-10-06. The headline shown is the
	// newest report's title, and for 1722 that was navbharattimes' Hindi
	// headline. twoaiTranslateTitle returned it untranslated because it
	// still required ANTHROPIC_API_KEY, which was removed on 2026-09-17, so
	// title_en came back equal to the Hindi title with an empty language and
	// the page led with it; the report list under it was never translated at
	// all. The translation now happens at harvest through twoaiGenerate, and
	// anything still foreign here is put into English before it is used.
	for i := range out {
		o := &out[i]
		o.Title, o.TitleOriginal, o.TitleLang = twoaiIncidentEnglish(db, o.IncidentID, o.Title, o.TitleOriginal, o.TitleLang)
		if o.TitleOriginal == "" || o.TitleOriginal == o.Title {
			o.TitleOriginal, o.TitleLang = "", ""
		} else {
			o.TitleEN = o.Title
		}
	}
	twoaiIncidentsEnrich(db, out)
	return out
}

// twoaiTranslateTitle returns an English rendering of a headline and the
// ISO 639-1 code of the language it was written in. English in, English out
// with lang "en" and no call made once the cache holds it.
//
// One call per distinct headline, ever: the result is cached on the
// headline's hash in twoai_translations, so a title seen on the briefing
// today and on its incident page tomorrow costs one call, not two, and
// nothing is retranslated on a rebuild. The model is asked for a literal
// rendering, not a rewrite, and to leave names and proper nouns alone.
//
// Until 2026-10-06 this called Claude Haiku through twoaiClaudeCall and
// returned the original untranslated whenever ANTHROPIC_API_KEY was unset,
// which it has been since 2026-09-17; that is why incident 1722 led with
// Hindi. It now goes through twoaiEnglish (twoai_english.go): an offline
// language check first, so English costs nothing, then twoaiGenerate under
// the stage name translate_title, on the pipeline model. On any failure the
// original comes back with an empty lang and the sweep tries again later.
func twoaiTranslateTitle(db *sql.DB, title string) (string, string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", ""
	}
	if en, lang := twoaiEnglishLive(db, title); lang != "" {
		return en, lang
	}
	if twoaiEnglishLang(title) == "" {
		return title, "en"
	}
	return title, ""
}

// EVERY OUTLET, AND AN ORIGINAL SUMMARY. Stephen, 2026-08-30: we are just
// doing links to pages, and the incidents need what the daily news gets.
//
// He is right, and the data was already there to do it. AIID catalogues an
// incident once and links every report of it, so twoai_incidents holds
// several rows per incident_id, and this section was rendering one row each
// and discarding the rest. An incident is a story carried by several
// outlets, exactly like a news story, and it gets the same treatment: a
// summary written in our own words from the reporting, and every outlet
// listed.
//
// The copyright line is unchanged and is the reason the summary is written
// rather than copied. AIID's own write-ups are CC BY-SA and the publishers'
// articles are theirs; we read the reporting, state the facts in our own
// words, and link out. The summary is cached on the report URL in
// pipeline.documents, the same store and the same summarizer the daily
// briefing uses, so an incident is summarised once and never again.
func twoaiIncidentsEnrich(db *sql.DB, out []incidentOut) {
	for i := range out {
		out[i].Reports = twoaiIncidentReports(db, out[i].IncidentID)
		domains := map[string]bool{}
		for _, r := range out[i].Reports {
			domains[r.Domain] = true
		}
		out[i].OutletCount = len(domains)

		// The summary comes from whichever report we can actually read.
		// A cached summary is reused; a paywalled or unreadable report is
		// skipped and the next one tried; if none can be read the entry
		// still publishes with its links, which is what it does today.
		for _, r := range out[i].Reports {
			var cached string
			db.QueryRow(`SELECT COALESCE(summary,'') FROM pipeline.documents WHERE url=$1`, r.URL).Scan(&cached)
			if cached != "" {
				out[i].Summary, out[i].SummaryDomain, out[i].SummaryURL = cached, r.Domain, r.URL
				break
			}
			text, err := twoaiIncidentFetchText(r.URL)
			if err != nil || len(text) < 600 {
				continue
			}
			sum, err := anthropicSummarize(r.Title, text)
			if err != nil || sum == "" {
				continue
			}
			db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, summary)
				SELECT id, $1, md5($2), $2, $3, $4 FROM pipeline.sources WHERE key='aiid'
				ON CONFLICT DO NOTHING`, fmt.Sprint("aiid:", out[i].IncidentID), r.URL, r.Title, sum)
			db.Exec(`UPDATE pipeline.documents SET summary=$1 WHERE url=$2 AND COALESCE(summary,'')=''`, sum, r.URL)
			out[i].Summary, out[i].SummaryDomain, out[i].SummaryURL = sum, r.Domain, r.URL
			break
		}
		// FALLBACK TO THE DATABASE'S OWN DESCRIPTION. Stephen, 2026-09-28, on
		// incident 1713: the page gave no account of what happened. Its only
		// report was a McClatchy paper that refuses our fetcher; 9 of 66
		// incidents were in the same state, every one reported only by an
		// outlet behind a paywall or a bot wall (Reuters, NYT, WSJ, FT, the
		// Washington Post). The AI Incident Database writes a description of
		// every incident on its cite page, which the page already links, so
		// when no report can be read the summary is written from that, in our
		// own words, and attributed to it.
		if out[i].Summary == "" {
			title := out[i].Title
			if len(out[i].Reports) > 0 {
				title = out[i].Reports[0].Title
			}
			if sum, cite := twoaiIncidentDescSummary(db, out[i].IncidentID, title); sum != "" {
				out[i].Summary, out[i].SummaryDomain, out[i].SummaryURL = sum, "incidentdatabase.ai", cite
			}
		}
	}
}

// twoaiIncidentDescSummary returns a summary written from the AI Incident
// Database's own description of an incident, cached on the cite URL, and the
// cite URL. Empty when there is no usable description.
func twoaiIncidentDescSummary(db *sql.DB, id int, title string) (string, string) {
	cite := fmt.Sprintf("https://incidentdatabase.ai/cite/%d", id)
	var cached string
	db.QueryRow(`SELECT COALESCE(summary,'') FROM pipeline.documents WHERE url=$1`, cite).Scan(&cached)
	if cached != "" {
		return cached, cite
	}
	desc := twoaiIncidentDescription(cite)
	if len(desc) < 150 {
		return "", cite
	}
	prompt := "Rewrite this incident description as one short paragraph of 60 to 110 words, entirely in your own words, for a news reference page. " +
		"State what happened, who was involved and what followed, using only the facts in the description; add nothing, and do not speculate. " +
		"Plain English, commas rather than dashes, no quotation longer than five words. Output only the paragraph.\n\n" +
		"Headline: " + title + "\n\nDescription:\n" + desc
	sum, _, err := twoaiGenerate("news_summary", "", prompt)
	sum = strings.TrimSpace(sum)
	if err != nil || sum == "" {
		return "", cite
	}
	db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, summary)
		SELECT id, $1, md5($2), $2, $3, $4 FROM pipeline.sources WHERE key='aiid'
		ON CONFLICT DO NOTHING`, fmt.Sprint("aiid-desc:", id), cite, title, sum)
	db.Exec(`UPDATE pipeline.documents SET summary=$1 WHERE url=$2 AND COALESCE(summary,'')=''`, sum, cite)
	return sum, cite
}

// twoaiIncidentDescription reads the editor-written Description section of an
// AI Incident Database cite page. Empty when the page or the section is
// missing; the caller then leaves the summary empty, as before.
func twoaiIncidentDescription(cite string) string {
	client := &http.Client{Timeout: 25 * time.Second}
	req, _ := http.NewRequest("GET", cite, nil)
	req.Header.Set("User-Agent", "theworldofai.org incident watch (contact: stephen@srjconsultingservices.com)")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	page := string(b)
	if m := regexp.MustCompile(`(?is)description-section[^>]*>(.*?)</(?:section|div)>`).FindStringSubmatch(page); m != nil {
		txt := html.UnescapeString(regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(m[1], " "))
		txt = strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(txt, " "))
		txt = strings.TrimLeft(strings.TrimSpace(strings.TrimPrefix(txt, "Description")), ": ")
		if len(txt) >= 150 {
			return txt
		}
	}
	if m := regexp.MustCompile(`(?i)<meta name="description" content="([^"]{150,})"`).FindStringSubmatch(page); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

// Reads one report page as text. Publisher prose never leaves this function:
// it goes to the summarizer and is discarded.
func twoaiIncidentFetchText(u string) (string, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "theworldofai.org incident watch (contact: stephen@srjconsultingservices.com)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if err != nil {
		return "", err
	}
	page := regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|header|footer|aside)[^>]*>.*?</(script|style|noscript|svg|nav|header|footer|aside)>`).ReplaceAllString(string(b), " ")
	txt := html.UnescapeString(regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(page, " "))
	txt = strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(txt, " "))
	if len(txt) > 20000 {
		txt = txt[:20000]
	}
	return txt, nil
}

// closeOpenAnchors repairs an <a> that an author closed with the wrong tag.
//
// The rule is narrow on purpose: an anchor whose text runs straight into a
// BLOCK closing tag was never closed, because valid markup would have put
// </a> first. Anything else is left exactly as written. It is a repair, not a
// sanitiser, and it must never rewrite HTML that is already correct.
func closeOpenAnchors(h string) string {
	return anchorLeakRe.ReplaceAllString(h, "$1$2</a>$3")
}

var anchorLeakRe = regexp.MustCompile(`(?is)(<a [^>]*>)([^<]*)(</(?:li|p|td|th|h[1-6]|div|ul|ol|blockquote)>)`)

// A PAGE PER INCIDENT. Stephen, 2026-08-31: a full page describing the
// report, with the link to the publisher only at the very bottom, so the
// reader is reading our content the whole way down.
//
// The briefing sends a reader straight out on the headline, which is the
// opposite of that. An incident already has everything a page needs: the
// facts AIID catalogues, the summary we write from the reporting, and every
// outlet that carried it. What it lacked was a reading, so a Sonnet passage
// says what the incident shows about deployed AI, written from these facts
// and nothing else and cached on their hash. The publisher links live in a
// sources block at the foot of the page.
//
// Slug is the AIID incident number, which is stable, public and citable:
// /ai-news/incident/1661/ will always be incident 1661.
const incidentReadingSystem = `You write for The World of AI, a reference site that catalogues what deployed AI systems actually do in the world.

You are given one incident from the AI Incident Database: its title, the outlets that reported it, their dates, and a summary of the reporting written in our own words. Write what this incident shows, in two or three short paragraphs, for a reader who has just read that summary.

Rules, in order:
1. Use ONLY the facts given. Never add a company, product, number, date, ruling or consequence that is not in them. If something is unknown, say it is not established.
2. Do not retell the summary. The reader has it. Say what kind of failure this is, where in a deployment it happened, and what it would have taken to catch it.
3. Name the failure mode plainly when the facts support it: a model asserting something false as fact, a system deployed without a human check, an impersonation, a system used outside the conditions it was built for. Do not reach for a category the facts do not show.
4. One sentence on what is NOT established: an allegation is not a finding, a lawsuit is not a verdict, and a report is not proof of intent.
5. Plain declarative sentences, one idea each. Commas rather than dashes. No speculation about motive, no advice, no moralising.
Return the paragraphs only, separated by blank lines, no heading, no preamble.`

func twoaiIncidentPages(db *sql.DB, incidents []incidentOut, today string) int {
	model := os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	built := 0
	for _, inc := range incidents {
		if inc.IncidentID == 0 || inc.Title == "" {
			continue
		}
		path := fmt.Sprintf("news/incident-%d.json", inc.IncidentID)

		// The reading, cached on the facts it was written from, so an
		// incident is interpreted once and again only if its reporting grows.
		facts, _ := json.MarshalIndent(map[string]any{
			"title": inc.Title, "incident_id": inc.IncidentID,
			"summary": inc.Summary, "reports": inc.Reports,
			"outlet_count": inc.OutletCount,
		}, "", "  ")
		h := sha256.Sum256(facts)
		hash := hex.EncodeToString(h[:8])
		metric := fmt.Sprintf("incident:%d", inc.IncidentID)
		var reading, rModel, rOn string
		db.QueryRow(`SELECT body, model, generated_on::text FROM twoai_industry_analysis
			WHERE metric=$1 AND data_hash=$2`, metric, hash).Scan(&reading, &rModel, &rOn)
		// The Anthropic key gate stayed after twoaiClaudeCall was routed to
		// Ollama, so no incident reading had been written since the key was
		// cut on 2026-09-17; found 2026-09-28. Same fix as twoaiThinSense.
		if reading == "" && inc.Summary != "" && (os.Getenv("ANTHROPIC_API_KEY") != "" || twoaiLLMFor("") != "anthropic") {
			if body, err := twoaiClaudeCall(model, incidentReadingSystem,
				"The incident:\n"+string(facts)+"\n\nWrite it now."); err == nil && len(body) > 150 {
				db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
					VALUES ($1,$2,$3,$4,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
					metric, hash, model, body)
				reading, rModel, rOn = body, model, today
			}
		}

		doc := map[string]any{
			"shape": "incident", "incident_id": inc.IncidentID, "title": inc.Title,
			"title_en": inc.TitleEN, "title_lang": inc.TitleLang, "title_original": inc.TitleOriginal,
			"summary": inc.Summary, "summary_domain": inc.SummaryDomain,
			"summary_url": inc.SummaryURL, "reports": inc.Reports,
			"outlet_count": inc.OutletCount, "published": inc.Published,
			"cite_url": inc.CiteURL, "generated": today,
		}
		if reading != "" {
			doc["reading"] = map[string]any{"body": reading, "model": rModel, "generated_on": rOn}
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'incident',$2::jsonb,NULL,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				url_count=1, updated_at=now()`, path, string(j)); err == nil {
			built++
		}
	}
	// An incident that drops out of the window keeps its page: a URL that has
	// been published never moves, and the record of a harm should not vanish
	// because newer ones arrived.
	//
	// ARCHIVED PAGES, 2026-09-28. Those pages were never touched again, so
	// the staleness report listed them daily as weeks overdue, and the six
	// with no summary (reported only behind paywalls) stayed empty after the
	// database description fallback arrived. Each out-of-window page is now
	// marked archived with a yearly refresh contract, and one with no summary
	// gets one from the database's description, a few a run.
	inWindow := map[string]bool{}
	for _, inc := range incidents {
		inWindow[fmt.Sprintf("news/incident-%d.json", inc.IncidentID)] = true
	}
	rows, err := db.Query(`SELECT path, (data->>'incident_id')::int, COALESCE(data->>'title',''),
			COALESCE(data->'reports'->0->>'title',''), COALESCE(data->>'summary','')
		FROM twoai_pages WHERE path LIKE 'news/incident-%' AND kind='incident'`)
	if err == nil {
		type old struct {
			path, title, rtitle, summary string
			id                           int
		}
		var olds []old
		for rows.Next() {
			var o old
			if rows.Scan(&o.path, &o.id, &o.title, &o.rtitle, &o.summary) == nil && !inWindow[o.path] {
				olds = append(olds, o)
			}
		}
		rows.Close()
		filled := 0
		for _, o := range olds {
			// In English too (bridge row 508): an archived page keeps the
			// headline and report titles it was built with, and before
			// 2026-10-06 some of those were left in their own language.
			patch := twoaiIncidentEnglishPatch(db, o.id, o.title)
			patch["archived"], patch["refresh_every_days"] = true, 365
			if o.summary == "" && filled < 6 {
				t := o.rtitle
				if t == "" {
					t = o.title
				}
				if sum, cite := twoaiIncidentDescSummary(db, o.id, t); sum != "" {
					patch["summary"], patch["summary_domain"], patch["summary_url"] = sum, "incidentdatabase.ai", cite
					filled++
				}
			}
			pj, _ := json.Marshal(patch)
			db.Exec(`UPDATE twoai_pages SET data = data || $2::jsonb, updated_at = now()
				WHERE path = $1 AND (data || $2::jsonb)::text IS DISTINCT FROM data::text`, o.path, string(pj))
		}
		fmt.Printf("publish_news: archived incident pages=%d summaries filled=%d\n", len(olds), filled)
	}
	fmt.Printf("publish_news: incident pages built=%d\n", built)
	return built
}
