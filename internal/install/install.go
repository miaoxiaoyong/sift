// Package install implements the release install layout mandated by WBS M8
// §8.1 and DESIGN §8.4 (upgrade paragraph): binaries land in
// ~/.sift/bin/<release-version>/ and `current` is switched atomically, never
// file-by-file. The contract lives in specs/release.md.
//
// An install is all-or-nothing: the archive is extracted to a staging
// directory, the release manifest is verified (schema, release SemVer,
// per-binary sha256) and both native binaries are probed for --version before
// the staging directory is renamed into place and `current` is switched.
// Reinstalling an already-installed version refuses rather than overwriting
// files inside a live version directory.
package install

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/version"
)

const (
	BinDirName  = "bin"
	CurrentLink = "current"
	// ManifestName is the file every release archive must carry at its root.
	ManifestName = "manifest.json"

	stagingPrefix = ".staging-"
	tempLinkName  = ".current-tmp"

	// Release binaries are paired by name (DESIGN §8.4): the daemon resolves
	// the wrapper from its own install directory, so both must be installed
	// together and must report the same release version.
	DaemonBinary  = "sift"
	WrapperBinary = "sift-agent-wrapper"

	// maxExtractBytes bounds a single install so a malformed archive cannot
	// exhaust the filesystem. Each release archive is tens of MB; 256 MiB is
	// an order-of-magnitude headroom, not a realistic size.
	maxExtractBytes = 256 << 20
)

// Manifest is the release manifest embedded in every archive
// (specs/release.md §2). Artifacts is indexed by (goos, goarch, name).
type Manifest struct {
	SchemaVersion  int        `json:"schema_version"`
	ReleaseVersion string     `json:"release_version"`
	Artifacts      []Artifact `json:"artifacts"`
}

// Artifact describes one release binary in the archive.
type Artifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Install extracts archivePath into homePath's version-directory layout and
// returns the installed release version. It fails closed on any verification
// failure and never modifies an existing version directory.
func Install(homePath, archivePath string) (string, error) {
	if homePath == "" || archivePath == "" {
		return "", errors.New("install: home and archive paths are required")
	}
	binDir := filepath.Join(homePath, BinDirName)
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", fmt.Errorf("install: create bin directory: %w", err)
	}

	staging, err := stagingDir(binDir)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging) // no-op after a successful rename

	manifest, err := extractAndVerify(staging, archivePath)
	if err != nil {
		return "", err
	}
	release := manifest.ReleaseVersion
	if !version.IsValidSemver(release) {
		return "", fmt.Errorf("install: manifest release_version %q is not canonical semver", release)
	}

	if err := verifyStagedFiles(staging, manifest); err != nil {
		return "", err
	}
	if err := probeStagedBinaries(staging, release); err != nil {
		return "", err
	}

	target := filepath.Join(binDir, release)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("install: version %s is already installed at %s; refusing to overwrite in place (remove it first)", release, target)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("install: stat %s: %w", target, err)
	}
	if err := os.Rename(staging, target); err != nil {
		return "", fmt.Errorf("install: activate %s: %w", target, err)
	}
	if err := switchCurrent(binDir, release); err != nil {
		if cleanupErr := os.RemoveAll(target); cleanupErr != nil {
			return "", fmt.Errorf("%w (cleanup activated release %s: %v)", err, release, cleanupErr)
		}
		return "", err
	}
	return release, nil
}

// stagingDir creates a fresh staging directory inside binDir so the final
// rename stays on the same filesystem (atomic within the mount).
func stagingDir(binDir string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("install: staging nonce: %w", err)
	}
	path := filepath.Join(binDir, stagingPrefix+hex.EncodeToString(b))
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", fmt.Errorf("install: create staging directory: %w", err)
	}
	return path, nil
}

