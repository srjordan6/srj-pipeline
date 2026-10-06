package main

// WHICH DOCKETS TO ASK ABOUT, AND HOW OFTEN, 2026-10-06 (bridge row 518).
//
// With 70 refresh calls a day for about 150 federal dockets, the refresh can
// no longer walk the whole list. Each docket gets a cadence from how recently
// it moved, and the dockets that are due are taken in priority order until
// the share is spent.
//
// CADENCE. A docket with a new entry in the last 30 days is checked daily;
// one quiet for 30 days weekly; one quiet for 90 days monthly. A docket never
// answered is due at once. A case the tracker marks closed or dismissed
// (status_badge Dismissed, Closed, Terminated or Decided, or a status that
// starts with one of those words) is not polled at all: it is checked only
// when the news intake (twoai_news_stories) or the AI Incident Database watch
// (twoai_incidents) has named both of its parties in the last seven days.
// Settled is not closed: Bartz v. Anthropic is settled and still has a
// fairness ruling to come.
//
// PRIORITY among the dockets that are due:
//
//  0. a new docket entry in the last seven days;
//  1. a hearing, conference, trial date or deadline in the next fourteen
//     days, read from the docket text the timeline already holds ("Motion
//     Hearing set for 10/20/2026", "Responses due by 10/16/2026"); the
//     tracker has no structured future-event field, and no timeline date is
//     in the future, so the docket text is the only reliable source; a
//     closed case the news has just named sits here too;
//  2. a case Stephen named: ai_lawsuits.priority above zero (a column added
//     for this, default 0) or one of the thirteen cases entered by hand at
//     launch (display_order under 1000);
//  3. everything else.
//
// Inside a tier the docket CourtListener answered longest ago goes first.
//
// ONE CALL A DOCKET. The refresh asks the docket-entries endpoint only for
// entries modified since the last answer (date_modified__gt), with fields=
// cut to the three it uses. The docket record itself is read only when
// something needs it (an unclassified case with nothing cached, or a link
// without its slug), and every docket record read is kept in
// twoai_cl_docket_cache with its date_modified, so classification reads the
// cache first and no docket record is fetched twice in a UTC day.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	clDay = 24 * time.Hour
	// A check is due this much before its cadence is up, so a docket read at
	// 18:05 one day is still due in the 18:00 run the next.
	clDueSlack = 2 * time.Hour
	// A docket is overdue once it is a day past its scheduled check.
	clOverdueGrace = clDay
	// The docket record fields any stage uses.
	clDocketFields = "id,absolute_url,date_modified,date_last_filing,date_terminated,nature_of_suit,case_name,docket_number"
)

// clCase is one federal docket on the tracker, as the scheduler sees it.
type clCase struct {
	ID          int64
	Slug        string
	URL         string
	DocketID    string
	CaseName    string
	Category    string
	Timeline    string // the stored timeline, raw JSON
	LastEntry   time.Time
	Filed       time.Time
	LastOK      time.Time // when CourtListener last answered for it
	Closed      bool
	Pinned      bool
	Upcoming    time.Time // earliest hearing or deadline in the next 14 days
	NewsMention time.Time // newest news or AIID mention, closed cases only
	Cached      bool      // a docket record is in twoai_cl_docket_cache

	Next    time.Time // scheduled check; zero means now
	Polled  bool      // false for a closed case nothing has named
	Due     bool
	Overdue bool
}

// clCadence is how often a docket is checked, from how long it has been
// since its last new entry (or its filing, when no entry is known).
func clCadence(c clCase, now time.Time) time.Duration {
	ref := c.LastEntry
	if ref.IsZero() {
		ref = c.Filed
	}
	if ref.IsZero() {
		return clDay
	}
	quiet := now.Sub(ref)
	switch {
	case quiet >= 90*clDay:
		return 30 * clDay
	case quiet >= 30*clDay:
		return 7 * clDay
	}
	return clDay
}

// clSchedule fills Next, Polled, Due and Overdue.
func clSchedule(c *clCase, now time.Time) {
	c.Polled, c.Due, c.Overdue, c.Next = true, false, false, time.Time{}
	if c.Closed {
		// Checked only when something has named it since the last answer.
		if c.NewsMention.IsZero() || (!c.LastOK.IsZero() && !c.LastOK.Before(c.NewsMention)) {
			c.Polled = false
			return
		}
		c.Next = c.NewsMention
	} else if !c.LastOK.IsZero() {
		c.Next = c.LastOK.Add(clCadence(*c, now))
	}
	c.Due = c.Next.IsZero() || !now.Before(c.Next.Add(-clDueSlack))
	c.Overdue = c.Next.IsZero() || now.After(c.Next.Add(clOverdueGrace))
}

