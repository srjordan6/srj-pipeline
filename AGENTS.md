# AGENTS.md — srj-pipeline

Read this before writing anything. It exists because of a real incident, not as
boilerplate.

## What this repository is

One Go binary, `pipeline`, with ~28 subcommands. `pipeline all` runs the daily
sequence from a Render cron (`crn-d9ju0tjrjlhs73f8jv6g`, Ohio) at 11:00 UTC. It
collects public data into PostgreSQL, renders every page of theworldofai.org
into `twoai_pages`, exports the whole content set to R2, and triggers the
Cloudflare build.

Every stage is a subcommand and runs as its own subprocess. `Run()` ignores
stage failures on purpose, so one bad upstream cannot block the site build.

## Before you write a line

```
git fetch origin main && git log --oneline -1 origin/main
```

**If HEAD has moved since your checkout, pull before editing.** On 2026-08-19 a
session working from a stale clone pushed a whole-file write of `main.go` and
silently deleted 193 lines of another session's work: a glossary lens merge that
had just made 2,109 audience readings visible for the first time, a drift
warning between two term tables, and two log fixes. Nothing errored. The only
evidence was one line missing from the next cron log, and the lenses would have
gone dark on 522 pages that night.

## After you push

**Verify the specific lines you changed are still present**, and that lines you
did not intend to touch are unchanged:

```
git show --stat HEAD
git show HEAD -- main.go | grep '^-' | head -40
```

A diff that deletes code you never read is the signature of the failure above.
If your commit removes lines outside its own subject, stop and rebase properly.

## Rules that are not negotiable

1. **Never rewrite a whole file you did not read at current HEAD.** Edit the
   region you mean to change.
2. **Compile before pushing.** `go build -o pipeline .` A syntax error here
   stops the site publishing.
3. **Verify computed figures against the live database** before putting them in
   a comment, a commit message, or a page. `SRJ:execute_sql` is read-only and
   free.
4. **A join must store what it matched on.** Attribution that keeps no evidence
   cannot be audited, and a wrong one survives quietly. This codebase has found
   that failure three times: MCP servers matched by substring (101 false
   attributions), patents matched without keeping the applicant string, and
   company URLs reduced by a hardcoded suffix list.
5. **Never match an entity on a name alone.** Require a hard identifier and
   store it.
6. **Published URLs never move.** `url_registry` fails the deploy if a live URL
   would vanish without a redirect.
7. **Do not spoof a browser to defeat a bot wall.** This crawler identifies
   itself and carries a contact address.

## Logging

A line should mean something happened. Do not report a ceiling (`50 models` when
50 is the API limit), do not log a permanent success as a warning, rate-limit
conditions that are the publisher's choice, and make an error message name the
fault it actually found.

## Deploys

`autoDeploy` is on for commits to `main`. **A push during a cron run kills that
run and restarts it.** On 2026-08-19 six pushes stretched one run across two
hours and forty-four minutes; the sequence itself takes about twenty. Check
whether a run is in progress before pushing, or expect to restart it.

## Where the reasoning lives

Architecture and history are in SQL, not in this repo: `site_arch_doc`
(`platform-v1-2026-08-18` and `v3-2026-08-18`) and `site_arch_changelog`, which
records failures and reversals at the same length as successes. Read the
changelog before concluding something is missing — it may have been removed
deliberately, and the reason will be there.
