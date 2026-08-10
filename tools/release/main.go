// Command release is the M8 §8.1 release tooling driver. It sits between
// goreleaser's build and archive steps (as a per-build post hook) and after
// the full snapshot pipeline (verify):
//
//	go run ./tools/release manifest --dist dist --version 0.1.0-dev [--allow-partial]
//	go run ./tools/release verify --dist dist
//
// The manifest contract is specs/release.md §2; the install path
// (internal/install) consumes the same closed JSON shape, so the generator and
// the verifier must stay in sync with the structs there.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/version"
)

const (
	schemaVersion = 1
	manifestName  = "manifest.json"
	checksumsName = "checksums.txt"
)

var binaries = []string{"sift", "sift-agent-wrapper"}

var combos = []struct{ goos, goarch string }{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
}

type manifest struct {
	SchemaVersion  int        `json:"schema_version"`
	ReleaseVersion string     `json:"release_version"`
	Artifacts      []artifact `json:"artifacts"`
}

type artifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "manifest":
		err = cmdManifest(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: go run ./tools/release manifest|verify [--dist DIR] [--version V] [--allow-partial]")
}

// cmdManifest scans the goreleaser dist directory and writes manifest.json
// covering every release binary it found. Called from the per-build post hook
// while builds run concurrently, so --allow-partial writes the manifest with
// the combos complete at that moment; the archive step only starts after all
// builds and hooks finish, by which time the last hook has written the full
// matrix.
func cmdManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	dist := fs.String("dist", "dist", "goreleaser dist directory")
	release := fs.String("version", "", "release version (matches the ldflags injection)")
	allowPartial := fs.Bool("allow-partial", false, "write a partial manifest instead of failing when some combos are missing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !version.IsValidSemver(*release) {
		return fmt.Errorf("--version %q is not canonical semver", *release)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	entries, err := scanBinaries(*dist)
	if err != nil {
		return err
	}
	if !*allowPartial && len(entries) != len(binaries)*len(combos) {
		return fmt.Errorf("incomplete dist matrix: found %d of %d binaries", len(entries), len(binaries)*len(combos))
	}
	data, err := json.MarshalIndent(manifest{SchemaVersion: schemaVersion, ReleaseVersion: *release, Artifacts: entries}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(*dist, manifestName)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "release: wrote %s with %d artifacts\n", path, len(entries))
	return nil
}

// scanBinaries locates every release binary in the dist layout
// dist/<binary>_<goos>_<goarch>[_v...]/<binary> and hashes it.
func scanBinaries(dist string) ([]artifact, error) {
	found := map[string]string{} // "goos/goarch/name" -> file path
	for _, bin := range binaries {
		matches, err := filepath.Glob(filepath.Join(dist, bin+"_*"))
		if err != nil {
			return nil, err
		}
		for _, dir := range matches {
			name := filepath.Base(dir)
			goos, goarch, ok := parseComboDir(name, bin)
			if !ok {
				continue
			}
			path := filepath.Join(dir, bin)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			found[goos+"/"+goarch+"/"+bin] = path
		}
	}
	entries := make([]artifact, 0, len(binaries)*len(combos))
	for _, combo := range combos {
		for _, bin := range binaries {
			path, ok := found[combo.goos+"/"+combo.goarch+"/"+bin]
			if !ok {
				continue
			}
			sum, err := fileSHA256(path)
			if err != nil {
				return nil, err
			}
			entries = append(entries, artifact{GOOS: combo.goos, GOARCH: combo.goarch, Name: bin, SHA256: sum})
		}
	}
	return entries, nil
}

