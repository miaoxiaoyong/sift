#!/usr/bin/env bash
# Hosting smoke harness (WBS M8 §8.2 / specs/hosting.md §8).
#
# Drives scripts/hosting-smoke.sh through the R1 P1-3 closing gates, repeatably
# and without touching a real launchd/systemd, a real install, or any network
# port. The harness builds the two release binaries from this tree, then runs
# the smoke with a scrubbed PATH and fake supervisor tools so every scenario is
# deterministic on a plain dev box. Scenarios are gated by host OS because the
# CLI's backend follows runtime.GOOS (darwin→launchd, linux→systemd):
#
#   scenario             backend         host    assertion
#   startup-failure      foreground      any     smoke exits != 0 (daemon dies at boot)
#   happy-foreground     foreground      any     smoke exits == 0 (daemon alive, clean shutdown)
#   no-new-pid           systemd (fake)  linux   smoke exits != 0 (kill, no autorestart)
#   stale-upgrade        systemd (fake)  linux   smoke exits != 0 (restart, no new release)
#   happy-systemd        systemd (fake)  linux   smoke exits == 0 (alive / new pid / new release)
#   unregistered-launchd launchd (fake)  darwin  smoke exits != 0 (kill, agent lost)
#   stale-upgrade-launchd launchd (fake) darwin  smoke exits != 0 (restart, no new release)
#   happy-launchd        launchd (fake)  darwin  smoke exits == 0 (alive / registered / new release)
#
# Usage (from the repo root):
#   ./scripts/hosting-smoke-test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SMOKE="$SCRIPT_DIR/hosting-smoke.sh"

scratch_root="$(mktemp -d)"
FAILURES=0
cleanup() {
	if (( FAILURES == 0 )); then
		rm -rf "$scratch_root"
	else
		echo "scratch kept for inspection: $scratch_root" >&2
	fi
}
trap cleanup EXIT

if ! command -v go >/dev/null 2>&1; then
	echo "go not found on PATH; cannot build the release binaries" >&2
	exit 1
fi

# tools/ holds every external command the smoke needs, minus launchctl and
# systemctl (the harness fakes those and must keep the real ones invisible).
tools_dir="$scratch_root/tools"
mkdir -p "$tools_dir"
for tool in mktemp mkdir chmod cp tar gzip sha256sum shasum awk sed uname tr cat readlink kill sleep rm basename head id bash; do
	real="$(command -v "$tool" 2>/dev/null || true)"
	# Skip bash builtins (kill etc.) — command -v returns the bare name.
	[[ -n "$real" && "$real" != "$tool" ]] || continue
	ln -s "$real" "$tools_dir/$tool"
done

# real/ : the two release binaries built from this tree.
real_dir="$scratch_root/real"
mkdir -p "$real_dir"
( cd "$REPO_ROOT" && go build -o "$real_dir/sift" ./cmd/sift )
( cd "$REPO_ROOT" && go build -o "$real_dir/sift-agent-wrapper" ./cmd/sift-agent-wrapper )

# write_fake_systemctl emits a systemctl whose behavior mirrors a systemd user
# manager for exactly the calls the smoke makes, driven by state/mode env vars:
#   FAKE_ON_KILL:     1 = spawn a fresh daemon when the tracked pid dies (the
#                     Restart=on-failure behavior); 0 = never (negative gate)
#   FAKE_RESTART_CMD: 1 = `restart` brings a daemon back; 0 = it starts nothing
write_fake_systemctl() {
	local state="$1" on_kill="$2" restart_cmd="$3"
	FAKE_SYSTEMD_STATE="$state" FAKE_ON_KILL="$on_kill" FAKE_RESTART_CMD="$restart_cmd" cat >"$tools_dir/systemctl" <<'FAKE'
#!/usr/bin/env bash
set -u
STATE="${FAKE_SYSTEMD_STATE:?}"
pid_file="$STATE/pid"
log="$STATE/daemon.log"
start_daemon() {
	[[ -f "$pid_file" ]] && kill "$(cat "$pid_file")" 2>/dev/null || true
	"${SIFT_HOME:?}/bin/current/sift" daemon >>"$log" 2>&1 &
	echo $! >"$pid_file"
}
stop_daemon() {
	[[ -f "$pid_file" ]] && kill "$(cat "$pid_file")" 2>/dev/null || true
	rm -f "$pid_file"
}
maybe_restart_on_kill() {
	if [[ -f "$pid_file" ]] && ! kill -0 "$(cat "$pid_file")" 2>/dev/null; then
		if [[ "${FAKE_ON_KILL:-1}" == "1" ]]; then start_daemon; else rm -f "$pid_file"; fi
	fi
}
args=("$@")
if [[ "${args[0]}" == "--user" ]]; then
	cmd="${args[1]:-}"
else
	cmd="${args[0]:-}"
fi
case "$cmd" in
	daemon-reload|is-system-running) exit 0 ;;
	enable) start_daemon; exit 0 ;;
	restart) stop_daemon; [[ "${FAKE_RESTART_CMD:-1}" == "1" ]] && start_daemon; exit 0 ;;
	disable) stop_daemon; exit 0 ;;
	show)
		if [[ "$*" == *MainPID* ]]; then
			maybe_restart_on_kill
			[[ -f "$pid_file" ]] && cat "$pid_file" || echo 0
		fi
		exit 0 ;;
