# run-pipeline.ps1 - runs one pipeline stage from Task Scheduler.
#
# Usage:  powershell -ExecutionPolicy Bypass -File C:\srj-data\run-pipeline.ps1 -Stage all
#         -Stage thinpages     (the old srj-thinpage-scraper)
#         -Stage inkbox_tick   (the old srj-inkbox-tick, every 5 minutes)
#
# What it does that a bare command would not:
#   - loads C:\srj-data\pipeline.env so the task has every credential
#   - refuses to start if the same stage is already running, because the
#     3-hourly schedule and a 25-minute run can overlap on a slow day and
#     two publishes fighting over the site is worse than one late run
#   - appends everything to ONE log, C:\srj-data\logs\pipeline.log, each run
#     in a block that opens with a ===== header naming the stage and closes
#     with its exit line (Stephen, 2026-10-05: when the pipeline runs I prefer
#     just to have one log). Rolled to pipeline.log.1 past 25 MB.
#   - records each stage's last result as one line of
#     C:\srj-data\logs\pipeline-status.txt, so a failed run is visible
#     without reading the log

param([Parameter(Mandatory=$true)][string]$Stage, [switch]$Now)

$repo   = 'C:\SRJ Website Code Archive\srj-pipeline'
$envf   = 'C:\srj-data\pipeline.env'
$logdir = 'C:\srj-data\logs'

# ONE HEAVY RUN AT A TIME, ACROSS ALL STAGES, NOT ONE PER STAGE NAME.
# Stephen, 2026-09-20, with four pipeline.exe processes live at once: one 43
# minutes old, two started in the same second. The lock below was per stage,
# so -Stage twoai, -Stage twoai_dart, -Stage twoai_ma_readings and
# -Stage twoai_learning_readings each took a different lock file and all four
# ran together. They then contended for the same database, the same binary and
# the same publish target, and the 43 minute one held pipeline.exe open so a
# rebuild renamed it to pipeline.exe~ and the run kept executing the OLD code.
# Its log showed none of the new stages, which read like a failed deploy and
# cost an hour chasing a push that had already happened.
#
# The lock did not fail. It guarded the stage name when the thing worth
# guarding is the binary and the database. Every heavy stage now shares one
# lock, so the second one waits.
#
# The tickers are exempt on purpose: inkbox_tick runs every five minutes and
# must not be blocked for the 25 minutes a twoai run takes. They touch the
# mailbox, not the site build, so they cannot collide with it.
#
# twoai_art is exempt for the same reason, Stephen 2026-09-22: run alone it
# writes page text for an hour or two through Ollama and never publishes, so
# it can run beside a twoai run without either one waiting. Its pages publish
# on the next twoai run.
# news_preview and news_gap only read the news tables (news_gap also writes
# one bridge row a day), so they may run beside a heavy run. 2026-10-03.
$solo   = @('inkbox_tick', 'email_route', 'twoai_art', 'backlog', 'source_pages', 'news_preview', 'news_gap')
$lock   = if ($solo -contains $Stage) { "C:\srj-data\pipeline-$Stage.lock" } else { 'C:\srj-data\pipeline.lock' }
$log    = Join-Path $logdir 'pipeline.log'
$status = Join-Path $logdir 'pipeline-status.txt'

New-Item -ItemType Directory -Path $logdir -Force | Out-Null

# ONE LOG, APPENDED A LINE AT A TIME. A file held open for a whole stage
# would lock out the five-minute tick that writes beside a heavy run, so
# each write opens, appends and closes, and retries briefly if the other
# writer has it at that instant. UTF-8, so it reads without iconv.
$utf8 = New-Object System.Text.UTF8Encoding($false)
# The pipeline writes UTF-8; read it as UTF-8, not the console code page,
# or a curly quote arrives as three junk characters.
try { [Console]::OutputEncoding = $utf8 } catch {}
function Write-Log([string]$text) {
  for ($i = 0; $i -lt 20; $i++) {
    try { [System.IO.File]::AppendAllText($log, $text + "`r`n", $utf8); return } catch { Start-Sleep -Milliseconds 50 }
  }
}
# The stage's line in pipeline-status.txt is replaced, every other kept.
function Set-Status([string]$text) {
  for ($i = 0; $i -lt 20; $i++) {
    try {
      $lines = @()
      if (Test-Path $status) { $lines = @([System.IO.File]::ReadAllLines($status, $utf8) | Where-Object { $_ -and -not $_.StartsWith("$Stage`t") }) }
      $lines += "$Stage`t$text"
      [System.IO.File]::WriteAllLines($status, [string[]]($lines | Sort-Object), $utf8)
      return
    } catch { Start-Sleep -Milliseconds 50 }
  }
}
function Stamp { Get-Date -Format 'yyyy-MM-dd HH:mm:ss' }

