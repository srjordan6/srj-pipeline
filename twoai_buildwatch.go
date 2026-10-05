package main

// twoai_buildwatch: Stephen is told when a build did not ship.
//
// On 2026-09-06 seven consecutive Cloudflare builds of theworldofai.org failed
// between 14:32 and 21:02. Two commits that mattered - the thin-page work and
// the date/time/uid stamp - were pushed, reported green by the workflow that
// only fires the hook, and never went live. The only place the failures were
// visible was the Cloudflare dashboard, which nobody was looking at. Stephen's
// instruction: "I need a system where I am notified when a build fails."
//
// HOW IT KNOWS, WITHOUT A CLOUDFLARE TOKEN. This PC holds a deploy hook (which
// can start a build) and a GitHub token (which can read the repo), and no
// Cloudflare API credential. So the watch does not ask Cloudflare anything. It
// asks two questions it can answer itself:
//
//   1. What is the newest commit on origin/main of twoai-site, and when was it
//      pushed?  (GitHub API, with the token publish.cmd already obtains.)
//   2. What commit is the live site built from?  (/api/build.json, which the
//      build writes from WORKERS_CI_COMMIT_SHA since 2026-09-06.)
//
// If the newest commit is older than the grace period and the live site is
// not built from it, the build did not ship - whoever pushed it, whatever
// broke, and whether it failed in Astro, in wrangler, or never started. That
// is the property Stephen cares about. A separate rule fires when the live
// build itself is more than a day old, which catches a pipeline that has
// stopped triggering builds at all.
//
// It runs inside the inkbox tick, every five minutes, and alerts ONCE per
// unshipped commit through the inkbox_outbox queue the same tick then sends.
// When the commit finally ships it says so, once, so an open alert is closed
// by the system rather than by Stephen checking. State lives in one small
// table this role owns.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	bwSite  = "https://theworldofai.org"
	bwRepo  = "srjordan6/twoai-site"
	bwGrace = 20 * time.Minute // a healthy build plus deploy takes 6 to 8 minutes
	// 2026-10-01: four builds failed in a row from a URL guard block and the
	// site went fifteen hours without shipping, inside the old thirty hour
	// window, so nobody was told. A publish runs every three hours and a
	// healthy build lands within the hour, so five hours without a build is a
	// failure, reported at once and again every six hours until it clears.
	bwStaleLive = 5 * time.Hour
	bwFrom      = "theworldofai" // the Inkbox identity the alert is sent as
)

// bwAlertTo is where the alert lands. BUILD_ALERT_TO overrides it. The default
// is the one inbox on this system a human reads rather than a machine mirrors.
func bwAlertTo() []string {
	if v := strings.TrimSpace(os.Getenv("BUILD_ALERT_TO")); v != "" {
		return strings.Split(v, ",")
	}
	return []string{"srj@srjconsultingservices.com"}
}

