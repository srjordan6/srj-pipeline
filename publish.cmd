@echo off
rem ============================================================================
rem  publish.cmd - publish srjconsultingservices.com from this PC
rem ============================================================================
rem  This is the PC-initiated publish workflow. It runs the same three
rem  pipeline stages the daily 11:00 UTC Render cron runs, from this
rem  machine, so website updates never depend on waiting for the cron:
rem
rem    1. sync_content  - exports SQL content (srj-audit-db) to the
rem                       srj-content GitHub repo (only what changed)
rem    2. favicons      - ensures the favicon files exist in srj-site
rem                       (idempotent, skips when unchanged)
rem    3. deploy_site   - fires the Cloudflare deploy hook so the site
rem                       rebuilds with the new content
rem
rem  FIRST-TIME SETUP (once):
rem    1. Install Go if missing:  winget install GoLang.Go
rem    2. Copy publish.env.example.cmd to publish.env.cmd
rem    3. Fill in the two secret values in publish.env.cmd (instructions
rem       are inside that file). publish.env.cmd stays on this PC only;
rem       it is gitignored and must never be committed.
rem
rem  EVERY PUBLISH AFTER THAT: double-click this file, or run  publish.cmd
rem  The Cloudflare build takes about 3 minutes after the script finishes.
rem ============================================================================
setlocal
cd /d "%~dp0"

if not exist publish.env.cmd (
  echo [publish] ERROR: publish.env.cmd not found.
  echo [publish] Copy publish.env.example.cmd to publish.env.cmd and fill in
  echo [publish] the values. See the instructions inside that file.
  exit /b 1
)
call publish.env.cmd

if "%DATABASE_URL%"=="" (
  echo [publish] ERROR: DATABASE_URL is empty. Edit publish.env.cmd.
  exit /b 1
)
if "%GITHUB_TOKEN%"=="" (
  echo [publish] ERROR: GITHUB_TOKEN is empty. Edit publish.env.cmd.
  exit /b 1
)

where go >nul 2>nul
if errorlevel 1 (
  echo [publish] ERROR: Go is not installed. Run:  winget install GoLang.Go
  echo [publish] then open a new terminal and run publish.cmd again.
  exit /b 1
)

echo [publish] Building pipeline...
go build -o pipeline-local.exe .
if errorlevel 1 (
  echo [publish] ERROR: build failed. Run "git pull" in this folder and retry.
  exit /b 1
)

echo [publish] 1/3 sync_content: SQL to srj-content repo...
pipeline-local.exe sync_content
if errorlevel 1 (
  echo [publish] ERROR: sync_content failed. Nothing was deployed.
  exit /b 1
)

echo [publish] 2/3 favicons: ensuring icon files in srj-site...
pipeline-local.exe favicons
if errorlevel 1 (
  echo [publish] WARNING: favicons step failed. Continuing, content still deploys.
)

echo [publish] 3/3 deploy_site: firing the Cloudflare build...
pipeline-local.exe deploy_site
if errorlevel 1 (
  echo [publish] ERROR: deploy hook failed. Content is in srj-content but the
  echo [publish] site did not rebuild. Retry: pipeline-local.exe deploy_site
  exit /b 1
)

echo.
echo [publish] Done. The Cloudflare build takes about 3 minutes.
echo [publish] Verify at https://srjconsultingservices.com/ with Ctrl+Shift+R.
endlocal
