#!/usr/bin/env bash
# Build the Docker image here and ship it to an EC2 box over SSH. No registry needed.
#
#   ./deploy.sh                 build, upload, swap the container, health-check
#   ./deploy.sh --setup         first time only: install Docker on the EC2 box
#   ./deploy.sh --rollback      go back to the previous image
#   ./deploy.sh --status        show what is running (container, image, health)
#   ./deploy.sh --logs          follow the container logs
#   ./deploy.sh --dry-run       print the plan and run every check, change nothing
#
# Options: --skip-tests (build without `go test`), --push-env (overwrite the server's .env
#          with your local one), --yes (no prompt), -h/--help.
#
# Config: deploy.env (copy deploy.env.example) or environment variables. Needs bash,
# docker, ssh (Linux, macOS, WSL or Git Bash on Windows).
#
# What it guarantees:
#   * Secrets never go in the image. The server keeps /opt/arena/.env, wallets.json and
#     the data volume. They are uploaded only if missing; wallets.json is NEVER
#     overwritten (it holds private keys); .env only with --push-env (a backup is kept).
#   * The new image is uploaded and verified BEFORE the old container is touched.
#   * The old container is stopped (graceful SIGTERM), the new one started and checked
#     against /readyz. If it is not healthy in time, the previous version is started
#     again automatically. The data volume is shared, so there is a short gap but never
#     two writers on the same files.
set -euo pipefail
cd "$(dirname "$0")"

# ---------- config ----------
[ -f deploy.env ] && { set -a; . ./deploy.env; set +a; }
EC2_HOST="${EC2_HOST:-}"                  # ip or dns name (required)
EC2_USER="${EC2_USER:-ec2-user}"          # ec2-user (Amazon Linux) | ubuntu (Ubuntu)
EC2_PORT="${EC2_PORT:-22}"
SSH_KEY="${SSH_KEY:-}"                    # path to the .pem; empty = ssh agent / default keys
REMOTE_DIR="${REMOTE_DIR:-/opt/arena}"    # holds .env and wallets.json on the server
APP_NAME="${APP_NAME:-arena}"             # container name + image repository
HOST_PORT="${HOST_PORT:-8080}"            # port opened on the server
BIND_ADDR="${BIND_ADDR:-0.0.0.0}"         # 127.0.0.1 = only reachable via reverse proxy / ssh tunnel
PLATFORM="${PLATFORM:-}"                  # linux/amd64 | linux/arm64 ; empty = detect from the server
HEALTH_PATH="${HEALTH_PATH:-/readyz}"     # 200 only once the price feed is fresh
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-120}"   # seconds to wait for a healthy new container
STOP_TIMEOUT="${STOP_TIMEOUT:-30}"        # seconds the old container gets to shut down
KEEP_IMAGES="${KEEP_IMAGES:-3}"           # old images kept on the server for rollback
DATA_VOLUME="${DATA_VOLUME:-${APP_NAME}-data}"

ACTION=deploy SKIP_TESTS=0 PUSH_ENV=0 YES=0 DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --setup) ACTION=setup ;;
    --rollback) ACTION=rollback ;;
    --status) ACTION=status ;;
    --logs) ACTION=logs ;;
    --dry-run) DRY=1 ;;
    --skip-tests) SKIP_TESTS=1 ;;
    --push-env) PUSH_ENV=1 ;;
    -y|--yes) YES=1 ;;
    -h|--help) sed -n 2,26p "$0"; exit 0 ;;
    *) echo "unknown option: $1 (see --help)"; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33m!!  %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

