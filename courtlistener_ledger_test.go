package main

import (
	"strings"
	"testing"
	"time"
)

var clTestMorning = time.Date(2026, 10, 6, 10, 5, 0, 0, time.UTC)
var clTestEvening = time.Date(2026, 10, 6, 18, 5, 0, 0, time.UTC)

// clTestTier applies a membership for one test and restores the default.
func clTestTier(t *testing.T, name string) {
	t.Helper()
	prev := clTierName
	clApplyTier(name)
	t.Cleanup(func() { clApplyTier(prev) })
}

func TestCLSharesAddUpToTheDay(t *testing.T) {
	for name, tier := range clTiers {
		sum := 0
		for b, n := range tier.Shares {
			sum += n
			found := false
			for _, o := range clBucketOrder {
				found = found || o == b
			}
			if !found {
				t.Errorf("tier %s: share %s is not in clBucketOrder", name, b)
			}
		}
		if sum != tier.Day {
			t.Errorf("tier %s: shares add up to %d, the day is %d", name, sum, tier.Day)
		}
		for b, n := range tier.HourCaps {
			if n >= tier.Hour {
				t.Errorf("tier %s: %s hour cap %d leaves nothing of the hour (%d)", name, b, n, tier.Hour)
			}
		}
		// The spacing alone must keep the minute window from biting.
		perMin := map[string]int{"free": 5, "1": 10, "2": 15, "3": 20, "4": 25}[name]
		if int(time.Minute/tier.Spacing) > perMin {
			t.Errorf("tier %s: %s apart allows %d a minute, the limit is %d", name, tier.Spacing, int(time.Minute/tier.Spacing), perMin)
		}
	}
}

// Tier 1 is the default since 2026-10-07: 300 a day, 75 an hour; since row
// 583 (2026-10-08) the pipeline keeps 270 of the 300 and leaves 30 to the
// connector that shares the account.
func TestCLTier1(t *testing.T) {
	clTestTier(t, "1")
	if clDayLimit != 270 || clHourLimit != 75 {
		t.Fatalf("tier 1: got %d a day, %d an hour", clDayLimit, clHourLimit)
	}
	if _, left := clAllowance(clBucketRefresh, map[string]int{}, clTestMorning, false); left != 150 {
		t.Errorf("tier 1 refresh share: got %d, want 150", left)
	}
	// After 18:00 an untouched alerts share rolls to the refresh too.
	used := map[string]int{clBucketRefresh: 150, clBucketRecap: 50, clBucketDiscovery: 30, clBucketClassify: 25}
	if _, left := clAllowance(clBucketRefresh, used, clTestEvening, false); left != 15 {
		t.Errorf("tier 1 evening rollover of the alerts share: got %d, want 15", left)
	}
	if _, left := clAllowance(clBucketAlerts, map[string]int{clBucketAlerts: 14}, clTestMorning, false); left != 1 {
		t.Errorf("tier 1 alerts share: got %d left, want 1", left)
	}
}

func TestCLAllowance(t *testing.T) {
	clTestTier(t, "free")
	cases := []struct {
		name       string
		bucket     string
		used       map[string]int
		now        time.Time
		reserveOK  bool
		wantCharge string
		wantLeft   int
	}{
		{"first run takes its share", clBucketRefresh, nil, clTestMorning, false, clBucketRefresh, 70},
		{"later run takes what is left", clBucketRefresh, map[string]int{clBucketRefresh: 50}, clTestMorning, false, clBucketRefresh, 20},
		{"recap share", clBucketRecap, map[string]int{clBucketRecap: 10}, clTestMorning, false, clBucketRecap, 15},
		{"spent share stops", clBucketClassify, map[string]int{clBucketClassify: 10}, clTestMorning, false, clBucketClassify, 0},
		{"spent share may use the reserve on a hand run", clBucketClassify, map[string]int{clBucketClassify: 10}, clTestMorning, true, clBucketReserve, 5},
		{"reserve by name", clBucketReserve, map[string]int{clBucketReserve: 2}, clTestMorning, false, clBucketReserve, 3},
		{"no rollover before 18:00", clBucketRefresh, map[string]int{clBucketRefresh: 70}, clTestMorning, false, clBucketRefresh, 0},
		{"after 18:00 unused shares roll to refresh", clBucketRefresh,
			map[string]int{clBucketRefresh: 70, clBucketRecap: 10, clBucketDiscovery: 2, clBucketClassify: 4}, clTestEvening, false,
			clBucketRefresh, 15 + 13 + 6},
		{"rollover never takes the reserve", clBucketRefresh, map[string]int{clBucketRefresh: 70}, clTestEvening, false, clBucketRefresh, 50},
		{"overspent shares do not roll negative", clBucketRefresh,
			map[string]int{clBucketRefresh: 60, clBucketRecap: 30}, clTestEvening, false, clBucketRefresh, 10 + 0 + 15 + 10 - 5},
		{"nothing but the reserve passes 120", clBucketRecap,
			map[string]int{clBucketRefresh: 110, clBucketRecap: 10}, clTestEvening, false, clBucketRecap, 0},
		{"the day is the day", clBucketReserve, map[string]int{clBucketRefresh: 125}, clTestEvening, true, clBucketReserve, 0},
	}
	for _, c := range cases {
		used := c.used
		if used == nil {
			used = map[string]int{}
		}
		charge, left := clAllowance(c.bucket, used, c.now, c.reserveOK)
		if left != c.wantLeft || (left > 0 && charge != c.wantCharge) {
			t.Errorf("%s: got %s %d, want %s %d", c.name, charge, left, c.wantCharge, c.wantLeft)
		}
	}
}

