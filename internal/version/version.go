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
//	-X github.com/miaoxiaoyong/sift/internal/version.Release={{.Version}}
//
// The CLI (`sift --version`), the wrapper (`sift-agent-wrapper --version`)
// and the daemon (doctor + every RPC envelope) all report the same Release
// value, and the wrapper/daemon handshake compares it as the binary version.
package version

import "regexp"

// Release is the semver release version of the sift binaries. Overridden by
// release builds through ldflags; never change the default without updating
// .goreleaser.yml's snapshot version_template and this package's tests.
var Release = "0.1.0-dev"

// semver accepts canonical SemVer 2.0.0 (major.minor.patch with optional
// pre-release and build metadata). The same grammar constrains the release
// version everywhere: wrapper resolution (internal/runtime), bootstrap
// handshake fields, install version directories and the release manifest.
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// IsValidSemver reports whether s is a canonical SemVer accepted by the
// release handshake and the install layout.
func IsValidSemver(s string) bool { return semver.MatchString(s) }
