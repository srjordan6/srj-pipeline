package main

// twoai_dart: filings from Korea's DART, so the other two HBM makers are
// tracked from primary sources rather than press coverage.
//
// Stephen, 2026-09-20: make certain we are tracking the producers of VRAM and
// their industry news. Three companies supply essentially all HBM, the memory
// on an AI accelerator: SK hynix, Samsung and Micron. Micron files with the
// SEC and twoai_companyfacts already reads EDGAR for it. The other two file
// with Korea's Financial Supervisory Service, which EDGAR does not cover, so
// until now two thirds of the world's HBM supply reached this site only
// through other people's reporting.
//
// NOT VERIFIED AGAINST THE LIVE API. Every attempt to reach opendart.fss.or.kr
// from the machine this was written on returned 503, while dart.fss.or.kr and
// englishdart.fss.or.kr on the adjacent address both answered 200. That reads
// as an egress block rather than an outage, but it means the request shapes
// below come from the published contract and not from a response anyone has
// seen. The first run is therefore treated as the test: every status code DART
// documents is named in the log in plain words, so a wrong key or a changed
// contract says so on line one instead of looking like an empty day.
//
// TWO CALLS A DAY, NOT TWO THOUSAND. corpCode.xml is the full register of
// every filer in Korea, about 100,000 rows in a zip. It is fetched once and
// cached, refreshed weekly, because a company's code does not change. The
// daily work is one list.json call per watched company.
//
// CODES ARE LOOKED UP, NEVER TYPED. It is tempting to paste SK hynix's
// corp_code into the watchlist and move on. A wrong code does not error: it
// returns somebody else's filings, and this site would publish them under the
// wrong company. The watchlist holds NAMES, and the register resolves them,
// which is the same reason twoai_thinmissing refuses a company name that is
// only a form of incorporation.

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const dartBase = "https://opendart.fss.or.kr/api/"

// twoaiDartStatus turns a DART status code into something a person reading the
// log can act on. The codes are the documented set; anything else is reported
// verbatim rather than swallowed.
func twoaiDartStatus(code, msg string) (ok bool, human string) {
	switch code {
	case "000":
		return true, ""
	case "010":
		return false, "the key is not registered. Check DART_API_KEY in pipeline.env"
	case "011":
		return false, "the key is suspended"
	case "012":
		return false, "access denied for this IP. DART can bind a key to an address"
	case "013":
		// Not an error. A company that filed nothing in the window is the
		// normal case on most days.
		return false, ""
	case "014":
		return false, "the file does not exist"
	case "020":
		return false, "daily call limit reached, 20,000 by default"
	case "021":
		return false, "per-minute call limit reached"
	case "100":
		return false, "a parameter was rejected: " + msg
	case "101":
		return false, "unauthorised key"
	case "800":
		return false, "DART is in scheduled maintenance"
	case "900":
		return false, "DART reported an unknown error"
	case "901":
		return false, "the key has expired and needs renewing"
	}
	return false, "undocumented status " + code + ": " + msg
}

