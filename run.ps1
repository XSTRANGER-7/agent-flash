<#
.SYNOPSIS
  One-command runner for Agent Arena on Windows: checks setup, builds, runs the
  read-only doctor, starts the server, and (optionally) a Cloudflare quick tunnel
  and the Arkiv sidecar. Ctrl+C stops everything it started.

.EXAMPLE
  .\run.ps1                      # paper money (real Flash quotes), http://127.0.0.1:8080
  .\run.ps1 -Tunnel              # same + public https://....trycloudflare.com URL
  .\run.ps1 -Mode dry            # sign only, never submit
  .\run.ps1 -Mode live           # REAL money (asks you to type LIVE first)
  .\run.ps1 -Tunnel -Detach      # leave it running in the background
  .\run.ps1 -Stop                # stop what -Detach started
#>
param(
  [ValidateSet("paper", "dry", "live")] [string]$Mode = "paper",
  [int]$Port = 8080,
  [switch]$Tunnel,
  [switch]$Arkiv,
  [switch]$NoDoctor,
  [switch]$SkipBuild,
  [switch]$Detach,
  [switch]$Stop,
  [switch]$ResetPaper   # start paper money over (old paper data goes to a backup folder)
)
$ErrorActionPreference = "Stop"
Set-Location -Path $PSScriptRoot
$logs = Join-Path $PSScriptRoot "logs"
New-Item -ItemType Directory -Force -Path $logs | Out-Null
$pidFile = Join-Path $logs "run.pids"
$exe = Join-Path $PSScriptRoot "bin\arena.exe"

function Say($msg, $color = "Gray") { Write-Host $msg -ForegroundColor $color }

function Stop-Started {
  if (-not (Test-Path $pidFile)) { return }
  foreach ($line in Get-Content $pidFile) {
    $id = 0
    if ([int]::TryParse($line, [ref]$id)) {
      try { Stop-Process -Id $id -Force -ErrorAction Stop; Say "stopped PID $id" } catch {}
    }
  }
  Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
}

if ($Stop) { Stop-Started; exit 0 }

# --- 1. Go ---------------------------------------------------------------------
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  $goBin = Join-Path $env:LOCALAPPDATA "Programs\go\bin"
  if (Test-Path (Join-Path $goBin "go.exe")) { $env:Path += ";$goBin" }
}
if (-not $SkipBuild -and -not (Get-Command go -ErrorAction SilentlyContinue)) {
  Say "Go is not installed. Get Go 1.24+ from https://go.dev/dl/ (or run with -SkipBuild if bin\arena.exe exists)." Red
  exit 1
}

# --- 2. .env (values are never printed) -----------------------------------------
if (-not (Test-Path ".env")) {
  Copy-Item ".env.example" ".env"
  Say "Created .env from .env.example. Put your Flash key in it (FLASH_API_KEY=dpka_...) and run again." Yellow
  exit 1
}
$envText = Get-Content ".env"
function EnvValue($name) {
  $line = $envText | Where-Object { $_ -match "^\s*$name\s*=" } | Select-Object -First 1
  if (-not $line) { return "" }
  return ($line -replace "^\s*$name\s*=\s*", "").Trim().Trim('"').Trim("'")
}
$key = EnvValue "FLASH_API_KEY"
if (-not $key.StartsWith("dpka_") -or $key -eq "dpka_replace_me") {
  Say "FLASH_API_KEY in .env is missing or still the placeholder. Add your own dpka_... key and run again." Red
  exit 1
}
$admin = EnvValue "ADMIN_TOKEN"
if ($admin.Length -lt 16 -or $admin -eq "replace_with_long_random_token") {
  $bytes = New-Object byte[] 24
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  $tok = -join ($bytes | ForEach-Object { $_.ToString("x2") })
  $new = $envText | ForEach-Object { if ($_ -match "^\s*ADMIN_TOKEN\s*=") { "ADMIN_TOKEN=$tok" } else { $_ } }
  if (-not ($envText -match "^\s*ADMIN_TOKEN\s*=")) { $new += "ADMIN_TOKEN=$tok" }
  [IO.File]::WriteAllLines((Join-Path $PSScriptRoot ".env"), $new, (New-Object Text.UTF8Encoding($false)))
  Say "Generated a new ADMIN_TOKEN in .env (open .env to copy it for the Operator sign-in)." Yellow
}

