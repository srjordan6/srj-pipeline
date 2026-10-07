package main

import "testing"

// Written from Stephen's description of the alert mail (row 565), not from a
// sample; the first real mail is kept raw in cl_alert_emails to check it.
func TestClParseAlertMailLabelled(t *testing.T) {
	text := `1 New Docket Entry for The New York Times Company v. Microsoft Corporation (1:23-cv-11195)

View Docket: https://www.courtlistener.com/docket/68117049/the-new-york-times-company-v-microsoft-corporation/

Document Number: 1645
Date Filed: Oct. 7, 2026
Description: Order on Motion for Leave to File Document

Document Number: 1646
Date Filed: Oct. 7, 2026
Description: Notice of Appearance
`
	docket, entries := clParseAlertMail(text)
	if docket != "68117049" {
		t.Fatalf("docket %q", docket)
	}
	if len(entries) != 2 || entries[0].Description != "Order on Motion for Leave to File Document" ||
		entries[0].DateFiled != "2026-10-07" || string(entries[0].EntryNumber) != `"1645"` {
		t.Fatalf("entries %+v", entries)
	}
}

func TestClParseAlertMailHeadingFirst(t *testing.T) {
	text := `Order on Motion for Leave to File Document
Document Number: 30
Date Filed: October 7, 2026
https://www.courtlistener.com/docket/74687027/wikihow-inc-v-openai-inc/`
	docket, entries := clParseAlertMail(text)
	if docket != "74687027" || len(entries) != 1 || entries[0].Description != "Order on Motion for Leave to File Document" {
		t.Fatalf("docket %q entries %+v", docket, entries)
	}
}

func TestClIsAlertMail(t *testing.T) {
	if !clIsAlertMail("Stephen <srj@srjconsultingservices.com>", "1 New Docket Entry for Gagleard v. Perplexity AI, Inc. (3:25-cv-04444)") {
		t.Error("forwarded alert by subject")
	}
	if clIsAlertMail("someone@example.com", "Hello") {
		t.Error("ordinary mail")
	}
}