# Roll the log past 25 MB, keeping one previous file.
if ((Test-Path $log) -and (Get-Item $log).Length -gt 25MB) {
  try { Move-Item $log "$log.1" -Force } catch {}
}

# TWO FULL RUNS A DAY UNTIL 2026-10-12. Stephen, 2026-10-03: slow things
# down, one run at the start of off-peak and one two hours before off-peak
# ends. Off-peak is outside 12:00 to 18:00 UTC on weekdays, so the runs are
# 18:00 and 10:00 UTC, every day. The 3-hourly srj-pipeline task still fires
# (its trigger cannot be changed without Stephen's password), and this gate
# skips it. The runs come from srj-pipeline-offpeak-1000utc and
# srj-pipeline-offpeak-1800utc. The gate lapses by itself on 2026-10-12.
# -Now runs a full pipeline by hand regardless.
if ($Stage -eq 'all' -and -not $Now -and (Get-Date) -lt [datetime]'2026-10-12') {
  $utcHour = (Get-Date).ToUniversalTime().Hour
  if ($utcHour -ne 10 -and $utcHour -ne 18) {
    Set-Status "$(Stamp) skipped: off-peak schedule until 2026-10-12, full runs only at 10:00 and 18:00 UTC"
    exit 0
  }
}

# One at a time per stage. The lock holds the process id AND the process
# start time, because a pid alone is not proof after a reboot: Windows reuses
# pids freely, so a lock left behind by a killed run can be impersonated by
# whatever unrelated process later inherits that number, and the stage then
# skips for up to three hours believing itself already running. A lock whose
# process no longer exists, or whose pid now belongs to a process that started
# at a different time, is stale immediately. A reboot mid-run on 2026-09-06
# left one behind after the twoai run died between twoai_publish_r2 and
# deploy_site; that pid happened to be free, but it did not have to be.
if (Test-Path $lock) {
  $age = (Get-Date) - (Get-Item $lock).LastWriteTime
  $lines = @(Get-Content $lock -ErrorAction SilentlyContinue)
  $owner = $lines | Select-Object -First 1
  $ownerStart = $lines | Select-Object -Skip 1 -First 1
  $proc = if ($owner) { Get-Process -Id ([int]$owner) -ErrorAction SilentlyContinue } else { $null }
  # Same pid is not enough. If the lock recorded a start time, the running
  # process must match it to within a second, or it is a different process
  # wearing a dead run's number.
  $alive = $false
  if ($proc) {
    if ([string]::IsNullOrWhiteSpace($ownerStart)) {
      $alive = $true   # lock written before start times were recorded
    } else {
      $alive = ([math]::Abs(($proc.StartTime - [datetime]$ownerStart).TotalSeconds) -lt 2)
      if (-not $alive) {
        Set-Status "$(Stamp) stale lock cleared: pid $owner was reused (lock start $ownerStart, live start $($proc.StartTime))"
      }
    }
  }
  if ($alive -and $age.TotalHours -lt 3) {
    Set-Status "$(Stamp) skipped: $Stage already running (pid $owner, lock $([int]$age.TotalMinutes) min old)"
    exit 0
  }
  if (-not $proc) {
    Set-Status "$(Stamp) stale lock cleared: pid $owner not running (lock $([int]$age.TotalMinutes) min old)"
  }
}
@($PID, (Get-Process -Id $PID).StartTime.ToString('o')) | Set-Content $lock

