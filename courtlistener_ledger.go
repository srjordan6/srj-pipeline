package main

// THE COURTLISTENER LEDGER, 2026-10-06 (content project bridge row 518,
// approved by Stephen the same day).
//
// A free CourtListener account gets 5 requests a minute, 50 an hour and 125
// a day, counted on rolling windows (Free Law Project API v4 overview, and
// the membership page: "5/minute, 50/hour, 125/day"). Every stage runs as
// its own process, so the count has to live in the database, and every
// request through clFetch takes one slot here first:
//
//   - SPACING. At least twelve seconds since the last call by any stage, so
//     the minute window (5) is never the one that bites;
//   - HOUR. Never more than 50 calls in the last sixty minutes, and never
//     more than 35 of them for the docket refresh, so the stages behind it
//     in the same run still find room; a stage that would have to wait past
//     its budget for the window stops instead;
//   - DAY. Never more than 125 in the UTC day, shared out as below;
//   - 429. A 429 means the server counted something the ledger did not.
//     It is recorded, and every stage stops for the rest of the UTC day.
//
// THE DAILY SHARES. refresh 70, recap 25, discovery 15, classify 10 and a
// reserve of 5 for hand runs. The first run of the day takes up to its
// share, later runs take what is left of it. After 18:00 UTC whatever the
// recap, discovery and classify shares have not used rolls to the docket
// refresh, which is the work that most needs it. The reserve is touched only
// by a caller whose bucket is "reserve" or a run with CL_RESERVE=1 set, and
// only once its own share is spent; nothing else may take the day past 120
// (on the free tier; the day less the reserve on any tier).
//
// Tables (schema in Go, IF NOT EXISTS):
//
//	twoai_courtlistener_budget  (day, stage) -> calls, last_call_at; stage
//	                            is the share charged: refresh, recap,
//	                            discovery, classify, reserve
//	twoai_courtlistener_calls   one row per request: called_at, stage,
//	                            path, status (0 until answered, 429 latches
//	                            the day); pruned after two days
//	twoai_courtlistener_state   small key/value: the last successful
//	                            discovery date and the defendant rotation
//
// TIER 1, 2026-10-07. Stephen took a CourtListener Tier 1 membership: 10 a
// minute, 75 an hour and 300 a day (TIER_1_RATES in the Free Law Project's
// cl/api/constants.py), and unlimited docket alerts. The limits and shares
// now come from clTiers, picked by CL_TIER (free, 1, 2, 3 or 4; 1 when
// unset), so a change of membership is one environment variable rather than
// a code change. The free tier keeps the numbers above. Tier 1 adds an
// alerts share, which subscribes the tracked dockets to CourtListener docket
// alerts (courtlistener_alerts.go); like recap, discovery and classify,
// whatever it has not used rolls to the refresh after 18:00 UTC.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The limits of the membership in use, set from clTiers by clApplyTier.
var (
	clDayLimit  int
	clHourLimit int
	clSpacing   time.Duration
)

const (
	// From this hour (UTC) unused recap, discovery and classify shares roll
	// to the docket refresh.
	clRolloverHourUTC = 18
	clCallsKeep       = 48 * time.Hour
	// One advisory lock key for every process that takes a slot.
	clLedgerLockKey = 5182026
)

const (
	clBucketRefresh   = "refresh"
	clBucketRecap     = "recap"
	clBucketDiscovery = "discovery"
	clBucketClassify  = "classify"
	clBucketAlerts    = "alerts"
	clBucketReserve   = "reserve"
)

// clMembership is one CourtListener membership: its three rolling limits, the
// spacing that keeps the minute window from being the one that bites, and
// how the day is shared out. Shares add up to the day.
type clMembership struct {
	Day, Hour int
	Spacing   time.Duration
	Shares    map[string]int
	HourCaps  map[string]int
}