// bwAlert is how a watch raises its voice: the email goes into the outbox for
// the same tick to send, and the same text goes straight into the theworldofai
// bridge mailbox, written here and not by mirroring the mail back later.
//
// 2026-10-02: fourteen alerts in a row, every build and backup notice since
// 2026-09-30 16:02 UTC, had failed to send. Inkbox refuses a conversation once
// twenty messages have gone unanswered (403 conversation_unhealthy), and the
// outbox parks a row after two tries. Nothing in the bridge said so, because
// the bridge row only ever came from inkbox_pull mirroring the sent copy, and
// an unsent mail has no copy. The project reads the bridge, Stephen reads the
// mail, and the two must not share a single point of failure. from_project
// names the watch so the project can tell a direct row from mirrored mail, and
// inkbox_pull skips our own alert mail so a late delivery cannot add a second
// row for the same event.
func bwAlert(db *sql.DB, watch, subj, text string) {
	db.Exec(`INSERT INTO inkbox_outbox (from_handle, channel, to_addrs, subject, body)
		VALUES ($1,'email',$2,$3,$4)`, bwFrom, pq.Array(bwAlertTo()), subj, text)
	if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body)
		VALUES ($1,'theworldofai',$2,$3)`, watch, subj, text); err != nil {
		fmt.Fprintf(os.Stderr, "%s: bridge row for %q: %v\n", watch, subj, err)
	}
	fmt.Println(watch+": queued:", subj)
}

func twoaiBuildWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_buildwatch (
		key text PRIMARY KEY, value text NOT NULL, at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	get := func(k string) string {
		var v string
		db.QueryRow(`SELECT value FROM twoai_buildwatch WHERE key=$1`, k).Scan(&v)
		return v
	}
	set := func(k, v string) {
		db.Exec(`INSERT INTO twoai_buildwatch (key, value, at) VALUES ($1,$2,now())
			ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, at=now()`, k, v)
	}

	// The live site.
	live := struct {
		Commit  string `json:"commit"`
		BuiltAt string `json:"built_at"`
		Bundle  string `json:"bundle"`
		// Written by scripts/url-guard.mjs since 2026-09-18. A pointer, so a
		// build from before that date reads as "says nothing", not "unguarded".
		URLGuard *struct {
			Guarded bool   `json:"guarded"`
			Reason  string `json:"reason"`
		} `json:"url_guard"`
	}{}
	body, err := twoaiJobsGet(bwSite+"/api/build.json", map[string]string{"Cache-Control": "no-cache"})
	if err != nil {
		return fmt.Errorf("live build.json: %w", err)
	}
	if err := json.Unmarshal(body, &live); err != nil {
		return fmt.Errorf("live build.json parse: %w", err)
	}
	builtAt, _ := time.Parse(time.RFC3339, live.BuiltAt)

	// The newest commit on main.
	hdr := map[string]string{"Accept": "application/vnd.github+json"}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		hdr["Authorization"] = "Bearer " + tok
	}
	body, err = twoaiJobsGet("https://api.github.com/repos/"+bwRepo+"/commits/main", hdr)
	if err != nil {
		return fmt.Errorf("origin/main: %w", err)
	}
	head := struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}{}
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("origin/main parse: %w", err)
	}
	pushedAt, _ := time.Parse(time.RFC3339, head.Commit.Committer.Date)
	subject := head.Commit.Message
	if i := strings.IndexByte(subject, '\n'); i > 0 {
		subject = subject[:i]
	}
	short := head.SHA
	if len(short) > 7 {
		short = short[:7]
	}
	now := time.Now().UTC()

	queue := func(subj, text string) { bwAlert(db, "buildwatch", subj, text) }

	// Rule 1: a commit on main that has not shipped.
	// live.Commit must LOOK like a commit before it is treated as evidence.
	// Build cb5075c7 published commit "main" from WORKERS_CI_COMMIT_SHA, which
	// is a branch name; comparing that to a SHA would have alerted on every
	// tick forever. Anything that is not 40 hex characters means "cannot tell",
	// and rule 2 still covers a site that has stopped building altogether.
	liveSHA := live.Commit
	if !bwIsSHA(liveSHA) {
		liveSHA = ""
	}
	shipped := liveSHA != "" && liveSHA == head.SHA

	// A COMMIT THAT CANNOT CHANGE THE SITE IS NOT A FAILED BUILD.
	//
	// First live catch, 2026-09-07: twoai-site 0079813 added four lines to
	// .github/workflows/security-scan.yml. Cloudflare ran no build, correctly,
	// because nothing in the published output could differ. The watch saw a
	// head that was not live, waited its twenty minutes and emailed. True in
	// letter, false in substance, and worse than useless: no build is coming,
	// so that alert would have stayed open until some unrelated commit shipped.
	// With Dependabot now open on both repositories, every merge touching only
	// .github or a lockfile would do the same, which is exactly the crying-wolf
	// failure 22a9adf was written to prevent, arriving by another door.
	//
	// So before alerting, ask GitHub what actually changed between the live
	// commit and head. If every changed path is CI configuration, editor
	// settings or documentation, the site is already current and there is
	// nothing to report. If the comparison cannot be made, alert anyway:
	// silence is the worse failure, and this is a safety net, not a filter.
	if !shipped && liveSHA != "" {
		if only, err := bwOnlyNonBuildPaths(liveSHA, head.SHA, hdr); err == nil && only {
			set("alerted_sha", head.SHA) // treat as handled so it never reopens
			fmt.Printf("buildwatch: head=%s changes nothing the build publishes, no alert\n", short)
			shipped = true
		}
	}
	// Until the first build after 2026-09-06 lands, build.json has no commit.
	// A missing commit is "cannot tell", not "failed"; only a present, different
	// commit is evidence. Rule 2 still covers a site that has stopped building.
	if !shipped && liveSHA != "" && now.Sub(pushedAt) > bwGrace && get("alerted_sha") != head.SHA {
		set("alerted_sha", head.SHA)
		queue(fmt.Sprintf("theworldofai.org build did not ship: %s", short),
			fmt.Sprintf(`The newest commit on twoai-site main has not reached the live site.

Commit:   %s
Subject:  %s
Pushed:   %s UTC (%d minutes ago)
Live:     built %s UTC from commit %s

A healthy build and deploy takes six to eight minutes. This one has had %d.
The Cloudflare build for the site worker either failed or never started.

