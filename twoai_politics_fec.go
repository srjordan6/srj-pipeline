package main

// twoai_politics_fec: money from AI companies and AI-focused political
// committees to federal candidates, from the Federal Election Commission,
// for the Money Beside Votes section of The Politics of AI.
//
// TWO HALVES, BECAUSE A COMMITTEE IS NEVER MATCHED ON NAME ALONE.
//
// 1. Discovery. For each organisation in polFECSeeds the FEC committee search
//    is asked for political committees whose name matches, and every hit is
//    written to twoai_pol_committees as status 'proposed' with the seed it
//    came from. A proposed committee is never read for money. "Google" finds
//    Google's PAC and also committees that only contain the word; a person
//    confirms which is which by setting status = 'confirmed' in SQL, after
//    checking the committee's own FEC page. The stage prints the proposed list
//    every run so the queue is visible.
//
// 2. Money, for confirmed committees only. Schedule B disbursements to other
//    committees (a PAC's contributions to candidate campaigns) and, for super
//    PACs, Schedule E independent expenditures for or against a named
//    candidate. Rows are stored with the FEC's own sub_id as the key and the
//    image_number and pdf_url so every figure on the site links to the page
//    of the filing it came from.
//
// KEEPING IT CURRENT. Per committee, per schedule, the next run asks only for
// transactions dated after the newest one held, less seven days, because
// committees file late and amend. FEC paging is keyset, so the stage passes
// back whatever last_indexes the previous page returned. polFECCallBudget
// caps calls a run well inside the 1,000 an hour a personal key allows.
//
// NOTHING IS DELETED. Amended transactions arrive with their own sub_id and
// are kept; the page build shows the latest amendment.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/lib/pq"
)

const (
	polFECBase       = "https://api.open.fec.gov/v1"
	polFECCallBudget = 400
	polFECCycle      = 2026
)

// Organisations whose political committees are worth proposing. Names only
// seed the search; nothing is trusted until confirmed. Add a seed here when
// a company or fund becomes relevant.
var polFECSeeds = []string{
	"Microsoft", "Alphabet", "Google", "Meta Platforms", "Amazon", "Apple", "NVIDIA",
	"OpenAI", "Anthropic", "IBM", "Oracle", "Palantir", "Intel", "Qualcomm", "Salesforce",
	"Andreessen Horowitz", "artificial intelligence",
}

type polFECPage struct {
	Pagination struct {
		Count       int               `json:"count"`
		Pages       int               `json:"pages"`
		LastIndexes map[string]any    `json:"last_indexes"`
	} `json:"pagination"`
	Results []json.RawMessage `json:"results"`
}

func polFECGet(client *http.Client, key, path string, v url.Values, calls *int) (*polFECPage, error) {
	if *calls >= polFECCallBudget {
		return nil, fmt.Errorf("call budget reached")
	}
	*calls++
	v.Set("api_key", key)
	resp, err := client.Get(polFECBase + path + "?" + v.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limited")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: http %d", path, resp.StatusCode)
	}
	var pg polFECPage
	if err := json.NewDecoder(resp.Body).Decode(&pg); err != nil {
		return nil, err
	}
	return &pg, nil
}