[ -n "$EC2_HOST" ] || die "EC2_HOST is not set. Copy deploy.env.example to deploy.env and fill it in."
case "$APP_NAME$REMOTE_DIR$DATA_VOLUME" in *[!A-Za-z0-9_./-]*) die "APP_NAME, REMOTE_DIR and DATA_VOLUME may only contain letters, digits and _ . / -" ;; esac
case "$REMOTE_DIR" in /*) ;; *) die "REMOTE_DIR must be an absolute path" ;; esac
case "$REMOTE_DIR" in /|/etc|/usr|/bin|/home|/root|/var) die "REMOTE_DIR=$REMOTE_DIR is not a safe place for app files" ;; esac

# ---------- ssh ----------
SSH_OPTS=(-p "$EC2_PORT" -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=15
          -o ServerAliveCountMax=8 -o StrictHostKeyChecking=accept-new)
SCP_OPTS=(-P "$EC2_PORT" -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new)
if [ -n "$SSH_KEY" ]; then
  [ -f "$SSH_KEY" ] || die "SSH_KEY not found: $SSH_KEY"
  SSH_OPTS+=(-i "$SSH_KEY" -o IdentitiesOnly=yes); SCP_OPTS+=(-i "$SSH_KEY" -o IdentitiesOnly=yes)
fi
TARGET="$EC2_USER@$EC2_HOST"
rssh() { ssh "${SSH_OPTS[@]}" "$TARGET" "$@"; }
rscp() { scp -q "${SCP_OPTS[@]}" "$@"; }

command -v ssh >/dev/null || die "ssh not found"
say "Checking SSH to $TARGET"
rssh 'echo ok' >/dev/null 2>&1 || die "cannot ssh to $TARGET (key, user, security group port $EC2_PORT?). Try: ssh ${SSH_KEY:+-i $SSH_KEY }-p $EC2_PORT $TARGET"

# docker may need sudo on the server until the user is in the docker group (after --setup + new login)
if rssh 'docker info >/dev/null 2>&1'; then RD="docker"
elif rssh 'sudo -n docker info >/dev/null 2>&1'; then RD="sudo -n docker"
else RD=""; fi

# ---------- setup ----------
if [ "$ACTION" = setup ]; then
  say "Installing Docker on $TARGET"
  [ "$DRY" = 1 ] && { echo "(dry run) would install docker and create $REMOTE_DIR"; exit 0; }
  rssh "REMOTE_DIR='$REMOTE_DIR' bash -s" <<'EOF'
set -eu
if ! command -v docker >/dev/null; then
  if command -v dnf >/dev/null; then sudo dnf install -y docker
  elif command -v yum >/dev/null; then sudo yum install -y docker
  elif command -v apt-get >/dev/null; then sudo apt-get update -y && sudo apt-get install -y docker.io curl
  else echo "unsupported distro: install Docker manually" >&2; exit 1; fi
fi
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER" || true
command -v curl >/dev/null || { sudo dnf install -y curl 2>/dev/null || sudo yum install -y curl 2>/dev/null || sudo apt-get install -y curl; }
sudo mkdir -p "$REMOTE_DIR" && sudo chown "$USER" "$REMOTE_DIR" && chmod 700 "$REMOTE_DIR"
sudo docker --version
EOF
  say "Done. Run ./deploy.sh next (it uploads your .env and wallets.json the first time)."
  exit 0
fi

[ -n "$RD" ] || die "Docker is not usable on the server. Run: ./deploy.sh --setup"

# ---------- remote ops that need no build ----------
remote_script_header="set -eu; RD='$RD'; APP='$APP_NAME'; DIR='$REMOTE_DIR'; PORT='$HOST_PORT'; BIND='$BIND_ADDR'; VOL='$DATA_VOLUME'; HP='$HEALTH_PATH'; HT='$HEALTH_TIMEOUT'; ST='$STOP_TIMEOUT'; KEEP='$KEEP_IMAGES'"

case "$ACTION" in
  status)
    rssh "$remote_script_header"'
      $RD ps -a --filter "name=^${APP}\$" --format "container: {{.Names}}  {{.Status}}  image: {{.Image}}"
      $RD images "$APP" --format "image: {{.Repository}}:{{.Tag}}  {{.CreatedSince}}  {{.Size}}" | head -8
      echo -n "health ($HP): "; curl -s -m 5 -o /dev/null -w "%{http_code}\n" "http://127.0.0.1:${PORT}${HP}" || echo unreachable'
    exit 0 ;;
  logs)
    exec ssh -t "${SSH_OPTS[@]}" "$TARGET" "$RD logs -f --tail 100 $APP" ;;
esac

# Everything that swaps the container lives in one remote script, used by deploy and rollback.
# $1 = image ref to run. It keeps the previous image tag in $APP:previous so --rollback works.
REMOTE_SWAP='
  mkdir -p "$DIR"
  [ -f "$DIR/.env" ] || { echo "missing $DIR/.env on the server" >&2; exit 1; }
  [ -f "$DIR/wallets.json" ] || { echo "missing $DIR/wallets.json on the server" >&2; exit 1; }
  NEW="$1"
  $RD image inspect "$NEW" >/dev/null || { echo "image $NEW is not on the server" >&2; exit 1; }
  # one deploy at a time
  LOCK="$DIR/.deploy.lock"
  if ! mkdir "$LOCK" 2>/dev/null; then
    if [ -n "$(find "$LOCK" -maxdepth 0 -mmin +30 2>/dev/null)" ]; then rmdir "$LOCK"; mkdir "$LOCK"
    else echo "another deploy is running ($LOCK). Remove it if that is stale." >&2; exit 1; fi
  fi
  trap "rmdir \"$LOCK\" 2>/dev/null || true" EXIT

  # the app reads MODE from the real environment first; the image defaults it to paper, so pass .env value through
  MODE_ENV=$(grep -E "^[[:space:]]*MODE[[:space:]]*=" "$DIR/.env" | head -1 | sed -E "s/^[^=]*=[[:space:]]*//; s/[[:space:]]+#.*$//" | tr -d "\r\042\047" || true)
  MODE_ARG=""; [ -n "$MODE_ENV" ] && MODE_ARG="-e MODE=$MODE_ENV"

  OLD_IMG=$($RD inspect -f "{{.Image}}" "$APP" 2>/dev/null || true)       # image id of the running container
  OLD_REF=$($RD inspect -f "{{.Config.Image}}" "$APP" 2>/dev/null || true)

  run_new() {  # $1 = image ref
    # .env is mounted (the app parses quotes/comments itself); these four must win over what is in it.
    # wallets.json is mounted read-write: every new account, sub-wallet and plan adds a key to it
    # (written in place, so the file keeps owner 65532 and mode 600). A read-only mount makes
    # "Create my account" fail.
    $RD run -d --name "$APP" --restart unless-stopped \
      -p "${BIND}:${PORT}:8080" \
      -e HTTP_ADDR=:8080 -e DATA_DIR=/app/data -e WALLETS_FILE=/app/secrets/wallets.json $MODE_ARG \
      -v "$DIR/.env:/app/.env:ro" \
      -v "$DIR/wallets.json:/app/secrets/wallets.json" \
      -v "$VOL:/app/data" \
      --log-driver json-file --log-opt max-size=20m --log-opt max-file=5 \
      "$1" >/dev/null
  }
  wait_healthy() {
    i=0
    while [ "$i" -lt "$HT" ]; do
      st=$($RD inspect -f "{{.State.Status}}" "$APP" 2>/dev/null || echo gone)
      [ "$st" = running ] || { echo "container is $st" >&2; return 1; }
      code=$(curl -s -m 3 -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}${HP}" || true)
      [ "$code" = 200 ] && return 0
      i=$((i+2)); sleep 2
    done
    echo "not healthy after ${HT}s (last HTTP status: ${code:-none})" >&2; return 1
  }

  if [ -n "$OLD_REF" ] && [ "$OLD_REF" != "$NEW" ]; then $RD tag "$OLD_IMG" "$APP:previous"; fi
  echo "stopping old container (up to ${ST}s for a graceful shutdown)"
  $RD stop -t "$ST" "$APP" >/dev/null 2>&1 || true
  $RD rm -f "$APP" >/dev/null 2>&1 || true

  echo "starting $NEW"
  if run_new "$NEW" && wait_healthy; then
    echo "healthy"
    $RD tag "$NEW" "$APP:current"
    # keep the newest KEEP tagged images, never delete current/previous or anything in use
    $RD images "$APP" --format "{{.Tag}} {{.ID}}" | grep -vE "^(latest|current|previous) " | tail -n +$((KEEP+1)) | while read -r t id; do
      $RD rmi "$APP:$t" >/dev/null 2>&1 || true
    done
    $RD image prune -f >/dev/null 2>&1 || true
    exit 0
  fi

  echo "--- new container logs ---" >&2; $RD logs --tail 40 "$APP" >&2 || true
  $RD rm -f "$APP" >/dev/null 2>&1 || true
  if [ -n "$OLD_IMG" ]; then
    echo "ROLLING BACK to the previous image" >&2
    if run_new "$OLD_IMG" && wait_healthy; then echo "previous version is running again" >&2; exit 3; fi
    echo "previous version did not come up either; see: docker logs $APP" >&2; exit 4
  fi
  echo "no previous version to roll back to" >&2; exit 1
