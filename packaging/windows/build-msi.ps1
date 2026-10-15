# Builds the Factor MSI around a factor.exe that is already built. CI's
# installer gate and the release job both run this, so what the gate builds
# is what ships.
param(
  [Parameter(Mandatory)] [string] $Exe,
  # The release tag, e.g. v0.53.0. An MSI version is numeric only, so a
  # suffix git describe adds is dropped.
  [Parameter(Mandatory)] [string] $Version,
  [Parameter(Mandatory)] [string] $Out
)
$ErrorActionPreference = "Stop"

# Pinned like svu: a new WiX release must not change what ships unnoticed,
# so a wix already on PATH is used only when it is this version.
$wix = "5.0.2"
if (-not ((Get-Command wix -ErrorAction SilentlyContinue) -and ((wix --version) -like "$wix*"))) {
  $tools = Join-Path ([IO.Path]::GetTempPath()) "factor-wix-$wix"
  if (-not (Test-Path "$tools\wix.exe")) {
    dotnet tool install --tool-path $tools wix --version $wix
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
  }
  $env:PATH = "$tools;$env:PATH"
}
wix extension add -g "WixToolset.UI.wixext/$wix" "WixToolset.Util.wixext/$wix"
if ($LASTEXITCODE) { exit $LASTEXITCODE }

$msiVersion = $Version.TrimStart("v") -replace "-.*$", ""
wix build "$PSScriptRoot\factor.wxs" -arch x64 -d "Version=$msiVersion" -d "Exe=$((Resolve-Path $Exe).Path)" `
  -ext "WixToolset.UI.wixext/$wix" -ext "WixToolset.Util.wixext/$wix" -o $Out
exit $LASTEXITCODE
