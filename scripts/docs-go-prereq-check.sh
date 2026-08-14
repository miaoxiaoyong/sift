#!/usr/bin/env bash
# Issue #949: keep the Go minimum-version prerequisite stated in README.md in
# lockstep with the root module's `go` directive in go.mod. go.mod is the
# single source of truth for the toolchain floor; this check fails CI if a
# doc drifts below it. Reviews/ and CHANGELOG.md are historical archives and
# intentionally excluded.
set -euo pipefail
cd "$(dirname "$0")/.."

# Root module toolchain floor: "go 1.25.0" -> "1.25".
gomod_ver="$(awk '/^go / {print $2; exit}' go.mod)"
floor="$(printf '%s' "$gomod_ver" | cut -d. -f1-2)"

# Every statement of a Go minimum prerequisite in the active/developer docs
# must advertise at least the go.mod floor. Currently the only such statement
# is in README.md's development section; if more are added they must be listed
# here so they cannot drift silently.
declare -a prereq_files=("README.md")

fail=0
for f in "${prereq_files[@]}"; do
  if ! grep -qF "Go ${floor}+" "$f"; then
    echo "::error file=$f::Expected \"需要 Go ${floor}+\" matching go.mod ($gomod_ver); got:"
    grep -nE "Go [0-9]+(\.[0-9]+)+\+" "$f" || true
    fail=1
  fi
done

exit "$fail"
