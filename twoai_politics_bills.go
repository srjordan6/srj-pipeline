package main

// twoai_politics_bills: every AI bill in the current Congress, its sponsors,
// and every recorded roll call vote on it, for The Politics of AI hub.
//
// WHY NOT THE EXISTING legiscan STAGE. That stage searches LegiScan by
// keyword across all fifty states, and relevance ranking leaves Congress
// thin: on 2026-09-21 it held 20 US bills. This stage reads the US master
// list instead, one call returning every bill in the session with its
// change_hash, and filters by title and description locally, so no federal
// AI bill is missed because it ranked low.
//
// KEEPING IT CURRENT. The master list is read every run (one call). A bill is
// fetched with getBill only when its change_hash differs from the one held,
// which is LegiScan's own signal that something moved: a new action, a new
// cosponsor, a vote. A roll call is fetched once, when first seen on a bill,
// because a recorded vote does not change. Budget: at most polBillBudget
// getBill and polVoteBudget getRollCall calls a run, so a backlog clears
// over a few days and the 30,000 a month LegiScan allowance is never close.
//
// ISSUES AND PREEMPTION are tagged by fixed patterns on the bill's own title
// and description, not by a model, so the From Issue to Law counts are
// reproducible and a reader can see why a bill is counted where it is.
//
// NOTHING IS DELETED. Members, bills, roll calls and votes are upserted.
//
// NOT YET DONE, stated: members carry LegiScan people_id and the identifiers
// LegiScan supplies; resolving them to site person uids, and to FEC candidate
// ids through a hard-identifier crosswalk, is the page-build step. Nothing
// from these tables publishes until that exists.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	polBillBudget = 150 // 289 AI bills on the first run; clears the backlog in two runs
	polVoteBudget = 60
)

// AI in a federal bill title or description. "AI" is matched case
// sensitively as a whole word, since Congress titles use it as a proper
// noun ("AI LEAD Act") and lower-case "ai" never means it.
var polAIWordsRe = regexp.MustCompile(`(?i)artificial intelligence|machine learning|deepfake|deep fake|generative model|large language model|chatbot|algorithmic|automated decision|facial recognition|digital replica|synthetic media|foundation model|frontier model`)
var polAICapsRe = regexp.MustCompile(`\bAI\b`)

// Fixed issue patterns for From Issue to Law. Order is display order.
var polIssues = []struct {
	slug string
	re   *regexp.Regexp
}{
	{"deepfakes-and-elections", regexp.MustCompile(`(?i)deepfake|deep fake|digital replica|synthetic media|election|campaign|political advertis`)},
	{"children-and-minors", regexp.MustCompile(`(?i)child|minor|kids|teen|student|school`)},
	{"jobs-and-workforce", regexp.MustCompile(`(?i)worker|workforce|employment|jobs|labor|displace|apprentic`)},
	{"bias-and-civil-rights", regexp.MustCompile(`(?i)bias|discriminat|civil rights|algorithmic accountab|fair`)},
	{"copyright-and-likeness", regexp.MustCompile(`(?i)copyright|likeness|voice|creator|artist|intellectual property|transparen.*train`)},
	{"frontier-safety", regexp.MustCompile(`(?i)frontier|safety institute|catastroph|biosecur|biological|chemical|nuclear|red.team`)},
	{"national-security-and-exports", regexp.MustCompile(`(?i)national security|defense|military|export|china|chip|semiconductor|adversar`)},
	{"energy-and-data-centers", regexp.MustCompile(`(?i)data center|datacenter|energy|electric|grid|power|permit`)},
	{"government-use", regexp.MustCompile(`(?i)federal agenc|government use|procurement|agency use|OMB|chief artificial intelligence officer`)},
	{"privacy", regexp.MustCompile(`(?i)privacy|personal data|surveillance|biometric`)},
	{"health", regexp.MustCompile(`(?i)health|medic|clinical|FDA|patient`)},
}

// Federal override or pause of state AI law.
var polPreemptRe = regexp.MustCompile(`(?i)preempt|moratorium|state law|state-level|national standard|uniform national|patchwork`)

func polIsAI(title, desc string) bool {
	s := title + " " + desc
	return polAIWordsRe.MatchString(s) || polAICapsRe.MatchString(s)
}

func polIssueTags(title, desc string) []string {
	s := title + " " + desc
	out := []string{}
	for _, is := range polIssues {
		if is.re.MatchString(s) {
			out = append(out, is.slug)
		}
	}
	return out
}

