#!/usr/bin/env bash
# Hosting smoke (WBS M8 §8.2 / specs/hosting.md §8).
#
# Exercises the three hosting paths against a real `sift` install:
#   1. install the platform unit (launchd / systemd user / foreground)
#   2. start the daemon through it
#   3. kill the daemon and confirm autorestart (supervised backends only)
#   4. install a second release archive and atomically switch `current`
#   5. `sift service restart` and confirm the new release is running
#
# Exit contract: every acceptance step is a hard gate. A daemon that fails to
# come up, a supervisor that does not produce a new PID/registration after a
# kill, or an upgrade whose new release is not running afterwards all exit
# non-zero. The script detects the available supervisor and degrades honestly:
# on a host without launchd/systemd it validates the foreground path only
# (start, clean shutdown), because V0 does not promise autorestart in
# foreground mode (DESIGN §11). It never opens a network port — the control
# plane is Unix sockets only.
#
# Usage:
#   SIFT=sift SIFT_AGENT_WRAPPER=sift-agent-wrapper ./scripts/hosting-smoke.sh
#
# It expects the two release binaries on PATH and write access to a temp
# SIFT_HOME. It creates a fresh SIFT_HOME so it never touches a real install.
# The negative/positive harness (scripts/hosting-smoke-test.sh) drives these
# gates with fake supervisors on a scrubbed PATH.
set -euo pipefail

SIFT_BIN="${SIFT:-sift}"
WRAPPER_BIN="${SIFT_AGENT_WRAPPER:-sift-agent-wrapper}"