func twoaiDartEnsure(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_dart_corps (
		corp_code text PRIMARY KEY,
		corp_name text NOT NULL,
		stock_code text,
		modify_date text,
		refreshed_on date NOT NULL DEFAULT current_date)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_dart_watch (
		name text PRIMARY KEY,          -- the name as DART spells it
		note text,                      -- why this company is watched
		corp_code text,                 -- resolved from the register, never typed
		entity_uid text,                -- our company page, where one exists
		active boolean NOT NULL DEFAULT true,
		added_on date NOT NULL DEFAULT current_date)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_dart_filings (
		rcept_no text PRIMARY KEY,      -- DART's receipt number, and the URL key
		corp_code text NOT NULL,
		corp_name text NOT NULL,
		report_nm text NOT NULL,        -- Korean, as filed
		report_en text,                 -- our label for the common types
		flr_nm text,                    -- who filed it
		rcept_dt date,
		rm text,
		url text,
		seen_on date NOT NULL DEFAULT current_date)`); err != nil {
		return err
	}
	// The two HBM suppliers EDGAR does not cover. Names as DART spells them;
	// the codes are resolved on the first run.
	_, err := db.Exec(`INSERT INTO twoai_dart_watch (name, note) VALUES
		('에스케이하이닉스', 'SK hynix. HBM supplier; not an SEC filer'),
		('삼성전자', 'Samsung Electronics. HBM supplier; not an SEC filer')
		ON CONFLICT (name) DO NOTHING`)
	return err
}

// twoaiDartReportEN labels the filing types worth recognising. Everything else
// keeps its Korean name and is stored as filed, because a wrong English label
// is worse than an honest Korean one.
var twoaiDartReportEN = map[string]string{
	"사업보고서":       "Annual report",
	"반기보고서":       "Half-year report",
	"분기보고서":       "Quarterly report",
	"주요사항보고서":     "Material event report",
	"현금ㆍ현물배당결정":   "Dividend decision",
	"자기주식취득결정":    "Share buyback decision",
	"유상증자결정":      "Paid-in capital increase",
	"단일판매ㆍ공급계약체결": "Single sale or supply contract",
	"영업실적등에대한전망":  "Earnings outlook",
	"매출액또는손익구조":   "Revenue or profit structure change",
	"타법인주식및출자증권":  "Investment in another company",
	"회사합병결정":      "Merger decision",
	"신규시설투자등":     "New facility investment",
}

func twoaiDartLabel(reportNM string) string {
	n := strings.TrimSpace(reportNM)
	for k, v := range twoaiDartReportEN {
		if strings.HasPrefix(n, k) || strings.Contains(n, k) {
			return v
		}
	}
	return ""
}

// twoaiDartRefreshCorps downloads the register of filers. About 100,000 rows
// in a zip; run weekly, not daily.
func twoaiDartRefreshCorps(db *sql.DB, key string, client *http.Client) error {
	var age sql.NullInt64
	db.QueryRow(`SELECT current_date - max(refreshed_on) FROM twoai_dart_corps`).Scan(&age)
	if age.Valid && age.Int64 < 7 {
		return nil
	}
	resp, err := client.Get(dartBase + "corpCode.xml?crtfc_key=" + url.QueryEscape(key))
	if err != nil {
		return fmt.Errorf("corpCode: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	// An error comes back as XML, not as a zip. Read it rather than failing on
	// a corrupt archive and leaving the reason unsaid.
	if !bytes.HasPrefix(body, []byte("PK")) {
		var e struct {
			Status  string `xml:"status"`
			Message string `xml:"message"`
		}
		xml.Unmarshal(body, &e)
		_, human := twoaiDartStatus(e.Status, e.Message)
		if human == "" {
			human = "not a zip and not a readable error"
		}
		return fmt.Errorf("corpCode: %s", human)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("corpCode zip: %w", err)
	}
	n := 0
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToUpper(f.Name), ".XML") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 256<<20))
		rc.Close()
		var doc struct {
			List []struct {
				CorpCode   string `xml:"corp_code"`
				CorpName   string `xml:"corp_name"`
				StockCode  string `xml:"stock_code"`
				ModifyDate string `xml:"modify_date"`
			} `xml:"list"`
		}
		if xml.Unmarshal(raw, &doc) != nil {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		st, err := tx.Prepare(`INSERT INTO twoai_dart_corps (corp_code, corp_name, stock_code, modify_date, refreshed_on)
			VALUES ($1,$2,$3,$4,current_date)
			ON CONFLICT (corp_code) DO UPDATE SET corp_name=$2, stock_code=$3, modify_date=$4, refreshed_on=current_date`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, r := range doc.List {
			code := strings.TrimSpace(r.CorpCode)
			if code == "" {
				continue
			}
			st.Exec(code, strings.ToValidUTF8(strings.TrimSpace(r.CorpName), ""),
				strings.TrimSpace(r.StockCode), strings.TrimSpace(r.ModifyDate))
			n++
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	fmt.Printf("twoai_dart: register refreshed, %d filers\n", n)
	return nil
}

// twoaiDartResolve fills in a corp_code for any watched name that lacks one.
// A name matching more than one listed filer is left unresolved and reported,
// because picking one would be a guess.
func twoaiDartResolve(db *sql.DB) {
	rows, err := db.Query(`SELECT name FROM twoai_dart_watch WHERE active AND COALESCE(corp_code,'') = ''`)
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		// A listed company has a stock code. That alone removes most of the
		// duplicate-name problem, because the register is full of unlisted
		// subsidiaries sharing a parent's name.
		var codes []string
		r2, err := db.Query(`SELECT corp_code FROM twoai_dart_corps
			WHERE corp_name = $1 AND COALESCE(stock_code,'') <> '' LIMIT 5`, n)
		if err != nil {
			continue
		}
		for r2.Next() {
			var c string
			if r2.Scan(&c) == nil {
				codes = append(codes, c)
			}
		}
		r2.Close()
		switch len(codes) {
		case 1:
			db.Exec(`UPDATE twoai_dart_watch SET corp_code=$2 WHERE name=$1`, n, codes[0])
			fmt.Printf("twoai_dart: resolved %s to corp_code %s\n", n, codes[0])
		case 0:
			fmt.Fprintf(os.Stderr, "twoai_dart: %s is not a listed filer in the register, left unresolved\n", n)
		default:
			fmt.Fprintf(os.Stderr, "twoai_dart: %s matches %d listed filers, left unresolved rather than guessed\n", n, len(codes))
		}
	}
}

type dartList struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	List    []struct {
		CorpCode string `json:"corp_code"`
		CorpName string `json:"corp_name"`
		ReportNM string `json:"report_nm"`
		RceptNo  string `json:"rcept_no"`
		FlrNM    string `json:"flr_nm"`
		RceptDt  string `json:"rcept_dt"`
		RM       string `json:"rm"`
	} `json:"list"`
}

// twoaiDart is the stage. One list.json call per watched company, over a
// window wide enough that a missed run costs nothing.
func twoaiDart(db *sql.DB) error {
	key := strings.TrimSpace(os.Getenv("DART_API_KEY"))
	if key == "" {
		fmt.Println("twoai_dart: DART_API_KEY not set, skipping")
		return nil
	}
	if err := twoaiDartEnsure(db); err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	if err := twoaiDartRefreshCorps(db, key, client); err != nil {
		// A stale register is survivable: codes already resolved keep working.
		fmt.Fprintln(os.Stderr, "twoai_dart:", err)
	}
	twoaiDartResolve(db)

	rows, err := db.Query(`SELECT name, corp_code FROM twoai_dart_watch
		WHERE active AND COALESCE(corp_code,'') <> '' ORDER BY name`)
	if err != nil {
		return err
	}
	type w struct{ name, code string }
	var watch []w
	for rows.Next() {
		var x w
		if rows.Scan(&x.name, &x.code) == nil {
			watch = append(watch, x)
		}
	}
	rows.Close()
	if len(watch) == 0 {
		fmt.Println("twoai_dart: nothing resolved to poll yet")
		return nil
	}

	bgn := time.Now().AddDate(0, 0, -30).Format("20060102")
	end := time.Now().Format("20060102")
	newRows, seen := 0, 0
	for _, c := range watch {
		q := url.Values{}
		q.Set("crtfc_key", key)
		q.Set("corp_code", c.code)
		q.Set("bgn_de", bgn)
		q.Set("end_de", end)
		q.Set("page_count", "100")
		resp, err := client.Get(dartBase + "list.json?" + q.Encode())
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_dart: %s: %v\n", c.name, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		var d dartList
		if err := json.Unmarshal(body, &d); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_dart: %s: reply was not JSON: %.120s\n", c.name, body)
			continue
		}
		ok, human := twoaiDartStatus(d.Status, d.Message)
		if !ok {
			if human != "" {
				fmt.Fprintf(os.Stderr, "twoai_dart: %s: %s\n", c.name, human)
			}
			// A rate limit or a dead key applies to every remaining company,
			// so stop rather than repeat the same failure down the list.
			if d.Status == "020" || d.Status == "021" || d.Status == "010" ||
				d.Status == "011" || d.Status == "101" || d.Status == "901" || d.Status == "800" {
				break
			}
			continue
		}
		for _, f := range d.List {
			seen++
			rc := strings.TrimSpace(f.RceptNo)
			if rc == "" {
				continue
			}
			var dt any
			if len(f.RceptDt) == 8 {
				dt = f.RceptDt[0:4] + "-" + f.RceptDt[4:6] + "-" + f.RceptDt[6:8]
			}
			res, err := db.Exec(`INSERT INTO twoai_dart_filings
				(rcept_no, corp_code, corp_name, report_nm, report_en, flr_nm, rcept_dt, rm, url)
				VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,NULLIF($8,''),$9)
				ON CONFLICT (rcept_no) DO NOTHING`,
				rc, strings.TrimSpace(f.CorpCode), strings.ToValidUTF8(f.CorpName, ""),
				strings.ToValidUTF8(f.ReportNM, ""), twoaiDartLabel(f.ReportNM),
				strings.ToValidUTF8(f.FlrNM, ""), dt, strings.TrimSpace(f.RM),
				"https://dart.fss.or.kr/dsaf001/main.do?rcpNo="+rc)
			if err != nil {
				fmt.Fprintln(os.Stderr, "twoai_dart store:", err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				newRows++
			}
		}
		time.Sleep(600 * time.Millisecond)
	}
	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_dart_filings`).Scan(&total)
	fmt.Printf("twoai_dart: watched=%d seen=%d new=%d total=%d\n", len(watch), seen, newRows, total)
	return nil
}
