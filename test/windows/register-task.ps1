# Registers the two bridges the harness needs, both running in the logged-on
# user's session so what runs has a real window station: a desktop to
# screenshot, a notification area for a tray icon, a cursor to move.
#
# They differ only in integrity level, and that difference matters.
# FactorSetup runs elevated because provisioning writes to HKLM, installs
# services and adds firewall rules, none of which guestcontrol can do - it
# hands out a UAC-filtered token with Administrators marked deny only.
# FactorTests deliberately does not: Chrome refuses to be driven elevated on
# Windows and silently relaunches itself de-elevated, so the process chromedp
# is watching exits and every browser test fails with an empty "chrome failed
# to start". Running the suite as an administrator was also never what a user
# does, so the unprivileged task is the honest one to test through.
$common = @{
  Action   = New-ScheduledTaskAction -Execute "powershell.exe" `
               -Argument "-NoProfile -ExecutionPolicy Bypass -File C:\factor-test\interactive.ps1"
  Settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
               -ExecutionTimeLimit (New-TimeSpan -Hours 3) -MultipleInstances IgnoreNew
}

foreach ($t in @(
  @{ Name = "FactorSetup"; Level = "Highest" },
  @{ Name = "FactorTests"; Level = "Limited" }
)) {
  $principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel $t.Level
  Register-ScheduledTask -TaskName $t.Name -Action $common.Action -Principal $principal `
    -Settings $common.Settings -Force | Out-Null
}

# The original name, kept registered so an older checkout still works.
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Highest
Register-ScheduledTask -TaskName "FactorInteractive" -Action $common.Action -Principal $principal `
  -Settings $common.Settings -Force | Out-Null

"registered FactorSetup, FactorTests and FactorInteractive for $env:USERNAME" | Set-Content C:\factor-test\elevate.done
