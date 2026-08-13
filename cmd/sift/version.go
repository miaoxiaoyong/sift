// `sift version` (issue #939): reports the running release version — the same
// value as `sift --version` — plus whether a newer release exists. It reuses
// the update command's latest-release query (releaseAPIURL /
// fetchLatestRelease) so the two surfaces cannot drift, and fails closed on a
// query error exactly like `sift update --check`. The JSON contract is the
// closed {current, latest, updated} triple shared with update --check, where
// updated here means "an update is available" (current < latest).
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/miaoxiaoyong/sift/internal/version"
)

// runVersion implements `sift version [--json]`. It needs no home and never
// dials the daemon: it prints the release and compares it against the GitHub
// releases/latest tag. A failed latest query is an error (exit 1), consistent
// with `sift update --check`; `sift --version` remains the offline-only
// version surface.
func runVersion(args []string, stdout, stderr io.Writer) int {
	jsonOutput := os.Getenv("SIFT_JSON") == "1"
	for _, a := range args {
		switch a {
		case "--json":
			jsonOutput = true
		default:
			report(stderr, fmt.Errorf("usage: sift version [--json]"))
			return 2
		}
	}
	current := version.Release
	if !version.IsValidSemver(current) {
		report(stderr, fmt.Errorf("当前版本 %q 不是合法 SemVer，无法比较", current))
		return 1
	}
	tag, err := fetchLatestRelease()
	if err != nil {
		report(stderr, err)
		return 1
	}
	latest := strings.TrimPrefix(tag, "v")
	if !version.IsValidSemver(latest) {
		report(stderr, fmt.Errorf("最新版本 %q 不是合法 SemVer，拒绝比较", latest))
		return 1
	}
	cmp := version.Compare(current, latest)
	updated := cmp < 0
	if jsonOutput {
		if err := printJSON(stdout, map[string]any{"current": current, "latest": latest, "updated": updated}); err != nil {
			report(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, versionCompareMessage(current, latest, cmp))
	return 0
}

// versionCompareMessage formats the human version line (issue #939):
// `Sift 0.4.0（最新 0.4.0，已是最新）` / `Sift 0.4.0（有更新 0.5.0，运行
// sift update）`. The latest<current case must never claim "已是最新" (the
// local build is newer than the release latest), mirroring update's rule.
func versionCompareMessage(current, latest string, cmp int) string {
	switch {
	case cmp < 0:
		return fmt.Sprintf("Sift %s（有更新 %s，运行 sift update 升级）\n", current, latest)
	case cmp == 0:
		return fmt.Sprintf("Sift %s（最新 %s，已是最新）\n", current, latest)
	default:
		return fmt.Sprintf("Sift %s（比 release 最新 %s 更新：本地版本）\n", current, latest)
	}
}
