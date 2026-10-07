package main

// legiscan_add: put one named bill into the corpus.
//
//	pipeline legiscan_add CA SB690
//
// Stephen, 2026-09-13: a National Law Review roundup led with California's
// SB 690, and the bill was not on /ai-laws/california/. The three other bills
// in the same article - CA SB 574, CA AB 1709, the NJ Kids Code Act - were
// all there, because they say "artificial intelligence" or "chatbot" in their
// text and the daily sweep's search terms found them. SB 690 restricts
// website-tracking lawsuits under the state's invasion-of-privacy law. It is
// privacy legislation that the AI-policy press covers as part of the same
// beat, and it does not say AI anywhere the sweep looks.
//
// That is a real gap, and it is not a bug in the sweep. The sweep finds bills
// by vocabulary, and a bill can matter to this beat without using the
// vocabulary. So this exists: a person who knows a bill belongs here names it,
// and it enters through exactly the same door - getBill, insertDoc, the same
// change_hash dedupe - so it renders, updates and ages like every other bill
// rather than as a hand-typed row that nothing refreshes.
//
// It does not judge relevance. Naming the bill is the judgement.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func legiscanAdd(db *sql.DB, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: pipeline legiscan_add <STATE> <BILL>   e.g. legiscan_add CA SB690")
	}
	state := strings.ToUpper(strings.TrimSpace(args[0]))
	bill := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(args[1]), " ", ""))
	key := os.Getenv("LEGISCAN_API_KEY")
	if key == "" {
		return fmt.Errorf("LEGISCAN_API_KEY not set")
	}
	var sourceID int
	if err := db.QueryRow(`SELECT id FROM pipeline.sources WHERE key='legiscan'`).Scan(&sourceID); err != nil {
		return fmt.Errorf("legiscan source row: %w", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}

	// getSearch by state and bill number returns the bill_id the sweep would
	// have used had the bill matched a search term.
	su := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getSearch&state=%s&query=%s", key, state, bill)
	resp, err := client.Get(su)
	if err != nil {
		return err
	}
	sb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var sr struct {
		Status       string                     `json:"status"`
		SearchResult map[string]json.RawMessage `json:"searchresult"`
	}
	if json.Unmarshal(sb, &sr) != nil || sr.Status != "OK" {
		return fmt.Errorf("legiscan search failed: %.200s", sb)
	}
	// searchresult is a map of index -> result, plus a "summary" key.
	billID, changeHash := 0, ""
	for k, raw := range sr.SearchResult {
		if k == "summary" {
			continue
		}
		var r struct {
			BillID     int    `json:"bill_id"`
			BillNumber string `json:"bill_number"`
			State      string `json:"state"`
			ChangeHash string `json:"change_hash"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		if strings.EqualFold(r.State, state) && strings.EqualFold(strings.ReplaceAll(r.BillNumber, " ", ""), bill) {
			billID, changeHash = r.BillID, r.ChangeHash
			break
		}
	}
	// A session that has adjourned is no longer "current" to getSearch, so a
	// bill from it cannot be found by number (Colorado HB 26-1263, 2026-09-27,
	// adjourned in May). The corpus may already hold its bill_id from an
	// earlier search row; if so, hydrate by id. A third argument can also
	// supply the id directly: legiscan_add CO HB1263 2121012.
	if billID == 0 && len(args) >= 3 {
		fmt.Sscanf(strings.TrimSpace(args[2]), "%d", &billID)
	}
	if billID == 0 {
		db.QueryRow(`SELECT (raw->>'bill_id')::int FROM pipeline.documents
			WHERE source_id=$1 AND url ILIKE $2 AND raw ? 'bill_id' ORDER BY fetched_at DESC LIMIT 1`,
			sourceID, "%/"+state+"/bill/"+bill+"/%").Scan(&billID)
		if billID != 0 {
			fmt.Printf("legiscan_add: %s %s not in the current session; using bill_id %d from the corpus\n", state, bill, billID)
		}
	}
	if billID == 0 {
		return fmt.Errorf("LegiScan has no %s %s in the current session; pass its LegiScan bill_id as a third argument", state, bill)
	}

	var exists bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pipeline.documents WHERE source_id=$1 AND external_id=$2 AND change_hash=$3)`,
		sourceID, fmt.Sprintf("%d", billID), changeHash).Scan(&exists)
	if exists {
		fmt.Printf("legiscan_add: %s %s (bill_id %d) is already in the corpus at this change_hash; nothing to do\n", state, bill, billID)
		return nil
	}

	bu := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getBill&id=%d", key, billID)
	br, err := client.Get(bu)
	if err != nil {
		return err
	}
	bb, _ := io.ReadAll(br.Body)
	br.Body.Close()
	var bp struct {
		Status string `json:"status"`
		Bill   struct {
			BillID     int    `json:"bill_id"`
			State      string `json:"state"`
			BillNumber string `json:"bill_number"`
			Title      string `json:"title"`
			URL        string `json:"url"`
			StatusDate string `json:"status_date"`
		} `json:"bill"`
	}
	if json.Unmarshal(bb, &bp) != nil || bp.Status != "OK" || bp.Bill.BillID == 0 {
		return fmt.Errorf("legiscan getBill %d: bad payload", billID)
	}
	var pub any
	if bp.Bill.StatusDate != "" {
		pub = bp.Bill.StatusDate
	}
	ok, err := insertDoc(db, sourceID, fmt.Sprintf("%d", billID), changeHash, bp.Bill.URL,
		bp.Bill.State+" "+bp.Bill.BillNumber+": "+bp.Bill.Title, pub, bb)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("legiscan_add: %s %s already present\n", state, bill)
		return nil
	}
	fmt.Printf("legiscan_add: added %s %s (bill_id %d): %s\n", state, bill, billID, bp.Bill.Title)
	fmt.Println("legiscan_add: it renders on /ai-laws/ after the next twoai run, and the daily sweep keeps it current from here")
	return nil
}

