package main

// twoai_backupwatch: Stephen is told the day a database backup stops working.
//
// 2026-09-28: the 4-hourly dump failed on every run from 2026-09-25 01:45 for
// three days, at a globals step with a PowerShell argument bug, and nothing
// reached the NAS. Each failure was written to logs\nasdump-last.txt, and
// nothing read it. The same night the dump and WAL shipping tasks were left
// disabled after a NAS file system check. Both were found only because
// Stephen looked at an empty Hybrid Backup Sync report.
//
// Runs in the five-minute inkbox tick, like the build watch, and reads the
// three things that say whether the database is protected:
//
//   nasdump      logs\nasdump-last.txt    FAILED, or no run for 5 hours
//   basebackup   logs\basebackup-last.txt FAILED, or no run for 26 hours
//   wal-ship     wal-ship.log last line   a failure, or nothing for 45 minutes
//
// One email when a check turns bad, one when it recovers, never a repeat in
// between. State lives in twoai_backupwatch.

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

type bwCheck struct {
	key, file string
	maxAge    time.Duration
	lastLine  bool // read the file's last line rather than its whole content
}

var backupChecks = []bwCheck{
	{"nasdump", `C:\srj-data\logs\nasdump-last.txt`, 5 * time.Hour, false},
	{"basebackup", `C:\srj-data\logs\basebackup-last.txt`, 26 * time.Hour, false},
	{"wal-ship", `C:\srj-data\wal-ship.log`, 45 * time.Minute, true},
}

func bkReadStatus(c bwCheck) (string, error) {
	f, err := os.Open(c.file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" && len(t) >= 19 && t[4] == '-' && t[13] == ':' {
			last = t
			if !c.lastLine {
				break
			}
		}
	}
	return last, sc.Err()
}

func twoaiBackupWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_backupwatch (
		key text PRIMARY KEY, state text NOT NULL, detail text, since timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	now := time.Now()
	for _, c := range backupChecks {
		line, err := bkReadStatus(c)
		state, detail := "ok", line
		switch {
		case err != nil:
			state, detail = "bad", "status file unreadable: "+err.Error()
		case line == "":
			state, detail = "bad", "status file has no dated line"
		default:
			t, perr := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
			low := strings.ToLower(line)
			switch {
			case perr != nil:
				state, detail = "bad", "cannot read the time on: "+line
			case strings.Contains(low, "fail") || strings.Contains(line, "exit=1"):
				state = "bad"
			case now.Sub(t) > c.maxAge:
				state, detail = "bad", fmt.Sprintf("no run for %s (limit %s). Last: %s", now.Sub(t).Round(time.Minute), c.maxAge, line)
			}
		}
		var prev string
		db.QueryRow(`SELECT state FROM twoai_backupwatch WHERE key=$1`, c.key).Scan(&prev)
		if prev == state {
			continue
		}
		db.Exec(`INSERT INTO twoai_backupwatch (key, state, detail, since) VALUES ($1,$2,$3,now())
			ON CONFLICT (key) DO UPDATE SET state=EXCLUDED.state, detail=EXCLUDED.detail, since=now()`, c.key, state, detail)
		if prev == "" && state == "ok" {
			continue // first sight of a healthy check: nothing to say
		}
		var subj, body string
		if state == "bad" {
			subj = "Database backup problem: " + c.key
			body = "The " + c.key + " check on the office PC failed.\n\n" + detail +
				"\n\nStatus file: " + c.file + "\n\nNothing is lost yet, but the database is not being protected the way it should be until this is fixed. You will get one more email when it recovers."
		} else {
			subj = "Database backup recovered: " + c.key
			body = "The " + c.key + " check is healthy again.\n\n" + detail
		}
		bwAlert(db, "backupwatch", subj, body)
		// A recovery closes the problem it answers. theworldofai, bridge row
		// 352 (2026-10-02): the problem rows for wal-ship and nasdump sat open
		// for two days after both jobs were healthy, until a person read the
		// job logs and acked them by hand. Only this job's rows, only ones
		// raised before this recovery, and only in the mailbox the alert went
		// to. A job that has not recovered keeps its row open.
		if state == "ok" {
			res, err := db.Exec(`UPDATE project_bridge SET status='ack', acked_at=now()
				WHERE to_project='theworldofai' AND status='open'
				AND from_project IN ('inkbox','backupwatch')
				AND topic=$1 AND created_at < now()`, "Database backup problem: "+c.key)
			if err != nil {
				fmt.Fprintf(os.Stderr, "backupwatch: ack problem rows for %s: %v\n", c.key, err)
			} else if n, _ := res.RowsAffected(); n > 0 {
				fmt.Printf("backupwatch: %s recovered, acked %d open problem row(s)\n", c.key, n)
			}
		}
	}
	return nil
}
