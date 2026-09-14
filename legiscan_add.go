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
		Status       string `json:"status"`
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
	if billID == 0 {
		return fmt.Errorf("LegiScan has no %s %s in the current session", state, bill)
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