// THE QUEUE. legiscan_add is a hand run on Stephen's PC, where the LegiScan
// key lives. A session elsewhere that knows a bill belongs here (theworldofai
// row 553, 2026-10-07: CA AB 1864, gene synthesis screening, signed with the
// AI package on 2026-09-30 and not AI by vocabulary) has no way to run it. So
// it names the bill in twoai_legiscan_queue, and the daily legiscan stage
// drains the queue through legiscanAdd, the same door, marking each row done
// or recording why it failed. A bill_id in the row covers an adjourned
// session, as the third argument does by hand.
func legiscanEnsureQueue(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_legiscan_queue (id serial PRIMARY KEY, state text NOT NULL, bill text NOT NULL,
		bill_id int, requested_by text, reason text, queued_at timestamptz NOT NULL DEFAULT now(),
		done_at timestamptz, outcome text)`)
}

func legiscanDrainQueue(db *sql.DB) {
	legiscanEnsureQueue(db)
	rows, err := db.Query(`SELECT id, state, bill, COALESCE(bill_id, 0) FROM twoai_legiscan_queue WHERE done_at IS NULL ORDER BY id LIMIT 20`)
	if err != nil {
		return
	}
	type item struct {
		id, billID  int
		state, bill string
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.state, &it.bill, &it.billID) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	for _, it := range items {
		args := []string{it.state, it.bill}
		if it.billID > 0 {
			args = append(args, fmt.Sprintf("%d", it.billID))
		}
		err := legiscanAdd(db, args)
		outcome := "added"
		if err != nil {
			outcome = "failed: " + trunc(err.Error(), 300)
			fmt.Printf("legiscan queue: %s %s: %v\n", it.state, it.bill, err)
		} else {
			fmt.Printf("legiscan queue: %s %s added\n", it.state, it.bill)
		}
		db.Exec(`UPDATE twoai_legiscan_queue SET done_at = now(), outcome = $2 WHERE id = $1`, it.id, outcome)
	}
}