func twoaiPoliticsFEC(db *sql.DB) error {
	key := os.Getenv("FEC_API_KEY")
	if key == "" {
		return fmt.Errorf("FEC_API_KEY not set")
	}
	client := &http.Client{Timeout: 45 * time.Second}
	calls := 0

	// 1. Discovery.
	proposed := 0
	for _, seed := range polFECSeeds {
		v := url.Values{}
		v.Set("q", seed)
		v.Set("per_page", "50")
		pg, err := polFECGet(client, key, "/committees/", v, &calls)
		if err != nil {
			fmt.Printf("twoai_politics_fec: search %q: %v\n", seed, err)
			continue
		}
		for _, raw := range pg.Results {
			var c struct {
				ID          string   `json:"committee_id"`
				Name        string   `json:"name"`
				Type        string   `json:"committee_type"`
				TypeFull    string   `json:"committee_type_full"`
				Designation string   `json:"designation_full"`
				Candidates  []string `json:"candidate_ids"`
				Cycles      []int    `json:"cycles"`
			}
			if json.Unmarshal(raw, &c) != nil || c.ID == "" {
				continue
			}
			// Candidate campaigns and parties are not AI money sources.
			switch c.Type {
			case "H", "S", "P", "X", "Y", "Z":
				continue
			}
			res, err := db.Exec(`
				INSERT INTO twoai_pol_committees (committee_id, uid, name, committee_type, committee_type_full,
					designation, candidate_ids, cycles, seed, status, raw)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'proposed',$10)
				ON CONFLICT (committee_id) DO UPDATE SET name = EXCLUDED.name, committee_type = EXCLUDED.committee_type,
					committee_type_full = EXCLUDED.committee_type_full, designation = EXCLUDED.designation,
					candidate_ids = EXCLUDED.candidate_ids, cycles = EXCLUDED.cycles, raw = EXCLUDED.raw, last_seen = now()
				RETURNING (xmax = 0)`,
				c.ID, polUID("fec-committee", c.ID), ldaClean(c.Name), c.Type, c.TypeFull, c.Designation,
				pq.Array(c.Candidates), pq.Array(c.Cycles), seed, ldaRaw(raw))
			_ = res
			if err != nil {
				fmt.Printf("twoai_politics_fec: store committee %s: %v\n", c.ID, err)
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = db.QueryRow(`SELECT count(*) FROM twoai_pol_committees WHERE status = 'proposed'`).Scan(&proposed)

	// 2. Money, confirmed committees only.
	rows, err := db.Query(`SELECT committee_id, committee_type FROM twoai_pol_committees WHERE status = 'confirmed' ORDER BY committee_id`)
	if err != nil {
		return err
	}
	type cm struct{ id, typ string }
	var confirmed []cm
	for rows.Next() {
		var c cm
		if rows.Scan(&c.id, &c.typ) == nil {
			confirmed = append(confirmed, c)
		}
	}
	rows.Close()

	storedB, storedE := 0, 0
	for _, c := range confirmed {
		// Schedule B: disbursements to other committees.
		var newest sql.NullTime
		_ = db.QueryRow(`SELECT max(txn_date) FROM twoai_pol_money WHERE committee_id = $1 AND schedule = 'B'`, c.id).Scan(&newest)
		minDate := fmt.Sprintf("%d-01-01", polFECCycle-1)
		if newest.Valid {
			minDate = newest.Time.AddDate(0, 0, -7).Format("2006-01-02")
		}
		v := url.Values{}
		v.Set("committee_id", c.id)
		v.Set("two_year_transaction_period", fmt.Sprint(polFECCycle))
		v.Set("min_date", minDate)
		v.Set("per_page", "100")
		v.Set("sort", "disbursement_date")
		for page := 0; page < 20; page++ {
			pg, err := polFECGet(client, key, "/schedules/schedule_b/", v, &calls)
			if err != nil {
				fmt.Printf("twoai_politics_fec: %s schedule B: %v\n", c.id, err)
				break
			}
			for _, raw := range pg.Results {
				var t struct {
					SubID       any     `json:"sub_id"`
					Recipient   string  `json:"recipient_name"`
					RecipientID string  `json:"recipient_committee_id"`
					Amount      float64 `json:"disbursement_amount"`
					Date        string  `json:"disbursement_date"`
					Desc        string  `json:"disbursement_description"`
					Image       string  `json:"image_number"`
					PDF         string  `json:"pdf_url"`
				}
				if json.Unmarshal(raw, &t) != nil || t.SubID == nil || t.RecipientID == "" {
					continue
				}
				if _, err := db.Exec(`
					INSERT INTO twoai_pol_money (sub_id, uid, schedule, committee_id, counterparty_name, counterparty_committee_id,
						candidate_id, support_oppose, amount, txn_date, description, image_number, pdf_url, raw)
					VALUES ($1,$2,'B',$3,$4,$5,NULL,NULL,$6,$7::date,$8,$9,$10,$11)
					ON CONFLICT (sub_id) DO UPDATE SET amount = EXCLUDED.amount, raw = EXCLUDED.raw, last_seen = now()`,
					fmt.Sprint(t.SubID), polUID("fec-txn", fmt.Sprint(t.SubID)), c.id, ldaClean(t.Recipient), t.RecipientID,
					t.Amount, polDate(t.Date), ldaClean(t.Desc), t.Image, t.PDF, ldaRaw(raw)); err == nil {
					storedB++
				}
			}
			if len(pg.Results) == 0 || len(pg.Pagination.LastIndexes) == 0 {
				break
			}
			for k, val := range pg.Pagination.LastIndexes {
				v.Set(k, fmt.Sprint(val))
			}
			time.Sleep(300 * time.Millisecond)
		}

		// Schedule E: independent expenditures, which is how a super PAC spends.
		if c.typ != "O" && c.typ != "U" && c.typ != "I" {
			continue
		}
		newest = sql.NullTime{}
		_ = db.QueryRow(`SELECT max(txn_date) FROM twoai_pol_money WHERE committee_id = $1 AND schedule = 'E'`, c.id).Scan(&newest)
		minDate = fmt.Sprintf("%d-01-01", polFECCycle-1)
		if newest.Valid {
			minDate = newest.Time.AddDate(0, 0, -7).Format("2006-01-02")
		}
		ve := url.Values{}
		ve.Set("committee_id", c.id)
		ve.Set("min_date", minDate)
		ve.Set("per_page", "100")
		for page := 0; page < 20; page++ {
			pg, err := polFECGet(client, key, "/schedules/schedule_e/", ve, &calls)
			if err != nil {
				fmt.Printf("twoai_politics_fec: %s schedule E: %v\n", c.id, err)
				break
			}
			for _, raw := range pg.Results {
				var t struct {
					SubID      any     `json:"sub_id"`
					Candidate  string  `json:"candidate_id"`
					CandName   string  `json:"candidate_name"`
					SupOpp     string  `json:"support_oppose_indicator"`
					Amount     float64 `json:"expenditure_amount"`
					Date       string  `json:"expenditure_date"`
					Dissem     string  `json:"dissemination_date"`
					Desc       string  `json:"expenditure_description"`
					Payee      string  `json:"payee_name"`
					Image      string  `json:"image_number"`
					PDF        string  `json:"pdf_url"`
				}
				if json.Unmarshal(raw, &t) != nil || t.SubID == nil || t.Candidate == "" {
					continue
				}
				d := t.Date
				if d == "" {
					d = t.Dissem
				}
				if _, err := db.Exec(`
					INSERT INTO twoai_pol_money (sub_id, uid, schedule, committee_id, counterparty_name, counterparty_committee_id,
						candidate_id, support_oppose, amount, txn_date, description, image_number, pdf_url, raw)
					VALUES ($1,$2,'E',$3,$4,NULL,$5,NULLIF($6,''),$7,$8::date,$9,$10,$11,$12)
					ON CONFLICT (sub_id) DO UPDATE SET amount = EXCLUDED.amount, raw = EXCLUDED.raw, last_seen = now()`,
					fmt.Sprint(t.SubID), polUID("fec-txn", fmt.Sprint(t.SubID)), c.id, ldaClean(t.CandName), t.Candidate,
					t.SupOpp, t.Amount, polDate(d), ldaClean(t.Desc+" "+t.Payee), t.Image, t.PDF, ldaRaw(raw)); err == nil {
					storedE++
				}
			}
			if len(pg.Results) == 0 || len(pg.Pagination.LastIndexes) == 0 {
				break
			}
			for k, val := range pg.Pagination.LastIndexes {
				ve.Set(k, fmt.Sprint(val))
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	if proposed > 0 {
		fmt.Printf("twoai_politics_fec: %d committees await confirmation (UPDATE twoai_pol_committees SET status='confirmed' WHERE committee_id IN (...)):\n", proposed)
		if r, err := db.Query(`SELECT committee_id, name, committee_type, seed FROM twoai_pol_committees WHERE status='proposed' ORDER BY seed, name LIMIT 60`); err == nil {
			for r.Next() {
				var id, name, typ, seed string
				if r.Scan(&id, &name, &typ, &seed) == nil {
					fmt.Printf("  %s  %-6s  %-22s  %s  https://www.fec.gov/data/committee/%s/\n", id, typ, seed, name, id)
				}
			}
			r.Close()
		}
	}
	fmt.Printf("twoai_politics_fec: calls=%d confirmed_committees=%d schedule_b_rows=%d schedule_e_rows=%d proposed=%d ok=true\n",
		calls, len(confirmed), storedB, storedE, proposed)
	return nil
}

// FEC dates arrive as 2026-03-31T00:00:00; the date column wants the day.
func polDate(s string) interface{} {
	if len(s) >= 10 {
		return s[:10]
	}
	return nil
}
