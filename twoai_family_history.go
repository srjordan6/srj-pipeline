package main

// A History section on every model family page. theworldofai bridge row 423,
// Stephen 2026-10-03, pointing at the Gemini article on Wikipedia as the kind
// of material he means.
//
// SOURCES. The family's English Wikipedia article, found through Wikidata
// (twoai_wikidata_models.enwiki_title, matched on the family's line name or
// developer plus line), read through the MediaWiki API, plus one or two of
// the developer's own announcements that the article cites and that can be
// fetched. Wikipedia text is CC BY-SA, so nothing is copied: the model writes
// the timeline in the site's own words from those facts, entries sharing an
// eight-word run with a source are dropped, and the section credits the
// article with its retrieval date. An entry whose date the sources do not
// state is left out, and a date more precise than the sources support is
// cut back to what they do support.
//
// FAMILIES WITH NO ARTICLE get a shorter history from our own catalog, the
// release dates of their members, and say that is the basis. No model call.
//
// REFRESH. Rewritten when the family's newest release moves on by more than
// 45 days (a new generation), when the article changes size by more than a
// tenth, or after 90 days. Two or three written a run, behind the family
// pages, under the two-runs-a-day schedule.
//
// Stored in twoai_family_history and attached to each family page doc as
// data.history on every run, after twoaiModelFamilies rewrites the docs.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type famHistEntry struct {
	Date string `json:"date"`
	Text string `json:"text"`
}

type famHistLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type famHistory struct {
	Basis     string         `json:"basis"` // "wikipedia" or "catalog"
	Entries   []famHistEntry `json:"entries"`
	Now       string         `json:"now"`
	WikiTitle string         `json:"wiki_title,omitempty"`
	WikiURL   string         `json:"wiki_url,omitempty"`
	Retrieved string         `json:"retrieved,omitempty"`
	Primary   []famHistLink  `json:"primary"`
	Written   string         `json:"written"`
}

var famHistClient = &http.Client{Timeout: 30 * time.Second}

const famHistUA = "theworldofai.org pipeline (https://theworldofai.org/; srj@srjconsultingservices.com)"

type famWiki struct {
	Title, Extract string
	RevID, Size    int64
	Links          []string
}