// parseComboDir splits dist dir names like sift_darwin_amd64_v1 or
// sift-agent-wrapper_linux_arm64_v8.0 into (goos, goarch), ignoring the
// goamd64/goarm64 suffix that goreleaser appends.
func parseComboDir(dirName, bin string) (goos, goarch string, ok bool) {
	rest, found := strings.CutPrefix(dirName, bin+"_")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "_")
	if len(parts) < 2 {
		return "", "", false
	}
	if !validGOOS(parts[0]) || !validGOARCH(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func validGOOS(s string) bool {
	return s == "darwin" || s == "linux"
}
func validGOARCH(s string) bool {
	return s == "amd64" || s == "arm64"
}

// cmdVerify checks the complete snapshot output: the manifest covers the full
// 8-binary matrix with hashes that match the dist binaries, all four archives
// exist and each carries both binaries plus a manifest whose entries match the
// bytes inside that archive, and checksums.txt hashes match the archives.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dist := fs.String("dist", "dist", "goreleaser dist directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	manifestBytes, err := os.ReadFile(filepath.Join(*dist, manifestName))
	if err != nil {
		return fmt.Errorf("read dist manifest: %w", err)
	}
	var m manifest
	if err := schema.Decode(manifestBytes, &m, schema.Closed); err != nil {
		return fmt.Errorf("dist manifest is not closed-valid: %w", err)
	}
	if m.SchemaVersion != schemaVersion {
		return fmt.Errorf("manifest schema_version %d, want %d", m.SchemaVersion, schemaVersion)
	}
	if !version.IsValidSemver(m.ReleaseVersion) {
		return fmt.Errorf("manifest release_version %q is not canonical semver", m.ReleaseVersion)
	}
	if len(m.Artifacts) != len(binaries)*len(combos) {
		return fmt.Errorf("manifest covers %d of %d expected binaries", len(m.Artifacts), len(binaries)*len(combos))
	}
	seen := map[string]bool{}
	for _, a := range m.Artifacts {
		key := a.GOOS + "/" + a.GOARCH + "/" + a.Name
		if seen[key] {
			return fmt.Errorf("manifest duplicates artifact %s", key)
		}
		seen[key] = true
		if !validGOOS(a.GOOS) || !validGOARCH(a.GOARCH) || !containsStr(binaries, a.Name) {
			return fmt.Errorf("manifest artifact %s is outside the release matrix", key)
		}
		if !sha256Hex.MatchString(a.SHA256) {
			return fmt.Errorf("manifest sha256 for %s is malformed", key)
		}
	}

	// Cross-check the manifest against the dist binaries themselves.
	for _, combo := range combos {
		for _, bin := range binaries {
			entry := manifestEntry(m.Artifacts, combo.goos, combo.goarch, bin)
			if entry == nil {
				return fmt.Errorf("no manifest entry for %s/%s/%s", combo.goos, combo.goarch, bin)
			}
			path, err := findBinary(*dist, combo.goos, combo.goarch, bin)
			if err != nil {
				return err
			}
			sum, err := fileSHA256(path)
			if err != nil {
				return err
			}
			if sum != entry.SHA256 {
				return fmt.Errorf("%s hash %s != manifest %s", path, sum, entry.SHA256)
			}
		}
	}

	// Each combo ships one archive with both binaries and a manifest whose
	// hashes match the bytes inside it.
	archiveNames := map[string]string{}
	for _, combo := range combos {
		name := fmt.Sprintf("sift_%s_%s_%s.tar.gz", m.ReleaseVersion, combo.goos, combo.goarch)
		archiveNames[combo.goos+"/"+combo.goarch] = name
		if _, err := os.Stat(filepath.Join(*dist, name)); err != nil {
			return fmt.Errorf("archive %s: %w", name, err)
		}
		if err := verifyArchive(filepath.Join(*dist, name), m, combo.goos, combo.goarch); err != nil {
			return err
		}
	}

	checksumsPath := filepath.Join(*dist, checksumsName)
	checksumLines, err := os.ReadFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", checksumsName, err)
	}
	byName := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(checksumLines)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("malformed checksum line %q", line)
		}
		byName[filepath.Base(fields[1])] = fields[0]
	}
	for _, name := range archiveNames {
		sum, err := fileSHA256(filepath.Join(*dist, name))
		if err != nil {
			return err
		}
		if byName[name] != sum {
			return fmt.Errorf("checksums.txt mismatch for %s (recorded %s, actual %s)", name, byName[name], sum)
		}
	}
	fmt.Fprintf(os.Stderr, "release: verify ok (%d artifacts, %d archives, checksums match)\n", len(m.Artifacts), len(archiveNames))
	return nil
}

// verifyArchive streams one archive and checks that it carries both release
// binaries and a manifest whose sha256 entries for this combo match the
// extracted bytes.
func verifyArchive(path string, m manifest, goos, goarch string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("archive %s: %w", path, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	got := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("archive %s: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive %s contains non-regular entry %q", path, hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("archive %s: read %s: %w", path, hdr.Name, err)
		}
		got[hdr.Name] = data
	}
	for _, bin := range binaries {
		data, ok := got[bin]
		if !ok {
			return fmt.Errorf("archive %s misses binary %s", path, bin)
		}
		entry := manifestEntry(m.Artifacts, goos, goarch, bin)
		if entry == nil {
			return fmt.Errorf("archive %s: no manifest entry for %s/%s/%s", path, goos, goarch, bin)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			return fmt.Errorf("archive %s: %s hash does not match its manifest entry", path, bin)
		}
	}
	inArchive, ok := got[manifestName]
	if !ok {
		return fmt.Errorf("archive %s misses %s", path, manifestName)
	}
	var inner manifest
	if err := schema.Decode(inArchive, &inner, schema.Closed); err != nil {
		return fmt.Errorf("archive %s: embedded manifest is not closed-valid: %w", path, err)
	}
	if inner.ReleaseVersion != m.ReleaseVersion {
		return fmt.Errorf("archive %s: embedded manifest release %q != %q", path, inner.ReleaseVersion, m.ReleaseVersion)
	}
	return nil
}

func findBinary(dist, goos, goarch, bin string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dist, fmt.Sprintf("%s_%s_%s_*", bin, goos, goarch)))
	if err != nil {
		return "", err
	}
	for _, dir := range matches {
		path := filepath.Join(dir, bin)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", fmt.Errorf("binary %s/%s/%s not found in %s", goos, goarch, bin, dist)
}

func manifestEntry(artifacts []artifact, goos, goarch, name string) *artifact {
	for i := range artifacts {
		if artifacts[i].GOOS == goos && artifacts[i].GOARCH == goarch && artifacts[i].Name == name {
			return &artifacts[i]
		}
	}
	return nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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