WORK="$(mktemp -d)"
cleanup() {
	if [[ -n "${SMOKE_HOME:-}" ]] && [[ -d "$SMOKE_HOME" ]]; then
		if command -v launchctl >/dev/null 2>&1; then
			launchctl bootout "gui/$(id -u)/cn.hexai.sift" >/dev/null 2>&1 || \
				launchctl remove cn.hexai.sift >/dev/null 2>&1 || true
		fi
		if command -v systemctl >/dev/null 2>&1; then
			systemctl --user disable --now "${HOSTING_UNIT:-sift}.service" >/dev/null 2>&1 || true
		fi
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

# Portable sha256: coreutils (sha256sum) or macOS shasum.
sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# Provision a fresh SIFT_HOME so the smoke never collides with a real one.
SMOKE_HOME="$WORK/home"
mkdir -p "$SMOKE_HOME"
chmod 700 "$SMOKE_HOME"
export SIFT_HOME="$SMOKE_HOME"

if ! command -v "$SIFT_BIN" >/dev/null 2>&1; then
	echo "sift binary '$SIFT_BIN' not found on PATH" >&2
	exit 2
fi
if ! command -v "$WRAPPER_BIN" >/dev/null 2>&1; then
	echo "sift-agent-wrapper binary '$WRAPPER_BIN' not found on PATH" >&2
	exit 2
fi

release_a="$("$SIFT_BIN" --version)"
echo "==> release under test: $release_a"

# Detect supervisor. systemctl --user only counts if a user manager is running
# (DBUS_SESSION_BUS_ADDRESS / XDG_RUNTIME_DIR present and reachable).
backend="foreground"
if command -v launchctl >/dev/null 2>&1 && [[ "$(uname -s)" == "Darwin" ]]; then
	backend="launchd"
elif command -v systemctl >/dev/null 2>&1 \
	&& [[ -n "${XDG_RUNTIME_DIR:-}" ]] \
	&& systemctl --user show 'sift.service' >/dev/null 2>&1 \
	&& systemctl --user is-system-running >/dev/null 2>&1; then
	backend="systemd"
fi
echo "==> detected backend: $backend"

# wait_for_daemon waits until the daemon responds on the operator socket. The
# offline doctor errors on a missing config/db, which is fine — the point is
# the daemon process responding; socket presence is the fallback probe.
wait_for_daemon() {
	local deadline=$((SECONDS + ${1:-20}))
	while (( SECONDS < deadline )); do
		if "$SIFT_BIN" doctor --offline >/dev/null 2>&1; then
			return 0
		fi
		[[ -S "$SIFT_HOME/siftd.sock" ]] && return 0
		sleep 1
	done
	return 1
}

# wait_for_marker waits until the upgrade shim daemon — the process running the
# NEW release under the supervisor — writes its release marker. The real daemon
# creates no marker; this is only how the smoke proves which release binary is
# executing after `sift service restart`.
wait_for_marker() {
	local want="$1" deadline=$((SECONDS + ${2:-20}))
	local marker="$SIFT_HOME/smoke-daemon-release.marker"
	while (( SECONDS < deadline )); do
		if [[ -f "$marker" ]] && [[ "$(cat "$marker")" == "$want" ]]; then
			return 0
		fi
		sleep 1
	done
	return 1
}

# socket_inode prints the filesystem inode of a unix socket path, or "" when
# the path is absent. It is the kill-before/after identity the autorestart gate
# compares: bindSocket unlinks the leftover socket and recreates it on rebind, so
# a rebound operator socket has a NEW inode while a stale leftover file keeps
# the OLD one. Portable across BSD stat (-f %i) and coreutils stat (-c %i).
socket_inode() {
	local p="$1"
	if [[ "$(uname -s)" == "Darwin" ]]; then
		stat -f %i "$p" 2>/dev/null || true
	else
		stat -c %i "$p" 2>/dev/null || true
	fi
}

# socket_responds proves the operator socket is connectable AND a live daemon
# answers on it, not merely that the socket file exists. It runs `sift doctor
# --json` (online) which dials siftd.sock through the operator client: a stale
# leftover socket file (no listener) fails the dial with "connection refused"
# and writes NO JSON to stdout, while a live daemon returns its doctor result
# as a JSON envelope regardless of that result's exit_code. stdout-JSON is
# therefore the live-listener signal, independent of doctor's findings.
socket_responds() {
	local out
	out="$("$SIFT_BIN" doctor --json 2>/dev/null || true)"
	[[ -n "$out" ]] && grep -q '"ok"' <<<"$out"
}

# A real install requires a release archive. Build one for the current platform
# from the two binaries on PATH (this mirrors what the release pipeline ships).
archive="$WORK/sift_${release_a}_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/').tar.gz"
manifest="$WORK/manifest.json"
make_archive() {
	local rel="$1" out="$2"
	local stage="$WORK/stage-$rel"
	mkdir -p "$stage"
	cp "$(command -v "$SIFT_BIN")" "$stage/sift"
	cp "$(command -v "$WRAPPER_BIN")" "$stage/sift-agent-wrapper"
	chmod +x "$stage/sift" "$stage/sift-agent-wrapper"
	sha_s="$(sha256_file "$stage/sift")"
	sha_w="$(sha256_file "$stage/sift-agent-wrapper")"
	goos="$(uname -s | tr '[:upper:]' '[:lower:]')"
	goarch="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
	cat >"$stage/manifest.json" <<JSON
{"schema_version":1,"release_version":"$rel","artifacts":[
{"goos":"$goos","goarch":"$goarch","name":"sift","sha256":"$sha_s"},
{"goos":"$goos","goarch":"$goarch","name":"sift-agent-wrapper","sha256":"$sha_w"}
]}
JSON
	tar -C "$stage" -czf "$out" sift sift-agent-wrapper manifest.json
}

make_archive "$release_a" "$archive"
echo "==> built release archive: $(basename "$archive")"

# 1. Install the release into the version-directory layout.
"$SIFT_BIN" install "$archive" >/dev/null
echo "==> installed release $release_a to \$SIFT_HOME/bin/current"

# 2. Install the hosting unit. `sift service install` writes the unit and, on a
#    supervised backend, loads it. The daemon is expected to come up (the
#    zero-config daemon binds the operator socket) and the script hard-fails
#    otherwise.
if [[ "$backend" == "foreground" ]]; then
	echo "==> foreground backend: skipping unit install (no supervisor)"
	echo "    running daemon in the foreground to validate the path"
	"$SIFT_BIN" daemon &
	daemon_pid=$!
	if wait_for_daemon 15; then
		echo "==> foreground daemon started (pid $daemon_pid; operator socket $SIFT_HOME/siftd.sock)"
		kill -TERM "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		echo "==> foreground daemon shut down cleanly on SIGTERM"
	else
		echo "!! foreground daemon failed to come up (see $SIFT_HOME/logs/)" >&2
		exit 1
	fi
	echo "==> SMOKE OK (foreground path only; autorestart not promised in this mode)"
	exit 0
fi

# Supervised path: install the unit, then verify autorestart on kill, then
# upgrade + restart.
HOSTING_UNIT="sift"
if [[ "$backend" == "launchd" ]]; then
	# launchd unit lives under the user home, not SIFT_HOME; point it there.
	mkdir -p "$WORK/userhome/Library/LaunchAgents"
	export HOME="$WORK/userhome"
fi

"$SIFT_BIN" service install
echo "==> installed $backend unit via 'sift service install'"

# Give the supervisor a moment to (re)start the daemon, then confirm it is up
# and listening on the operator socket: a daemon that never comes up is a
# smoke failure, not a warning.
if ! wait_for_daemon 30; then
	echo "!! daemon did not come up under $backend within 30s (see $SIFT_HOME/logs/)" >&2
	exit 1
fi
echo "==> daemon came up under $backend"
if [[ ! -S "$SIFT_HOME/siftd.sock" ]]; then
	echo "!! daemon is up but the operator socket $SIFT_HOME/siftd.sock is missing" >&2
	exit 1
fi
echo "==> operator socket present: $SIFT_HOME/siftd.sock"

# 3. Crash autorestart: kill the daemon and expect the supervisor to restart it.
if [[ "$backend" == "systemd" ]]; then
	# Kill the MainPID the unit tracks; Restart=on-failure fires on non-zero.
	unit_pid="$(systemctl --user show -p MainPID --value sift.service 2>/dev/null || echo 0)"
	if [[ "$unit_pid" =~ ^[0-9]+$ ]] && (( unit_pid > 1 )); then
		kill -KILL "$unit_pid" 2>/dev/null || true
		sleep 4
		new_pid="$(systemctl --user show -p MainPID --value sift.service 2>/dev/null || echo 0)"
		if [[ "$new_pid" =~ ^[0-9]+$ ]] && (( new_pid > 1 )) && (( new_pid != unit_pid )); then
			echo "==> autorestart verified: pid $unit_pid -> $new_pid"
		else
			echo "!! autorestart did not produce a new pid (was $unit_pid, now $new_pid)" >&2
			exit 1
		fi
	else
		echo "!! no tracked MainPID for sift.service; cannot verify autorestart" >&2
		exit 1
	fi
elif [[ "$backend" == "launchd" ]]; then
	plist_pid="$(launchctl list cn.hexai.sift 2>/dev/null | awk '/"PID"/{gsub(/[^0-9]/,""); print}' | head -1 || echo "")"
	if [[ -n "$plist_pid" ]] && (( plist_pid > 1 )); then
		# Record the pre-kill operator-socket identity. After kill -KILL the
		# listener dies but the socket FILE remains on disk; `[ -S siftd.sock ]`
		# stays true for that stale leftover, so file-existence alone would
		# misreport a dead instance as recovered. The autorestart gate must prove
		# launchd handed the label to a FRESH instance that REBOUND the control
		# plane (new inode) and is ANSWERING on it (live doctor RPC), not that a
		# leftover socket file still exists (hosting §8).
		launchd_sock="$SIFT_HOME/siftd.sock"
		old_inode="$(socket_inode "$launchd_sock")"
		kill -KILL "$plist_pid" 2>/dev/null || true
		new_pid=""
		new_inode=""
		lost=0
		deadline=$((SECONDS + 30))
		while (( SECONDS < deadline )); do
			listed="$(launchctl list cn.hexai.sift 2>/dev/null || true)"
			if [[ -z "$listed" ]]; then
				# launchd lost the label entirely; autorestart cannot happen.
				lost=$((lost + 1))
				if (( lost >= 5 )); then
					break
				fi
				sleep 1
				continue
			fi
			new_pid="$(printf '%s\n' "$listed" | awk '/"PID"/{gsub(/[^0-9]/,""); print}' | head -1 || echo "")"
			new_inode="$(socket_inode "$launchd_sock")"
			# A recovered instance satisfies ALL THREE at once: a fresh pid under
			# the same label, a rebound socket whose inode differs from the stale
			# leftover, and a live listener that answers a doctor RPC. The stale
			# leftover keeps the old inode and refuses the dial, so it can never
			# pass even if a stale pid lingered in the registration.
			if [[ -n "$new_pid" ]] && (( new_pid > 1 )) && (( new_pid != plist_pid )) \
				&& [[ -n "$new_inode" ]] && [[ "$new_inode" != "$old_inode" ]] \
				&& socket_responds; then
				break
			fi
			sleep 1
		done
		if [[ -n "$new_pid" ]] && (( new_pid > 1 )) && (( new_pid != plist_pid )) \
			&& [[ -n "$new_inode" ]] && [[ "$new_inode" != "$old_inode" ]] \
			&& socket_responds; then
			echo "==> autorestart verified: launchd restarted the agent pid $plist_pid -> $new_pid; operator socket rebound (inode ${old_inode:-none} -> ${new_inode}) and answered a live doctor RPC"
		else
			echo "!! launchd did not restart the agent under a new pid / rebound operator socket (pid $plist_pid -> ${new_pid:-none}; inode ${old_inode:-none} -> ${new_inode:-none})" >&2
			exit 1
		fi
	else
		echo "!! launchd reports no PID for the agent; cannot verify autorestart" >&2
		exit 1
	fi
fi

# 4. Atomic upgrade: install a second release and switch `current`.
release_b="$release_a-smoke"
make_archive "$release_b" "$WORK/upgrade.tar.gz"
# The install probe compares --version to the manifest release_version, so the
# staged binaries must actually report release_b. Rebuild them in place.
stage_b="$WORK/stage-b"
mkdir -p "$stage_b"
# Copy current binaries but they report release_a, not release_b. Build tiny
# version-reporting shims so the probe passes for the upgrade combo.
cat >"$stage_b/sift" <<SH
#!/bin/sh
case "\$1" in
  --version) echo "$release_b" ;;
  daemon)
    # Prove which release is executing under the supervisor: write a marker
    # naming this release, then stay alive like a daemon.
    echo "$release_b" > "\${SIFT_HOME:?}/smoke-daemon-release.marker"
    trap 'exit 0' TERM INT
    while :; do sleep 30; done
    ;;
  *) echo "smoke shim"; sleep 60 ;;
