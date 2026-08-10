#!/usr/bin/env bash
# Release snapshot pipeline (WBS M8 §8.1 / Issue #903).
#
# Runs the full goreleaser snapshot dry-run in one shot: builds the 8 release
# binaries (2 ids × 4 combos, CGO_ENABLED=0), generates the release manifest
# from the built binaries, and archives each combo with the manifest plus
# checksums. `goreleaser release --snapshot` never publishes, so this is safe
# to run locally and in CI.
#
# The manifest generation normally happens inside goreleaser's per-build post
# hooks; this script only exists as a convenience entry point that also
# verifies the output.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${SIFT_RELEASE_VERSION:-0.1.0-dev}"

goreleaser release --snapshot --clean
go run ./tools/release verify --dist dist
echo "release snapshot ok: dist/ contains 4 archives + manifest.json + checksums.txt"
echo "artifacts:"
ls -1 dist/*.tar.gz dist/manifest.json dist/checksums.txt
