# One-time setup for the Windows test guest. Delivered and run through Guest
# Additions because it is the step that has to happen before there is any ssh
# to run it over. Idempotent by design: re-running repairs a guest rather
# than doubling anything up.
param(
  [Parameter(Mandatory=$true)][string]$PubKeyFile,
  [string]$PassFile = "",
  [Parameter(Mandatory=$true)][string]$User,
  [Parameter(Mandatory=$true)][string]$GoVersion,
  [switch]$SkipRaceToolchain
)

$ErrorActionPreference = "Stop"
# Invoke-WebRequest renders a progress bar per chunk, which costs more than
# the download on a VM. Silencing it turns minutes into seconds.
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$root = "C:\factor-test"
function Say($m) { Write-Host "[bootstrap] $m" }

# Everything is unpacked into a staging directory this script owns and then
# moved into place. Unpacking straight into C:\ failed for every one of the
# 14,539 entries in the Go distribution, starting with the first: the drive
# root is not somewhere a non-installer process gets to write a tree, and the
# only thing that ever succeeded there was Expand-Archive, which streams every
# entry through the pipeline and takes long enough to look like a hang.
#
# The destination is also never spelled with a trailing backslash. PowerShell
# hands a native command its raw command line, and "C:\" reaches the CRT as an
# escaped quote, so tar is given a malformed argument and unpacks nothing.
function InstallTree($archive, $topLevel, $target) {
  $stage = "$root\stage"
  Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path $stage | Out-Null

  if ($archive -like "*.exe") {
    & $archive -y "-o$stage" | Out-Null          # 7-Zip self-extractor
  } elseif (Get-Command tar.exe -ErrorAction SilentlyContinue) {
    & tar.exe -xf $archive -C $stage
    if ($LASTEXITCODE -ne 0) {
      Say "tar exited $LASTEXITCODE; falling back to Expand-Archive (slow)"
      Expand-Archive $archive -DestinationPath $stage -Force
    }
  } else {
    Expand-Archive $archive -DestinationPath $stage -Force
  }

  $src = Join-Path $stage $topLevel
  if (-not (Test-Path $src)) { throw "$archive did not unpack a $topLevel directory" }
  Remove-Item $target -Recurse -Force -ErrorAction SilentlyContinue
  Move-Item $src $target
  Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
}

# --- OpenSSH server -------------------------------------------------------
# The capability is the supported route, but it needs Windows Update to
# answer. A guest pointed at a WSUS that will not serve it is common enough
# to be worth the fallback rather than a confusing failure.
if (-not (Get-Service sshd -ErrorAction SilentlyContinue)) {
  Say "installing OpenSSH Server"
  try {
    Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 | Out-Null
  } catch {
    Say "the capability would not install ($($_.Exception.Message)); falling back to the GitHub build"
    $rel = Invoke-RestMethod "https://api.github.com/repos/PowerShell/Win32-OpenSSH/releases/latest"
    $url = ($rel.assets | Where-Object { $_.name -eq "OpenSSH-Win64.zip" }).browser_download_url
    Invoke-WebRequest $url -OutFile "$root\openssh.zip"
    Expand-Archive "$root\openssh.zip" -DestinationPath "C:\Program Files" -Force
    & "C:\Program Files\OpenSSH-Win64\install-sshd.ps1"
  }
}
Set-Service sshd -StartupType Automatic
Start-Service sshd