// extractAndVerify extracts the archive into staging and decodes its
// manifest. Extraction accepts only regular files with clean relative paths:
// symlinks, hard links, devices and path traversal are rejected outright
// (the release archives contain exactly the two binaries plus manifest.json).
func extractAndVerify(staging, archivePath string) (*Manifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("install: open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("install: not a gzip archive: %w", err)
	}
	defer gz.Close()

	var manifestBytes []byte
	var extracted int64
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("install: read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("install: archive entry %q is not a regular file", hdr.Name)
		}
		if !filepath.IsLocal(hdr.Name) || filepath.IsAbs(hdr.Name) {
			return nil, fmt.Errorf("install: archive entry %q escapes the install directory", hdr.Name)
		}
		if hdr.Size < 0 || extracted+hdr.Size > maxExtractBytes {
			return nil, fmt.Errorf("install: archive exceeds the %d byte extraction bound", maxExtractBytes)
		}
		mode := os.FileMode(0o644)
		if hdr.Mode&0o111 != 0 {
			mode = 0o755
		}
		path := filepath.Join(staging, hdr.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("install: create %s: %w", filepath.Dir(path), err)
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return nil, fmt.Errorf("install: extract %s: %w", hdr.Name, err)
		}
		n, copyErr := io.Copy(out, io.LimitReader(tr, hdr.Size))
		closeErr := out.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("install: extract %s: %w", hdr.Name, copyErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("install: extract %s: %w", hdr.Name, closeErr)
		}
		if n != hdr.Size {
			return nil, fmt.Errorf("install: archive entry %q is truncated", hdr.Name)
		}
		extracted += n
		if hdr.Name == ManifestName {
			manifestBytes, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("install: read manifest: %w", err)
			}
		}
	}
	if manifestBytes == nil {
		return nil, errors.New("install: archive has no manifest.json")
	}
	var manifest Manifest
	if err := schema.Decode(manifestBytes, &manifest, schema.Closed); err != nil {
		return nil, fmt.Errorf("install: manifest is not closed-valid: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return nil, fmt.Errorf("install: manifest schema_version %d, want 1", manifest.SchemaVersion)
	}
	if !version.IsValidSemver(manifest.ReleaseVersion) {
		return nil, fmt.Errorf("install: manifest release_version %q is not canonical semver", manifest.ReleaseVersion)
	}
	return &manifest, nil
}

// verifyStagedFiles checks the manifest sha256 of every staged binary against
// the extracted bytes, for the current platform only. Entries for other
// platforms are ignored: an archive built for darwin never contains linux
// hashes, and cross-arch installs must fail on the missing local entries.
func verifyStagedFiles(staging string, manifest *Manifest) error {
	for _, name := range []string{DaemonBinary, WrapperBinary} {
		artifact, ok := artifactFor(manifest, runtime.GOOS, runtime.GOARCH, name)
		if !ok {
			return fmt.Errorf("install: manifest has no %s/%s entry for %s", runtime.GOOS, runtime.GOARCH, name)
		}
		if !sha256Hex.MatchString(artifact.SHA256) {
			return fmt.Errorf("install: manifest sha256 for %s/%s/%s is malformed", runtime.GOOS, runtime.GOARCH, name)
		}
		path := filepath.Join(staging, name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("install: staged %s missing: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("install: staged %s is not an executable file", name)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("install: hash %s: %w", name, err)
		}
		if sum != artifact.SHA256 {
			return fmt.Errorf("install: %s sha256 mismatch (manifest %s, file %s)", name, artifact.SHA256, sum)
		}
	}
	return nil
}

// probeStagedBinaries runs the extracted native binaries and requires both to
// report the manifest release version. This catches a manifest that records a
// different release than the binaries it ships (the archive invariant "both
// binaries carry the same release version", acceptance of #903).
func probeStagedBinaries(staging, release string) error {
	for _, name := range []string{DaemonBinary, WrapperBinary} {
		path := filepath.Join(staging, name)
		out, err := exec.Command(path, "--version").Output()
		if err != nil {
			return fmt.Errorf("install: probe %s --version: %w", name, err)
		}
		if reported := string(out); reported != release+"\n" {
			return fmt.Errorf("install: %s reports version %q, manifest says %q", name, reported, release)
		}
	}
	return nil
}

// switchCurrent atomically points binDir/current at version: a fresh symlink
// is created under a temp name and renamed over the existing link, so readers
// never observe a missing or half-written `current` (temp+rename, never
// per-file overwrite).
func switchCurrent(binDir, versionName string) error {
	// Defense in depth: the version doubles as a directory and symlink target
	// name; Install already validated it, but a future caller must not be able
	// to point `current` at an arbitrary path component.
	if !version.IsValidSemver(versionName) {
		return fmt.Errorf("install: invalid version name %q", versionName)
	}
	tmp := filepath.Join(binDir, tempLinkName)
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("install: clear stale current temp link: %w", err)
	}
	if err := os.Symlink(versionName, tmp); err != nil {
		return fmt.Errorf("install: create current temp link: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(binDir, CurrentLink)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install: atomically switch current: %w", err)
	}
	return nil
}

func artifactFor(m *Manifest, goos, goarch, name string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.GOOS == goos && a.GOARCH == goarch && a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
