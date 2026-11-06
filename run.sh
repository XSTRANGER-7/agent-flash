#!/usr/bin/env bash
# One-command runner for Agent Arena (Linux / macOS / Git Bash): checks setup,
# builds, runs the read-only doctor, starts the server and, optionally, a
# Cloudflare quick tunnel and the Arkiv sidecar. Ctrl+C stops everything.
#
#   ./run.sh                    paper money (real Flash quotes) on http://127.0.0.1:8080
#   ./run.sh --tunnel           same + public https://....trycloudflare.com URL
#   ./run.sh dry                sign only, never submit
#   ./run.sh live               REAL money (asks you to type LIVE first)
#   ./run.sh paper --port 9090 --arkiv --no-doctor
set -euo pipefail
cd "$(dirname "$0")"

MODE=paper PORT=8080 TUNNEL=0 ARKIV=0 DOCTOR=1 RESET=0
while [ $# -gt 0 ]; do
  case "$1" in
    paper|dry|live) MODE=$1 ;;
    --tunnel) TUNNEL=1 ;;
    --arkiv) ARKIV=1 ;;
    --no-doctor) DOCTOR=0 ;;
    --reset-paper) RESET=1 ;;
    --port) PORT=$2; shift ;;
    -h|--help) sed -n 2,10p "$0"; exit 0 ;;
    *) echo "unknown option: $1 (see --help)"; exit 2 ;;
  esac
  shift
done

mkdir -p logs bin
EXE=./bin/arena
[ -f bin/arena.exe ] && [ "${OS:-}" = "Windows_NT" ] && EXE=./bin/arena.exe

envval() { grep -E "^[[:space:]]*$1[[:space:]]*=" .env 2>/dev/null | head -1 | sed -E "s/^[^=]*=[[:space:]]*//; s/^[\"']//; s/[\"'][[:space:]]*$//" | tr -d '\r' || true; }

command -v go >/dev/null || { echo "Go 1.24+ is required: https://go.dev/dl/"; exit 1; }
if [ ! -f .env ]; then
  cp .env.example .env
  echo "Created .env from .env.example. Put your Flash key in it (FLASH_API_KEY=dpka_...) and run again."
  exit 1
fi
KEY=$(envval FLASH_API_KEY)
case "$KEY" in dpka_replace_me|"") echo "FLASH_API_KEY in .env is missing or the placeholder."; exit 1 ;; dpka_*) ;; *) echo "FLASH_API_KEY should start with dpka_"; exit 1 ;; esac
ADMIN=$(envval ADMIN_TOKEN)
if [ ${#ADMIN} -lt 16 ] || [ "$ADMIN" = replace_with_long_random_token ]; then
  TOK=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
  if grep -qE '^[[:space:]]*ADMIN_TOKEN[[:space:]]*=' .env; then
    sed -i.bak -E "s/^[[:space:]]*ADMIN_TOKEN[[:space:]]*=.*/ADMIN_TOKEN=$TOK/" .env && rm -f .env.bak
  else
    echo "ADMIN_TOKEN=$TOK" >> .env
  fi
  echo "Generated a new ADMIN_TOKEN in .env (open .env to copy it for the Operator sign-in)."
fi

echo "Building..."
go build -trimpath -o "$EXE" ./cmd/arena
[ $RESET = 1 ] && "$EXE" reset-paper

if [ ! -f wallets.json ]; then
  echo "No wallets.json: creating sub-wallets (3 agents + follower1, follower2)..."
  "$EXE" keygen -followers follower1,follower2
  chmod 600 wallets.json 2>/dev/null || true
  echo "wallets.json holds private keys. Back it up; never commit or share it."
fi

export MODE HTTP_ADDR="127.0.0.1:$PORT"
if [ "$MODE" = live ]; then
  echo
  echo "LIVE MODE: real orders with real money on Base."
  echo "  Agents auto-trade real money: $( [ "$(envval LIVE_AGENTS)" = true ] && echo 'YES (LIVE_AGENTS=true)' || echo 'no (only your test trades)')"
  echo "  Total live budget: \$$(envval LIVE_BUDGET_USD || true) (LIVE_BUDGET_USD, default 5)"
  read -r -p "Type LIVE to continue: " ans
  [ "$ans" = LIVE ] || { echo "Cancelled."; exit 1; }
fi
if [ $DOCTOR = 1 ]; then
  echo "Running doctor (read-only checks)..."
  "$EXE" doctor || { echo "Doctor found a required problem (see above). Fix it or use --no-doctor."; exit 1; }
fi

PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; echo "Stopped."; }
trap cleanup EXIT INT TERM

"$EXE" serve >logs/arena.log 2>logs/arena.err.log &
PIDS+=($!)
LOCAL="http://127.0.0.1:$PORT"
for _ in $(seq 1 60); do
  curl -fsS "$LOCAL/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$LOCAL/healthz" >/dev/null 2>&1 || { echo "Server did not start:"; tail -15 logs/arena.err.log logs/arena.log; exit 1; }

PUBLIC=""
if [ $TUNNEL = 1 ]; then
  if ! command -v cloudflared >/dev/null; then
    echo "cloudflared not found. Install: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
  else
    : >logs/tunnel.log
    cloudflared tunnel --no-autoupdate --protocol http2 --url "$LOCAL" >>logs/tunnel.log 2>&1 &
    PIDS+=($!)
    for _ in $(seq 1 60); do
      PUBLIC=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' logs/tunnel.log | head -1 || true)
      [ -n "$PUBLIC" ] && break
      sleep 1
    done
  fi
fi

if [ $ARKIV = 1 ]; then
  if command -v npm >/dev/null; then
    [ -d arkiv-sync/node_modules ] || npm --prefix arkiv-sync install
    npm --prefix arkiv-sync run build
    (cd arkiv-sync && npm start) >logs/arkiv.log 2>&1 &
    PIDS+=($!)
    [ -n "$(envval ARKIV_SYNC_URL)" ] || echo "Set ARKIV_SYNC_URL=http://127.0.0.1:8787 in .env so the server sends events to the sidecar."
  else
    echo "Node.js (npm) not found; skipping the Arkiv sidecar."
  fi
fi

echo
echo "Agent Arena is running ($MODE mode)"
echo "  App:     $LOCAL"
[ -n "$PUBLIC" ] && echo "  Public:  $PUBLIC   (new URL every start)"
echo "  Sign in: Operator page, with ADMIN_TOKEN from .env"
echo "  Logs:    logs/arena.log"
echo "Showing warnings and errors. Press Ctrl+C to stop everything."
tail -n 0 -F logs/arena.log | grep --line-buffered -E '"level":"(WARN|ERROR)"' | grep --line-buffered -v 'outbox delivery failed' || true
