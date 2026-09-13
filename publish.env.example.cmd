@echo off
rem ============================================================================
rem  publish.env.example.cmd - TEMPLATE for publish.env.cmd
rem ============================================================================
rem  SETUP (one time): copy this file to publish.env.cmd in the same folder,
rem  then replace the DATABASE_URL placeholder below. publish.env.cmd is
rem  gitignored; it never leaves this PC.
rem
rem  DATABASE_URL - the ONLY value you must paste:
rem    Render dashboard - srj-audit-db (the Postgres) - Connect - External
rem    Database URL - Copy. It looks like:
rem    postgres://srj_audit_db_user:PASSWORD@dpg-d8s1jte8bjmc73blao1g-a.ohio-postgres.render.com/srj_audit_db
rem    Use the EXTERNAL URL (hostname ends in .ohio-postgres.render.com).
rem    If connections fail, append ?sslmode=require to the end.
rem
rem  GITHUB_TOKEN - leave empty. publish.cmd obtains it automatically from
rem    this PC's git credential store (the same credential git push uses).
rem    Only fill it if that automatic step ever fails.
rem
rem  CLOUDFLARE_DEPLOY_HOOK - paste your own. Cloudflare dashboard - the
rem    Worker - Settings - Builds - Deploy hooks - copy the URL.
rem
rem    A real hook URL used to be filled in here, in a file that IS committed,
rem    on the reasoning that "its only capability is starting a site build".
rem    That reasoning is wrong. Anyone holding it can trigger unlimited
rem    builds, which burns the account's build minutes, and on a site that
rem    publishes whatever the pipeline last wrote, it decides WHEN content
rem    goes live. Removed 2026-09-13 before this repo goes public.
rem
rem    REMOVING IT HERE DOES NOT REVOKE IT. The old hook is in the git
rem    history and always will be. Delete hook e69389bf-e2f0-4d52-8eb2-
rem    8fb0fdb75e02 in the Cloudflare dashboard and create a new one; that
rem    is the only action that actually revokes it.
rem ============================================================================

set DATABASE_URL=PASTE_EXTERNAL_DATABASE_URL_HERE
set GITHUB_TOKEN=
set CLOUDFLARE_DEPLOY_HOOK=PASTE_DEPLOY_HOOK_URL_HERE
