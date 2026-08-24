# Adds what the local voice tier needs: a real Python for the Patter venv, and
# SoX for capture and playback. Kept out of bootstrap.ps1 because it is a
# choice - the default guest carries neither, and the gate says so - but kept
# in the repo so the snapshot it produces is reproducible.
param(
  [string]$PythonVersion = "3.12.8",
  [string]$SoxVersion    = "14.4.2"
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$root = "C:\factor-test"
function Say($m) { Write-Host "[voice] $m" }

# Looking python.exe up is not a test for Python on Windows. The App Execution
# Aliases put stubs for python.exe and python3.exe in WindowsApps which resolve
# happily and then exit 9009 with an advert for the Microsoft Store, so the
# lookup succeeds on a machine with no interpreter at all. Run it and read the
# version back instead - which is exactly what Factor's own probe does.
function HasRealPython {
  if (-not (Get-Command python.exe -ErrorAction SilentlyContinue)) { return $false }
  try {
    $old = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    $out = (& python.exe --version 2>&1 | Out-String)
    $ErrorActionPreference = $old
    return ($LASTEXITCODE -eq 0 -and $out -match 'Python 3\.')
  } catch { return $false }
}

function RefreshPath {
  $env:PATH = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" +
              [Environment]::GetEnvironmentVariable("Path", "User")
}

# --- Python ---------------------------------------------------------------
if (-not (HasRealPython)) {
  $exe = "$root\python-$PythonVersion.exe"
  if (-not (Test-Path $exe)) {
    Say "downloading Python $PythonVersion"
    Invoke-WebRequest "https://www.python.org/ftp/python/$PythonVersion/python-$PythonVersion-amd64.exe" -OutFile $exe
  }
  Say "installing Python $PythonVersion"
  $p = Start-Process $exe -ArgumentList '/quiet','InstallAllUsers=1','PrependPath=1','Include_pip=1','Include_test=0','Include_launcher=1' -Wait -PassThru
  if ($p.ExitCode -ne 0) { throw "the Python installer exited $($p.ExitCode)" }
  Remove-Item $exe -Force

  # The stubs sit in the user's PATH ahead of anything the installer prepends,
  # so they go on shadowing the real interpreter after a successful install.
  # Removing them is what Windows' own message tells a user to do, and it is
  # what makes `python` and `python3` name the interpreter on this machine.
  foreach ($stub in @("python.exe", "python3.exe")) {
    Remove-Item (Join-Path $env:LOCALAPPDATA "Microsoft\WindowsApps\$stub") -Force -ErrorAction SilentlyContinue
  }
  RefreshPath
  if (-not (HasRealPython)) { throw "Python installed but python.exe still does not answer" }
}

# --- SoX ------------------------------------------------------------------
# windowsCapture wants rec on PATH and windowsPlayback wants play. The Windows
# archive ships sox.exe alone; SoX picks its mode from argv[0], so the two are
# made as copies of it.
if (-not (Get-Command rec.exe -ErrorAction SilentlyContinue)) {
  $zip = "$root\sox-$SoxVersion.zip"
  if (-not (Test-Path $zip)) {
    # A named mirror, not the /download page: that one answers with an HTML
    # interstitial whose real link carries a short-lived token, so it cannot be
    # pinned. Skipped entirely when the host has already pushed an archive
    # here, which is how a guest with no route to SourceForge is provisioned.
    Say "downloading SoX $SoxVersion"
    Invoke-WebRequest "https://phoenixnap.dl.sourceforge.net/project/sox/sox/$SoxVersion/sox-$SoxVersion-win32.zip" -OutFile $zip -UserAgent "Mozilla/5.0"
  }
  Say "unpacking SoX $SoxVersion"
  $stage = "$root\sox-stage"
  Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path $stage | Out-Null
  & tar.exe -xf $zip -C $stage
  if ($LASTEXITCODE -ne 0) { Expand-Archive $zip -DestinationPath $stage -Force }

  $soxExe = Get-ChildItem $stage -Recurse -Filter sox.exe | Select-Object -First 1
  if (-not $soxExe) { throw "the SoX archive carried no sox.exe" }
  Remove-Item "C:\sox" -Recurse -Force -ErrorAction SilentlyContinue
  Move-Item $soxExe.Directory.FullName "C:\sox"
  Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
  foreach ($alias in @("rec.exe", "play.exe")) {
    if (-not (Test-Path "C:\sox\$alias")) { Copy-Item "C:\sox\sox.exe" "C:\sox\$alias" }
  }
  Remove-Item $zip -Force

  $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
  if ($machinePath -notlike "*C:\sox*") {
    [Environment]::SetEnvironmentVariable("Path", "$machinePath;C:\sox", "Machine")
  }
  RefreshPath
}

$ErrorActionPreference = "Continue"
Say "python: $(& python.exe --version 2>&1)"
Say "py:     $(& py.exe --version 2>&1)"
Say "pip:    $(& python.exe -m pip --version 2>&1)"
Say "rec:    $(if (Test-Path 'C:\sox\rec.exe') { 'C:\sox\rec.exe' } else { 'MISSING' })"
Say "play:   $(if (Test-Path 'C:\sox\play.exe') { 'C:\sox\play.exe' } else { 'MISSING' })"
exit 0
