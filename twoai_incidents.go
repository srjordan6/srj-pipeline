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
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/url"
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

type incidentOut struct {
	IncidentID int    `json:"incident_id"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Domain     string `json:"domain"`
	Published  string `json:"published"`
	CiteURL    string `json:"cite_url"`
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
	return out
}