// famWikiFetch reads one article's plain text, revision and external links.
func famWikiFetch(title string) (*famWiki, error) {
	q := url.Values{
		"action": {"query"}, "prop": {"extracts|revisions|extlinks"}, "explaintext": {"1"},
		"exsectionformat": {"wiki"}, "rvprop": {"ids|size"}, "ellimit": {"500"},
		"titles": {title}, "format": {"json"}, "formatversion": {"2"}, "redirects": {"1"},
	}
	req, _ := http.NewRequest("GET", "https://en.wikipedia.org/w/api.php?"+q.Encode(), nil)
	req.Header.Set("User-Agent", famHistUA)
	resp, err := famHistClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("wikipedia %d", resp.StatusCode)
	}
	var r struct {
		Query struct {
			Pages []struct {
				Title     string `json:"title"`
				Missing   bool   `json:"missing"`
				Extract   string `json:"extract"`
				Revisions []struct {
					RevID int64 `json:"revid"`
					Size  int64 `json:"size"`
				} `json:"revisions"`
				ExtLinks []struct {
					URL string `json:"url"`
				} `json:"extlinks"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Query.Pages) == 0 {
		return nil, fmt.Errorf("wikipedia: unreadable answer")
	}
	p := r.Query.Pages[0]
	if p.Missing || p.Extract == "" {
		return nil, fmt.Errorf("wikipedia: no article %q", title)
	}
	w := &famWiki{Title: p.Title, Extract: p.Extract}
	if len(p.Revisions) > 0 {
		w.RevID, w.Size = p.Revisions[0].RevID, p.Revisions[0].Size
	}
	for _, l := range p.ExtLinks {
		w.Links = append(w.Links, l.URL)
	}
	return w, nil
}

var famSectionRe = regexp.MustCompile(`(?m)^==+\s*(.+?)\s*==+\s*$`)
var famWantSection = regexp.MustCompile(`(?i)history|release|development|background|version|generation|model|launch|reception|controvers|incident|timeline|overview`)
var famSkipSection = regexp.MustCompile(`(?i)see also|references|notes|external links|further reading|bibliography`)

// famHistSource keeps the lead and the history-like sections, capped.
func famHistSource(extract string) string {
	idx := famSectionRe.FindAllStringSubmatchIndex(extract, -1)
	var b strings.Builder
	end := len(extract)
	if len(idx) > 0 {
		end = idx[0][0]
	}
	b.WriteString(strings.TrimSpace(extract[:end]))
	for i, m := range idx {
		head := extract[m[2]:m[3]]
		stop := len(extract)
		if i+1 < len(idx) {
			stop = idx[i+1][0]
		}
		if famSkipSection.MatchString(head) || !famWantSection.MatchString(head) {
			continue
		}
		b.WriteString("\n\n" + head + "\n" + strings.TrimSpace(extract[m[1]:stop]))
		if b.Len() > 16000 {
			break
		}
	}
	s := b.String()
	if len(s) > 16000 {
		s = s[:16000]
	}
	return s
}

var famTitleTagRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// famPrimary picks up to two of the article's own references that sit on the
// developer's site and read as announcements, and fetches them.
func famPrimary(db *sql.DB, links []string, devUID string) ([]famHistLink, string) {
	var site string
	if devUID != "" {
		db.QueryRow(`SELECT COALESCE(website,'') FROM twoai_company_profiles WHERE uid=$1`, devUID).Scan(&site)
	}
	dom := strings.TrimPrefix(publisherFromURL(site), "www.")
	if parts := strings.Split(dom, "."); len(parts) > 2 {
		dom = strings.Join(parts[len(parts)-2:], ".")
	}
	if dom == "" {
		return nil, ""
	}
	var out []famHistLink
	var text strings.Builder
	for _, l := range links {
		if len(out) >= 2 {
			break
		}
		h := publisherFromURL(l)
		if !(h == dom || strings.HasSuffix(h, "."+dom)) {
			continue
		}
		if !regexp.MustCompile(`(?i)blog|news|announc|introduc|research|press|launch`).MatchString(l) {
			continue
		}
		t := newsArticleText(db, l)
		if t == "" {
			continue
		}
		title := l
		if req, err := http.NewRequest("GET", l, nil); err == nil {
			req.Header.Set("User-Agent", famHistUA)
			if resp, err := famHistClient.Do(req); err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if m := famTitleTagRe.FindSubmatch(b); m != nil {
					title = strings.TrimSpace(enfPlain(string(m[1])))
				}
			}
		}
		out = append(out, famHistLink{Title: trunc(title, 140), URL: l})
		fmt.Fprintf(&text, "\n\nDeveloper announcement %s:\n%s", l, trunc(t, 3500))
	}
	return out, text.String()
}

var famEntryRe = regexp.MustCompile(`^(?:ENTRY:\s*)?(\d{4})(?:-(\d{2}))?(?:-(\d{2}))?\s*\|\s*(.+)$`)

// famHistVerify cuts a date back to the precision the sources support, or
// reports false when even the year is not in them.
func famHistVerify(y, m, d, src string) (string, bool) {
	if !strings.Contains(src, y) {
		return "", false
	}
	if m == "" {
		return y, true
	}
	mi, _ := strconv.Atoi(m)
	if mi < 1 || mi > 12 {
		return y, true
	}
	mon := time.Month(mi).String()
	monthOK := strings.Contains(src, mon+" "+y) || strings.Contains(src, y+"-"+m) ||
		regexp.MustCompile(mon+` \d{1,2}, `+y).MatchString(src) || regexp.MustCompile(`\d{1,2} `+mon+` `+y).MatchString(src)
	if !monthOK {
		return y, true
	}
	if d == "" {
		return y + "-" + m, true
	}
	di, _ := strconv.Atoi(d)
	dayOK := strings.Contains(src, y+"-"+m+"-"+d) ||
		strings.Contains(src, fmt.Sprintf("%s %d, %s", mon, di, y)) || strings.Contains(src, fmt.Sprintf("%d %s %s", di, mon, y))
	if !dayOK {
		return y + "-" + m, true
	}
	return y + "-" + m + "-" + d, true
}

// famCopied reports an entry sharing an eight-word run with the source.
func famCopied(text string, grams map[string]bool) bool {
	w := strings.Fields(newsEntNorm(text))
	for i := 0; i+8 <= len(w); i++ {
		if grams[strings.Join(w[i:i+8], " ")] {
			return true
		}
	}
	return false
}

func famGrams(src string) map[string]bool {
	w := strings.Fields(newsEntNorm(src))
	g := make(map[string]bool, len(w))
	for i := 0; i+8 <= len(w); i++ {
		g[strings.Join(w[i:i+8], " ")] = true
	}
	return g
}

const famHistSystem = `You write the History section of an AI model family page for The World of AI, a reference site read by business leaders, developers and policy readers. You are given an encyclopedia article about the family, perhaps one or two of the developer's announcements, and facts from our model catalog. Write in your own words: never copy a sentence or a distinctive phrase from the sources. Give four to eight dated entries in time order on how the family came to be, its first release, each major generation, and any notable incidents or controversies the sources record. Use only dates the sources state. If a source gives only a month or a year, give only that. Leave out any event without a stated date. Then two or three sentences on where the family stands now. Plain English, commas rather than dashes, no markdown. Reply in exactly this form, one entry per line, nothing else:
ENTRY: YYYY-MM-DD | one sentence
NOW: two or three sentences`

type famHistFamily struct {
	path, uid, name, line, dev, devUID, latest, first string
	members                                           []map[string]any
}

// famHistCatalog builds the shorter history from member release dates.
func famHistCatalog(f famHistFamily) famHistory {
	type rel struct{ date, name string }
	var rs []rel
	for _, m := range f.members {
		d, _ := m["released"].(string)
		n, _ := m["name"].(string)
		if d == "" || n == "" {
			continue
		}
		if i := strings.Index(n, ": "); i > 0 {
			n = n[i+2:]
		}
		rs = append(rs, rel{d, n})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].date < rs[j].date })
	h := famHistory{Basis: "catalog", Primary: []famHistLink{}, Written: time.Now().UTC().Format("2006-01-02")}
	if len(rs) == 0 {
		return h
	}
	h.Entries = append(h.Entries, famHistEntry{rs[0].date, fmt.Sprintf("%s, the earliest version in our catalog, was released.", rs[0].name)})
	lastYear := rs[0].date[:4]
	for _, r := range rs[1:] {
		if len(h.Entries) >= 4 {
			break
		}
		if r.date[:4] != lastYear {
			h.Entries = append(h.Entries, famHistEntry{r.date, fmt.Sprintf("%s was released, the first version of %s in our catalog.", r.name, r.date[:4])})
			lastYear = r.date[:4]
		}
	}
	last := rs[len(rs)-1]
	if last.date != h.Entries[len(h.Entries)-1].Date || len(h.Entries) == 1 {
		if len(rs) > 1 {
			h.Entries = append(h.Entries, famHistEntry{last.date, fmt.Sprintf("%s, the newest version in our catalog, was released.", last.name)})
		}
	}
	h.Now = fmt.Sprintf("%s has %d versions in our model catalog, from %s to %s.", f.name, len(rs), rs[0].date, last.date)
	return h
}

func twoaiFamilyHistory(db *sql.DB) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_family_history (
		uid text PRIMARY KEY, basis text NOT NULL, wiki_title text, wiki_revid bigint, wiki_size bigint,
		latest_release text, history jsonb NOT NULL, model text, written_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		fmt.Println("twoai_family_history:", err)
		return
	}
	rows, err := db.Query(`SELECT path, data->>'uid', COALESCE(data->>'name',''), COALESCE(data->>'line',''), COALESCE(data->>'developer',''),
			COALESCE(data->'developer_company'->>'uid',''), COALESCE(data->>'latest_release',''), COALESCE(data->>'first_release',''),
			COALESCE(data->'members','[]'::jsonb)::text
		FROM twoai_pages WHERE path LIKE 'tech/family-%' ORDER BY path`)
	if err != nil {
		fmt.Println("twoai_family_history:", err)
		return
	}
	var fams []famHistFamily
	for rows.Next() {
		var f famHistFamily
		var mj string
		if rows.Scan(&f.path, &f.uid, &f.name, &f.line, &f.dev, &f.devUID, &f.latest, &f.first, &mj) == nil {
			json.Unmarshal([]byte(mj), &f.members)
			fams = append(fams, f)
		}
	}
	rows.Close()

	// Wikipedia titles by Wikidata label, folded.
	wiki := map[string]string{}
	if wr, err := db.Query(`SELECT label, enwiki_title FROM twoai_wikidata_models WHERE enwiki_title IS NOT NULL AND enwiki_title <> ''`); err == nil {
		for wr.Next() {
			var l, t string
			if wr.Scan(&l, &t) == nil {
				wiki[newsEntNorm(l)] = t
			}
		}
		wr.Close()
	}

	perRun := 3
	if v, err := strconv.Atoi(os.Getenv("TWOAI_FAMILY_HISTORY_PER_RUN")); err == nil && v >= 0 {
		perRun = v
	}
	written, catalog, attached := 0, 0, 0
	for _, f := range fams {
		title := wiki[newsEntNorm(f.line)]
		if title == "" {
			title = wiki[newsEntNorm(f.name)]
		}
		var basis, oldTitle, oldLatest string
		var oldRev, oldSize sql.NullInt64
		var oldAt time.Time
		have := db.QueryRow(`SELECT basis, COALESCE(wiki_title,''), wiki_revid, wiki_size, COALESCE(latest_release,''), written_at
			FROM twoai_family_history WHERE uid=$1`, f.uid).Scan(&basis, &oldTitle, &oldRev, &oldSize, &oldLatest, &oldAt) == nil
		newGen := func() bool {
			a, e1 := time.Parse("2006-01-02", oldLatest)
			b, e2 := time.Parse("2006-01-02", f.latest)
			return e1 != nil || e2 != nil || b.Sub(a) > 45*24*time.Hour
		}

		if title == "" {
			// Catalog basis: free, so kept current every run.
			if !have || basis != "catalog" || oldLatest != f.latest {
				h := famHistCatalog(f)
				if len(h.Entries) > 0 {
					raw, _ := json.Marshal(h)
					db.Exec(`INSERT INTO twoai_family_history (uid, basis, latest_release, history, written_at) VALUES ($1,'catalog',$2,$3::jsonb,now())
						ON CONFLICT (uid) DO UPDATE SET basis='catalog', wiki_title=NULL, wiki_revid=NULL, wiki_size=NULL,
						latest_release=EXCLUDED.latest_release, history=EXCLUDED.history, model=NULL, written_at=now()`, f.uid, f.latest, string(raw))
					catalog++
				}
			}
		} else if written < perRun {
			due := !have || basis != "wikipedia" || oldTitle != title || newGen() || time.Since(oldAt) > 90*24*time.Hour
			var w *famWiki
			if !due && oldRev.Valid {
				// Only an article that changed materially is worth a rewrite.
				if fw, err := famWikiFetch(title); err == nil && fw.RevID != oldRev.Int64 && oldSize.Valid && oldSize.Int64 > 0 {
					d := float64(fw.Size-oldSize.Int64) / float64(oldSize.Int64)
					if d > 0.1 || d < -0.1 {
						due, w = true, fw
					}
				}
			}
			if due {
				if w == nil {
					fw, err := famWikiFetch(title)
					if err != nil {
						fmt.Printf("twoai_family_history: %s: %v\n", f.name, err)
					}
					w = fw
				}
				if w != nil {
					if h, model, ok := famHistWrite(db, f, w); ok {
						raw, _ := json.Marshal(h)
						db.Exec(`INSERT INTO twoai_family_history (uid, basis, wiki_title, wiki_revid, wiki_size, latest_release, history, model, written_at)
							VALUES ($1,'wikipedia',$2,$3,$4,$5,$6::jsonb,$7,now())
							ON CONFLICT (uid) DO UPDATE SET basis='wikipedia', wiki_title=EXCLUDED.wiki_title, wiki_revid=EXCLUDED.wiki_revid,
							wiki_size=EXCLUDED.wiki_size, latest_release=EXCLUDED.latest_release, history=EXCLUDED.history,
							model=EXCLUDED.model, written_at=now()`, f.uid, title, w.RevID, w.Size, f.latest, string(raw), model)
						written++
						fmt.Printf("twoai_family_history: wrote %s from Wikipedia %q, %d entries\n", f.name, w.Title, len(h.Entries))
					} else if !have {
						// The article gave too little: the catalog history
						// stands in until the next attempt.
						h := famHistCatalog(f)
						if len(h.Entries) > 0 {
							raw, _ := json.Marshal(h)
							db.Exec(`INSERT INTO twoai_family_history (uid, basis, latest_release, history, written_at) VALUES ($1,'catalog',$2,$3::jsonb, now() - interval '89 days')
								ON CONFLICT (uid) DO NOTHING`, f.uid, f.latest, string(raw))
						}
					}
				}
			}
		}
		// Attach whatever is stored to the page doc, every run.
		var hj string
		if db.QueryRow(`SELECT history::text FROM twoai_family_history WHERE uid=$1`, f.uid).Scan(&hj) == nil {
			if _, err := db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{history}', $1::jsonb) WHERE path=$2`, hj, f.path); err == nil {
				attached++
			}
		}
	}
	fmt.Printf("twoai_family_history: families=%d written=%d catalog=%d attached=%d ok=true\n", len(fams), written, catalog, attached)
}

