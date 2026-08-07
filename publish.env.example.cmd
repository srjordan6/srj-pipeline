@echo off
rem ============================================================================
rem  publish.env.example.cmd - TEMPLATE for publish.env.cmd
rem ============================================================================
rem  SETUP: copy this file to publish.env.cmd (same folder), then replace the
rem  two placeholder values below. publish.env.cmd is gitignored; it never
rem  leaves this PC. Do NOT put real secrets in THIS example file.
rem
rem  WHERE TO GET EACH VALUE (both are on the Render dashboard):
rem
rem  DATABASE_URL
rem    Render dashboard - srj-audit-db (the Postgres) - Connect - External
rem    Database URL - Copy. It looks like:
rem    postgres://srj_audit_db_user:PASSWORD@dpg-d8s1jte8bjmc73blao1g-a.ohio-postgres.render.com/srj_audit_db
rem    Use the EXTERNAL URL (the hostname ends in .ohio-postgres.render.com),
rem    not the internal one. If connections fail, append ?sslmode=require
rem
rem  GITHUB_TOKEN
rem    Render dashboard - srj-pipeline (the Ohio cron) - Environment tab -
rem    GITHUB_TOKEN - reveal - Copy. Same token the daily cron uses to push
rem    to srj-content and srj-site.
rem
rem  CLOUDFLARE_DEPLOY_HOOK is already filled in below; it is the srj-site
rem  build hook and its only capability is starting a build.
rem ============================================================================

set DATABASE_URL=PASTE_EXTERNAL_DATABASE_URL_HERE
set GITHUB_TOKEN=PASTE_GITHUB_TOKEN_HERE
set CLOUDFLARE_DEPLOY_HOOK=https://api.cloudflare.com/client/v4/workers/builds/deploy_hooks/e69389bf-e2f0-4d52-8eb2-8fb0fdb75e02