if (-not (Get-NetFirewallRule -Name factor-ssh -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -Name factor-ssh -DisplayName "Factor test ssh" `
    -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
}

# --- authorized key -------------------------------------------------------
# An administrator's key does not live in their profile: sshd reads
# administrators_authorized_keys for anyone in the Administrators group, and
# silently ignores it unless the ACL excludes every non-admin. Getting that
# ACL wrong is the single most common reason this setup ends in a password
# prompt, so both files are written and the ACL is set explicitly.
$pub = (Get-Content -Raw $PubKeyFile).Trim()
$admKeys = "C:\ProgramData\ssh\administrators_authorized_keys"
Set-Content -Path $admKeys -Value $pub -Encoding ascii
icacls.exe $admKeys /inheritance:r /grant "Administrators:F" /grant "SYSTEM:F" | Out-Null

$userSsh = Join-Path $env:USERPROFILE ".ssh"
New-Item -ItemType Directory -Force -Path $userSsh | Out-Null
Set-Content -Path (Join-Path $userSsh "authorized_keys") -Value $pub -Encoding ascii
Say "installed the test key"

# --- logon ----------------------------------------------------------------
# The desktop, tray and grid-vision tests need a real interactive session,
# and Windows only creates one at logon, so the VM has to log itself in.
$pass = ""
if ($PassFile -and (Test-Path $PassFile)) {
  $raw = Get-Content -Raw $PassFile -ErrorAction SilentlyContinue
  if ($raw) { $pass = $raw.TrimEnd("`r","`n") }
}

if ($pass) {
  # AutoAdminLogon stores this in clear text - that is what the mechanism is.
  # Acceptable for a throwaway test VM on loopback, and nowhere else.
  $wl = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"
  Set-ItemProperty $wl AutoAdminLogon    "1"
  Set-ItemProperty $wl DefaultUserName   $User
  Set-ItemProperty $wl DefaultPassword   $pass
  Set-ItemProperty $wl DefaultDomainName $env:COMPUTERNAME
  Remove-ItemProperty $wl AutoLogonCount -ErrorAction SilentlyContinue
  Say "autologon set for $User"
} else {
  # Windows already logs a sole blank-password account in on its own, so
  # AutoAdminLogon is left alone - pointing it at an empty DefaultPassword is
  # a known way to break the logon that currently works.
  #
  # What a blank password does need is this: every non-console logon is
  # refused for such an account, which is what blocks guestcontrol and ssh
  # key auth alike. Clearing it requires an elevated session, so on a fresh
  # guest it is the one irreducibly manual step - an elevated shell running
  # `reg add HKLM\SYSTEM\CurrentControlSet\Control\Lsa /v
  # LimitBlankPasswordUse /t REG_DWORD /d 0 /f`. It is re-asserted here so a
  # later bootstrap cannot silently lose it.
  Set-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Lsa" LimitBlankPasswordUse 0
  Say "blank-password account: LimitBlankPasswordUse cleared, autologon left to Windows"
}

# --- keep the session awake and visible -----------------------------------
# A locked or blanked session is not one a screenshot can be taken in, and
# the desktop tests would fail on a machine that simply went to sleep.
$pers = "HKLM:\SOFTWARE\Policies\Microsoft\Windows\Personalization"
New-Item -Path $pers -Force | Out-Null
Set-ItemProperty $pers NoLockScreen 1
Set-ItemProperty "HKCU:\Control Panel\Desktop" ScreenSaveActive "0"
Set-ItemProperty "HKCU:\Control Panel\Desktop" ScreenSaveTimeOut "0"
powercfg /change monitor-timeout-ac 0
powercfg /change standby-timeout-ac 0
powercfg /change hibernate-timeout-ac 0

# Windows Update rebooting mid-suite would read as a hung test rather than as
# what it is. A test VM restored from a snapshot every run has nothing to
# gain from patching itself.
Set-Service wuauserv -StartupType Disabled -ErrorAction SilentlyContinue
Stop-Service wuauserv -Force -ErrorAction SilentlyContinue

# Defender scans every object file a Go build writes, which roughly doubles
# the suite's wall clock. Excluding the trees the build touches is the single
# largest speedup available here.
try {
  Add-MpPreference -ExclusionPath $root, "C:\go", "C:\w64devkit", "$env:USERPROFILE\go", "$env:USERPROFILE\src"
  Add-MpPreference -ExclusionProcess "go.exe", "gcc.exe", "link.exe", "compile.exe"
  Say "added Defender exclusions"
} catch { Say "could not set Defender exclusions: $($_.Exception.Message)" }

# --- Go -------------------------------------------------------------------
# bsdtar ships with Windows 10 1803+ and unpacks a zip far faster than
# Expand-Archive, which reads the whole archive through the pipeline.
if (-not (Test-Path "C:\go\bin\go.exe")) {
  $zip = "$root\go.zip"
  # A retry after a failed unpack should not pay for the download again.
  if (-not (Test-Path $zip)) {
    Say "downloading Go $GoVersion"
    Invoke-WebRequest "https://go.dev/dl/go$GoVersion.windows-amd64.zip" -OutFile $zip
  }
  Say "unpacking Go"
  InstallTree $zip "go" "C:\go"
  if (-not (Test-Path "C:\go\bin\go.exe")) { throw "Go did not unpack to C:\go" }
  Remove-Item $zip -Force
}

# --- race toolchain -------------------------------------------------------
# The race detector needs cgo, and cgo on Windows needs a C compiler.
# w64devkit is a single zip with no installer and no registry footprint,
# which is the only kind of dependency worth adding to a snapshot.
if (-not $SkipRaceToolchain -and -not (Test-Path "C:\w64devkit\bin\gcc.exe")) {
  try {
    Say "downloading w64devkit for the race detector"
    # w64devkit stopped publishing .zip at 2.x and ships 7-Zip self-extractors
    # instead, so the .exe is matched first and the .zip kept for older tags.
    $rel = Invoke-RestMethod "https://api.github.com/repos/skeeto/w64devkit/releases/latest"
    $asset = $rel.assets | Where-Object { $_.name -match '^w64devkit-x64-.*\.7z\.exe$' } | Select-Object -First 1
    if (-not $asset) { $asset = $rel.assets | Where-Object { $_.name -match '^w64devkit-x64-.*\.zip$' } | Select-Object -First 1 }
    if (-not $asset) { throw "no x64 w64devkit asset in release $($rel.tag_name)" }
    $archive = Join-Path $root $asset.name
    if (-not (Test-Path $archive)) { Invoke-WebRequest $asset.browser_download_url -OutFile $archive }
    InstallTree $archive "w64devkit" "C:\w64devkit"
    if (-not (Test-Path "C:\w64devkit\bin\gcc.exe")) { throw "w64devkit did not unpack" }
    Remove-Item $archive -Force
  } catch {
    Say "no race toolchain: $($_.Exception.Message) - the suite will still run without -race"
  }
}

# --- PATH -----------------------------------------------------------------
$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
foreach ($dir in @("C:\go\bin", "C:\w64devkit\bin", "$env:USERPROFILE\go\bin")) {
  if ($machinePath -notlike "*$dir*") { $machinePath = "$machinePath;$dir" }
}
[Environment]::SetEnvironmentVariable("Path", $machinePath, "Machine")
[Environment]::SetEnvironmentVariable("GOTOOLCHAIN", "local", "Machine")

# --- the interactive runners ----------------------------------------------
# Anything ssh starts lands in a non-interactive window station: no desktop to
# capture, no notification area for a tray icon to register in. Tasks bound to
# the logged-on session are how a command crosses back into one that has both.
# Two of them, differing only in integrity - see register-task.ps1 for why the
# suite must not run elevated.
& "$root\register-task.ps1"
Say "registered the interactive task pair"

# The password came in only to be written into Winlogon; it has no reason to
# stay on the disk the snapshot is about to capture.
Remove-Item $PassFile -Force -ErrorAction SilentlyContinue

Restart-Service sshd
Say "done - reboot for autologon to take effect, then snapshot"
