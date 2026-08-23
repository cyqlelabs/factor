# Registers the one bridge the harness needs. The task runs in the logged-on
# user's session at high integrity, which answers both problems at once:
# guestcontrol and ssh hand out a UAC-filtered token with Administrators
# marked "deny only", and neither has a desktop behind it. A task registered
# Interactive + Highest has both.
$action = New-ScheduledTaskAction -Execute "powershell.exe" `
  -Argument "-NoProfile -ExecutionPolicy Bypass -File C:\factor-test\interactive.ps1"
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -ExecutionTimeLimit (New-TimeSpan -Hours 3) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName FactorInteractive -Action $action -Principal $principal `
  -Settings $settings -Force | Out-Null
"registered for $env:USERNAME" | Set-Content C:\factor-test\elevate.done