var clTiers = map[string]clMembership{
	// 5/min, 50/hour, 125/day.
	"free": {Day: 125, Hour: 50, Spacing: 12 * time.Second,
		Shares: map[string]int{clBucketRefresh: 70, clBucketRecap: 25, clBucketDiscovery: 15, clBucketClassify: 10,
			clBucketReserve: 5},
		HourCaps: map[string]int{clBucketRefresh: 35}},
	// 10/min, 75/hour, 300/day. Seven seconds apart is under 9 a minute.
	// The alerts share subscribes about 150 dockets over three days, then
	// sits nearly idle and rolls to the refresh each evening.
	// TIER 1 LEAVES 30 OF ITS 300 TO THE CONNECTOR. theworldofai row 583
	// (2026-10-08): Stephen's CourtListener MCP connector authenticates as
	// the same account, so its reads count against the same 300 a day. The
	// pipeline keeps 270 and splits them as asked: refresh 150, recap 50,
	// discovery 30, classify 25, alerts 15. The ~150 dockets are subscribed,
	// so the alerts share now only covers new cases and resubscriptions.
	"1": {Day: 270, Hour: 75, Spacing: 7 * time.Second,
		Shares: map[string]int{clBucketRefresh: 150, clBucketRecap: 50, clBucketDiscovery: 30, clBucketClassify: 25,
			clBucketAlerts: 15, clBucketReserve: 0},
		HourCaps: map[string]int{clBucketRefresh: 45, clBucketAlerts: 15}},
	// 15/min, 150/hour, 600/day.
	"2": {Day: 600, Hour: 150, Spacing: 5 * time.Second,
		Shares: map[string]int{clBucketRefresh: 300, clBucketRecap: 120, clBucketDiscovery: 50, clBucketClassify: 50,
			clBucketAlerts: 60, clBucketReserve: 20},
		HourCaps: map[string]int{clBucketRefresh: 90, clBucketAlerts: 30}},
	// 20/min, 250/hour, 1000/day.
	"3": {Day: 1000, Hour: 250, Spacing: 4 * time.Second,
		Shares: map[string]int{clBucketRefresh: 520, clBucketRecap: 200, clBucketDiscovery: 80, clBucketClassify: 80,
			clBucketAlerts: 80, clBucketReserve: 40},
		HourCaps: map[string]int{clBucketRefresh: 150, clBucketAlerts: 40}},
	// 25/min, 300/hour, 1400/day.
	"4": {Day: 1400, Hour: 300, Spacing: 3 * time.Second,
		Shares: map[string]int{clBucketRefresh: 740, clBucketRecap: 280, clBucketDiscovery: 110, clBucketClassify: 110,
			clBucketAlerts: 100, clBucketReserve: 60},
		HourCaps: map[string]int{clBucketRefresh: 180, clBucketAlerts: 50}},
}

// clDefaultTier is the membership held since 2026-10-07.
const clDefaultTier = "1"

var (
	clTierName string
	clShares   map[string]int
	// clHourCaps holds the most calls one share may make in a rolling hour,
	// where that is less than the hour itself. The refresh runs first in the
	// sequence and has the largest share, so without a cap it fills the hour
	// and the classifier and the RECAP harvest behind it in the same run get
	// nothing; with the full runs twice a day, they would get nothing all day.
	clHourCaps map[string]int
)

func init() {
	name := strings.TrimSpace(strings.ToLower(os.Getenv("CL_TIER")))
	name = strings.TrimPrefix(name, "tier")
	name = strings.TrimSpace(name)
	if _, ok := clTiers[name]; !ok {
		if name != "" {
			fmt.Printf("CourtListener: CL_TIER=%q is not a known membership, using tier %s\n", os.Getenv("CL_TIER"), clDefaultTier)
		}
		name = clDefaultTier
	}
	clApplyTier(name)
}

// clApplyTier sets the limits and shares of a membership in clTiers.
func clApplyTier(name string) {
	t := clTiers[name]
	clTierName = name
	clDayLimit, clHourLimit, clSpacing = t.Day, t.Hour, t.Spacing
	clShares, clHourCaps = t.Shares, t.HourCaps
}

// clBucketOrder is the order the report line names the shares in.
var clBucketOrder = []string{clBucketRefresh, clBucketRecap, clBucketDiscovery, clBucketClassify, clBucketAlerts, clBucketReserve}

// clRollover lists the shares whose unused calls go to the refresh after
// clRolloverHourUTC.
var clRollover = []string{clBucketRecap, clBucketDiscovery, clBucketClassify, clBucketAlerts}

var (
	clLedgerMu sync.Mutex
	clLedgerDB *sql.DB
	// clLedgerOff skips the database entirely. Tests only: a run with no
	// ledger would have no limit at all.
	clLedgerOff bool
	clBucket    string
	// clLatchedDay is the UTC day this process saw a 429 on, so the latch
	// holds even if writing it to the database failed.
	clLatchedDay string
	clEnsured    bool
	clStopNoted  bool
)

