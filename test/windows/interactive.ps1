# The bridge into the logged-on session. Started by the FactorInteractive
# scheduled task, which is registered against the interactive logon type, so
# what runs here has a real window station: a desktop to screenshot, a
# notification area for a tray icon, a cursor that can be moved.
#
# The contract with the host is three files. job.ps1 is the work, job.out is
# everything it printed, and job.rc appearing is the signal that it finished.
# job.rc is removed first so the host can never read a stale exit code and
# call the previous run's result this run's.
$root = "C:\factor-test"
$job  = "$root\job.ps1"
$out  = "$root\job.out"
$rc   = "$root\job.rc"

Remove-Item $rc -Force -ErrorAction SilentlyContinue
Set-Content -Path $out -Value "" -Encoding utf8

if (-not (Test-Path $job)) {
  Set-Content -Path $out -Value "no job.ps1 to run" -Encoding utf8
  Set-Content -Path $rc -Value "2"
  exit
}

# cmd does the redirecting rather than PowerShell: `*>` turns a child's
# stderr into error records and reorders it against stdout, which is how a
# failing test's output ends up unreadable. cmd merges the two streams as the
# process wrote them and hands back the real exit code.
& cmd.exe /c "powershell.exe -NoProfile -ExecutionPolicy Bypass -File ""$job"" > ""$out"" 2>&1"
Set-Content -Path $rc -Value "$LASTEXITCODE"