'

swap_remote() { rssh "bash -c $(printf '%q' "$remote_script_header; $REMOTE_SWAP") _ '$1'"; }

if [ "$ACTION" = rollback ]; then
  rssh "$RD image inspect '$APP:previous' >/dev/null 2>&1" || die "no previous image on the server"
  say "Rolling back to $APP:previous"
  [ "$DRY" = 1 ] && { echo "(dry run) would run $APP:previous"; exit 0; }
  swap_remote "$APP:previous"; say "Rolled back."; exit 0
fi

# ---------- deploy: local checks ----------
command -v docker >/dev/null || die "docker not found locally"
docker info >/dev/null 2>&1 || die "the local Docker daemon is not running (start Docker Desktop)"

if ! git diff --quiet HEAD -- 2>/dev/null || [ -n "$(git ls-files --others --exclude-standard 2>/dev/null)" ]; then
  warn "working tree has uncommitted changes; they ARE included in the image"
  DIRTY="-dirty"
else DIRTY=""; fi
SHA=$(git rev-parse --short HEAD 2>/dev/null || echo nogit)
TAG="${SHA}${DIRTY}-$(date -u +%Y%m%d%H%M%S)"
IMAGE="$APP_NAME:$TAG"

if [ -z "$PLATFORM" ]; then
  case "$(rssh 'uname -m')" in
    x86_64|amd64) PLATFORM=linux/amd64 ;;
    aarch64|arm64) PLATFORM=linux/arm64 ;;
    *) die "unknown server architecture; set PLATFORM=linux/amd64 or linux/arm64" ;;
  esac