// clUTCDay is the start of t's day in UTC.
func clUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// clUse names the share this process's calls are charged to and the
// database the ledger lives in. Every stage calls it before its first
// CourtListener request.
func clUse(db *sql.DB, bucket string) {
	clLedgerMu.Lock()
	clLedgerDB = db
	clBucket = bucket
	clStopNoted = false
	clLedgerMu.Unlock()
	clEnsureLedger(db)
}

func clEnsureLedger(db *sql.DB) {
	clLedgerMu.Lock()
	done := clEnsured
	clEnsured = true
	clLedgerMu.Unlock()
	if done || db == nil {
		return
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS twoai_courtlistener_budget (day date NOT NULL, stage text NOT NULL,
			calls int NOT NULL DEFAULT 0, last_call_at timestamptz, PRIMARY KEY (day, stage))`,
		`CREATE TABLE IF NOT EXISTS twoai_courtlistener_calls (id bigserial PRIMARY KEY,
			called_at timestamptz NOT NULL DEFAULT now(), stage text NOT NULL, path text, status int NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS twoai_courtlistener_calls_at ON twoai_courtlistener_calls (called_at)`,
		`CREATE TABLE IF NOT EXISTS twoai_courtlistener_state (key text PRIMARY KEY, value text, updated_at timestamptz DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS twoai_cl_docket_cache (docket_id bigint PRIMARY KEY, fetched_at timestamptz NOT NULL,
			date_modified timestamptz, body jsonb)`,
		`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS priority int NOT NULL DEFAULT 0`,
		`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS docket_ok_at timestamptz`,
	} {
		if _, err := db.Exec(q); err != nil {
			fmt.Println("CourtListener ledger schema:", err)
		}
	}
	db.Exec(`DELETE FROM twoai_courtlistener_calls WHERE called_at < $1`, time.Now().Add(-clCallsKeep))
}

// clAllowance says which share a call is charged to and how many calls
// that share still has today. used holds today's calls per share.
func clAllowance(bucket string, used map[string]int, now time.Time, reserveOK bool) (string, int) {
	total := 0
	for _, n := range used {
		total += n
	}
	dayRoom := clDayLimit - total
	if dayRoom <= 0 {
		return bucket, 0
	}
	if bucket != clBucketReserve {
		own := clShares[bucket] - used[bucket]
		if bucket == clBucketRefresh && now.UTC().Hour() >= clRolloverHourUTC {
			for _, b := range clRollover {
				own += max(0, clShares[b]-used[b])
			}
		}
		// Nothing but the reserve may take the day past its last few calls.
		ordinary := (clDayLimit - clShares[clBucketReserve]) - (total - used[clBucketReserve])
		own = min(own, ordinary, dayRoom)
		if own > 0 {
			return bucket, own
		}
		if !reserveOK {
			return bucket, 0
		}
	}
	r := min(clShares[clBucketReserve]-used[clBucketReserve], dayRoom)
	if r > 0 {
		return clBucketReserve, r
	}
	return bucket, 0
}

// clHourWait is how long until a call fits a rolling hour that allows
// limit calls, given the times of the calls already made. Zero when it
// fits now.
func clHourWait(recent []time.Time, now time.Time, limit int) time.Duration {
	var in []time.Time
	for _, t := range recent {
		if t.After(now.Add(-time.Hour)) && !t.After(now) {
			in = append(in, t)
		}
	}
	if limit <= 0 || len(in) < limit {
		return 0
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Before(in[j]) })
	// Room for one more once all but limit-1 of them have aged out.
	oldest := in[len(in)-limit]
	return oldest.Add(time.Hour).Sub(now) + time.Second
}

// clSpacingWait is how long until clSpacing has passed since the last call.
func clSpacingWait(last, now time.Time) time.Duration {
	if last.IsZero() {
		return 0
	}
	if w := last.Add(clSpacing).Sub(now); w > 0 {
		return w
	}
	return 0
}

func clLatchDay() {
	clLedgerMu.Lock()
	clLatchedDay = clUTCDay(time.Now()).Format("2006-01-02")
	clLedgerMu.Unlock()
}

func clLedger() *sql.DB {
	clLedgerMu.Lock()
	defer clLedgerMu.Unlock()
	if clLedgerDB == nil && !clLedgerOff {
		if db, err := sql.Open("postgres", os.Getenv("DATABASE_URL")); err == nil && db.Ping() == nil {
			clLedgerDB = db
		}
	}
	return clLedgerDB
}

// clRefuse prints why the ledger said no, once per process, and returns
// the error the stage stops on.
func clRefuse(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	clLedgerMu.Lock()
	noted := clStopNoted
	clStopNoted = true
	clLedgerMu.Unlock()
	if !noted {
		fmt.Println("CourtListener: " + msg)
	}
	return fmt.Errorf("%w: %s", errCLBudget, msg)
}

// clReserve takes one slot for a request to path, waiting for the spacing
// or the hour window when the wait fits the stage budget. It returns the
// call's ledger id, which clRecord later stamps with the HTTP status.
func clReserve(path string) (int64, error) {
	clLedgerMu.Lock()
	latched := clLatchedDay == clUTCDay(time.Now()).Format("2006-01-02")
	bucket := clBucket
	clLedgerMu.Unlock()
	if latched {
		return 0, clRefuse("HTTP 429 earlier today, no calls until 00:00 UTC")
	}
	reserveOK := os.Getenv("CL_RESERVE") != ""
	if bucket == "" {
		if !reserveOK {
			return 0, clRefuse("no share named for this caller (call clUse, or set CL_RESERVE=1 for a hand run)")
		}
		bucket = clBucketReserve
	}
	if clLedgerOff {
		return 0, nil
	}
	db := clLedger()
	if db == nil {
		return 0, clRefuse("ledger database unreachable, not calling without it")
	}
	clEnsureLedger(db)
	for {
		now := time.Now()
		day := clUTCDay(now)
		tx, err := db.Begin()
		if err != nil {
			return 0, clRefuse("ledger: %v", err)
		}
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, clLedgerLockKey); err != nil {
			tx.Rollback()
			return 0, clRefuse("ledger lock: %v", err)
		}
		var hit bool
		tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_courtlistener_calls WHERE status = 429 AND called_at >= $1)`, day).Scan(&hit)
		if hit {
			tx.Rollback()
			clLatchDay()
			return 0, clRefuse("HTTP 429 earlier today, no calls until 00:00 UTC")
		}
		used := map[string]int{}
		rows, err := tx.Query(`SELECT stage, calls FROM twoai_courtlistener_budget WHERE day = $1`, day)
		if err != nil {
			tx.Rollback()
			return 0, clRefuse("ledger read: %v", err)
		}
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				used[s] = n
			}
		}
		rows.Close()
		charge, left := clAllowance(bucket, used, now, reserveOK)
		if left <= 0 {
			tx.Rollback()
			total := 0
			for _, n := range used {
				total += n
			}
			return 0, clRefuse("%s share spent (%d of %d used by %s today, %d of %d in all)",
				bucket, used[bucket], clShares[bucket], bucket, total, clDayLimit)
		}
		var recent, mine []time.Time
		rows, err = tx.Query(`SELECT called_at, stage FROM twoai_courtlistener_calls WHERE called_at > $1`, now.Add(-time.Hour))
		if err != nil {
			tx.Rollback()
			return 0, clRefuse("ledger read: %v", err)
		}
		var last time.Time
		for rows.Next() {
			var t time.Time
			var s string
			if rows.Scan(&t, &s) == nil {
				recent = append(recent, t)
				if s == charge {
					mine = append(mine, t)
				}
				if t.After(last) {
					last = t
				}
			}
		}
		rows.Close()
		hourWait := max(clHourWait(recent, now, clHourLimit), clHourWait(mine, now, clHourCaps[charge]))
		wait := max(hourWait, clSpacingWait(last, now))
		if wait > 0 {
			tx.Rollback()
			if !clFits(wait, clBudgetLeft()) {
				if hourWait > 0 {
					return 0, clRefuse("%d calls in the last hour (%d by %s), the window opens in %s, past this stage's budget",
						len(recent), len(mine), charge, wait.Round(time.Second))
				}
				return 0, clRefuse("stage budget spent")
			}
			if hourWait > 0 {
				fmt.Printf("CourtListener: %d calls in the last hour (%d by %s), waiting %s for the window\n",
					len(recent), len(mine), charge, wait.Round(time.Second))
			}
			time.Sleep(wait)
			continue
		}
		var id int64
		if err := tx.QueryRow(`INSERT INTO twoai_courtlistener_calls (called_at, stage, path) VALUES ($1, $2, $3) RETURNING id`,
			now, charge, trunc(path, 500)).Scan(&id); err != nil {
			tx.Rollback()
			return 0, clRefuse("ledger write: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO twoai_courtlistener_budget (day, stage, calls, last_call_at) VALUES ($1, $2, 1, $3)
			ON CONFLICT (day, stage) DO UPDATE SET calls = twoai_courtlistener_budget.calls + 1, last_call_at = EXCLUDED.last_call_at`,
			day, charge, now); err != nil {
			tx.Rollback()
			return 0, clRefuse("ledger write: %v", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, clRefuse("ledger commit: %v", err)
		}
		return id, nil
	}
}

// clRecord stamps a call with the status the server answered (0 when no
// answer arrived). A 429 here is what latches the day for every stage.
func clRecord(id int64, status int) {
	if id == 0 || clLedgerOff {
		return
	}
	if db := clLedger(); db != nil {
		db.Exec(`UPDATE twoai_courtlistener_calls SET status = $2 WHERE id = $1`, id, status)
	}
}

// clStateGet and clStateSet keep the few values discovery needs between days.
func clStateGet(db *sql.DB, key string) string {
	var v sql.NullString
	db.QueryRow(`SELECT value FROM twoai_courtlistener_state WHERE key = $1`, key).Scan(&v)
	return v.String
}

func clStateSet(db *sql.DB, key, value string) {
	db.Exec(`INSERT INTO twoai_courtlistener_state (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
}

// clDayUse is today's ledger, for the report line.
type clDayUse struct {
	Total   int
	By      map[string]int
	Latched bool
	Status  map[int]int // answers by HTTP status, 0 for none
}

func clUsageToday(db *sql.DB, now time.Time) clDayUse {
	u := clDayUse{By: map[string]int{}, Status: map[int]int{}}
	day := clUTCDay(now)
	if rows, err := db.Query(`SELECT stage, calls FROM twoai_courtlistener_budget WHERE day = $1`, day); err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				u.By[s] = n
				u.Total += n
			}
		}
		rows.Close()
	}
	if rows, err := db.Query(`SELECT status, count(*) FROM twoai_courtlistener_calls WHERE called_at >= $1 AND called_at < $2 GROUP BY status`,
		day, day.Add(24*time.Hour)); err == nil {
		for rows.Next() {
			var s, n int
			if rows.Scan(&s, &n) == nil {
				u.Status[s] = n
			}
		}
		rows.Close()
	}
	u.Latched = u.Status[http.StatusTooManyRequests] > 0
	// THE CONNECTOR'S CALLS ARE NOT IN THE LEDGER (row 583). When CourtListener
	// reports more use today than the ledger recorded, the difference is
	// charged to an "external" share that no stage may spend, so the day's
	// room shrinks by exactly what the connector took. The live figure comes
	// from CL_USAGE_URL, the usage endpoint the connector reads (its answer
	// carries limits with rate "300/day" and a used count); unset, or
	// unreachable, the ledger counts only itself, as before.
	if live, ok := clLiveUsedToday(); ok && live > u.Total {
		u.By["external"] = live - u.Total
		u.Total = live
	}
	return u
}