# --- 3. Build ------------------------------------------------------------------
if (-not $SkipBuild) {
  $running = Get-Process -Name arena -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }
  if ($running) {
    Say "bin\arena.exe is already running (PID $($running.Id -join ', ')). Stop it first (.\run.ps1 -Stop, or close its window), or use -SkipBuild." Red
    exit 1
  }
  Say "Building..." Cyan
  & go build -trimpath -o $exe ./cmd/arena
  if ($LASTEXITCODE -ne 0) { Say "Build failed." Red; exit 1 }
}
if (-not (Test-Path $exe)) { Say "bin\arena.exe not found; run without -SkipBuild." Red; exit 1 }
if ($ResetPaper) {
  Stop-Started
  & $exe reset-paper
  if ($LASTEXITCODE -ne 0) { Say "Could not reset paper data (is another arena still running?)." Red; exit 1 }
}

# --- 4. Wallets ----------------------------------------------------------------
if (-not (Test-Path "wallets.json")) {
  Say "No wallets.json: creating sub-wallets (3 agents + follower1, follower2)..." Cyan
  & $exe keygen -followers follower1,follower2
  if ($LASTEXITCODE -ne 0) { Say "keygen failed." Red; exit 1 }
  # Windows has no chmod: limit the key file to the current user.
  & icacls "wallets.json" /inheritance:r /grant:r "$($env:USERNAME):(R,W)" | Out-Null
  Say "wallets.json holds private keys. Back it up somewhere safe; never commit or share it." Yellow
}

# --- 5. Mode guard + doctor ----------------------------------------------------
$oldMode, $oldAddr = $env:MODE, $env:HTTP_ADDR
$env:MODE = $Mode
$env:HTTP_ADDR = "127.0.0.1:$Port"
if ($Mode -eq "live") {
  Say ""
  Say "LIVE MODE: real orders with real money on Base." Red
  Say ("  Agents auto-trade real money: " + $(if ((EnvValue "LIVE_AGENTS") -eq "true") { "YES (LIVE_AGENTS=true)" } else { "no (only your test trades)" })) Yellow
  Say ("  Total live budget: `$" + $(if (EnvValue "LIVE_BUDGET_USD") { EnvValue "LIVE_BUDGET_USD" } else { "5" }) + " (LIVE_BUDGET_USD)") Yellow
  $answer = Read-Host "Type LIVE to continue"
  if ($answer -ne "LIVE") { Say "Cancelled." ; $env:MODE, $env:HTTP_ADDR = $oldMode, $oldAddr; exit 1 }
}
if (-not $NoDoctor) {
  Say "Running doctor (read-only checks)..." Cyan
  & $exe doctor
  if ($LASTEXITCODE -ne 0) { Say "Doctor found a required problem (see above). Fix it, or run with -NoDoctor." Red; exit 1 }
}

