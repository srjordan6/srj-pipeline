# wal-ship.ps1 - ships archived WAL to the NAS and prunes what is safely there.
#
# Runs every 15 minutes as scheduled task srj-wal-ship, under Stephen's
# account (SYSTEM has no credential for \\SRJNAS).
#
# WHY POSTGRES ARCHIVES LOCALLY AND THIS SHIPS IT, rather than archive_command
# writing straight to the NAS: the Postgres service runs as NetworkService,
# which has no credential for the share, and a failing archive_command makes
# Postgres retain WAL until the disk fills. Archiving to a local folder always
# succeeds; getting it off the machine is this script's problem, and a failure
# here costs a delayed copy rather than a stalled database.
#
# THE TWO-HOUR WINDOW. A segment is deleted locally only when the NAS holds a
# byte-identical copy AND the segment is more than two hours old. That window
# is a margin against a copy that looked fine and was not. It means the local
# folder always holds about two hours of WAL, which on a heavy write day is
# several gigabytes - that is the window doing its job, not a backlog. On
# 2026-09-06 the warning threshold was 4 GB, and a two-hour window at that
# day's rate came to exactly 4.0 GB, so the job would have warned about itself
# working correctly. Raised to 16 GB: four times what the heaviest day can put
# in the window, so it can now only fire if the copy has actually stopped.

$src = 'C:\srj-data\wal'
$nas = '\\SRJNAS\postgres-backups\wal'
$log = 'C:\srj-data\wal-ship.log'
function Log($m) { "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') $m" | Add-Content $log }
if (-not (Test-Path $nas)) { New-Item -ItemType Directory -Path $nas -Force | Out-Null }
if (-not (Test-Path $nas)) { Log "FAILED NAS unreachable, WAL retained locally"; exit 1 }
robocopy $src $nas /E /R:2 /W:5 /NP /NFL /NDL | Out-Null
if ($LASTEXITCODE -ge 8) { Log "FAILED copy rc=$LASTEXITCODE"; exit 1 }
$freed = 0
foreach ($f in Get-ChildItem $src -File | Where-Object LastWriteTime -lt (Get-Date).AddHours(-2)) {
  $r = Join-Path $nas $f.Name
  if ((Test-Path $r) -and ((Get-Item $r).Length -eq $f.Length)) { Remove-Item $f.FullName -Force; $freed++ }
}
$n = (Get-ChildItem $src -File).Count
$mb = [math]::Round(((Get-ChildItem $src -File | Measure-Object Length -Sum).Sum)/1MB)
Log "ok shipped, pruned=$freed local=$n files $mb MB"
if ($mb -gt 16000) { Log "WARNING local WAL over 16 GB - archiving may be failing, check pg_stat_archiver" }