var (
	clLiveOnce   sync.Once
	clLiveUsed   int
	clLiveOK     bool
	clLiveClient = &http.Client{Timeout: 20 * time.Second}
)

// clLiveUsedToday reads today's used count for the account from the
// CourtListener usage endpoint, once per process. It is not charged to the
// ledger: the usage check has its own small limit and never spends the
// account's quota.
func clLiveUsedToday() (int, bool) {
	clLiveOnce.Do(func() {
		u := strings.TrimSpace(os.Getenv("CL_USAGE_URL"))
		tok := os.Getenv("COURTLISTENER_TOKEN")
		if u == "" || tok == "" {
			return
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Token "+tok)
		req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
		resp, err := clLiveClient.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "courtlistener usage:", err)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "courtlistener usage: http %d %s\n", resp.StatusCode, trunc(string(raw), 120))
			return
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			return
		}
		// Either the connector's shape ({current_usage: {user: {limits: [...]}}})
		// or a bare list of limits; the "/day" entry is the one that matters.
		var limits []any
		if cu, ok := doc["current_usage"].(map[string]any); ok {
			if user, ok := cu["user"].(map[string]any); ok {
				limits, _ = user["limits"].([]any)
			}
		}
		if limits == nil {
			limits, _ = doc["limits"].([]any)
		}
		for _, l := range limits {
			m, _ := l.(map[string]any)
			if rate, _ := m["rate"].(string); strings.HasSuffix(rate, "/day") {
				if used, ok := m["used"].(float64); ok {
					clLiveUsed, clLiveOK = int(used), true
				}
			}
		}
	})
	return clLiveUsed, clLiveOK
}

