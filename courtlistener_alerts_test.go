package main

import (
	"encoding/json"
	"testing"
)

func TestCLAnyID(t *testing.T) {
	cases := map[string]string{
		`4214664`:   "4214664",
		`"4214664"`: "4214664",
		`"https://www.courtlistener.com/api/rest/v4/dockets/4214664/"`: "4214664",
		`null`: "",
		``:     "",
	}
	for in, want := range cases {
		if got := clAnyID(json.RawMessage(in)); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// The shape of CourtListener's docket alert webhook, from its own dummy
// event (cl/users/templates/includes/docket_alert_webhook_dummy.txt).
func TestCLWebhookPayload(t *testing.T) {
	body := `{"payload":{"results":[{"id":2208776613,"docket":4214664,"date_filed":"2022-10-11",
		"description":"MOTION for Settlement Preliminary Approval","entry_number":140,
		"recap_documents":[]}]},"webhook":{"version":2,"event_type":1,"date_created":"2022-10-18T13:03:18-07:00"}}`
	var w struct {
		Payload struct {
			Results []clEntry `json:"results"`
		} `json:"payload"`
		Webhook struct {
			EventType int `json:"event_type"`
		} `json:"webhook"`
	}
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatal(err)
	}
	if w.Webhook.EventType != 1 || len(w.Payload.Results) != 1 {
		t.Fatalf("parsed %+v", w)
	}
	e := w.Payload.Results[0]
	if clAnyID(e.Docket) != "4214664" || e.DateFiled != "2022-10-11" || string(e.EntryNumber) != "140" {
		t.Fatalf("entry %+v", e)
	}
}

// A docket on an alert is polled weekly while webhooks arrive, never more
// often, and keeps a slower cadence it already had.
func TestCLAlertBackstop(t *testing.T) {
	now := clTestMorning
	busy := clCase{LastEntry: now.Add(-2 * clDay), Alerted: true}
	if d := clCadence(busy, now); d != 7*clDay {
		t.Errorf("busy alerted docket: got %s, want a week", d)
	}
	busy.Alerted = false
	if d := clCadence(busy, now); d != clDay {
		t.Errorf("busy docket without an alert: got %s, want a day", d)
	}
	quiet := clCase{LastEntry: now.Add(-120 * clDay), Alerted: true}
	if d := clCadence(quiet, now); d != 30*clDay {
		t.Errorf("quiet alerted docket: got %s, want a month", d)
	}
}

func TestCLAlertLimit(t *testing.T) {
	t.Setenv("CL_ALERT_LIMIT", "")
	clTestTier(t, "free")
	if n := clAlertLimit(); n != 5 {
		t.Errorf("free tier: got %d alerts, want 5", n)
	}
	clApplyTier("1")
	if n := clAlertLimit(); n != 0 {
		t.Errorf("tier 1: got a limit of %d, want none", n)
	}
	t.Setenv("CL_ALERT_LIMIT", "15")
	if n := clAlertLimit(); n != 15 {
		t.Errorf("override: got %d, want 15", n)
	}
}