try {
  Get-Content $envf | ForEach-Object {
    if ($_ -match '^([^=#][^=]*)=(.*)$') { Set-Item -Path "env:$($matches[1].Trim())" -Value $matches[2] }
  }
  Set-Location $repo

  # Rebuild when the SOURCE IS NEWER THAN THE BINARY. On Render a push
  # deployed itself; here nothing does, and two commits sat on origin unbuilt
  # on 2026-09-06 while the scheduler ran yesterday's binary.
  #
  # The first version of this compared HEAD before and after the pull, and it
  # missed the commonest case entirely: a commit made IN THIS CLONE. Claude
  # commits and pushes from C:\SRJ Website Code Archive\srj-pipeline, so by
  # the time a scheduled run starts the commit is already local, the pull says
  # "Already up to date", HEAD does not move, and nothing rebuilds. On
  # 2026-09-08 that left pipeline.exe a full day stale with three unbuilt
  # commits in it, including a guard that stops a language model's refusal to
  # write a summary being published as one. The code was committed, pushed,
  # reported done - and never once ran.
  #
  # Comparing file times is true however the code arrived: pull, local commit,
  # or a hand edit that was never committed at all. Timestamps rather than
  # hashes, because that is the question being asked - is the binary older
  # than what it was built from.
  $before = (git rev-parse HEAD 2>$null)
  git pull -q 2>&1 | Out-Null
  $after = (git rev-parse HEAD 2>$null)
  # A SOLO STAGE RUNS ITS OWN COPY OF THE BINARY. Stephen, 2026-09-22: a
  # twoai run held pipeline.exe open for two hours, so twoai_art could not
  # rebuild it, fell back to the old binary, and that binary did not know the
  # stage existed ("unknown source: twoai_art"). A stage allowed to run beside
  # the heavy runs must not share the file the heavy runs keep open.
  $exeName = if ($solo -contains $Stage) { "pipeline-$Stage.exe" } else { 'pipeline.exe' }
  $exe = Join-Path $repo $exeName
  $newestGo = (Get-ChildItem $repo -Filter *.go -File | Sort-Object LastWriteTime -Descending | Select-Object -First 1)
  $stale = (-not (Test-Path $exe)) -or
           ($newestGo -and (Get-Item $exe).LastWriteTime -lt $newestGo.LastWriteTime)
  $buildNote = ''
  $buildOut = @()
  if ($before -ne $after -or $stale) {
    $why = if ($before -ne $after) { "origin moved to $after" } else { "source newer than binary ($($newestGo.Name))" }
    $env:PATH = "$env:PATH;C:\Program Files\Go\bin"
    $buildOut = @(go build -o $exeName . 2>&1 | ForEach-Object { "$_" })
    if ($LASTEXITCODE -ne 0) {
      # Carried into the final status line rather than written separately. The
      # line below used Set-Content, which overwrote this note every run, so a
      # BUILD FAILED message survived only until the stage finished - the one
      # record you would go looking for, erased by the thing that replaced it.
      # The compiler's output goes into the run's block of pipeline.log.
      $buildNote = " BUILD FAILED ($why), ran previous binary, compiler output in pipeline.log"
    } else {
      $buildNote = " rebuilt ($why)"
    }
  }

  $started = Get-Date
  $header = "===== $(Stamp) $Stage started$buildNote ====="
  # A QUIET TICK LEAVES NOTHING IN THE LOG. inkbox_tick runs every five
  # minutes and almost every run says nothing happened (Stephen, 2026-09-10:
  # too many logs). Its output is held, and written only when the tick did
  # something: a non-zero exit, anything that looks like an error, mail, a
  # task, a queued or sent message, an alert, or buildwatch finding a commit
  # that has not shipped. "alert" counts as a whole word or a non-zero count
  # only, because every tick prints own_alerts=0. Every other stage streams
  # into the log as it runs, so a killed run still leaves what it did.
  $quietable = $Stage -in 'inkbox_tick','inkbox_outbox'
  if ($quietable) {
    $out = @(& $exe $Stage 2>&1 | ForEach-Object { "$_" })
    $rc = $LASTEXITCODE
    $body = ($out + $buildOut) -join "`n"
    $interesting = ($rc -ne 0) -or ($body -match '(?im)(error|fail|panic|refused|timeout|shipped=false|queued=[1-9]|sent=[1-9]|emailed=[1-9]|\balerts?\b|alerts=[1-9]|mail=[1-9]|tasks=[1-9]|unreadable=[1-9])')
    if ($interesting) {
      Write-Log $header
      foreach ($l in $buildOut) { Write-Log $l }
      foreach ($l in $out) { Write-Log $l }
    }
  } else {
    Write-Log $header
    foreach ($l in $buildOut) { Write-Log $l }
    & $exe $Stage 2>&1 | ForEach-Object { Write-Log "$_" }
    $rc = $LASTEXITCODE
  }
  $mins = [math]::Round(((Get-Date) - $started).TotalMinutes, 1)
  $line = "$(Stamp) $Stage exit=$rc after $mins min$buildNote"
  if (-not $quietable -or $interesting) { Write-Log "===== $line =====" }
  Set-Status $line
  exit $rc
} finally {
  Remove-Item $lock -Force -ErrorAction SilentlyContinue
}
