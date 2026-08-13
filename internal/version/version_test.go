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
		"1.0.0-alpha.1", "1.0.0-0a", "1.0.0-x-y-z.--", "1.0.0+0build.1-rc.10000aaa-kk-0.1",
	} {
		if !IsValidSemver(good) {
			t.Errorf("%q should be valid", good)
		}
	}
	for _, bad := range []string{
		"", "dev", "0.1", "0.1.0.1", "v0.1.0", "01.2.3", "0.1.0-", "0.1.0/../evil",
		"0.1.0-dev\n", "1.0.0-", "abc", "../0.1.0", "0.1.0 ",
		// SemVer 2.0.0 explicitly forbids numeric pre-release identifiers with
		// leading zeroes and empty dot-separated identifiers in either the
		// pre-release or the build metadata section.
		"0.1.0-01", "1.0.0-00", "1.0.0-alpha.01", "0.1.0-alpha..x", "0.1.0+foo..bar",
		"0.1.0-alpha.", "0.1.0+", "0.1.0+foo.", "1.0.0-alpha_beta", "1.0.0-rc.1+build+2",
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

// TestComparePinsSemverPrecedence runs the full ordering chain from semver.org
// §11 (example at the end of the precedence section) plus the explicit
// major/minor/patch and equality cases the update command relies on.
func TestComparePinsSemverPrecedence(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
	}
	for i, a := range ordered {
		for j, b := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", a, b, got, want)
			}
			if got := Compare(b, a); got != -want {
				t.Errorf("Compare(%q, %q) = %d, want %d", b, a, got, -want)
			}
		}
	}
}

func TestCompareCoreNumericOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},     // minor
		{"0.2.0", "0.10.0", -1},    // numeric, not lexicographic
		{"1.2.3", "1.2.4", -1},     // patch
		{"2.0.0", "1.99.99", 1},    // major wins over minor/patch
		{"1.0.0", "1.0.0", 0},      // equal
		{"0.1.0-dev", "0.1.0", -1}, // pre-release below release
		{"0.1.0", "0.1.0-dev", 1},
		{"0.1.0-2", "0.1.0-11", -1},       // numeric identifiers compared numerically
		{"0.1.0-1", "0.1.0-alpha", -1},    // numeric < alphanumeric
		{"0.1.0-alpha", "0.1.0-ALPHA", 1}, // ASCII order (A < a)
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCompareIgnoresBuildMetadata(t *testing.T) {
	// Build metadata never participates in precedence (§11); the cores must
	// compare equal regardless of the metadata halves.
	for _, a := range []string{"1.0.0", "1.0.0+build.1", "1.0.0+0build.1-rc.10000aaa-kk-0.1"} {
		for _, b := range []string{"1.0.0", "1.0.0+build.2", "1.0.0+foo"} {
			if got := Compare(a, b); got != 0 {
				t.Errorf("Compare(%q, %q) = %d, want 0", a, b, got)
			}
		}
	}
}

func TestCompareInvalidInputsCompareEqual(t *testing.T) {
	// Compare's contract is canonical SemVer; the zero is the documented
	// placeholder for invalid input, and the update command rejects invalid
	// versions with IsValidSemver before it ever reaches Compare.
	for _, tc := range []struct {
		a, b string
	}{
		{"abc", "1.0.0"},
		{"v1.0.0", "1.0.0"},
		{"0.1", "1.0.0"},
		{"", ""},
	} {
		if got := Compare(tc.a, tc.b); got != 0 {
			t.Errorf("Compare(%q, %q) = %d, want 0", tc.a, tc.b, got)
		}
	}
}
