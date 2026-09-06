# nasdump.ps1 - four times a day, dumps srj_audit and copies it to the NAS.
#
# Runs as scheduled task srj-pg-nasdump at 01:45 and every six hours, under
# Stephen's account: SYSTEM has no credential for \\SRJNAS and no pgpass.conf,
# so it would fail at the copy and again at the dump.
#
# WHY IT DUMPS LOCALLY FIRST AND COPIES SECOND. pg_dump --jobs=4 writing
# straight onto SMB is slow, and a network blip mid-write leaves a directory
# that looks complete and restores to nothing. Local disk is fast and does not
# blip; the copy is the part that can safely retry, and robocopy does. The
# byte-count comparison afterwards is what makes "it copied" mean something.
#
# WHY EVERY EXIT GOES THROUGH Finish. On 2026-09-04 console history was pasted
# into the bottom of this file. PowerShell rejected the script before its
# first line, so the log simply stopped rather than showing an error, the
# scheduled task reported Ready, and no backup was produced for a day. Nobody
# noticed because silence looked like success. Finish writes the exit code to
# nasdump-last.txt on every path including every failure, and the morning
# check reads that file. A status line appended at the end of the script would
# only ever record success, because each FAILED path exits before reaching it.
#
# THE 900 MB FLOOR is a sanity check, not a size limit: a dump smaller than
# that is a dump of something that is not this database.

$bin   = 'C:\Program Files\PostgreSQL\18\bin'
$local = 'C:\srj-data\dumps'
$nas   = '\\SRJNAS\postgres-backups\db_backups'
$log   = 'C:\srj-data\nasdump.log'
$status = 'C:\srj-data\logs\nasdump-last.txt'
$keep  = 90
$stamp = Get-Date -Format 'yyyyMMdd_HHmm'
$dir   = Join-Path $local $stamp
function Log($m) { "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') $m" | Add-Content $log }

# Every exit goes through Finish, so a failure is always recorded in
# nasdump-last.txt. Appending the status line to the end of the script
# would only ever record success: each FAILED path exits before reaching
# it, which is the same invisible-failure shape that hid the parse error
# on 2026-09-04.
function Finish($code, $msg) {
  Log $msg
  New-Item -ItemType Directory -Path (Split-Path $status) -Force | Out-Null
  "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') nasdump exit=$code $msg" | Set-Content $status
  exit $code
}

New-Item -ItemType Directory -Path (Split-Path $status) -Force | Out-Null
Log "start $stamp"

& "$bin\pg_dump.exe" -U postgres -d srj_audit --format=directory --jobs=4 --enable-row-security --file=$dir 2>>$log
if ($LASTEXITCODE -ne 0 -or -not (Test-Path (Join-Path $dir 'toc.dat'))) { Finish 1 "FAILED at dump" }
$size = (Get-ChildItem $dir -Recurse -File | Measure-Object Length -Sum).Sum
if ($size -lt 900MB) { Finish 1 "FAILED dump too small $([math]::Round($size/1MB)) MB" }
Log "dump ok $([math]::Round($size/1MB)) MB"

if (-not (Test-Path $nas)) { Finish 1 "FAILED NAS unreachable" }
robocopy $dir (Join-Path $nas $stamp) /E /R:3 /W:10 /NP /NFL /NDL /LOG+:$log | Out-Null
if ($LASTEXITCODE -ge 8) { Finish 1 "FAILED at copy rc=$LASTEXITCODE" }
$remote = (Get-ChildItem (Join-Path $nas $stamp) -Recurse -File | Measure-Object Length -Sum).Sum
if ($remote -ne $size) { Finish 1 "FAILED size mismatch local=$size remote=$remote" }
Log "copy ok, verified $([math]::Round($remote/1MB)) MB"

Get-ChildItem $nas   -Directory | Where-Object { $_.Name -match '^\d{8}_\d{4}$' -and $_.CreationTime -lt (Get-Date).AddDays(-$keep) } | Remove-Item -Recurse -Force
Get-ChildItem $local -Directory | Where-Object { $_.Name -match '^\d{8}_\d{4}$' } | Sort-Object Name -Descending | Select-Object -Skip 7 | Remove-Item -Recurse -Force
$onnas = (Get-ChildItem $nas -Directory | Where-Object Name -match '^\d{8}_\d{4}$').Count
Finish 0 "ok $stamp, $onnas on NAS"