// clUsageLine is the one line every CourtListener stage ends on and the
// freshness bridge carries.
func clUsageLine(u clDayUse, overdue int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CourtListener: used %d of %d today (refresh %d, recap %d, discovery %d, classify %d",
		u.Total, clDayLimit, u.By[clBucketRefresh], u.By[clBucketRecap], u.By[clBucketDiscovery], u.By[clBucketClassify])
	if n := u.By[clBucketAlerts]; n > 0 {
		fmt.Fprintf(&b, ", alerts %d", n)
	}
	if n := u.By[clBucketReserve]; n > 0 {
		fmt.Fprintf(&b, ", reserve %d", n)
	}
	fmt.Fprintf(&b, "); %d dockets overdue", overdue)
	if u.Latched {
		fmt.Fprintf(&b, "; HTTP 429 seen %d times, stopped for the UTC day", u.Status[http.StatusTooManyRequests])
	}
	return b.String()
}

// clUsageSummary is a day's use without the docket count, for yesterday.
func clUsageSummary(u clDayUse) string {
	s := fmt.Sprintf("used %d of %d (refresh %d, recap %d, discovery %d, classify %d, alerts %d, reserve %d)",
		u.Total, clDayLimit, u.By[clBucketRefresh], u.By[clBucketRecap], u.By[clBucketDiscovery],
		u.By[clBucketClassify], u.By[clBucketAlerts], u.By[clBucketReserve])
	if n := u.Status[http.StatusTooManyRequests]; n > 0 {
		s += fmt.Sprintf(", HTTP 429 %d times", n)
	}
	return s
}