// famHistWrite asks the model for the timeline and keeps only what the
// sources support.
func famHistWrite(db *sql.DB, f famHistFamily, w *famWiki) (famHistory, string, bool) {
	src := famHistSource(w.Extract)
	primary, ptext := famPrimary(db, w.Links, f.devUID)
	latestName := ""
	for _, m := range f.members {
		if d, _ := m["released"].(string); d == f.latest {
			latestName, _ = m["name"].(string)
		}
	}
	user := fmt.Sprintf("Family: %s, developer %s.\nCatalog facts: %d versions in our catalog, first released %s, newest released %s (%s).\n\nEncyclopedia article: %s\n%s%s",
		f.name, f.dev, len(f.members), f.first, f.latest, latestName, w.Title, src, ptext)
	out, model, err := twoaiGenerate("family_history", famHistSystem, user)
	if err != nil || isRefusal(out) {
		return famHistory{}, model, false
	}
	all := src + ptext + " " + f.first + " " + f.latest
	grams := famGrams(src + ptext)
	h := famHistory{Basis: "wikipedia", WikiTitle: w.Title, Retrieved: time.Now().UTC().Format("2006-01-02"),
		WikiURL: "https://en.wikipedia.org/wiki/" + url.PathEscape(strings.ReplaceAll(w.Title, " ", "_")),
		Primary: primary, Written: time.Now().UTC().Format("2006-01-02")}
	if h.Primary == nil {
		h.Primary = []famHistLink{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*"))
		if strings.HasPrefix(strings.ToUpper(line), "NOW:") {
			h.Now = strings.TrimSpace(line[4:])
			continue
		}
		m := famEntryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		date, ok := famHistVerify(m[1], m[2], m[3], all)
		text := strings.TrimSpace(m[4])
		if !ok || text == "" || famCopied(text, grams) {
			continue
		}
		h.Entries = append(h.Entries, famHistEntry{date, text})
	}
	sort.SliceStable(h.Entries, func(i, j int) bool { return h.Entries[i].Date < h.Entries[j].Date })
	if len(h.Entries) > 8 {
		h.Entries = h.Entries[:8]
	}
	if famCopied(h.Now, grams) {
		h.Now = ""
	}
	return h, model, len(h.Entries) >= 3
}
