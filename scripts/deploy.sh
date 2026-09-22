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
# Encrypted signing key material, shipped alongside executiond.env when the env
# file selects POLYMARKET_PRIVATE_KEY_FILE.
KEY_FILE="$PRIVATE_DIR/private-key.json"
PASSPHRASE_FILE="$PRIVATE_DIR/private-key.pass"
USE_KEY_FILE=0

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
	for command_name in go ssh scp tar sha256sum stat; do
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
	# Only real settings matter: the commented examples in the template carry
	# <CHANGE_ME> on purpose and are meant to be left alone.
	! grep -Eq '^[[:space:]]*[^#[:space:]].*<CHANGE_ME>' "$env_file" || \
		fail "replace every <CHANGE_ME> value in $env_file (commented lines are ignored)"
	local has_plaintext_key=0 has_key_file=0
	grep -Eq '^[[:space:]]*POLYMARKET_PRIVATE_KEY=[^[:space:]]+' "$env_file" && has_plaintext_key=1
	grep -Eq '^[[:space:]]*POLYMARKET_PRIVATE_KEY_FILE=[^[:space:]]+' "$env_file" && has_key_file=1
	if (( has_plaintext_key && has_key_file )); then
		fail "$env_file sets both POLYMARKET_PRIVATE_KEY and POLYMARKET_PRIVATE_KEY_FILE; use exactly one"
	fi
	if (( ! has_plaintext_key && ! has_key_file )); then
		fail "$env_file must set POLYMARKET_PRIVATE_KEY_FILE (preferred) or POLYMARKET_PRIVATE_KEY"
	fi
	if (( has_key_file )); then
		USE_KEY_FILE=1
		require_key_paths "$env_file"
		require_encrypted_key
	else
		printf '[deploy:%s] WARNING: %s ships the signing key in plaintext; see scripts/README.md for polykey\n' \
			"$HOST" "$env_file" >&2
	fi
	grep -Eq '^[[:space:]]*EXECUTION_POSTGRES_URL=[^[:space:]]+' "$env_file" || \
		fail "$env_file must set EXECUTION_POSTGRES_URL"
	if grep -Eq '^[[:space:]]*EXECUTION_POSTGRES_URL=postgres://user:password@127\.0\.0\.1:5432/execution' "$env_file"; then
		fail "$env_file still uses the placeholder EXECUTION_POSTGRES_URL; point it at the real database"
	fi
}

# require_key_paths checks that the daemon will look for the keystore where
# install_bundle actually puts it. The upload destinations are fixed, so a
# custom path in the env file would otherwise pass preflight and only fail at
# startup, after the deploy has already swapped the release.
require_key_paths() {
	local env_file=$1 name expected actual
	for name in POLYMARKET_PRIVATE_KEY_FILE POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE; do
		case $name in
			POLYMARKET_PRIVATE_KEY_FILE) expected="$CONFIG_ROOT/private-key.json" ;;
			*) expected="$CONFIG_ROOT/private-key.pass" ;;
		esac
		actual=$(sed -nE "s/^[[:space:]]*$name=[[:space:]]*//p" "$env_file" | tail -n1)
		actual=${actual%$'\r'}
		actual=${actual%"${actual##*[![:space:]]}"}
		actual=${actual#\"}
		actual=${actual%\"}
		[[ -n $actual ]] || fail "$env_file must set $name=$expected"
		[[ $actual == "$expected" ]] || \
			fail "$env_file sets $name=$actual, but deploy.sh installs the key bundle at $expected; use $expected"
	done
}

# require_encrypted_key checks the keystore bundle locally, so a wrong
# passphrase or a loose file mode fails the deploy instead of the daemon.
require_encrypted_key() {
	[[ -f $KEY_FILE ]] || fail "missing encrypted signing key: $KEY_FILE (create it with: go run ./cmd/polykey encrypt --key-file $KEY_FILE --passphrase-file $PASSPHRASE_FILE)"
	[[ -f $PASSPHRASE_FILE ]] || fail "missing passphrase file: $PASSPHRASE_FILE"
	local mode secret
	for secret in "$KEY_FILE" "$PASSPHRASE_FILE"; do
		mode=$(stat -c '%a' "$secret" 2>/dev/null || stat -f '%Lp' "$secret")
		[[ $mode == 600 ]] || fail "$secret has mode $mode; run: chmod 600 $secret"
	done
	local local_polykey address
	local_polykey=$(mktemp)
	(cd "$REPO_ROOT" && go build -o "$local_polykey" ./cmd/polykey) || fail "could not build polykey"
	address=$("$local_polykey" verify --key-file "$KEY_FILE" --passphrase-file "$PASSPHRASE_FILE") || {
		rm -f "$local_polykey"
		fail "$KEY_FILE cannot be decrypted with $PASSPHRASE_FILE"
	}
	rm -f "$local_polykey"
	log "signing key unlocks to $address"
}

build_bundle() {
	STAGE_DIR=$(mktemp -d)
	log "testing source"
	(cd "$REPO_ROOT" && go test ./...)
	log "building $OS/$ARCH release $RELEASE_ID"
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -o "$STAGE_DIR/executiond" ./cmd/executiond)
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -o "$STAGE_DIR/executiontest" ./cmd/executiontest)
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -o "$STAGE_DIR/polykey" ./cmd/polykey)
	tar -C "$STAGE_DIR" -czf "$STAGE_DIR/executiond-$RELEASE_ID.tar.gz" executiond executiontest polykey
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
	local key_material=()
	if (( USE_KEY_FILE )); then
		key_material=("$KEY_FILE" "$PASSPHRASE_FILE")
	fi
	scp "$STAGE_DIR/executiond-$RELEASE_ID.tar.gz" "$STAGE_DIR/executiond-$RELEASE_ID.tar.gz.sha256" \
		"$PRIVATE_DIR/executiond.env" ${key_material[@]+"${key_material[@]}"} \
		"$TARGET@$HOST:$remote_stage/"
	ssh "$TARGET@$HOST" "RELEASE_ID='$RELEASE_ID' REMOTE_STAGE='$remote_stage' PM2_BIN='$PM2_BIN' APP_USER='$APP_USER' APP_GROUP='$APP_GROUP' INSTALL_ROOT='$INSTALL_ROOT' CONFIG_ROOT='$CONFIG_ROOT' DATA_ROOT='$DATA_ROOT' USE_KEY_FILE='$USE_KEY_FILE' bash -s" <<'REMOTE'
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