func TestCLHourWait(t *testing.T) {
	clTestTier(t, "free")
	now := clTestMorning
	var calls []time.Time
	for i := 0; i < 49; i++ {
		calls = append(calls, now.Add(-time.Duration(59-i)*time.Minute))
	}
	if w := clHourWait(calls, now, clHourLimit); w != 0 {
		t.Fatalf("49 calls in the hour should leave room, got wait %s", w)
	}
	calls = append(calls, now.Add(-30*time.Second))
	// 50 in the hour: the oldest, 59 minutes ago, ages out in one minute.
	if w := clHourWait(calls, now, clHourLimit); w != time.Minute+time.Second {
		t.Fatalf("50 calls: want a wait of 1m1s, got %s", w)
	}
	// Calls older than an hour do not count.
	old := []time.Time{now.Add(-61 * time.Minute), now.Add(-90 * time.Minute)}
	if w := clHourWait(append(old, calls[1:]...), now, clHourLimit); w != 0 {
		t.Fatalf("49 recent calls plus old ones should leave room, got wait %s", w)
	}
	// No cap means no wait.
	if w := clHourWait(calls, now, 0); w != 0 {
		t.Fatalf("no cap: got wait %s", w)
	}
}

// The refresh stops at 35 in the hour and leaves the rest to the others.
func TestCLRefreshHourCap(t *testing.T) {
	clTestTier(t, "free")
	now := clTestMorning
	var refresh []time.Time
	for i := 0; i < 35; i++ {
		refresh = append(refresh, now.Add(-time.Duration(40-i)*time.Minute))
	}
	if w := clHourWait(refresh, now, clHourCaps[clBucketRefresh]); w != 20*time.Minute+time.Second {
		t.Fatalf("35 refresh calls: want the oldest, 40 minutes ago, to age out in 20m1s, got %s", w)
	}
	if w := clHourWait(refresh[1:], now, clHourCaps[clBucketRefresh]); w != 0 {
		t.Fatalf("34 refresh calls should leave room, got %s", w)
	}
	if clHourCaps[clBucketRecap] != 0 || clHourCaps[clBucketClassify] != 0 {
		t.Fatal("only the refresh is capped inside the hour")
	}
}

func TestCLSpacingWait(t *testing.T) {
	clTestTier(t, "free")
	now := clTestMorning
	if w := clSpacingWait(time.Time{}, now); w != 0 {
		t.Errorf("no last call: want 0, got %s", w)
	}
	if w := clSpacingWait(now.Add(-5*time.Second), now); w != 7*time.Second {
		t.Errorf("5s ago: want 7s, got %s", w)
	}
	if w := clSpacingWait(now.Add(-30*time.Second), now); w != 0 {
		t.Errorf("30s ago: want 0, got %s", w)
	}
}