Read the log: https://dash.cloudflare.com/ -> Workers & Pages -> twoai-site -> Deployments.
Or from a session: Cloudflare:workers_builds_list_builds workerId=469a1508424e4e748d5ed287ce7e5502,
then workers_builds_get_build_logs on the newest UUID and read the tail.

This message is sent once per unshipped commit by the buildwatch stage in
srj-pipeline, every five minutes from the inkbox tick. You will get one more
when it ships.`,
				head.SHA, subject, pushedAt.Format("2006-01-02 15:04"), int(now.Sub(pushedAt).Minutes()),
				builtAt.Format("2006-01-02 15:04"), firstN(liveSHA, 7), int(now.Sub(pushedAt).Minutes())))
	}

	// Recovery: the commit we alerted on is now live.
	if shipped && get("alerted_sha") == head.SHA && get("recovered_sha") != head.SHA {
		set("recovered_sha", head.SHA)
		queue(fmt.Sprintf("theworldofai.org shipped: %s", short),
			fmt.Sprintf("Commit %s (%s) is live, built %s UTC. The earlier alert is closed.",
				short, subject, builtAt.Format("2006-01-02 15:04")))
	}

	// Rule 2: nothing has shipped for five hours, whatever git says. Repeats
	// every six hours while it lasts, and closes itself when a build lands.
	if !builtAt.IsZero() && now.Sub(builtAt) > bwStaleAfter(now) {
		lastAt, _ := time.Parse(time.RFC3339, get("stale_alert_at"))
		if lastAt.IsZero() || now.Sub(lastAt) > 6*time.Hour {
			set("stale_alert_at", now.Format(time.RFC3339))
			set("stale_open", "1")
			reason := bwLastBuildFailure()
			queue(fmt.Sprintf("theworldofai.org has not rebuilt in %.0f hours", now.Sub(builtAt).Hours()),
				fmt.Sprintf(`The live site was built %s UTC, %.0f hours ago. A publish runs every three
hours and a healthy build lands within the hour, so the build or the deploy has
been failing since then. Bundle on the site: %s.

%s

Read the log: https://dash.cloudflare.com/ -> Workers & Pages -> twoai-site -> Deployments.
You will get one more email when a build lands.`,
					builtAt.Format("2006-01-02 15:04"), now.Sub(builtAt).Hours(), live.Bundle, reason))
		}
	} else if get("stale_open") == "1" {
		set("stale_open", "")
		queue("theworldofai.org is building again",
			fmt.Sprintf("A build landed at %s UTC, bundle %s. The earlier alert is closed.",
				builtAt.Format("2006-01-02 15:04"), live.Bundle))
	}

	// Rule 3: the live build shipped without its URL guard. Found 2026-09-18:
	// Bot Fight Mode answered the guard's fetch with a 403 challenge, the guard
	// read that as an empty sitemap and passed open, and it had done so on every
	// build since the mode went on, with one line in a 15,000 line log as the
	// only trace. Published URLs never disappear, and the guard is what stands
	// between a build that drops some and production, so an unguarded deploy is
	// reported the way a failed one is. Once per build.
	if live.URLGuard != nil && !live.URLGuard.Guarded && live.BuiltAt != "" && get("unguarded_build") != live.BuiltAt {
		set("unguarded_build", live.BuiltAt)
		queue("theworldofai.org shipped without its URL guard",
			fmt.Sprintf(`The live build, %s UTC from commit %s, was deployed without being
checked for dropped URLs. The guard could not read the live site and passed open.

What it said: %s

