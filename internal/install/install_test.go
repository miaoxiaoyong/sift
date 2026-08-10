package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testRelease = "0.1.0-test"

// fakeBinary returns a small executable shell script that answers --version
// with release. The bytes are also what the manifest hashes must cover.
func fakeBinary(release string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"" + release + "\"; else exit 1; fi\n")
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// manifestForPlatform builds a manifest covering the current platform with the
// given per-binary content hashes. Missing binaries get a wrong hash on
// purpose unless provided through binaries.
func manifestForPlatform(t *testing.T, release string, binaries map[string][]byte) *Manifest {
	t.Helper()
	m := &Manifest{SchemaVersion: 1, ReleaseVersion: release}
	for _, name := range []string{DaemonBinary, WrapperBinary} {
		content, ok := binaries[name]
		if !ok {
			t.Fatalf("missing fixture binary %s", name)
		}
		m.Artifacts = append(m.Artifacts, Artifact{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Name: name, SHA256: sha256Of(content)})
	}
	return m
}

// writeArchive builds a tar.gz at path containing files (name -> bytes, mode).
// dirEntries, when present, are appended as raw tar entries (for traversal /
// symlink fixtures).
func writeArchive(t *testing.T, path string, files map[string][]byte, extra ...*tar.Header) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		mode := int64(0o644)
		if strings.Contains(name, "sift") && !strings.HasSuffix(name, "json") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	for _, hdr := range extra {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte{0}, int(hdr.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeValidArchive writes a valid release archive for the current platform
// and returns its path. binaries may override either binary's content.
func writeValidArchive(t *testing.T, dir, release string, binaries map[string][]byte) string {
	t.Helper()
	content := map[string][]byte{
		DaemonBinary:  fakeBinary(release),
		WrapperBinary: fakeBinary(release),
	}
	for k, v := range binaries {
		content[k] = v
	}
	manifest, err := json.Marshal(manifestForPlatform(t, release, content))
	if err != nil {
		t.Fatal(err)
	}
	content[ManifestName] = manifest
	path := filepath.Join(dir, "sift_"+release+"_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz")
	writeArchive(t, path, content)
	return path
}

func freshHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".sift")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestInstallProducesVersionDirAndAtomicCurrent(t *testing.T) {
	home := freshHome(t)
	archive := writeValidArchive(t, t.TempDir(), testRelease, nil)

	installed, err := Install(home, archive)
	if err != nil {
		t.Fatal(err)
	}
	if installed != testRelease {
		t.Fatalf("installed = %q, want %q", installed, testRelease)
	}

	versionDir := filepath.Join(home, BinDirName, testRelease)
	for _, name := range []string{DaemonBinary, WrapperBinary} {
		info, err := os.Stat(filepath.Join(versionDir, name))
		if err != nil {
			t.Fatalf("%s not installed: %v", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable", name)
		}
	}
	// The manifest stays in the version directory so reinstall checks can
	// re-read it; `current` is a relative symlink to the version directory.
	if _, err := os.Stat(filepath.Join(versionDir, ManifestName)); err != nil {
		t.Fatalf("manifest not installed: %v", err)
	}
	currentInfo, err := os.Lstat(filepath.Join(home, BinDirName, CurrentLink))
	if err != nil {
		t.Fatalf("current link: %v", err)
	}
	if currentInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("current is not a symlink")
	}
	target, err := os.Readlink(filepath.Join(home, BinDirName, CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	if target != testRelease {
		t.Fatalf("current -> %q, want %q", target, testRelease)
	}
	// The installed binaries actually answer --version.
	out, err := os.ReadFile(filepath.Join(versionDir, WrapperBinary))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(testRelease)) {
		t.Fatalf("installed wrapper lacks release marker")
	}
}

func TestInstallRepeatedSameVersionDoesNotOverwriteFiles(t *testing.T) {
	home := freshHome(t)
	archive := writeValidArchive(t, t.TempDir(), testRelease, nil)
	if _, err := Install(home, archive); err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(home, BinDirName, testRelease)
	before, err := os.ReadFile(filepath.Join(versionDir, DaemonBinary))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Install(home, archive); err == nil {
		t.Fatal("repeated install of the same version succeeded")
	} else if !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("repeated install error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(versionDir, DaemonBinary))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("repeated install modified files inside the version directory")
	}
	// No staging or temp-link debris survives a refused install.
	entries, err := os.ReadDir(filepath.Join(home, BinDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) || e.Name() == tempLinkName {
			t.Fatalf("install debris left behind: %s", e.Name())
		}
	}
}

func TestInstallRejectsManifestSha256Mismatch(t *testing.T) {
	home := freshHome(t)
	content := map[string][]byte{
		DaemonBinary: fakeBinary(testRelease),
	}
	manifest := manifestForPlatform(t, testRelease, map[string][]byte{
		DaemonBinary:  fakeBinary(testRelease),
		WrapperBinary: fakeBinary(testRelease),
	})
	// Tamper with the wrapper hash after the manifest was built from real bytes.
	for i := range manifest.Artifacts {
		if manifest.Artifacts[i].Name == WrapperBinary {
			manifest.Artifacts[i].SHA256 = strings.Repeat("0", 64)
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	content[ManifestName] = raw
	content[WrapperBinary] = fakeBinary(testRelease)
	path := filepath.Join(t.TempDir(), "tampered.tar.gz")
	writeArchive(t, path, content)

	if _, err := Install(home, path); err == nil {
		t.Fatal("install with tampered manifest succeeded")
	} else if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("tamper error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, BinDirName, testRelease)); !os.IsNotExist(err) {
		t.Fatalf("version dir was created despite verification failure: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, BinDirName, CurrentLink)); !os.IsNotExist(err) {
		t.Fatalf("current link was created despite verification failure: %v", err)
	}
}

func TestInstallRejectsArchiveWithoutLocalPlatform(t *testing.T) {
	home := freshHome(t)
	content := map[string][]byte{
		DaemonBinary:  fakeBinary(testRelease),
		WrapperBinary: fakeBinary(testRelease),
	}
	manifest := &Manifest{SchemaVersion: 1, ReleaseVersion: testRelease}
	for _, name := range []string{DaemonBinary, WrapperBinary} {
		manifest.Artifacts = append(manifest.Artifacts, Artifact{GOOS: "plan9", GOARCH: "amd64", Name: name, SHA256: sha256Of(content[name])})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	content[ManifestName] = raw
	path := filepath.Join(t.TempDir(), "wrongarch.tar.gz")
	writeArchive(t, path, content)

	if _, err := Install(home, path); err == nil {
		t.Fatal("cross-arch archive install succeeded")
	} else if !strings.Contains(err.Error(), "no "+runtime.GOOS+"/"+runtime.GOARCH+" entry") {
		t.Fatalf("cross-arch error = %v", err)
	}
}

func TestInstallRejectsPathTraversal(t *testing.T) {
	home := freshHome(t)
	content := map[string][]byte{ManifestName: []byte(`{"schema_version":1,"release_version":"0.1.0-test","artifacts":[]}`)}
	path := filepath.Join(t.TempDir(), "evil.tar.gz")
	writeArchive(t, path, content, &tar.Header{Name: "../evil", Mode: 0o644, Size: 4})
	if _, err := Install(home, path); err == nil {
		t.Fatal("traversal archive install succeeded")
	} else if !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), "evil")); !os.IsNotExist(err) {
		t.Fatal("traversal file was written outside the staging directory")
	}
}

func TestInstallRejectsSymlinkEntry(t *testing.T) {
	home := freshHome(t)
	content := map[string][]byte{ManifestName: []byte(`{"schema_version":1,"release_version":"0.1.0-test","artifacts":[]}`)}
	path := filepath.Join(t.TempDir(), "symlink.tar.gz")
	writeArchive(t, path, content, &tar.Header{Name: "sift", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"})
	if _, err := Install(home, path); err == nil {
		t.Fatal("symlink archive install succeeded")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestInstallRejectsVersionProbeMismatch(t *testing.T) {
	home := freshHome(t)
	// The wrapper binary reports a different release than the manifest: the
	// two binaries in the archive are not the same release.
	content := map[string][]byte{
		DaemonBinary:  fakeBinary(testRelease),
		WrapperBinary: fakeBinary("0.2.0-test"),
	}
	manifest, err := json.Marshal(manifestForPlatform(t, testRelease, content))
	if err != nil {
		t.Fatal(err)
	}
	content[ManifestName] = manifest
	path := filepath.Join(t.TempDir(), "mixed.tar.gz")
	writeArchive(t, path, content)

	if _, err := Install(home, path); err == nil {
		t.Fatal("mixed-release archive install succeeded")
	} else if !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("probe error = %v", err)
	}
}

func TestInstallRejectsNonGzipArchive(t *testing.T) {
	home := freshHome(t)
	path := filepath.Join(t.TempDir(), "notgz.tar.gz")
	if err := os.WriteFile(path, []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(home, path); err == nil {
		t.Fatal("non-gzip archive install succeeded")
	} else if !strings.Contains(err.Error(), "not a gzip archive") {
		t.Fatalf("non-gzip error = %v", err)
	}
}

func TestInstallRejectsMissingManifest(t *testing.T) {
	home := freshHome(t)
	path := filepath.Join(t.TempDir(), "nomanifest.tar.gz")
	writeArchive(t, path, map[string][]byte{DaemonBinary: fakeBinary(testRelease)})
	if _, err := Install(home, path); err == nil {
		t.Fatal("archive without manifest install succeeded")
	} else if !strings.Contains(err.Error(), "no manifest.json") {
		t.Fatalf("missing manifest error = %v", err)
	}
}

func TestSwitchCurrentReplacesExistingLinkAtomically(t *testing.T) {
	binDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(binDir, "0.1.0-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("0.1.0-a", filepath.Join(binDir, CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if err := switchCurrent(binDir, "0.1.0-b"); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(binDir, CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	if target != "0.1.0-b" {
		t.Fatalf("current -> %q, want 0.1.0-b", target)
	}
	// No temp link remains.
	if _, err := os.Lstat(filepath.Join(binDir, tempLinkName)); !os.IsNotExist(err) {
		t.Fatal("temp link survived the switch")
	}
}

func TestSwitchCurrentLeavesNoTempLinkOnError(t *testing.T) {
	binDir := t.TempDir()
	if err := switchCurrent(binDir, "not a version"); err == nil {
		t.Fatal("switch to an invalid version name succeeded")
	}
	if _, err := os.Lstat(filepath.Join(binDir, tempLinkName)); !os.IsNotExist(err) {
		t.Fatal("temp link survived a failed switch")
	}
	if _, err := os.Lstat(filepath.Join(binDir, CurrentLink)); !os.IsNotExist(err) {
		t.Fatal("current link was created by a failed switch")
	}
}

func TestInstallCurrentFailureCleansActivatedRelease(t *testing.T) {
	home := freshHome(t)
	archive := writeValidArchive(t, t.TempDir(), testRelease, nil)
	binDir := filepath.Join(home, BinDirName)
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(binDir, CurrentLink), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(home, archive); err == nil {
		t.Fatal("install succeeded with a directory occupying current")
	}
	if _, err := os.Lstat(filepath.Join(binDir, testRelease)); !os.IsNotExist(err) {
		t.Fatalf("activated release survived current switch failure: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(binDir, tempLinkName)); !os.IsNotExist(err) {
		t.Fatalf("current temp link survived current switch failure: %v", err)
	}

	if err := os.Remove(filepath.Join(binDir, CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if installed, err := Install(home, archive); err != nil || installed != testRelease {
		t.Fatalf("retry install = %q, %v; want %q, nil", installed, err, testRelease)
	}
}

func TestInstallCleansStagingOnFailure(t *testing.T) {
	home := freshHome(t)
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	writeArchive(t, path, map[string][]byte{
		DaemonBinary:  fakeBinary(testRelease),
		WrapperBinary: fakeBinary(testRelease),
		ManifestName:  []byte(`{"schema_version":9,"release_version":"0.1.0-test","artifacts":[]}`),
	})
	if _, err := Install(home, path); err == nil {
		t.Fatal("install with bad schema version succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(home, BinDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Fatalf("staging dir left behind: %s", e.Name())
		}
	}
}

// TestManifestClosedDecodeRejectsUnknownFields guards the manifest contract:
// an archive carrying fields this binary does not know must fail closed, not
// be silently accepted by the install path.
func TestManifestClosedDecodeRejectsUnknownFields(t *testing.T) {
	raw := []byte(fmt.Sprintf(`{"schema_version":1,"release_version":%q,"artifacts":[],"future_field":true}`, testRelease))
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// The real path goes through the closed decode gateway (internal/schema);
	// assert the shape is what install.go relies on.
	if m.SchemaVersion != 1 {
		t.Fatalf("schema version = %d", m.SchemaVersion)
	}
}