// clReportLine measures the day and the docket schedule and returns the
// report line.
func clReportLine(db *sql.DB) string {
	now := time.Now()
	clEnsureLedger(db)
	overdue := 0
	if plan, err := clDocketPlan(db, now); err == nil {
		for _, c := range plan {
			if c.Overdue {
				overdue++
			}
		}
	}
	return clUsageLine(clUsageToday(db, now), overdue)
}

// clServerUsage reads CourtListener's own count from its usage API, which
// has a throttle of its own and keeps answering after the main one is spent,
// so it is read outside the ledger. The line is empty when it cannot be read.
func clServerUsage() string {
	tok := os.Getenv("COURTLISTENER_TOKEN")
	if tok == "" {
		return ""
	}
	req, err := http.NewRequest("GET", clAPIBase+"/api-usage/", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
	req.Header.Set("Authorization", "Token "+tok)
	resp, err := clHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("CourtListener server count: usage API answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		Current []map[string]any `json:"current_usage"`
		History map[string]any   `json:"historical_usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return clServerUsageLine(out.Current, out.History, time.Now())
}

// clServerUsageLine formats the usage API's answer: the rolling windows of
// the main "user" scope and the server's count for today.
func clServerUsageLine(current []map[string]any, history map[string]any, now time.Time) string {
	var parts []string
	for _, s := range current {
		if scope, _ := s["scope"].(string); scope != "" && scope != "user" {
			continue
		}
		win := clAnyInt(s["window_seconds"])
		label := map[int]string{60: "minute", 3600: "hour", 86400: "day"}[win]
		if label == "" {
			label = fmt.Sprintf("%ds", win)
		}
		p := fmt.Sprintf("%s %d of %d", label, clAnyInt(s["used"]), clAnyInt(s["limit"]))
		if b, _ := s["blocked"].(bool); b {
			p += " BLOCKED"
		}
		parts = append(parts, p)
	}
	today := clUTCDay(now).Format("2006-01-02")
	if v, ok := history[today]; ok {
		parts = append(parts, fmt.Sprintf("server history for %s: %d requests", today, clAnyInt(v)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "CourtListener server count: " + strings.Join(parts, ", ")
}

func clAnyInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		var n int
		fmt.Sscanf(x, "%d", &n)
		return n
	case map[string]any:
		// A history entry may be an object with a count in it.
		for _, k := range []string{"count", "total", "requests"} {
			if n, ok := x[k]; ok {
				return clAnyInt(n)
			}
		}
	}
	return 0
}