func polUID(ns, id string) string {
	h := sha256.Sum256([]byte(ns + ":" + id))
	return hex.EncodeToString(h[:4])
}

func polGetLegiscan(client *http.Client, key, op, extra string) ([]byte, error) {
	u := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=%s%s", key, op, extra)
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: http %d", op, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

type polMasterRow struct {
	BillID      int          `json:"bill_id"`
	Number      string       `json:"number"`
	ChangeHash  legiscanHash `json:"change_hash"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
}

type polBill struct {
	BillID      int          `json:"bill_id"`
	BillNumber  string       `json:"bill_number"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	URL         string       `json:"url"`
	StateLink   string       `json:"state_link"`
	Status      int          `json:"status"`
	StatusDate  string       `json:"status_date"`
	ChangeHash  legiscanHash `json:"change_hash"`
	Session     struct {
		SessionID   int    `json:"session_id"`
		SessionName string `json:"session_name"`
	} `json:"session"`
	Sponsors []struct {
		PeopleID      int    `json:"people_id"`
		Name          string `json:"name"`
		Party         string `json:"party"`
		Role          string `json:"role"`
		District      string `json:"district"`
		SponsorTypeID int    `json:"sponsor_type_id"`
		SponsorOrder  int    `json:"sponsor_order"`
		OpenSecrets   string `json:"opensecrets_id"`
		VoteSmart     int    `json:"votesmart_id"`
		Ballotpedia   string `json:"ballotpedia"`
	} `json:"sponsors"`
	Votes []struct {
		RollCallID int    `json:"roll_call_id"`
		Date       string `json:"date"`
		Desc       string `json:"desc"`
		Chamber    string `json:"chamber"`
	} `json:"votes"`
	History json.RawMessage `json:"history"`
}

func twoaiPoliticsBills(db *sql.DB) error {
	key := os.Getenv("LEGISCAN_API_KEY")
	if key == "" {
		return fmt.Errorf("LEGISCAN_API_KEY not set")
	}
	client := &http.Client{Timeout: 60 * time.Second}

	body, err := polGetLegiscan(client, key, "getMasterList", "&state=US")
	if err != nil {
		return err
	}
	var ml struct {
		Status     string                     `json:"status"`
		Masterlist map[string]json.RawMessage `json:"masterlist"`
	}
	if err := json.Unmarshal(body, &ml); err != nil || ml.Status != "OK" {
		return fmt.Errorf("getMasterList: bad payload (%v)", err)
	}

	held := map[int]string{}
	if r, err := db.Query(`SELECT bill_id, change_hash FROM twoai_pol_bills`); err == nil {
		for r.Next() {
			var id int
			var h string
			if r.Scan(&id, &h) == nil {
				held[id] = h
			}
		}
		r.Close()
	} else {
		return fmt.Errorf("read held bills: %w", err)
	}

	var todo []polMasterRow
	seen, ai := 0, 0
	for k, raw := range ml.Masterlist {
		if k == "session" {
			continue
		}
		var row polMasterRow
		if json.Unmarshal(raw, &row) != nil || row.BillID == 0 {
			continue
		}
		seen++
		if !polIsAI(row.Title, row.Description) {
			continue
		}
		ai++
		if h, ok := held[row.BillID]; ok && h == string(row.ChangeHash) && h != "" {
			continue
		}
		todo = append(todo, row)
	}

	fetched, failed := 0, 0
	for _, row := range todo {
		if fetched >= polBillBudget {
			break
		}
		bb, err := polGetLegiscan(client, key, "getBill", fmt.Sprintf("&id=%d", row.BillID))
		if err != nil {
			failed++
			fmt.Printf("twoai_politics_bills: getBill %d: %v\n", row.BillID, err)
			continue
		}
		var bp struct {
			Status string  `json:"status"`
			Bill   polBill `json:"bill"`
		}
		if json.Unmarshal(bb, &bp) != nil || bp.Status != "OK" || bp.Bill.BillID == 0 {
			failed++
			continue
		}
		fetched++
		b := bp.Bill
		hash := string(b.ChangeHash)
		if hash == "" {
			hash = string(row.ChangeHash)
		}
		issues := polIssueTags(b.Title, b.Description)
		preempt := polPreemptRe.MatchString(b.Title + " " + b.Description)
		sponsorsJSON, _ := json.Marshal(b.Sponsors)
		votesJSON, _ := json.Marshal(b.Votes)
		var statusDate interface{}
		if b.StatusDate != "" {
			statusDate = b.StatusDate
		}
		if _, err := db.Exec(`
			INSERT INTO twoai_pol_bills (bill_id, uid, bill_number, session_id, session_name, title, description,
				status, status_date, url, state_link, change_hash, issues, preemption, sponsors, votes, history, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::date,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			ON CONFLICT (bill_id) DO UPDATE SET title = EXCLUDED.title, description = EXCLUDED.description,
				status = EXCLUDED.status, status_date = EXCLUDED.status_date, url = EXCLUDED.url,
				state_link = EXCLUDED.state_link, change_hash = EXCLUDED.change_hash, issues = EXCLUDED.issues,
				preemption = EXCLUDED.preemption, sponsors = EXCLUDED.sponsors, votes = EXCLUDED.votes,
				history = EXCLUDED.history, raw = EXCLUDED.raw, last_seen = now()`,
			b.BillID, polUID("legiscan-bill", fmt.Sprint(b.BillID)), b.BillNumber, b.Session.SessionID,
			b.Session.SessionName, ldaClean(b.Title), ldaClean(b.Description), b.Status, statusDate, b.URL,
			b.StateLink, hash, pq.Array(issues), preempt, sponsorsJSON, votesJSON, nullJSON(b.History), ldaRaw(bb)); err != nil {
			failed++
			fmt.Printf("twoai_politics_bills: store bill %d: %v\n", b.BillID, err)
			continue
		}
		for _, s := range b.Sponsors {
			if s.PeopleID == 0 {
				continue
			}
			_, _ = db.Exec(`
				INSERT INTO twoai_pol_members (people_id, uid, name, party, role, district, opensecrets_id, votesmart_id, ballotpedia)
				VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,0),NULLIF($9,''))
				ON CONFLICT (people_id) DO UPDATE SET name = EXCLUDED.name, party = EXCLUDED.party, role = EXCLUDED.role,
					district = EXCLUDED.district, opensecrets_id = COALESCE(EXCLUDED.opensecrets_id, twoai_pol_members.opensecrets_id),
					votesmart_id = COALESCE(EXCLUDED.votesmart_id, twoai_pol_members.votesmart_id),
					ballotpedia = COALESCE(EXCLUDED.ballotpedia, twoai_pol_members.ballotpedia), last_seen = now()`,
				s.PeopleID, polUID("legiscan-person", fmt.Sprint(s.PeopleID)), ldaClean(s.Name), s.Party, s.Role,
				s.District, s.OpenSecrets, s.VoteSmart, s.Ballotpedia)
			_, _ = db.Exec(`
				INSERT INTO twoai_pol_sponsorships (bill_id, people_id, sponsor_type_id, sponsor_order)
				VALUES ($1,$2,$3,$4)
				ON CONFLICT (bill_id, people_id) DO UPDATE SET sponsor_type_id = EXCLUDED.sponsor_type_id,
					sponsor_order = EXCLUDED.sponsor_order, last_seen = now()`,
				b.BillID, s.PeopleID, s.SponsorTypeID, s.SponsorOrder)
		}
		time.Sleep(400 * time.Millisecond)
	}

	// Roll calls not yet held, on any bill held. A vote never changes once
	// recorded, so each is fetched exactly once.
	votesFetched := 0
	rc, err := db.Query(`
		SELECT DISTINCT (v->>'roll_call_id')::int, b.bill_id
		FROM twoai_pol_bills b, jsonb_array_elements(b.votes) v
		WHERE (v->>'roll_call_id') IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM twoai_pol_rollcalls r WHERE r.roll_call_id = (v->>'roll_call_id')::int)
		LIMIT $1`, polVoteBudget)
	if err != nil {
		return fmt.Errorf("roll call queue: %w", err)
	}
	type pair struct{ rc, bill int }
	var queue []pair
	for rc.Next() {
		var p pair
		if rc.Scan(&p.rc, &p.bill) == nil {
			queue = append(queue, p)
		}
	}
	rc.Close()
	for _, p := range queue {
		vb, err := polGetLegiscan(client, key, "getRollCall", fmt.Sprintf("&id=%d", p.rc))
		if err != nil {
			failed++
			continue
		}
		var vp struct {
			Status   string `json:"status"`
			RollCall struct {
				RollCallID int    `json:"roll_call_id"`
				Date       string `json:"date"`
				Desc       string `json:"desc"`
				Yea        int    `json:"yea"`
				Nay        int    `json:"nay"`
				NV         int    `json:"nv"`
				Absent     int    `json:"absent"`
				Passed     int    `json:"passed"`
				Chamber    string `json:"chamber"`
				Votes      []struct {
					PeopleID int    `json:"people_id"`
					VoteText string `json:"vote_text"`
				} `json:"votes"`
			} `json:"roll_call"`
		}
		if json.Unmarshal(vb, &vp) != nil || vp.Status != "OK" || vp.RollCall.RollCallID == 0 {
			failed++
			continue
		}
		r := vp.RollCall
		var d interface{}
		if r.Date != "" {
			d = r.Date
		}
		if _, err := db.Exec(`
			INSERT INTO twoai_pol_rollcalls (roll_call_id, uid, bill_id, vote_date, description, chamber, yea, nay, nv, absent, passed, raw)
			VALUES ($1,$2,$3,$4::date,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (roll_call_id) DO NOTHING`,
			r.RollCallID, polUID("legiscan-rollcall", fmt.Sprint(r.RollCallID)), p.bill, d, ldaClean(r.Desc),
			r.Chamber, r.Yea, r.Nay, r.NV, r.Absent, r.Passed == 1, ldaRaw(vb)); err != nil {
			failed++
			fmt.Printf("twoai_politics_bills: store roll call %d: %v\n", r.RollCallID, err)
			continue
		}
		for _, v := range r.Votes {
			_, _ = db.Exec(`INSERT INTO twoai_pol_votes (roll_call_id, people_id, vote_text) VALUES ($1,$2,$3)
				ON CONFLICT (roll_call_id, people_id) DO NOTHING`, r.RollCallID, v.PeopleID, v.VoteText)
		}
		votesFetched++
		time.Sleep(400 * time.Millisecond)
	}

	// Every member who cast a vote needs a name and party beside that vote,
	// and getRollCall gives only people_id. Unknown voters are looked up once
	// with getPerson, capped per run; the 535 members of Congress are all
	// known within a few runs and after that this is zero calls a day.
	peopleFetched := 0
	if pr, err := db.Query(`SELECT DISTINCT v.people_id FROM twoai_pol_votes v
		WHERE NOT EXISTS (SELECT 1 FROM twoai_pol_members m WHERE m.people_id = v.people_id) LIMIT 150`); err == nil {
		var ids []int
		for pr.Next() {
			var id int
			if pr.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		pr.Close()
		for _, id := range ids {
			pb, err := polGetLegiscan(client, key, "getPerson", fmt.Sprintf("&id=%d", id))
			if err != nil {
				failed++
				continue
			}
			var pp struct {
				Status string `json:"status"`
				Person struct {
					PeopleID    int    `json:"people_id"`
					Name        string `json:"name"`
					Party       string `json:"party"`
					Role        string `json:"role"`
					District    string `json:"district"`
					OpenSecrets string `json:"opensecrets_id"`
					VoteSmart   int    `json:"votesmart_id"`
					Ballotpedia string `json:"ballotpedia"`
				} `json:"person"`
			}
			if json.Unmarshal(pb, &pp) != nil || pp.Status != "OK" || pp.Person.PeopleID == 0 {
				failed++
				continue
			}
			p := pp.Person
			if _, err := db.Exec(`
				INSERT INTO twoai_pol_members (people_id, uid, name, party, role, district, opensecrets_id, votesmart_id, ballotpedia)
				VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,0),NULLIF($9,''))
				ON CONFLICT (people_id) DO NOTHING`,
				p.PeopleID, polUID("legiscan-person", fmt.Sprint(p.PeopleID)), ldaClean(p.Name), p.Party, p.Role,
				p.District, p.OpenSecrets, p.VoteSmart, p.Ballotpedia); err == nil {
				peopleFetched++
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	var bills, members, rolls int
	_ = db.QueryRow(`SELECT (SELECT count(*) FROM twoai_pol_bills), (SELECT count(*) FROM twoai_pol_members),
		(SELECT count(*) FROM twoai_pol_rollcalls)`).Scan(&bills, &members, &rolls)
	fmt.Printf("twoai_politics_bills: session_bills=%d ai=%d changed=%d fetched=%d roll_calls_fetched=%d people_fetched=%d failed=%d | held bills=%d members=%d roll_calls=%d ok=true\n",
		seen, ai, len(todo), fetched, votesFetched, peopleFetched, failed, bills, members, rolls)
	if len(todo) > fetched {
		fmt.Printf("twoai_politics_bills: %d changed bills left for the next run (budget %d)\n", len(todo)-fetched, polBillBudget)
	}
	return nil
}

func nullJSON(b json.RawMessage) interface{} {
	if len(b) == 0 || string(b) == "null" {
		return []byte("[]")
	}
	return ldaRaw(b)
}

var _ = strings.TrimSpace