Until this is fixed a build that loses published pages will deploy. The guard is
scripts/url-guard.mjs in twoai-site; it reads the live sitemap and
/unlisted-urls.json from theworldofai.org, then from the workers.dev hostname.
If both refuse, look at Cloudflare Security, Events for the build runner.`,
				builtAt.Format("2006-01-02 15:04"), firstN(liveSHA, 7), live.URLGuard.Reason))
	}

	fmt.Printf("buildwatch: head=%s pushed=%s live=%s built=%s shipped=%v\n",
		short, pushedAt.Format("15:04"), firstN(liveSHA, 7), builtAt.Format("01-02 15:04"), shipped)
	return nil
}

// bwIsSHA reports whether s is a full 40-character hex commit hash.
func bwIsSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// bwOnlyNonBuildPaths reports whether every file changed between two commits is
// one the published site cannot depend on. Compares via the GitHub API, which
// returns the file list for a range directly.
//
// The list is deliberately short and deliberately conservative. .github is CI
// and never reaches the bundle. Editor and lint configuration, licences and
// loose markdown at the repository root are the same. EVERYTHING ELSE COUNTS,
// including package.json and lockfiles, because a dependency bump does change
// what is published even when the source does not. When in doubt the answer is
// "this could have changed the site", so the alert fires.
func bwOnlyNonBuildPaths(base, head string, hdr map[string]string) (bool, error) {
	body, err := twoaiJobsGet("https://api.github.com/repos/"+bwRepo+"/compare/"+base+"..."+head, hdr)
	if err != nil {
		return false, err
	}
	var cmp struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil {
		return false, err
	}
	if len(cmp.Files) == 0 {
		return false, fmt.Errorf("no files in comparison")
	}
	for _, f := range cmp.Files {
		n := strings.ToLower(f.Filename)
		switch {
		case strings.HasPrefix(n, ".github/"),
			strings.HasPrefix(n, ".vscode/"),
			n == ".gitignore", n == ".gitattributes", n == ".editorconfig",
			n == "license", n == "licence",
			(strings.HasSuffix(n, ".md") && !strings.Contains(n, "/")):
			continue
		}
		return false, nil
	}
	return true, nil
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

// bwLastBuildFailure reads the newest Cloudflare build's log, when a token with
// Workers Builds read is on hand (CLOUDFLARE_BUILDS_TOKEN, else
// CLOUDFLARE_API_TOKEN), and returns the lines that explain a failure: the URL
// guard's verdict and the dropped URLs, or any error line, so the email says
// why rather than only that. Without a token it says so.
func bwLastBuildFailure() string {
	acct := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	tok := os.Getenv("CLOUDFLARE_BUILDS_TOKEN")
	if tok == "" {
		tok = os.Getenv("CLOUDFLARE_API_TOKEN")
	}
	if acct == "" || tok == "" {
		return "Why: unknown from here (no Cloudflare token with Workers Builds read in pipeline.env)."
	}
	hdr := map[string]string{"Authorization": "Bearer " + tok}
	const worker = "469a1508424e4e748d5ed287ce7e5502"
	b, err := twoaiJobsGet("https://api.cloudflare.com/client/v4/accounts/"+acct+"/builds/workers/"+worker+"/builds?per_page=1", hdr)
	if err != nil {
		return "Why: the Cloudflare builds list could not be read (" + trunc(err.Error(), 120) + ")."
	}
	var lst struct {
		Result struct {
			Builds []struct {
				BuildUUID string `json:"build_uuid"`
				Status    string `json:"status"`
				Outcome   string `json:"build_outcome"`
				CreatedOn string `json:"created_on"`
			} `json:"builds"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &lst) != nil || len(lst.Result.Builds) == 0 {
		return "Why: the Cloudflare builds list came back empty or unreadable."
	}
	bl := lst.Result.Builds[0]
	lb, err := twoaiJobsGet("https://api.cloudflare.com/client/v4/accounts/"+acct+"/builds/builds/"+bl.BuildUUID+"/logs", hdr)
	if err != nil {
		return fmt.Sprintf("Why: newest build %s is %s (%s) at %s; its log could not be read.", firstN(bl.BuildUUID, 8), bl.Outcome, bl.Status, bl.CreatedOn)
	}
	var lg struct {
		Result struct {
			Lines []struct {
				Line string `json:"line"`
			} `json:"lines"`
		} `json:"result"`
	}
	json.Unmarshal(lb, &lg)
	var keep []string
	for _, l := range lg.Result.Lines {
		t := strings.TrimSpace(l.Line)
		if strings.Contains(t, "url-guard:") || strings.HasPrefix(t, "/") && len(keep) > 0 && strings.Contains(keep[len(keep)-1], "drops") ||
			strings.Contains(t, "Failed:") || strings.Contains(t, "error occurred") || strings.Contains(t, "Error:") {
			keep = append(keep, t)
		}
	}
	if len(keep) > 12 {
		keep = keep[len(keep)-12:]
	}
	if len(keep) == 0 {
		return fmt.Sprintf("Why: newest build %s is %s (%s) at %s; no failure line found in its log.", firstN(bl.BuildUUID, 8), bl.Outcome, bl.Status, bl.CreatedOn)
	}
	return fmt.Sprintf("Why (newest build %s, %s, %s):\n  %s", firstN(bl.BuildUUID, 8), bl.Outcome, bl.CreatedOn, strings.Join(keep, "\n  "))
}

// bwStaleAfter is how long the live site may go without a build. Until
// 2026-10-12 run-pipeline.ps1 lets full runs start only at 10:00 and 18:00
// UTC, a gap of up to sixteen hours, so five hours raised a false alert every
// afternoon (row 475, built 11:43 UTC, alerted 16:47 UTC). The gate lapses
// on its own date and so does this.
func bwStaleAfter(now time.Time) time.Duration {
	if now.Before(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		return 18 * time.Hour
	}
	return bwStaleLive
}
