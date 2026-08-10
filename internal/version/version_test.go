package version

import "testing"

func TestReleaseDefaultIsCanonicalSemver(t *testing.T) {
	// The default is what dev and `goreleaser release --snapshot` builds
	// inject. Every consumer (wrapper resolution, bootstrap handshake, install
	// directories, release manifest) rejects non-SemVer values, so the default
	// must stay parseable.
	if !IsValidSemver(Release) {
		t.Fatalf("default release version %q is not canonical semver", Release)
	}
}

func TestIsValidSemver(t *testing.T) {
	for _, good := range []string{
		"0.1.0", "0.1.0-dev", "0.1.0-dev.1", "0.1.0+build.5", "1.0.0", "10.20.30-rc.1+build.2",
	} {
		if !IsValidSemver(good) {
			t.Errorf("%q should be valid", good)
		}
	}
	for _, bad := range []string{
		"", "dev", "0.1", "0.1.0.1", "v0.1.0", "01.2.3", "0.1.0-", "0.1.0/../evil",
		"0.1.0-dev\n", "1.0.0-", "abc", "../0.1.0", "0.1.0 ",
	} {
		if IsValidSemver(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestIsValidSemverRejectsPathSeparators(t *testing.T) {
	// Version doubles as an install directory name; separators must never
	// parse as valid.
	for _, s := range []string{"0.1.0/..", "../0.1.0", "0.1.0\\..", "a/b"} {
		if IsValidSemver(s) {
			t.Errorf("path-like version %q must be rejected", s)
		}
	}
}