// clTier is the priority tier, lower first.
func clTier(c clCase, now time.Time) int {
	if !c.LastEntry.IsZero() && now.Sub(c.LastEntry) <= 7*clDay {
		return 0
	}
	if (!c.Upcoming.IsZero() && !c.Upcoming.Before(clUTCDay(now)) && c.Upcoming.Sub(now) <= 14*clDay) ||
		(c.Closed && !c.NewsMention.IsZero()) {
		return 1
	}
	if c.Pinned {
		return 2
	}
	return 3
}

// clSortByPriority orders dockets by tier, then longest since answered.
func clSortByPriority(cs []clCase, now time.Time) {
	sort.SliceStable(cs, func(i, j int) bool {
		ti, tj := clTier(cs[i], now), clTier(cs[j], now)
		if ti != tj {
			return ti < tj
		}
		if cs[i].LastOK.Equal(cs[j].LastOK) {
			return cs[i].Slug < cs[j].Slug
		}
		return cs[i].LastOK.Before(cs[j].LastOK) // zero, never answered, first
	})
}

// A date the docket text says something will happen on.
var clEventRe = regexp.MustCompile(`(?i)\b(?:(?:re)?set|(?:re)?scheduled|continued|adjourned)\s+(?:for|to|until)\s+(\d{1,2})/(\d{1,2})/(\d{2,4})` +
	`|\bdue\s+(?:by|on)\s+(\d{1,2})/(\d{1,2})/(\d{2,4})` +
	`|\b(?:hearing|trial|conference)\s+on\s+(\d{1,2})/(\d{1,2})/(\d{2,4})`)

// clUpcomingEvent returns the earliest hearing, conference, trial or
// deadline named in the docket text that falls in the next fourteen days,
// today included, or the zero time.
func clUpcomingEvent(titles []string, now time.Time) time.Time {
	today := clUTCDay(now)
	end := today.Add(14 * clDay)
	var best time.Time
	for _, t := range titles {
		for _, m := range clEventRe.FindAllStringSubmatch(t, -1) {
			for g := 1; g+2 < len(m); g += 3 {
				if m[g] == "" {
					continue
				}
				mo, _ := strconv.Atoi(m[g])
				d, _ := strconv.Atoi(m[g+1])
				y, _ := strconv.Atoi(m[g+2])
				if y < 100 {
					y += 2000
				}
				if mo < 1 || mo > 12 || d < 1 || d > 31 {
					continue
				}
				at := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
				if at.Before(today) || at.After(end) {
					continue
				}
				if best.IsZero() || at.Before(best) {
					best = at
				}
			}
		}
	}
	return best
}

// clClosedStatus says whether the tracker marks a case closed or dismissed.
func clClosedStatus(badge, status string) bool {
	b := strings.ToLower(strings.TrimSpace(badge))
	for _, w := range []string{"dismissed", "closed", "terminated", "decided"} {
		if strings.Contains(b, w) {
			return true
		}
	}
	s := strings.ToLower(strings.TrimSpace(status))
	for _, w := range []string{"dismissed", "closed", "terminated"} {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

var clVersusRe = regexp.MustCompile(`(?i)\s+v(?:s)?\.?\s+`)

var clPartyStop = map[string]bool{
	"the": true, "in": true, "re": true, "estate": true, "of": true, "people": true, "state": true,
	"united": true, "states": true, "inc": true, "inc.": true, "llc": true, "corp": true, "et": true, "al": true,
	"a": true, "an": true, "doe": true, "john": true, "jane": true, "matter": true,
}

// clCaseParties returns one distinctive word from each side of a caption,
// "Thomson Reuters v. Ross Intelligence" giving "Thomson" and "Ross", for a
// cheap match against news headlines. Empty when the caption has no "v.".
func clCaseParties(name string) (string, string) {
	sides := clVersusRe.Split(name, 2)
	if len(sides) != 2 {
		return "", ""
	}
	pick := func(s string) string {
		for _, w := range strings.FieldsFunc(s, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '&')
		}) {
			if len(w) >= 3 && !clPartyStop[strings.ToLower(w)] {
				return w
			}
		}
		return ""
	}
	return pick(sides[0]), pick(sides[1])
}

var clDocketIDRe = regexp.MustCompile(`/docket/(\d+)`)

func clParseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t
		}
	}
	return time.Time{}
}

// clDocketPlan reads every active federal docket and schedules it.
func clDocketPlan(db *sql.DB, now time.Time) ([]clCase, error) {
	rows, err := db.Query(`SELECT l.id, l.slug, l.courtlistener_url, COALESCE(l.case_name,''), COALESCE(l.category,''),
			COALESCE(l.status_badge,''), COALESCE(l.status,''), COALESCE(l.latest_development_date::text,''),
			COALESCE(l.filed_date::text,''), l.docket_ok_at, COALESCE(l.timeline::text,'[]'),
			COALESCE(l.priority,0), COALESCE(l.display_order,100),
			c.docket_id IS NOT NULL
		FROM ai_lawsuits l
		LEFT JOIN twoai_cl_docket_cache c ON c.docket_id::text = substring(l.courtlistener_url from '/docket/(\d+)')
		WHERE l.is_active AND l.courtlistener_url IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	var out []clCase
	for rows.Next() {
		var c clCase
		var badge, status, latest, filed string
		var ok sql.NullTime
		var priority, order int
		if err := rows.Scan(&c.ID, &c.Slug, &c.URL, &c.CaseName, &c.Category, &badge, &status, &latest, &filed,
			&ok, &c.Timeline, &priority, &order, &c.Cached); err != nil {
			rows.Close()
			return nil, err
		}
		if m := clDocketIDRe.FindStringSubmatch(c.URL); m != nil {
			c.DocketID = m[1]
		}
		if ok.Valid {
			c.LastOK = ok.Time
		}
		c.Filed = clParseDate(filed)
		c.LastEntry = clParseDate(latest)
		c.Closed = clClosedStatus(badge, status)
		c.Pinned = priority > 0 || order < 1000
		var tl []struct {
			Date  string `json:"date"`
			Title string `json:"title"`
		}
		json.Unmarshal([]byte(c.Timeline), &tl)
		var recent []string
		for _, e := range tl {
			d := clParseDate(e.Date)
			if d.After(c.LastEntry) && !d.After(now) {
				c.LastEntry = d
			}
			if !d.IsZero() && now.Sub(d) <= 120*clDay {
				recent = append(recent, e.Title)
			}
		}
		c.Upcoming = clUpcomingEvent(recent, now)
		out = append(out, c)
	}
	rows.Close()
	for i := range out {
		c := &out[i]
		if c.Closed {
			p, d := clCaseParties(c.CaseName)
			if p != "" && d != "" {
				lp, ld := "%"+p+"%", "%"+d+"%"
				var a, b sql.NullTime
				db.QueryRow(`SELECT max(first_published) FROM twoai_news_stories WHERE first_published > $3
					AND ((headline ILIKE $1 AND headline ILIKE $2) OR (story->>'Summary' ILIKE $1 AND story->>'Summary' ILIKE $2))`,
					lp, ld, now.Add(-7*clDay)).Scan(&a)
				db.QueryRow(`SELECT max(first_seen) FROM twoai_incidents WHERE first_seen > $3 AND title ILIKE $1 AND title ILIKE $2`,
					lp, ld, now.Add(-7*clDay)).Scan(&b)
				if a.Valid {
					c.NewsMention = a.Time
				}
				if b.Valid && b.Time.After(c.NewsMention) {
					c.NewsMention = b.Time
				}
			}
		}
		clSchedule(c, now)
	}
	return out, nil
}

// clDocketRec is the part of a docket record the stages use.
type clDocketRec struct {
	ID             int64  `json:"id"`
	AbsoluteURL    string `json:"absolute_url"`
	DateModified   string `json:"date_modified"`
	DateLastFiling string `json:"date_last_filing"`
	DateTerminated string `json:"date_terminated"`
	NatureOfSuit   string `json:"nature_of_suit"`
	CaseName       string `json:"case_name"`
}

// clCachedDocket reads a docket record from the cache.
func clCachedDocket(db *sql.DB, id string) (clDocketRec, time.Time, bool) {
	var rec clDocketRec
	var body []byte
	var at time.Time
	if db.QueryRow(`SELECT fetched_at, COALESCE(body::text,'{}') FROM twoai_cl_docket_cache WHERE docket_id::text = $1`, id).Scan(&at, &body) != nil {
		return rec, at, false
	}
	json.Unmarshal(body, &rec)
	return rec, at, true
}

// clDocket returns a docket record, from the cache when it was fetched
// today (UTC), otherwise from CourtListener, storing what comes back. The
// status is 200 for a cached answer.
func clDocket(db *sql.DB, id string) (clDocketRec, int, error) {
	if rec, at, ok := clCachedDocket(db, id); ok && !at.Before(clUTCDay(time.Now())) {
		return rec, 200, nil
	}
	var raw json.RawMessage
	status, err := clFetch(clAPIBase+"/dockets/"+id+"/?fields="+clDocketFields, &raw)
	if err != nil {
		return clDocketRec{}, status, err
	}
	var rec clDocketRec
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, status, err
	}
	var modified any
	if t, err := time.Parse(time.RFC3339Nano, rec.DateModified); err == nil {
		modified = t
	}
	if _, err := db.Exec(`INSERT INTO twoai_cl_docket_cache (docket_id, fetched_at, date_modified, body) VALUES ($1, now(), $2, $3)
		ON CONFLICT (docket_id) DO UPDATE SET fetched_at = now(), date_modified = EXCLUDED.date_modified, body = EXCLUDED.body`,
		id, modified, string(raw)); err != nil {
		fmt.Println("CourtListener docket cache:", err)
	}
	return rec, status, nil
}

// clMissedChecks lists the dockets past their scheduled check, most
// overdue first, at most limit of them, and the total.
func clMissedChecks(db *sql.DB, limit int) ([]string, int) {
	now := time.Now()
	plan, err := clDocketPlan(db, now)
	if err != nil {
		return nil, 0
	}
	var late []clCase
	for _, c := range plan {
		if c.Overdue {
			late = append(late, c)
		}
	}
	sort.SliceStable(late, func(i, j int) bool {
		if late[i].Next.IsZero() != late[j].Next.IsZero() {
			return late[i].Next.IsZero()
		}
		return late[i].Next.Before(late[j].Next)
	})
	var out []string
	for i, c := range late {
		if i == limit {
			break
		}
		last, due := "never answered", "now"
		if !c.LastOK.IsZero() {
			last = "last answered " + c.LastOK.UTC().Format("2006-01-02")
		}
		if !c.Next.IsZero() {
			due = c.Next.UTC().Format("2006-01-02")
		}
		out = append(out, fmt.Sprintf("%s (%s, due %s, every %s)", c.Slug, last, due, clCadenceLabel(clCadence(c, now))))
	}
	return out, len(late)
}

// clUsageReport is the cl_usage stage: the ledger, CourtListener's own
// count, and the docket schedule, without a counted call.
func clUsageReport(db *sql.DB) error {
	clEnsureLedger(db)
	now := time.Now()
	fmt.Println(clReportLine(db))
	if s := clServerUsage(); s != "" {
		fmt.Println(s)
	}
	plan, err := clDocketPlan(db, now)
	if err != nil {
		return err
	}
	cadence := map[string]int{}
	tiers := map[int]int{}
	due, overdue, never := 0, 0, 0
	var recent []clCase
	for _, c := range plan {
		switch {
		case !c.Polled:
			cadence["closed, not polled"]++
		case c.Closed:
			cadence["closed, named in the news"]++
		default:
			cadence["every "+clCadenceLabel(clCadence(c, now))]++
		}
		if c.Due {
			due++
			tiers[clTier(c, now)]++
		}
		if c.Overdue {
			overdue++
		}
		if c.LastOK.IsZero() {
			never++
		}
		if !c.LastEntry.IsZero() && now.Sub(c.LastEntry) <= 7*clDay {
			recent = append(recent, c)
		}
	}
	fmt.Printf("cl_usage: %d federal dockets, %d due now, %d overdue, %d never answered; cadence %v\n",
		len(plan), due, overdue, never, cadence)
	fmt.Printf("cl_usage: due by tier: new entry in 7 days %d, hearing or deadline in 14 days %d, named by Stephen %d, the rest %d\n",
		tiers[0], tiers[1], tiers[2], tiers[3])
	sort.Slice(recent, func(i, j int) bool { return recent[i].LastEntry.After(recent[j].LastEntry) })
	for _, c := range recent {
		last := "never"
		if !c.LastOK.IsZero() {
			last = c.LastOK.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		fmt.Printf("cl_usage: new entry %s  %s  last answered %s\n", c.LastEntry.Format("2006-01-02"), c.Slug, last)
	}
	return nil
}

func clCadenceLabel(d time.Duration) string {
	switch d {
	case clDay:
		return "day"
	case 7 * clDay:
		return "week"
	case 30 * clDay:
		return "month"
	}
	return d.String()
}
