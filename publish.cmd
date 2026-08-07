@echo off
rem ============================================================================
rem  publish.cmd - publish srjconsultingservices.com from this PC
rem ============================================================================
rem  PC-initiated publish workflow. Runs the same three pipeline stages the
rem  daily 11:00 UTC Render cron runs, from this machine:
rem
rem    1. sync_content  - exports SQL content (srj-audit-db) to srj-content
rem    2. favicons      - ensures the favicon files exist in srj-site
rem    3. deploy_site   - fires the Cloudflare build hook
rem
rem  GITHUB_TOKEN is derived automatically from this PC's git credential
rem  store (the same credential git push already uses), so the only value
rem  publish.env.cmd needs is DATABASE_URL.
rem
rem  FIRST-TIME SETUP (once):
rem    1. Copy publish.env.example.cmd to publish.env.cmd
rem    2. Paste the external DATABASE_URL into it (instructions inside)
rem  EVERY PUBLISH AFTER THAT: double-click this file.
rem ============================================================================
setlocal enabledelayedexpansion
cd /d "%~dp0"

if not exist publish.env.cmd (
  echo [publish] ERROR: publish.env.cmd not found.
  echo [publish] Copy publish.env.example.cmd to publish.env.cmd and paste the
  echo [publish] DATABASE_URL into it. Instructions are inside that file.
  pause
  exit /b 1
)
call publish.env.cmd

if "%DATABASE_URL%"=="" (
  echo [publish] ERROR: DATABASE_URL is empty. Edit publish.env.cmd.
  pause
  exit /b 1
)
if "%DATABASE_URL%"=="PASTE_EXTERNAL_DATABASE_URL_HERE" (
  echo [publish] ERROR: DATABASE_URL is still the placeholder. Edit publish.env.cmd.
  pause
  exit /b 1
)

rem --- GITHUB_TOKEN: use env file value if set, else ask git's credential store
if "%GITHUB_TOKEN%"=="" (
  > "%TEMP%\srj_cred_req.txt" (
    echo protocol=https
    echo host=github.com
    echo.
  )
  for /f "usebackq tokens=1,* delims==" %%A in (`git credential fill ^< "%TEMP%\srj_cred_req.txt" 2^>nul`) do (
    if /i "%%A"=="password" set "GITHUB_TOKEN=%%B"
  )
  del "%TEMP%\srj_cred_req.txt" >nul 2>nul
)
if "%GITHUB_TOKEN%"=="" (
  echo [publish] ERROR: could not obtain a GitHub token from git's credential
  echo [publish] store. Either run "git push" once in this folder to refresh
  echo [publish] stored credentials, or set GITHUB_TOKEN in publish.env.cmd.
  pause
  exit /b 1
)

where go >nul 2>nul
if errorlevel 1 (
  echo [publish] ERROR: Go is not installed or not on PATH. Run:
  echo [publish]   winget install GoLang.Go
  echo [publish] then open a NEW window and run publish.cmd again.
  pause
  exit /b 1
)

echo [publish] Building pipeline...
go build -o pipeline-local.exe .
if errorlevel 1 (
  echo [publish] ERROR: build failed. Run "git pull" in this folder and retry.
  pause
  exit /b 1
)

echo [publish] 1/3 sync_content: SQL to srj-content repo...
pipeline-local.exe sync_content
if errorlevel 1 (
  echo [publish] ERROR: sync_content failed. Nothing was deployed.
  pause
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
  echo [publish] site did not rebuild. Run publish.cmd again to retry.
  pause
  exit /b 1
)

echo.
echo [publish] Done. The Cloudflare build takes about 3 minutes.
echo [publish] Verify at https://srjconsultingservices.com/ with Ctrl+Shift+R.
pause
endlocal
