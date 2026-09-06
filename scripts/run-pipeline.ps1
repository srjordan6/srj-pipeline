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
#   - writes a dated log per stage and keeps the last 30 days
#   - records the exit code, so a failed run is visible without reading logs

param([Parameter(Mandatory=$true)][string]$Stage)

$repo   = 'C:\SRJ Website Code Archive\srj-pipeline'
$envf   = 'C:\srj-data\pipeline.env'
$logdir = 'C:\srj-data\logs'
$lock   = "C:\srj-data\pipeline-$Stage.lock"
$stamp  = Get-Date -Format 'yyyyMMdd_HHmm'
$log    = Join-Path $logdir "$Stage-$stamp.log"
$status = Join-Path $logdir "$Stage-last.txt"

New-Item -ItemType Directory -Path $logdir -Force | Out-Null

# One at a time per stage. The lock holds the process id; a lock whose
# process no longer exists is stale immediately - a reboot mid-run left one
# behind on 2026-09-06 and blocked the next scheduled run for three hours.
if (Test-Path $lock) {
  $age = (Get-Date) - (Get-Item $lock).LastWriteTime
  $owner = Get-Content $lock -ErrorAction SilentlyContinue | Select-Object -First 1
  $alive = $owner -and (Get-Process -Id ([int]$owner) -ErrorAction SilentlyContinue)
  if ($alive -and $age.TotalHours -lt 3) {
    "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') skipped: $Stage already running (pid $owner, lock $([int]$age.TotalMinutes) min old)" | Add-Content $status
    exit 0
  }
  if (-not $alive) {
    "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') stale lock cleared: pid $owner not running (lock $([int]$age.TotalMinutes) min old)" | Add-Content $status
  }
}
$PID | Set-Content $lock

try {
  Get-Content $envf | ForEach-Object {
    if ($_ -match '^([^=#][^=]*)=(.*)$') { Set-Item -Path "env:$($matches[1].Trim())" -Value $matches[2] }
  }
  Set-Location $repo

  # Rebuild when origin has moved. On Render a push deployed itself; here
  # nothing does, and two commits sat on origin unbuilt on 2026-09-06 while
  # the scheduler ran yesterday's binary. Pull, and if HEAD changed, build.
  $before = (git rev-parse HEAD 2>$null)
  git pull -q 2>&1 | Out-Null
  $after = (git rev-parse HEAD 2>$null)
  if ($before -ne $after -or -not (Test-Path .\pipeline.exe)) {
    $env:PATH = "$env:PATH;C:\Program Files\Go\bin"
    go build -o pipeline.exe . 2>&1 | Tee-Object -FilePath (Join-Path $logdir "build-$stamp.log") | Out-Null
    if ($LASTEXITCODE -ne 0) {
      "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') $Stage BUILD FAILED at $after, running previous binary -> build-$stamp.log" | Add-Content $status
    } else {
      "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') rebuilt pipeline.exe at $after" | Add-Content $status
    }
  }

  $started = Get-Date
  & .\pipeline.exe $Stage 2>&1 | Tee-Object -FilePath $log | Out-Null
  $rc = $LASTEXITCODE
  $mins = [math]::Round(((Get-Date) - $started).TotalMinutes, 1)
  "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') $Stage exit=$rc after $mins min -> $log" | Set-Content $status
  Get-ChildItem $logdir -Filter "$Stage-*.log" | Where-Object LastWriteTime -lt (Get-Date).AddDays(-30) | Remove-Item -Force
  exit $rc
} finally {
  Remove-Item $lock -Force -ErrorAction SilentlyContinue
}