# --- 6. Start ------------------------------------------------------------------
Stop-Started
$started = @()
$srv = Start-Process -FilePath $exe -ArgumentList "serve" -WorkingDirectory $PSScriptRoot -WindowStyle Hidden -PassThru `
  -RedirectStandardOutput (Join-Path $logs "arena.log") -RedirectStandardError (Join-Path $logs "arena.err.log")
$started += $srv.Id
$env:MODE, $env:HTTP_ADDR = $oldMode, $oldAddr
$local = "http://127.0.0.1:$Port"
$up = $false
for ($i = 0; $i -lt 60; $i++) {
  Start-Sleep -Seconds 1
  if ($srv.HasExited) { break }
  try { if ((Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$local/healthz").StatusCode -eq 200) { $up = $true; break } } catch {}
}
if (-not $up) {
  Say "Server did not start. Last log lines:" Red
  Get-Content (Join-Path $logs "arena.err.log"), (Join-Path $logs "arena.log") -Tail 15 -ErrorAction SilentlyContinue
  try { Stop-Process -Id $srv.Id -Force } catch {}
  exit 1
}

$public = ""
if ($Tunnel) {
  $cf = (Get-Command cloudflared -ErrorAction SilentlyContinue).Source
  foreach ($p in @("C:\Program Files (x86)\cloudflared\cloudflared.exe", "C:\Program Files\cloudflared\cloudflared.exe")) {
    if (-not $cf -and (Test-Path $p)) { $cf = $p }
  }
  if (-not $cf) {
    Say "cloudflared not found. Install it: winget install --id Cloudflare.cloudflared   (then run again with -Tunnel)" Yellow
  } else {
    $tlog = Join-Path $logs "tunnel.log"
    Remove-Item $tlog -ErrorAction SilentlyContinue
    # http2 (not QUIC): works on more networks. Quick tunnels buffer SSE, the app also polls.
    $tun = Start-Process -FilePath $cf -ArgumentList "tunnel --no-autoupdate --protocol http2 --url $local" -WindowStyle Hidden -PassThru `
      -RedirectStandardOutput (Join-Path $logs "tunnel.out.log") -RedirectStandardError $tlog
    $started += $tun.Id
    for ($i = 0; $i -lt 60 -and -not $public; $i++) {
      Start-Sleep -Seconds 1
      $m = Select-String -Path $tlog -Pattern "https://[a-z0-9-]+\.trycloudflare\.com" -ErrorAction SilentlyContinue | Select-Object -First 1
      if ($m) { $public = $m.Matches[0].Value }
    }
    if (-not $public) { Say "Tunnel started but no URL yet; see logs\tunnel.log" Yellow }
  }
}

if ($Arkiv) {
  if (-not (Get-Command npm -ErrorAction SilentlyContinue)) {
    Say "Node.js (npm) not found; skipping the Arkiv sidecar. Install Node 22+." Yellow
  } else {
    if (-not (Test-Path "arkiv-sync\node_modules")) { & npm --prefix arkiv-sync install }
    & npm --prefix arkiv-sync run build
    $ark = Start-Process -FilePath "cmd.exe" -ArgumentList "/c npm start" -WorkingDirectory (Join-Path $PSScriptRoot "arkiv-sync") -WindowStyle Hidden -PassThru `
      -RedirectStandardOutput (Join-Path $logs "arkiv.log") -RedirectStandardError (Join-Path $logs "arkiv.err.log")
    $started += $ark.Id
    if (-not (EnvValue "ARKIV_SYNC_URL")) { Say "Set ARKIV_SYNC_URL=http://127.0.0.1:8787 in .env so the server sends events to the sidecar." Yellow }
  }
}
$started | Set-Content $pidFile

Say ""
Say "Agent Arena is running ($Mode mode)" Green
Say "  App:     $local"
if ($public) { Say "  Public:  $public   (new URL every start)" Green }
Say "  Sign in: Operator page, with ADMIN_TOKEN from .env"
Say "  Logs:    logs\arena.log$(if ($Tunnel) { ', logs\tunnel.log' })"
if ($Detach) { Say "Running in the background. Stop with: .\run.ps1 -Stop"; exit 0 }
Say "Showing warnings and errors. Press Ctrl+C to stop everything." Cyan
try {
  Get-Content (Join-Path $logs "arena.log") -Wait -Tail 0 | Where-Object { $_ -match '"level":"(WARN|ERROR)"' -and $_ -notmatch 'outbox delivery failed' }
} finally {
  Stop-Started
  Say "Stopped." Green
}