esac
exit 0
FAKE
	chmod +x "$tools_dir/systemctl"
}

# write_fake_launchctl is the launchd analog: load starts the daemon, list
# reports the pid and exit status (registration), kickstart -k restarts it,
# bootout/remove stop it.
write_fake_launchctl() {
	local state="$1" on_kill="$2" restart_cmd="$3"
	FAKE_LAUNCHD_STATE="$state" FAKE_ON_KILL="$on_kill" FAKE_RESTART_CMD="$restart_cmd" cat >"$tools_dir/launchctl" <<'FAKE'
#!/usr/bin/env bash
set -u
STATE="${FAKE_LAUNCHD_STATE:?}"
pid_file="$STATE/pid"
log="$STATE/daemon.log"
start_daemon() {
	[[ -f "$pid_file" ]] && kill "$(cat "$pid_file")" 2>/dev/null || true
	"${SIFT_HOME:?}/bin/current/sift" daemon >>"$log" 2>&1 &
	echo $! >"$pid_file"
}
stop_daemon() {
	[[ -f "$pid_file" ]] && kill "$(cat "$pid_file")" 2>/dev/null || true
	rm -f "$pid_file"
}
maybe_restart_on_kill() {
	if [[ -f "$pid_file" ]] && ! kill -0 "$(cat "$pid_file")" 2>/dev/null; then
		if [[ "${FAKE_ON_KILL:-1}" == "1" ]]; then start_daemon; else rm -f "$pid_file"; fi
	fi
}
case "${1:-}" in
	list)
		maybe_restart_on_kill
		if [[ -f "$pid_file" ]] && kill -0 "$(cat "$pid_file")" 2>/dev/null; then
			printf '"PID" = %s;\n' "$(cat "$pid_file")"
			exit 0
		fi
		exit 1
		;;
	kickstart) stop_daemon; [[ "${FAKE_RESTART_CMD:-1}" == "1" ]] && start_daemon; exit 0 ;;
	load) start_daemon; exit 0 ;;
	bootout|remove|unload) stop_daemon; exit 0 ;;
esac
exit 0
FAKE
	chmod +x "$tools_dir/launchctl"
}

# run_smoke executes the smoke in a fresh, env-scrubbed dir; the scenario's
# bin/ holds the real binaries unless the scenario pre-placed fakes. Extra env
# assignments are passed as a single "VAR=VAL VAR2=VAL2" string.
run_smoke() {
	local name="$1" extra_env="${2:-}"
	local scratch="$scratch_root/$name"
	local log="$scratch/smoke.log"
	mkdir -p "$scratch/bin" "$scratch/state"
	if [[ ! -x "$scratch/bin/sift" ]]; then
		cp "$real_dir/sift" "$scratch/bin/sift"
		cp "$real_dir/sift-agent-wrapper" "$scratch/bin/sift-agent-wrapper"
	fi
	local rc
	set +e
	( cd "$REPO_ROOT" && env -i PATH="$scratch/bin:$tools_dir" HOME="$scratch/home" \
		XDG_RUNTIME_DIR="$scratch/xdg" $extra_env bash "$SMOKE" ) >"$log" 2>&1
	rc=$?
	set -e
	echo "$rc"
}

check() {
	local name="$1" want="$2" got="$3"
	if [[ "$want" == "nonzero" ]]; then
		if (( got != 0 )); then
			echo "PASS: $name (exit $got)"
		else
			echo "FAIL: $name: expected non-zero exit, got $got" >&2
			FAILURES=$((FAILURES + 1))
		fi
	else
		if (( got == 0 )); then
			echo "PASS: $name (exit $got)"
		else
			echo "FAIL: $name: expected exit 0, got $got" >&2
			FAILURES=$((FAILURES + 1))
		fi
	fi
}

