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
	stored, skipped := 0, 0
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
		// NOTE: it.Description is deliberately never stored. It holds the
		// publisher's article excerpt, which is not ours to republish.
		if _, err := db.Exec(`INSERT INTO twoai_incidents
			(guid, incident_id, report_id, title, url, domain, published)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (guid) DO UPDATE SET incident_id=EXCLUDED.incident_id,
				report_id=EXCLUDED.report_id, title=EXCLUDED.title, url=EXCLUDED.url,
				domain=EXCLUDED.domain, published=EXCLUDED.published, last_seen=now()`,
			guid, incID, repID, title, link, host, pub); err == nil {
			stored++
		}
	}
	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_incidents`).Scan(&total)
	fmt.Printf("twoai_incidents: feed items=%d stored=%d skipped=%d total=%d\n",
		len(f.Items), stored, skipped, total)
}

type incidentReport struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Domain    string `json:"domain"`
	Published string `json:"published"`
}

type incidentOut struct {
	IncidentID int    `json:"incident_id"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Domain     string `json:"domain"`
	Published  string `json:"published"`
	CiteURL    string `json:"cite_url"`
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
			incident_id, title, url, domain, COALESCE(published::text,'')
		FROM twoai_incidents
		WHERE published IS NOT NULL
		ORDER BY incident_id, published DESC, first_seen DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var o incidentOut
		if rows.Scan(&o.IncidentID, &o.Title, &o.URL, &o.Domain, &o.Published) != nil {
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
	twoaiIncidentsEnrich(db, out)
	return out
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
		rows, err := db.Query(`SELECT title, url, domain, COALESCE(published::text,'')
			FROM twoai_incidents WHERE incident_id=$1
			ORDER BY published DESC NULLS LAST, report_id`, out[i].IncidentID)
		if err != nil {
			continue
		}
		domains := map[string]bool{}
		for rows.Next() {
			var r incidentReport
			if rows.Scan(&r.Title, &r.URL, &r.Domain, &r.Published) != nil {
				continue
			}
			out[i].Reports = append(out[i].Reports, r)
			domains[r.Domain] = true
		}
		rows.Close()
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
	}
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
		model = "claude-sonnet-4-6"
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
		if reading == "" && inc.Summary != "" && os.Getenv("ANTHROPIC_API_KEY") != "" {
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
	fmt.Printf("publish_news: incident pages built=%d\n", built)
	return built
}
