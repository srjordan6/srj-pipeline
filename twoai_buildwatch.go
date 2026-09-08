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
	bwSite      = "https://theworldofai.org"
	bwRepo      = "srjordan6/twoai-site"
	bwGrace     = 20 * time.Minute // a healthy build plus deploy takes 6 to 8 minutes
	bwStaleLive = 30 * time.Hour   // the daily run should have shipped something by now
	bwFrom      = "theworldofai"   // the Inkbox identity the alert is sent as
)

// bwAlertTo is where the alert lands. BUILD_ALERT_TO overrides it. The default
// is the one inbox on this system a human reads rather than a machine mirrors.
func bwAlertTo() []string {
	if v := strings.TrimSpace(os.Getenv("BUILD_ALERT_TO")); v != "" {
		return strings.Split(v, ",")
	}
	return []string{"srj@srjconsultingservices.com"}
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

	queue := func(subj, text string) {
		db.Exec(`INSERT INTO inkbox_outbox (from_handle, channel, to_addrs, subject, body)
			VALUES ($1,'email',$2,$3,$4)`, bwFrom, pq.Array(bwAlertTo()), subj, text)
		fmt.Println("buildwatch: queued:", subj)
	}

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

	// Rule 2: nothing has shipped in over a day, whatever git says. Once a day.
	if !builtAt.IsZero() && now.Sub(builtAt) > bwStaleLive && get("stale_alert_day") != now.Format("2006-01-02") {
		set("stale_alert_day", now.Format("2006-01-02"))
		queue("theworldofai.org has not rebuilt in over a day",
			fmt.Sprintf(`The live site was built %s UTC, %.0f hours ago. The daily pipeline
fires a build after every publish, so a site this old means the trigger, the
build or the deploy has been failing since then. Bundle on the site: %s.`,
				builtAt.Format("2006-01-02 15:04"), now.Sub(builtAt).Hours(), live.Bundle))
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
