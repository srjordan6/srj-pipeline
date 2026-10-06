package main

// ONE COURTLISTENER CLIENT FOR EVERY STAGE, 2026-10-05.
//
// Three stages read CourtListener: intel (docket refresh, resolve, discover),
// twoai_recap (RECAP documents) and twoai_lawsuit_classify (nature of suit,
// inside twoai_lawsuit_fill). Each had its own idea of a rate limit, and on
// the morning run of 2026-10-05 all three lost. Every caller now goes through
// clFetch, which checks the stage's time budget before every wait and every
// request, and returns errCLBudget, rather than sleeping into the stage
// deadline and being killed, when the wait would not fit.
//
// RATIONED, 2026-10-06 (content project bridge row 518, approved by Stephen).
// Since 7 May 2026 a free CourtListener account gets 5 requests a minute, 50
// an hour and 125 a day, all on rolling windows. The 2026-10-05 client waited
// out the minute windows and so walked straight into the hourly and daily
// ones: on 2026-10-06 every CourtListener stage of every run ended on a
// Retry-After of up to fifty minutes, and twoai_lawsuit_classify read nothing
// at all. So every request now first takes a slot from the ledger in
// courtlistener_ledger.go, which spaces calls twelve seconds apart across
// processes, stops at 50 in any rolling hour and at 125 in the UTC day, and
// shares the day out between the stages. An HTTP 429 is no longer waited
// out: it means the server counts differently from the ledger, so it is
// recorded and every stage stops CourtListener work for the rest of the UTC
// day instead of asking again into the wall.
//
// errCLBudget is not a failure. A stage that gets it stops cleanly, prints
// one line saying how far it got, and leaves the rest of its dockets at the
// front of the queue for the next run.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	clAPIBase = "https://www.courtlistener.com/api/rest/v4"
	// The longest Retry-After worth waiting for on a 502, 503 or 504.
	clMaxWait = 2 * time.Minute
	// Attempts per request, the first included. Only a 502, 503 or 504 is
	// tried again, and every attempt costs a slot of the day's 125.
	clMaxRetries = 2
	// First backoff step when the server sends no Retry-After; it doubles.
	clBackoffBase = 5 * time.Second
	// Kept free after any wait, so the request that follows it, and the
	// database write after that, still land inside the budget.
	clMargin = 20 * time.Second
	// Budget for a caller that never set one, such as a hand run of a
	// single function.
	clDefaultBudget = 3 * time.Minute
)

// errCLBudget means the stage's CourtListener time or share is spent. The
// message keeps the words "rate limited" because older callers match on them.
var errCLBudget = errors.New("courtlistener rate limited, stage budget spent")

var (
	clMu       sync.Mutex
	clDeadline time.Time // when this stage's CourtListener work must stop
	clHTTP     = &http.Client{Timeout: 30 * time.Second}
	// Every stage runs as its own subprocess, so process start is stage start.
	clProcessStart = time.Now()
)

// clSetStageDeadline gives CourtListener calls until the stage's deadline
// in twoaiStageDeadline less reserve, counted from when the stage started.
// The reserve is time kept back for whatever the stage does afterwards.
func clSetStageDeadline(stage string, reserve time.Duration) {
	limit := twoaiStageDeadlineDefault
	if d, ok := twoaiStageDeadline[stage]; ok {
		limit = d
	}
	clMu.Lock()
	clDeadline = clProcessStart.Add(limit - reserve)
	clMu.Unlock()
}

// clSetBudget gives CourtListener calls d from now, for a step inside a
// stage whose deadline is mostly meant for something else.
func clSetBudget(d time.Duration) {
	clMu.Lock()
	clDeadline = time.Now().Add(d)
	clMu.Unlock()
}

// clBudgetLeft is how long CourtListener calls may still take.
func clBudgetLeft() time.Duration {
	clMu.Lock()
	defer clMu.Unlock()
	if clDeadline.IsZero() {
		clDeadline = time.Now().Add(clDefaultBudget)
	}
	return time.Until(clDeadline)
}

// clIsBudget reports whether err means "stop for this run", as opposed to
// a failure that belongs to one docket.
func clIsBudget(err error) bool { return errors.Is(err, errCLBudget) }

// clRetryAfter reads a Retry-After header, which RFC 9110 allows as a
// number of seconds or as an HTTP date. ok is false when the header is
// absent or unreadable, so the caller falls back to its own backoff.
func clRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// clBackoff is the wait before attempt n+1 when the server gave no
// Retry-After: 5s, 10s, 20s, 40s, never more than clMaxWait.
func clBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := clBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= clMaxWait {
			return clMaxWait
		}
	}
	return d
}

// clFits reports whether a wait of w, plus the margin the request after it
// needs, fits in the time left.
func clFits(w, left time.Duration) bool {
	return w+clMargin <= left
}

// clFetch reads one CourtListener URL into out. It returns the last HTTP
// status seen (0 when no answer arrived) and an error, which is errCLBudget
// wrapped when the stage should stop for this run.
func clFetch(u string, out any) (int, error) {
	what := strings.TrimPrefix(u, clAPIBase)
	status := 0
	for attempt := 1; attempt <= clMaxRetries; attempt++ {
		// One slot of the day per request, taken before it is sent: the
		// server counts what it receives, refusals included.
		slot, err := clReserve(what)
		if err != nil {
			return http.StatusTooManyRequests, err
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			clRecord(slot, 0)
			return 0, err
		}
		req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
		if tok := os.Getenv("COURTLISTENER_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Token "+tok)
		}
		resp, err := clHTTP.Do(req)
		if err != nil {
			clRecord(slot, 0)
			return 0, err
		}
		status = resp.StatusCode
		clRecord(slot, status)
		switch {
		case status == http.StatusOK:
			err := json.NewDecoder(resp.Body).Decode(out)
			resp.Body.Close()
			return status, err
		case status == http.StatusTooManyRequests:
			// The server counts differently from the ledger, or something
			// else spent the account. Asking again only digs the hole
			// deeper, so the day is over for every stage.
			wait, _ := clRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			clLatchDay()
			fmt.Printf("CourtListener: HTTP 429 on %s (Retry-After %s), no more CourtListener calls until 00:00 UTC\n",
				trunc(what, 120), wait.Round(time.Second))
			return status, fmt.Errorf("%w: %s (HTTP 429, latched for the UTC day)", errCLBudget, what)
		case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
			wait, ok := clRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			if !ok {
				wait = clBackoff(attempt)
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if attempt == clMaxRetries {
				continue
			}
			if wait > clMaxWait || !clFits(wait, clBudgetLeft()) {
				return status, fmt.Errorf("%w: %s (HTTP %d, window %s does not fit)", errCLBudget, what, status, wait.Round(time.Second))
			}
			time.Sleep(wait)
		default:
			resp.Body.Close()
			return status, fmt.Errorf("courtlistener %s: %s", what, resp.Status)
		}
	}
	return status, fmt.Errorf("courtlistener %s: http %d after %d attempts", what, status, clMaxRetries)
}

// clGet reads an API path, relative to the v4 base, with extra query
// parameters merged into any the path already carries (a "next" cursor).
func clGet(path string, params map[string]string, out any) error {
	u, err := url.Parse(clAPIBase + path)
	if err != nil {
		return err
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	_, err = clFetch(u.String(), out)
	return err
}