# secret_owner prints the install(1) ownership flags for a file under
# $CONFIG_ROOT. The env file is sourced by run.sh as root, so root owns it:
# anything $APP_USER can write, root would execute on the next restart. The key
# bundle is opened by the daemon itself, after it has dropped to $APP_USER, so
# that has to be owned by $APP_USER.
secret_owner() {
	case $1 in
		executiond.env) printf -- '-o root -g root' ;;
		*) printf -- '-o %s -g %s' "$APP_USER" "$APP_GROUP" ;;
	esac
}

rollback() {
	local status=$?
	trap - ERR
	(( activation_started )) || exit "$status"
	for secret in executiond.env private-key.json private-key.pass; do
		if [[ -f $REMOTE_STAGE/previous/$secret ]]; then
			install $(secret_owner "$secret") -m 0600 "$REMOTE_STAGE/previous/$secret" "$CONFIG_ROOT/$secret"
		else
			rm -f "$CONFIG_ROOT/$secret"
		fi
	done
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
# executiond (not root) execs the daemon and must be able to read the key
# material under $CONFIG_ROOT, so the directory is group-owned by $APP_GROUP.
install -d -o root -g "$APP_GROUP" -m 0750 "$CONFIG_ROOT"
install -d -o "$APP_USER" -g "$APP_GROUP" -m 0750 "$DATA_ROOT"
(cd "$REMOTE_STAGE" && sha256sum -c "executiond-$RELEASE_ID.tar.gz.sha256")
tar -C "$REMOTE_STAGE" -xzf "$REMOTE_STAGE/executiond-$RELEASE_ID.tar.gz"
install -d -o root -g root -m 0755 "$release_dir"
install -o root -g root -m 0755 "$REMOTE_STAGE/executiond" "$release_dir/executiond"
install -o root -g root -m 0755 "$REMOTE_STAGE/executiontest" "$release_dir/executiontest"
install -o root -g root -m 0755 "$REMOTE_STAGE/polykey" "$release_dir/polykey"

if [[ -L $current_link ]]; then
	previous_release=$(readlink -f "$current_link")
fi
install -d -m 0700 "$REMOTE_STAGE/previous"
for secret in executiond.env private-key.json private-key.pass; do
	if [[ -f $CONFIG_ROOT/$secret ]]; then
		install -m 0600 "$CONFIG_ROOT/$secret" "$REMOTE_STAGE/previous/$secret"
	fi
done
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

# root owns the env file: run.sh sources it as root before dropping privileges,
# so a file $APP_USER could write would be a root-shell injection point. The
# daemon never reads it -- it inherits these as environment variables across
# the runuser -p.
install -o root -g root -m 0600 "$REMOTE_STAGE/executiond.env" "$CONFIG_ROOT/executiond.env"
if (( USE_KEY_FILE )); then
	install -o "$APP_USER" -g "$APP_GROUP" -m 0600 "$REMOTE_STAGE/private-key.json" "$CONFIG_ROOT/private-key.json"
	install -o "$APP_USER" -g "$APP_GROUP" -m 0600 "$REMOTE_STAGE/private-key.pass" "$CONFIG_ROOT/private-key.pass"
else
	rm -f "$CONFIG_ROOT/private-key.json" "$CONFIG_ROOT/private-key.pass"
fi
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
