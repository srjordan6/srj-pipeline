package main

// ONE COURTLISTENER CLIENT FOR EVERY STAGE, 2026-10-05.
//
// Three stages read CourtListener: intel (docket refresh, resolve, discover),
// twoai_recap (RECAP documents) and twoai_lawsuit_classify (nature of suit,
// inside twoai_lawsuit_fill). Each had its own idea of a rate limit, and on
// the morning run of 2026-10-05 all three lost. CourtListener answered with
// Retry-After windows of 46 to 51 seconds. clGet treated any wait of thirty
// seconds or more as a spent quota and latched, so twoai_recap stopped after
// 2 of 12 dockets having used 14 seconds of an eight minute deadline; intel
// waited three windows out by hand and checked 9 dockets in five minutes;
// discover gave up on its first call; and the classifier, on its own client
// with a fixed 5, 10, 15 second ladder that ignored Retry-After, saw HTTP 429
// twice and recorded nothing. With 129 of 147 federal dockets never answered,
// the freshness report showed them all overdue.
//
// A minute-long window is a short throttle, not a spent quota, and the stage
// can afford to wait it out as long as there is time left. So every caller
// now goes through clFetch, which:
//
//   - honours Retry-After, in seconds or as an HTTP date, up to clMaxWait;
//     a longer window really is a spent quota and ends the work at once;
//   - backs off exponentially when no Retry-After is sent, for at most
//     clMaxRetries attempts;
//   - checks the stage's time budget before every wait and every request,
//     and returns errCLBudget, rather than sleeping into the stage deadline
//     and being killed, when the wait would not fit.
//
// The latch from 2026-08-30 stays: once CourtListener has said wait, every
// later call in the run waits for the same window instead of asking again.
// What changed is that the latch is waited out while the budget allows,
// instead of failing every later call on sight.
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
	// The longest Retry-After worth waiting for. CourtListener's short
	// windows run about fifty seconds; anything past two minutes is the
	// hourly quota and no amount of waiting inside one stage will help.
	clMaxWait = 2 * time.Minute
	// Attempts per request, the first included.
	clMaxRetries = 4
	// First backoff step when the server sends no Retry-After; it doubles.
	clBackoffBase = 5 * time.Second
	// Kept free after any wait, so the request that follows it, and the
	// database write after that, still land inside the budget.
	clMargin = 20 * time.Second
	// Budget for a caller that never set one, such as a hand run of a
	// single function.
	clDefaultBudget = 3 * time.Minute
)

// errCLBudget means the stage's CourtListener time is spent. The message
// keeps the words "rate limited" because older callers match on them.
var errCLBudget = errors.New("courtlistener rate limited, stage budget spent")

var (
	clMu             sync.Mutex
	clDeadline       time.Time // when this stage's CourtListener work must stop
	clThrottledUntil time.Time // the server's last Retry-After, as a time
	clHTTP           = &http.Client{Timeout: 30 * time.Second}
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

func clLatch(d time.Duration) {
	clMu.Lock()
	defer clMu.Unlock()
	if until := time.Now().Add(d); until.After(clThrottledUntil) {
		clThrottledUntil = until
	}
}

// clWaitOutLatch sleeps through an open throttle window when it fits in the
// budget, and returns errCLBudget when it does not.
func clWaitOutLatch(what string) error {
	clMu.Lock()
	w := time.Until(clThrottledUntil)
	clMu.Unlock()
	left := clBudgetLeft()
	if w <= 0 {
		if left < clMargin {
			return fmt.Errorf("%w: %s (%s left)", errCLBudget, what, left.Round(time.Second))
		}
		return nil
	}
	if w > clMaxWait || !clFits(w, left) {
		return fmt.Errorf("%w: %s (window %s, %s left)", errCLBudget, what, w.Round(time.Second), left.Round(time.Second))
	}
	time.Sleep(w + time.Second)
	return nil
}

// clFetch reads one CourtListener URL into out. It returns the last HTTP
// status seen (0 when no answer arrived) and an error, which is errCLBudget
// wrapped when the stage should stop for this run.
func clFetch(u string, out any) (int, error) {
	what := strings.TrimPrefix(u, clAPIBase)
	status := 0
	for attempt := 1; attempt <= clMaxRetries; attempt++ {
		if err := clWaitOutLatch(what); err != nil {
			return http.StatusTooManyRequests, err
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
		if tok := os.Getenv("COURTLISTENER_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Token "+tok)
		}
		resp, err := clHTTP.Do(req)
		if err != nil {
			return 0, err
		}
		status = resp.StatusCode
		switch {
		case status == http.StatusOK:
			err := json.NewDecoder(resp.Body).Decode(out)
			resp.Body.Close()
			return status, err
		case status == http.StatusTooManyRequests || status == http.StatusBadGateway ||
			status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
			wait, ok := clRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			if !ok {
				wait = clBackoff(attempt)
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if wait > clMaxWait {
				// The hourly quota, not a burst. Latch it so every later
				// call in this run stops at once rather than asking again.
				clLatch(wait)
				return status, fmt.Errorf("%w: %s (server asked for %s)", errCLBudget, what, wait.Round(time.Second))
			}
			clLatch(wait)
			// The wait itself happens at the top of the loop, budget checked.
		default:
			resp.Body.Close()
			return status, fmt.Errorf("courtlistener %s: %s", what, resp.Status)
		}
	}
	if status == http.StatusTooManyRequests {
		return status, fmt.Errorf("%w: %s (still throttled after %d attempts)", errCLBudget, what, clMaxRetries)
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
