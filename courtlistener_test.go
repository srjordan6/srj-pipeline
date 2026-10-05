package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"47", 47 * time.Second, true},
		{" 51 ", 51 * time.Second, true},
		{"-3", 0, true},
		{"Mon, 05 Oct 2026 12:01:30 GMT", 90 * time.Second, true},
		{"Mon, 05 Oct 2026 11:59:00 GMT", 0, true}, // already past
		{"soon", 0, false},
	}
	for _, c := range cases {
		got, ok := clRetryAfter(c.in, now)
		if got != c.want || ok != c.ok {
			t.Errorf("clRetryAfter(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCLBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, clMaxWait, clMaxWait}
	for i, w := range want {
		if got := clBackoff(i + 1); got != w {
			t.Errorf("clBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := clBackoff(0); got != clBackoffBase {
		t.Errorf("clBackoff(0) = %v, want %v", got, clBackoffBase)
	}
}

func TestCLFits(t *testing.T) {
	if !clFits(50*time.Second, 2*time.Minute) {
		t.Error("a 50s window should fit in two minutes")
	}
	if clFits(50*time.Second, 60*time.Second) {
		t.Error("a 50s window should not fit in 60s once the margin is kept")
	}
}

func clResetForTest(budget time.Duration) {
	clMu.Lock()
	clThrottledUntil = time.Time{}
	clDeadline = time.Now().Add(budget)
	clMu.Unlock()
}

// A short Retry-After is waited out and the read then succeeds.
func TestCLFetchWaitsOutShortThrottle(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"nature_of_suit":"820 Copyright"}`))
	}))
	defer srv.Close()
	clResetForTest(time.Minute)
	var out struct {
		NatureOfSuit string `json:"nature_of_suit"`
	}
	status, err := clFetch(srv.URL, &out)
	if err != nil || status != 200 || out.NatureOfSuit != "820 Copyright" {
		t.Fatalf("got status %d err %v out %+v", status, err, out)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

// A window that does not fit the budget stops at once with errCLBudget, and
// the latch stops the next call without asking the server again.
func TestCLFetchStopsWhenBudgetSpent(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "50")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	clResetForTest(30 * time.Second)
	started := time.Now()
	var out struct{}
	_, err := clFetch(srv.URL, &out)
	if !clIsBudget(err) {
		t.Fatalf("want errCLBudget, got %v", err)
	}
	_, err = clFetch(srv.URL, &out)
	if !clIsBudget(err) {
		t.Fatalf("second call: want errCLBudget, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 call to the server, got %d", calls)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("stopping should not sleep, took %s", time.Since(started))
	}
	clResetForTest(time.Minute)
}

// A non-throttle refusal is a per-docket error, not a reason to stop.
func TestCLFetchNotFoundIsNotBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	clResetForTest(time.Minute)
	var out struct{}
	status, err := clFetch(srv.URL, &out)
	if err == nil || clIsBudget(err) || status != 404 {
		t.Fatalf("got status %d err %v", status, err)
	}
}