# --- scenario 1: foreground daemon dies at boot ---------------------------------
s1="$scratch_root/startup-failure"
mkdir -p "$s1/bin"
cat >"$s1/bin/sift" <<FAKE
#!/usr/bin/env bash
if [[ "\${1:-}" == "daemon" ]]; then
	echo "smoke: injected daemon startup failure" >&2
	exit 1
fi
exec "$real_dir/sift" "\$@"
FAKE
chmod +x "$s1/bin/sift"
cat >"$s1/bin/sift-agent-wrapper" <<FAKE
#!/usr/bin/env bash
exec "$real_dir/sift-agent-wrapper" "\$@"
FAKE
chmod +x "$s1/bin/sift-agent-wrapper"

rc="$(run_smoke startup-failure)"
check "startup-failure: foreground daemon exiting at boot must fail the smoke" nonzero "$rc"

# --- scenario 2: foreground happy path (daemon comes up, clean shutdown) --------
rc="$(run_smoke happy-foreground)"
check "happy-foreground: foreground daemon alive + clean shutdown must pass" zero "$rc"

# --- systemd scenarios (linux only: the CLI backend follows runtime.GOOS) --------
if [[ "$(uname -s)" == "Linux" ]]; then
	write_fake_systemctl "$scratch_root/no-new-pid/state" 0 0
	rc="$(run_smoke no-new-pid "FAKE_SYSTEMD_STATE=$scratch_root/no-new-pid/state FAKE_ON_KILL=0 FAKE_RESTART_CMD=0")"
	check "no-new-pid: supervisor not producing a new pid after kill must fail the smoke" nonzero "$rc"

	write_fake_systemctl "$scratch_root/stale-upgrade/state" 1 0
	rc="$(run_smoke stale-upgrade "FAKE_SYSTEMD_STATE=$scratch_root/stale-upgrade/state FAKE_ON_KILL=1 FAKE_RESTART_CMD=0")"
	check "stale-upgrade: daemon not running on the new release after restart must fail the smoke" nonzero "$rc"

	write_fake_systemctl "$scratch_root/happy-systemd/state" 1 1
	rc="$(run_smoke happy-systemd "FAKE_SYSTEMD_STATE=$scratch_root/happy-systemd/state FAKE_ON_KILL=1 FAKE_RESTART_CMD=1")"
	check "happy-systemd: daemon alive, new pid after kill, new release after restart must pass" zero "$rc"
else
	echo "SKIP: systemd scenarios require Linux (host is $(uname -s))"
fi

# --- launchd scenarios (darwin only) ----------------------------------------------
if [[ "$(uname -s)" == "Darwin" ]]; then
	write_fake_launchctl "$scratch_root/unregistered-launchd/state" 0 0
	rc="$(run_smoke unregistered-launchd "FAKE_LAUNCHD_STATE=$scratch_root/unregistered-launchd/state FAKE_ON_KILL=0 FAKE_RESTART_CMD=0")"
	check "unregistered-launchd: agent not registered after kill must fail the smoke" nonzero "$rc"

	write_fake_launchctl "$scratch_root/stale-upgrade-launchd/state" 1 0
	rc="$(run_smoke stale-upgrade-launchd "FAKE_LAUNCHD_STATE=$scratch_root/stale-upgrade-launchd/state FAKE_ON_KILL=1 FAKE_RESTART_CMD=0")"
	check "stale-upgrade-launchd: daemon not running on the new release after restart must fail the smoke" nonzero "$rc"

	write_fake_launchctl "$scratch_root/happy-launchd/state" 1 1
	rc="$(run_smoke happy-launchd "FAKE_LAUNCHD_STATE=$scratch_root/happy-launchd/state FAKE_ON_KILL=1 FAKE_RESTART_CMD=1")"
	check "happy-launchd: daemon alive, re-registered after kill, new release after restart must pass" zero "$rc"
else
	echo "SKIP: launchd scenarios require Darwin (host is $(uname -s))"
fi

if (( FAILURES > 0 )); then
	echo "==> $FAILURES scenario(s) FAILED; logs under $scratch_root/*/smoke.log" >&2
	exit 1
fi
echo "==> all hosting-smoke scenarios passed"