esac
SH
cat >"$stage_b/sift-agent-wrapper" <<SH
#!/bin/sh
[ "\$1" = "--version" ] && echo "$release_b" || exit 0
SH
chmod +x "$stage_b/sift" "$stage_b/sift-agent-wrapper"
goos="$(uname -s | tr '[:upper:]' '[:lower:]')"
goarch="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
sha_s="$(sha256_file "$stage_b/sift")"
sha_w="$(sha256_file "$stage_b/sift-agent-wrapper")"
cat >"$stage_b/manifest.json" <<JSON
{"schema_version":1,"release_version":"$release_b","artifacts":[
{"goos":"$goos","goarch":"$goarch","name":"sift","sha256":"$sha_s"},
{"goos":"$goos","goarch":"$goarch","name":"sift-agent-wrapper","sha256":"$sha_w"}
]}
JSON
tar -C "$stage_b" -czf "$WORK/upgrade.tar.gz" sift sift-agent-wrapper manifest.json

"$SIFT_BIN" install "$WORK/upgrade.tar.gz" >/dev/null
current_target="$(readlink "$SIFT_HOME/bin/current")"
if [[ "$current_target" == "$release_b" ]]; then
	echo "==> atomic upgrade verified: current -> $current_target"
else
	echo "!! current -> '$current_target', expected '$release_b'" >&2
	exit 1
fi

# 5. Restart the unit to pick up the new release, then prove the new release is
#    the process under the supervisor: the upgrade shim writes its marker.
"$SIFT_BIN" service restart
if wait_for_marker "$release_b" 30; then
	echo "==> 'sift service restart' reloaded the unit onto the new release"
	echo "    new release running: $(cat "$SIFT_HOME/smoke-daemon-release.marker")"
else
	echo "!! daemon did not come back on the new release $release_b after restart" >&2
	exit 1
fi
echo "==> SMOKE OK ($backend backend: install / autorestart / atomic-upgrade / restart)"