fi

# disk: the compressed upload is small but docker load needs the uncompressed layers
FREE_KB=$(rssh "(df -Pk /var/lib/docker 2>/dev/null || df -Pk /) | awk 'NR==2{print \$4}'" | tr -dc 0-9)
[ -n "$FREE_KB" ] && [ "$FREE_KB" -lt 1048576 ] && die "less than 1 GB free on the server (docker dir); free space first (docker system prune)"

# secrets: upload only what the server lacks
HAVE_ENV=0; HAVE_WAL=0
rssh "test -f '$REMOTE_DIR/.env'" && HAVE_ENV=1
rssh "test -f '$REMOTE_DIR/wallets.json'" && HAVE_WAL=1
if [ $HAVE_ENV = 0 ] || [ $PUSH_ENV = 1 ]; then [ -f .env ] || die ".env missing locally (copy .env.example and fill it in)"; fi
[ $HAVE_WAL = 1 ] || [ -f wallets.json ] || die "wallets.json missing locally and on the server (run: go run ./cmd/arena keygen)"
if [ $HAVE_ENV = 0 ] || [ $PUSH_ENV = 1 ]; then
  grep -qE '^[[:space:]]*FLASH_API_KEY=dpka_' .env && ! grep -q 'dpka_replace_me' .env || die ".env has no real FLASH_API_KEY"
  if grep -qE '^[[:space:]]*MODE[[:space:]]*=[[:space:]]*live' .env; then warn "local .env has MODE=live (real money)"; fi
fi

