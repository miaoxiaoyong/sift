// Package version holds the Sift release version.
//
// The release version is deliberately separate from the two protocol
// versions that already existed before M8:
//
//   - config.Version (internal/config) is the config file protocol version and
//     stays 1; this package never changes it.
//   - controlplane.ProtocolMajor/Minor are the wire protocol version and stay
//     1/0.
//
// Release is a canonical SemVer for the shipped binaries. Dev and snapshot
// builds keep the placeholder default; release builds override it via ldflags
// -X from goreleaser (`.goreleaser.yml`):
//
//	-X github.com/xsift/sift/internal/version.Release={{.Version}}
//
// The CLI (`sift --version`), the wrapper (`sift-agent-wrapper --version`)
// and the daemon (doctor + every RPC envelope) all report the same Release
// value, and the wrapper/daemon handshake compares it as the binary version.
package version

import (
	"regexp"
	"strings"
)

// Release is the semver release version of the sift binaries. Overridden by
// release builds through ldflags; never change the default without updating
// .goreleaser.yml's snapshot version_template and this package's tests.
var Release = "0.1.0-dev"

// semver accepts canonical SemVer 2.0.0 (major.minor.patch with optional
// pre-release and build metadata), exactly as specified by semver.org:
// numeric identifiers never carry leading zeroes, dot-separated identifiers
// are never empty, and no identifier characters other than [0-9A-Za-z-].
// The same grammar constrains the release version everywhere: wrapper
// resolution (internal/runtime), bootstrap handshake fields, install
// version directories and the release manifest.
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?` +
	`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// IsValidSemver reports whether s is a canonical SemVer accepted by the
// release handshake and the install layout.
func IsValidSemver(s string) bool { return semver.MatchString(s) }

// Compare returns -1, 0 or +1 comparing two canonical SemVer 2.0.0 strings
// by the precedence rules of semver.org §11: major.minor.patch numerically,
// pre-release identifiers dot-separated (numeric identifiers sort before
// alphanumeric ones, numeric identifiers numerically, alphanumeric ones in
// ASCII order, and a shorter pre-release before a longer one sharing all its
// identifiers), and a pre-release below the same core without one. Build
// metadata is ignored. Inputs that are not canonical SemVer (IsValidSemver
// false) compare equal; a fail-closed caller must reject them before
// comparing rather than trusting the zero.
func Compare(a, b string) int {
	ma := semver.FindStringSubmatch(a)
	mb := semver.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0
	}
	for i := 1; i <= 3; i++ {
		if c := compareNumeric(ma[i], mb[i]); c != 0 {
			return c
		}
	}
	// A release core outranks the same core with a pre-release; build
	// metadata never participates (it is not captured by the regex).
	switch {
	case ma[4] == "" && mb[4] == "":
		return 0
	case ma[4] == "":
		return 1
	case mb[4] == "":
		return -1
	}
	return comparePrerelease(ma[4], mb[4])
}

// compareNumeric compares two non-negative integer identifiers that carry no
// leading zeroes exactly as numbers. Comparing length then byte order is
// exact for arbitrary precision and avoids strconv overflow on absurd cores.
func compareNumeric(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePrerelease orders two non-empty pre-release strings per §11.
func comparePrerelease(a, b string) int {
	ai, bi := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		x, y := ai[i], bi[i]
		if isNumericID(x) && isNumericID(y) {
			if c := compareNumeric(x, y); c != 0 {
				return c
			}
			continue
		}
		if isNumericID(x) != isNumericID(y) {
			if isNumericID(x) {
				return -1 // numeric identifiers have lower precedence
			}
			return 1
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	if len(ai) < len(bi) {
		return -1 // the shorter pre-release has lower precedence
	}
	if len(ai) > len(bi) {
		return 1
	}
	return 0
}

// isNumericID reports whether a pre-release identifier is purely numeric.
// The grammar forbids leading zeroes, so numeric identifiers compare exactly.
func isNumericID(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