func TestCLCadence(t *testing.T) {
	now := clTestMorning
	cases := []struct {
		lastEntry, filed time.Time
		want             time.Duration
	}{
		{now.Add(-3 * clDay), time.Time{}, clDay},
		{now.Add(-29 * clDay), time.Time{}, clDay},
		{now.Add(-30 * clDay), time.Time{}, 7 * clDay},
		{now.Add(-89 * clDay), time.Time{}, 7 * clDay},
		{now.Add(-90 * clDay), time.Time{}, 30 * clDay},
		{time.Time{}, now.Add(-200 * clDay), 30 * clDay}, // no entry: the filing date
		{time.Time{}, time.Time{}, clDay},                // nothing known: daily
	}
	for i, c := range cases {
		if got := clCadence(clCase{LastEntry: c.lastEntry, Filed: c.filed}, now); got != c.want {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
}

func TestCLSchedule(t *testing.T) {
	now := clTestMorning
	sched := func(c clCase) clCase { clSchedule(&c, now); return c }

	// Never answered: due and overdue at once.
	if c := sched(clCase{LastEntry: now.Add(-2 * clDay)}); !c.Due || !c.Overdue {
		t.Errorf("never answered: want due and overdue, got %+v", c)
	}
	// Daily, answered yesterday evening: due this morning within the slack.
	if c := sched(clCase{LastEntry: now.Add(-2 * clDay), LastOK: now.Add(-23 * time.Hour)}); !c.Due || c.Overdue {
		t.Errorf("daily, 23h ago: want due, not overdue, got due=%v overdue=%v", c.Due, c.Overdue)
	}
	// Daily, answered three hours ago: not due.
	if c := sched(clCase{LastEntry: now.Add(-2 * clDay), LastOK: now.Add(-3 * time.Hour)}); c.Due {
		t.Error("daily, 3h ago: should not be due")
	}
	// Daily, answered three days ago: overdue.
	if c := sched(clCase{LastEntry: now.Add(-2 * clDay), LastOK: now.Add(-3 * clDay)}); !c.Overdue {
		t.Error("daily, 3 days ago: should be overdue")
	}
	// Weekly (quiet 40 days), answered 5 days ago: not due.
	if c := sched(clCase{LastEntry: now.Add(-40 * clDay), LastOK: now.Add(-5 * clDay)}); c.Due {
		t.Error("weekly, 5 days ago: should not be due")
	}
	// Monthly (quiet 100 days), answered 31 days ago: due, not yet overdue.
	if c := sched(clCase{LastEntry: now.Add(-100 * clDay), LastOK: now.Add(-31*clDay + time.Hour)}); !c.Due || c.Overdue {
		t.Errorf("monthly, 31 days ago: want due, not overdue, got due=%v overdue=%v", c.Due, c.Overdue)
	}
	// Closed with no news: never polled, never overdue, even never answered.
	if c := sched(clCase{Closed: true}); c.Polled || c.Due || c.Overdue {
		t.Errorf("closed, quiet: want left alone, got %+v", c)
	}
	// Closed and named in the news since the last answer: due.
	if c := sched(clCase{Closed: true, LastOK: now.Add(-60 * clDay), NewsMention: now.Add(-2 * clDay)}); !c.Due {
		t.Error("closed, in the news: should be due")
	}
	// Closed, named in the news but already answered since: left alone.
	if c := sched(clCase{Closed: true, LastOK: now.Add(-1 * clDay), NewsMention: now.Add(-2 * clDay)}); c.Due || c.Polled {
		t.Error("closed, answered after the mention: should be left alone")
	}
}

func TestCLPriorityOrder(t *testing.T) {
	now := clTestMorning
	cs := []clCase{
		{Slug: "rest-old", LastEntry: now.Add(-200 * clDay), LastOK: now.Add(-40 * clDay)},
		{Slug: "pinned", Pinned: true, LastEntry: now.Add(-60 * clDay), LastOK: now.Add(-8 * clDay)},
		{Slug: "hearing", LastEntry: now.Add(-20 * clDay), LastOK: now.Add(-2 * clDay), Upcoming: now.Add(5 * clDay)},
		{Slug: "new-recent-check", LastEntry: now.Add(-1 * clDay), LastOK: now.Add(-20 * time.Hour)},
		{Slug: "new-never", LastEntry: now.Add(-3 * clDay)},
		{Slug: "rest-never", LastEntry: now.Add(-50 * clDay)},
		{Slug: "hearing-too-far", LastEntry: now.Add(-20 * clDay), LastOK: now.Add(-3 * clDay), Upcoming: now.Add(20 * clDay)},
	}
	clSortByPriority(cs, now)
	want := []string{"new-never", "new-recent-check", "hearing", "pinned", "rest-never", "rest-old", "hearing-too-far"}
	for i, w := range want {
		if cs[i].Slug != w {
			var got []string
			for _, c := range cs {
				got = append(got, c.Slug)
			}
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestCLUpcomingEvent(t *testing.T) {
	now := clTestMorning // 2026-10-06
	titles := []string{
		"Set Hearing as to 730 Motion for Sanctions: Motion Hearing set for 10/20/2026 at 10:30 AM in San Francisco",
		"ELECTRONIC NOTICE of Hearing. Status Conference set for 9/30/2026 at 12:00 PM", // past
		"Set/Reset Deadlines: Motions due by 10/16/2026. Responses due by 10/30/2026",
		"Initial Case Management Conference set for 1/8/2027", // too far
		"ORDER granting stipulation, filed 10/9/2026",         // a date, but no event
	}
	got := clUpcomingEvent(titles, now)
	if want := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got := clUpcomingEvent([]string{"Scheduling Conference reset for 10/7/26 at 11:00 AM"}, now); !got.Equal(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("two-digit year: got %s", got)
	}
	if got := clUpcomingEvent([]string{"Status Conference set for 10/6/2026"}, now); !got.Equal(clUTCDay(now)) {
		t.Fatalf("today counts: got %s", got)
	}
	if got := clUpcomingEvent([]string{"nothing scheduled"}, now); !got.IsZero() {
		t.Fatalf("no event: got %s", got)
	}
}

func TestCLClosedStatus(t *testing.T) {
	cases := []struct {
		badge, status string
		want          bool
	}{
		{"Active Litigation", "Filed; docket monitoring active", false},
		{"Settled", "Settled for $1.5 billion; final approval under advisement", false},
		{"Decided", "Appeal voluntarily dismissed by OpenAI", true},
		{"Dismissed", "", true},
		{"Active Litigation", "Dismissed with prejudice on 2026-09-01", true},
		{"Active Litigation", "Closed", true},
	}
	for _, c := range cases {
		if got := clClosedStatus(c.badge, c.status); got != c.want {
			t.Errorf("%q / %q: got %v, want %v", c.badge, c.status, got, c.want)
		}
	}
}

func TestCLCaseParties(t *testing.T) {
	cases := []struct{ name, p, d string }{
		{"Thomson Reuters v. Ross Intelligence", "Thomson", "Ross"},
		{"The New York Times Co. v. Microsoft Corp.", "New", "Microsoft"},
		{"In re OpenAI Copyright Litigation", "", ""},
		{"Doe v. Character Technologies, Inc.", "", "Character"},
		{"Getty Images (US), Inc. vs Stability AI", "Getty", "Stability"},
	}
	for _, c := range cases {
		p, d := clCaseParties(c.name)
		if p != c.p || d != c.d {
			t.Errorf("%q: got %q %q, want %q %q", c.name, p, d, c.p, c.d)
		}
	}
}

func TestCLUsageLine(t *testing.T) {
	clTestTier(t, "free")
	u := clDayUse{Total: 61, By: map[string]int{clBucketRefresh: 40, clBucketRecap: 14, clBucketDiscovery: 2, clBucketClassify: 5},
		Status: map[int]int{}}
	got := clUsageLine(u, 7)
	want := "CourtListener: used 61 of 125 today (refresh 40, recap 14, discovery 2, classify 5); 7 dockets overdue"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	u.Status[429] = 1
	u.Latched = true
	if got := clUsageLine(u, 7); !strings.Contains(got, "HTTP 429 seen 1 times") {
		t.Fatalf("latched line: %q", got)
	}
}

func TestCLServerUsageLine(t *testing.T) {
	clTestTier(t, "free")
	current := []map[string]any{
		{"scope": "user", "window_seconds": float64(60), "used": float64(1), "limit": float64(5)},
		{"scope": "user", "window_seconds": float64(3600), "used": float64(12), "limit": float64(50)},
		{"scope": "user", "window_seconds": float64(86400), "used": float64(80), "limit": float64(125), "blocked": true},
		{"scope": "api_usage", "window_seconds": float64(60), "used": float64(1), "limit": float64(10)},
	}
	history := map[string]any{"2026-10-06": float64(80), "total": float64(400)}
	got := clServerUsageLine(current, history, clTestMorning)
	want := "CourtListener server count: minute 1 of 5, hour 12 of 50, day 80 of 125 BLOCKED, server history for 2026-10-06: 80 requests"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