echo
echo "  server:    $TARGET:$EC2_PORT   ($PLATFORM)"
echo "  image:     $IMAGE"
echo "  container: $APP_NAME  ->  $BIND_ADDR:$HOST_PORT"
echo "  secrets:   .env $([ $HAVE_ENV = 1 ] && { [ $PUSH_ENV = 1 ] && echo 'on server, will be REPLACED (backup kept)' || echo 'on server, kept'; } || echo 'will be uploaded')," \
     "wallets.json $([ $HAVE_WAL = 1 ] && echo 'on server, kept' || echo 'will be uploaded')"
echo "  tests:     $([ $SKIP_TESTS = 1 ] && echo 'skipped' || echo 'run during the build (a failing test stops the deploy)')"
echo
if [ "$DRY" = 1 ]; then say "Dry run: all checks passed. Nothing was built or changed."; exit 0; fi
if [ "$YES" = 0 ] && [ -t 0 ]; then read -r -p "Deploy? [y/N] " a; [ "$a" = y ] || [ "$a" = Y ] || { echo Cancelled.; exit 1; }; fi

# ---------- build ----------
say "Building $IMAGE ($PLATFORM)"
BUILD_ARGS=(--platform "$PLATFORM" -t "$IMAGE")
[ $SKIP_TESTS = 1 ] && BUILD_ARGS+=(--build-arg RUN_TESTS=0)
docker build "${BUILD_ARGS[@]}" .

# ---------- ship ----------
say "Uploading secrets that the server lacks"
rssh "mkdir -p '$REMOTE_DIR' && chmod 700 '$REMOTE_DIR'"
if [ $HAVE_ENV = 0 ] || [ $PUSH_ENV = 1 ]; then
  [ $HAVE_ENV = 1 ] && rssh "cp -p '$REMOTE_DIR/.env' '$REMOTE_DIR/.env.bak.$(date -u +%Y%m%d%H%M%S)'"
  rscp .env "$TARGET:$REMOTE_DIR/.env.new" && rssh "chmod 644 '$REMOTE_DIR/.env.new' && mv '$REMOTE_DIR/.env.new' '$REMOTE_DIR/.env'"
fi
if [ $HAVE_WAL = 0 ]; then
  rscp wallets.json "$TARGET:$REMOTE_DIR/wallets.json.new" && rssh "sudo -n chown 65532:65532 '$REMOTE_DIR/wallets.json.new' && chmod 600 '$REMOTE_DIR/wallets.json.new' && mv -n '$REMOTE_DIR/wallets.json.new' '$REMOTE_DIR/wallets.json'"
  # The container runs as non-root uid 65532 and the app refuses a wallets.json that is not chmod 600, so that file is
  # owned by 65532 with mode 600. .env is 644 so the same uid can read it. REMOTE_DIR is 700: other users cannot reach either.
fi

say "Streaming the image to the server (docker save | gzip | ssh | docker load)"
if command -v pv >/dev/null; then SIZE=$(docker image inspect "$IMAGE" --format '{{.Size}}'); PIPE=(pv -s "$SIZE"); else PIPE=(cat); fi
docker save "$IMAGE" | "${PIPE[@]}" | gzip -1 | ssh "${SSH_OPTS[@]}" "$TARGET" "gzip -dc | $RD load" | tail -1
rssh "$RD image inspect '$IMAGE' >/dev/null" || die "image did not arrive on the server"

# ---------- swap ----------
say "Swapping containers on the server"
if swap_remote "$IMAGE"; then
  say "Deployed $IMAGE"
  rssh "curl -s -m 5 http://127.0.0.1:${HOST_PORT}${HEALTH_PATH}" || true; echo
  echo "Open: http://$EC2_HOST:$HOST_PORT   (security group must allow TCP $HOST_PORT)"
  echo "Roll back: ./deploy.sh --rollback    Logs: ./deploy.sh --logs"
else
  rc=$?
  [ $rc = 3 ] && die "New version failed its health check; the PREVIOUS version was restored and is running."
  die "Deploy failed (exit $rc). Check: ./deploy.sh --status and ./deploy.sh --logs"
fi
