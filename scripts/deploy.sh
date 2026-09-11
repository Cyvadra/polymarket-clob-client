#!/usr/bin/env bash
set -Eeuo pipefail

if (( $# > 1 )); then
	printf 'usage: %s <ssh-host>\n' "$0" >&2
	exit 2
fi
HOST=${1:-${EXECUTIOND_DEPLOY_HOST:-${PMM_DEPLOY_HOST:-}}}

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
PRIVATE_DIR="$SCRIPT_DIR/private/$HOST"

# Optional per-host deploy overrides (EXECUTIOND_PM2_BIN, etc.); see
# scripts/deploy.env.example.
if [[ -n $HOST && -f "$PRIVATE_DIR/deploy.env" ]]; then
	# shellcheck disable=SC1091
	source "$PRIVATE_DIR/deploy.env"
fi

TARGET=${EXECUTIOND_DEPLOY_TARGET:-root}
ARCH=${EXECUTIOND_DEPLOY_ARCH:-amd64}
OS=${EXECUTIOND_DEPLOY_OS:-linux}
PM2_BIN=${EXECUTIOND_PM2_BIN:-/root/.nvm/versions/node/v24.21.0/bin/pm2}
APP_USER=executiond
APP_GROUP=executiond
INSTALL_ROOT=/opt/executiond
CONFIG_ROOT=/etc/executiond
DATA_ROOT=/var/lib/executiond
RELEASE_ID=${EXECUTIOND_RELEASE_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
STAGE_DIR=

log() {
	printf '[deploy:%s] %s\n' "$HOST" "$*"
}

fail() {
	printf '[deploy:%s] ERROR: %s\n' "$HOST" "$*" >&2
	exit 1
}

cleanup() {
	[[ -z ${STAGE_DIR:-} ]] || rm -rf "$STAGE_DIR"
}

require_commands() {
	local command_name
	for command_name in go ssh scp tar sha256sum; do
		command -v "$command_name" >/dev/null || fail "required command not found: $command_name"
	done
}

require_deploy_host() {
	[[ -n $HOST ]] || fail "usage: $0 <ssh-host>"
}

require_go_version() {
	# go.mod carries a minimum-version directive (e.g. "go 1.24.0"), so require
	# the local toolchain to be at least that, not an exact match.
	local required actual lowest
	required=$(awk '$1 == "go" { print $2; exit }' "$REPO_ROOT/go.mod")
	actual=$(go env GOVERSION | sed 's/^go//')
	lowest=$(printf '%s\n%s\n' "$required" "$actual" | sort -V | head -n1)
	[[ $lowest == "$required" ]] || fail "Go $required or newer is required; found $actual"
}

require_private_bundle() {
	local env_file="$PRIVATE_DIR/executiond.env"
	[[ -f $env_file ]] || fail "missing private production file: $env_file"
	! grep -q '<CHANGE_ME>' "$env_file" || fail "replace every <CHANGE_ME> value in $env_file"
	grep -Eq '^[[:space:]]*POLYMARKET_PRIVATE_KEY=[^[:space:]]+' "$env_file" || \
		fail "$env_file must set POLYMARKET_PRIVATE_KEY"
	grep -Eq '^[[:space:]]*EXECUTION_POSTGRES_URL=[^[:space:]]+' "$env_file" || \
		fail "$env_file must set EXECUTION_POSTGRES_URL"
	if grep -Eq '^[[:space:]]*EXECUTION_POSTGRES_URL=postgres://user:password@127\.0\.0\.1:5432/execution' "$env_file"; then
		fail "$env_file still uses the placeholder EXECUTION_POSTGRES_URL; point it at the real database"
	fi
}

build_bundle() {
	STAGE_DIR=$(mktemp -d)
	log "testing source"
	(cd "$REPO_ROOT" && go test ./...)
	log "building $OS/$ARCH release $RELEASE_ID"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -o "$STAGE_DIR/executiond" ./cmd/executiond)
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -o "$STAGE_DIR/executiontest" ./cmd/executiontest)
	tar -C "$STAGE_DIR" -czf "$STAGE_DIR/executiond-$RELEASE_ID.tar.gz" executiond executiontest
	(cd "$STAGE_DIR" && sha256sum "executiond-$RELEASE_ID.tar.gz" >"executiond-$RELEASE_ID.tar.gz.sha256")
}

verify_target() {
	local target
	target=$(ssh "$TARGET@$HOST" 'os=$(uname -s | tr "[:upper:]" "[:lower:]"); case $(uname -m) in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) arch=$(uname -m) ;; esac; printf "%s/%s\\n" "$os" "$arch"') || \
		fail "could not determine target platform"
	[[ $target == "$OS/$ARCH" ]] || fail "target is $target; expected $OS/$ARCH"
}

upload_and_activate() {
	local remote_stage=/tmp/executiond-deploy-$RELEASE_ID
	ssh "$TARGET@$HOST" "rm -rf '$remote_stage' && install -d -m 0700 '$remote_stage'"
	scp "$STAGE_DIR/executiond-$RELEASE_ID.tar.gz" "$STAGE_DIR/executiond-$RELEASE_ID.tar.gz.sha256" \
		"$PRIVATE_DIR/executiond.env" \
		"$TARGET@$HOST:$remote_stage/"
	ssh "$TARGET@$HOST" "RELEASE_ID='$RELEASE_ID' REMOTE_STAGE='$remote_stage' PM2_BIN='$PM2_BIN' APP_USER='$APP_USER' APP_GROUP='$APP_GROUP' INSTALL_ROOT='$INSTALL_ROOT' CONFIG_ROOT='$CONFIG_ROOT' DATA_ROOT='$DATA_ROOT' bash -s" <<'REMOTE'
set -Eeuo pipefail

release_dir="$INSTALL_ROOT/releases/$RELEASE_ID"
current_link="$INSTALL_ROOT/current"
run_script="$INSTALL_ROOT/run.sh"
node_bin="$(dirname "$PM2_BIN")/node"
previous_release=
activation_started=0

cleanup() {
	rm -rf "$REMOTE_STAGE"
}
trap cleanup EXIT

pm2_app_status() {
	# Prints "status restart_time" for the executiond pm2 app, or nothing if absent.
	"$PM2_BIN" jlist | "$node_bin" -e '
		const apps = JSON.parse(require("fs").readFileSync(0, "utf8"));
		const app = apps.find((a) => a.name === "executiond");
		if (app) process.stdout.write(app.pm2_env.status + " " + app.pm2_env.restart_time + "\n");
	'
}

start_pm2_app() {
	"$PM2_BIN" delete executiond >/dev/null 2>&1 || true
	"$PM2_BIN" start "$run_script" --name executiond --interpreter bash --cwd "$DATA_ROOT"
	"$PM2_BIN" save
}

rollback() {
	local status=$?
	trap - ERR
	(( activation_started )) || exit "$status"
	if [[ -f $REMOTE_STAGE/previous/executiond.env ]]; then
		install -o root -g root -m 0600 "$REMOTE_STAGE/previous/executiond.env" "$CONFIG_ROOT/executiond.env"
	else
		rm -f "$CONFIG_ROOT/executiond.env"
	fi
	if [[ -n $previous_release && -d $previous_release ]]; then
		ln -sfn "$previous_release" "$current_link.new"
		mv -Tf "$current_link.new" "$current_link"
		start_pm2_app || true
	else
		rm -f "$current_link" "$current_link.new"
		"$PM2_BIN" delete executiond >/dev/null 2>&1 || true
		"$PM2_BIN" save >/dev/null 2>&1 || true
	fi
	exit "$status"
}

command -v "$PM2_BIN" >/dev/null || { echo "[executiond] ERROR: pm2 not found at $PM2_BIN (set EXECUTIOND_PM2_BIN)" >&2; exit 1; }
command -v runuser >/dev/null || { echo "[executiond] ERROR: runuser not found; required to drop to $APP_USER" >&2; exit 1; }

if ! systemctl is-active --quiet pmm-nats.service 2>/dev/null; then
	echo "[executiond] WARNING: pmm-nats.service is not active; executiond needs a NATS server at its EXECUTION_NATS_URL" >&2
fi

if ! getent group "$APP_GROUP" >/dev/null; then
	groupadd --system "$APP_GROUP"
fi
if ! getent passwd "$APP_USER" >/dev/null; then
	useradd --system --gid "$APP_GROUP" --home-dir "$DATA_ROOT" --shell /usr/sbin/nologin "$APP_USER"
fi

install -d -o root -g root -m 0755 "$INSTALL_ROOT" "$INSTALL_ROOT/releases"
install -d -o root -g root -m 0750 "$CONFIG_ROOT"
install -d -o "$APP_USER" -g "$APP_GROUP" -m 0750 "$DATA_ROOT"
(cd "$REMOTE_STAGE" && sha256sum -c "executiond-$RELEASE_ID.tar.gz.sha256")
tar -C "$REMOTE_STAGE" -xzf "$REMOTE_STAGE/executiond-$RELEASE_ID.tar.gz"
install -d -o root -g root -m 0755 "$release_dir"
install -o root -g root -m 0755 "$REMOTE_STAGE/executiond" "$release_dir/executiond"
install -o root -g root -m 0755 "$REMOTE_STAGE/executiontest" "$release_dir/executiontest"

if [[ -L $current_link ]]; then
	previous_release=$(readlink -f "$current_link")
fi
install -d -m 0700 "$REMOTE_STAGE/previous"
if [[ -f $CONFIG_ROOT/executiond.env ]]; then
	install -m 0600 "$CONFIG_ROOT/executiond.env" "$REMOTE_STAGE/previous/executiond.env"
fi
activation_started=1
trap rollback ERR

# pm2 (and this wrapper) run as root so it can read binaries under /root and
# manage a single system-wide daemon; the wrapper drops to the unprivileged
# $APP_USER before execing the actual executiond binary, mirroring the old
# systemd unit's User=executiond isolation.
cat > "$run_script" <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail
set -a
source "$CONFIG_ROOT/executiond.env"
set +a
exec runuser -u "$APP_USER" -p -- "$current_link/executiond"
EOF
chmod 0755 "$run_script"

install -o root -g root -m 0600 "$REMOTE_STAGE/executiond.env" "$CONFIG_ROOT/executiond.env"
ln -sfn "$release_dir" "$current_link.new"
mv -Tf "$current_link.new" "$current_link"
start_pm2_app

# executiond exposes no HTTP endpoint; treat it as ready once pm2 reports it
# online and stable (no new restarts) for a few consecutive seconds.
read -r baseline_status baseline_restarts <<<"$(pm2_app_status)"
stable=0
ready=0
for attempt in {1..20}; do
	read -r status restarts <<<"$(pm2_app_status)"
	if [[ $status == "online" && ${restarts:-0} == "$baseline_restarts" ]]; then
		stable=$((stable + 1))
	else
		stable=0
	fi
	if (( stable >= 5 )); then
		ready=1
		break
	fi
	sleep 1
done
if (( ! ready )); then
	"$PM2_BIN" logs executiond --lines 50 --nostream >&2 || true
	false
fi
trap - ERR

find "$INSTALL_ROOT/releases" -mindepth 1 -maxdepth 1 -type d -name '20*T*Z' -printf '%T@ %p\n' \
	| sort -nr | awk 'NR > 3 { $1=""; sub(/^ /, ""); print }' \
	| while IFS= read -r old_release; do
			[[ $old_release == "$(readlink -f "$current_link")" ]] || rm -rf -- "$old_release"
		done
REMOTE
}

main() {
	trap cleanup EXIT
	require_deploy_host
	require_commands
	require_go_version
	require_private_bundle
	verify_target
	build_bundle
	upload_and_activate
	log "release $RELEASE_ID is active"
}

main "$@"
