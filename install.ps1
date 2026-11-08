# oxyp installer for Windows: one paste, zero thinking.
#   irm https://raw.githubusercontent.com/lirrensi/oxyp/main/install.ps1 | iex
# Piped args are NOT supported via iex; for options download first:
#   Invoke-WebRequest -OutFile install.ps1 <url above>
#   .\install.ps1 -Go -Version v0.3.0 -Dir C:\tools
param(
  [switch]$Go,            # build from source via go install (needs Go 1.21+)
  [string]$Version = "latest",
  [string]$Dir = ""       # install dir (default: %LOCALAPPDATA%\oxyp)
)
$ErrorActionPreference = "Stop"

$Repo = "lirrensi/oxyp"
$Pkg = "github.com/lirrensi/oxyp/cmd/oxyp"

function Die($msg) { Write-Error $msg; exit 1 }

# 1. install dir: default %LOCALAPPDATA%\oxyp, always user-writable, no admin
if ([string]::IsNullOrWhiteSpace($Dir)) {
  $Dir = Join-Path $env:LOCALAPPDATA "oxyp"
}
New-Item -ItemType Directory -Force -Path $Dir | Out-Null
$exe = Join-Path $Dir "oxyp.exe"

# 2. fetch or build
if ($Go) {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Die "go not found — drop -Go or install Go 1.21+"
  }
  Write-Host "building oxyp@$Version via go install -> $Dir ..."
  $env:GOBIN = $Dir
  & go install "$Pkg@$Version"
  if ($LASTEXITCODE -ne 0) { Die "go install failed" }
  # go install names the exe after the package dir (oxyp.exe) — verify
  if (-not (Test-Path $exe)) { Die "go install succeeded but $exe missing" }
} else {
  $arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { Die "32-bit Windows is not supported — use -Go (needs Go 1.21+)" }
  if ($Version -eq "latest") {
    $url = "https://github.com/$Repo/releases/latest/download/oxyp-windows-$arch.exe"
  } else {
    $url = "https://github.com/$Repo/releases/download/$Version/oxyp-windows-$arch.exe"
  }
  Write-Host "downloading oxyp $Version (windows/$arch) -> $Dir ..."
  try {
    Invoke-WebRequest -Uri $url -OutFile $exe -UseBasicParsing
  } catch {
    Die "download failed: $url ($($_.Exception.Message))"
  }
}

# 3. user PATH: registry (HKCU, no admin), deduped, current session updated too
$canon = {
  param($p)
  try { (Resolve-Path $p -ErrorAction Stop).Path.ToLowerInvariant() }
  catch { $p.ToLowerInvariant() }
}
$want = &$canon $Dir
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$parts = @()
if ($userPath) { $parts = @($userPath -split ";" | Where-Object { $_ -ne "" }) }
$already = @($parts | Where-Object { (&$canon $_) -eq $want }).Count -gt 0
if (-not $already) {
  [Environment]::SetEnvironmentVariable("Path", (($parts + $Dir) -join ";"), "User")
  Write-Host "added $Dir to user PATH (new shells pick it up automatically)"
}
if (@($env:Path -split ";" | Where-Object { (&$canon $_) -eq $want }).Count -eq 0) {
  $env:Path = "$Dir;$env:Path"
}

# 4. verify
try {
  & $exe --help | Out-Null
} catch {
  Die "$exe failed to run ($($_.Exception.Message))"
}
if (Get-Command oxyp -ErrorAction SilentlyContinue) {
  Write-Host "done: $( (Get-Command oxyp).Source ) is ready — try: oxyp servers"
} else {
  Write-Host "done: $exe installed and runs — restart your shell, then: oxyp servers"
}
