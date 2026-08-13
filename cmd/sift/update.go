// Command surface for `sift update`: in-place self-upgrade of an already
// installed release. It reuses the install.sh contract (release API -> tag_name,
// per-combo archive + checksums.txt, fail-closed sha256) and hands the verified
// archive to internal/install, which owns the version-directory layout and the
// atomic `current` switch (specs/release.md §3). Only the download/verify step
// is new here; the layout, config schema, daemon and RPC protocol are untouched.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/install"
	"github.com/miaoxiaoyong/sift/internal/version"
)

// releaseEndpoints locate the GitHub release metadata and download assets.
// They are variables so tests can point them at an httptest server and run
// `sift update` hermetically.
var (
	releaseAPIURL          = "https://api.github.com/repos/miaoxiaoyong/sift/releases/latest"
	releaseDownloadBaseURL = "https://github.com/miaoxiaoyong/sift/releases/download"
)

const (
	// maxArchiveBytes bounds a download so a misbehaving server cannot fill
	// the disk; each release archive is tens of MB.
	maxArchiveBytes = 256 << 20
	// maxMetadataBytes bounds the JSON release response and checksums.txt.
	maxMetadataBytes = 1 << 20
	// checksumAttempts mirrors install.sh's `curl --retry 3` for transient
	// network failures; backoff stays small so hermetically-served tests
	// never block.
	checksumAttempts = 3
)

var updateChecksumEntry = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// runUpdate implements `sift update`. It installs the --version-pinned
// release unconditionally when given (install.sh pin contract: any
// explicitly specified version is downloaded and installed, including older
// ones); otherwise it compares the running release (version.Release) against
// the latest GitHub release. It downloads the per-platform archive plus
// checksums.txt, verifies the sha256 fail-closed, and delegates the install
// to internal/install.Install.
func runUpdate(args []string, home config.Home, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "only report current vs latest, do not install")
	pinned := fs.String("version", "", "pin the release version to download (default: latest)")
	force := fs.Bool("force", false, "reinstall even when the version is already current")
	jsonFlag := fs.Bool("json", false, "emit machine-readable output")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		report(stderr, fmt.Errorf("usage: sift update [--check] [--version X] [--force] [--json]"))
		return 2
	}
	jsonOutput := os.Getenv("SIFT_JSON") == "1" || *jsonFlag

	current := version.Release
	if !version.IsValidSemver(current) {
		report(stderr, fmt.Errorf("当前版本 %q 不是合法 SemVer，无法比较", current))
		return 1
	}

	latest := *pinned
	if latest != "" {
		latest = strings.TrimPrefix(latest, "v")
		if !version.IsValidSemver(latest) {
			report(stderr, fmt.Errorf("目标版本 %q 不是合法 SemVer，拒绝更新", *pinned))
			return 1
		}
	} else {
		tag, err := fetchLatestRelease()
		if err != nil {
			report(stderr, err)
			return 1
		}
		latest = strings.TrimPrefix(tag, "v")
	}
	if !version.IsValidSemver(latest) {
		report(stderr, fmt.Errorf("目标版本 %q 不是合法 SemVer，拒绝更新", latest))
		return 1
	}

	cmp := version.Compare(current, latest)
	// Pin semantics (install.sh contract): `--version X` installs X
	// unconditionally, so an explicitly pinned version is never blocked by
	// the newer gate and may be older than the running release. The newer
	// gate applies only when tracking the latest release (no --version).
	pinnedExplicit := *pinned != ""
	if *check {
		if jsonOutput {
			return emitUpdateJSON(stdout, stderr, current, latest, false)
		}
		fmt.Fprint(stdout, updateCompareMessage(current, latest, cmp))
		return 0
	}

	if !pinnedExplicit && cmp >= 0 && !*force {
		if jsonOutput {
			return emitUpdateJSON(stdout, stderr, current, latest, false)
		}
		fmt.Fprint(stdout, updateCompareMessage(current, latest, cmp))
		return 0
	}

	goos, goarch, err := releasePlatform()
	if err != nil {
		report(stderr, err)
		return 1
	}
	if !jsonOutput {
		if pinnedExplicit {
			fmt.Fprintf(stdout, "当前 %s → 目标 %s，正在安装…\n", current, latest)
		} else {
			fmt.Fprintf(stdout, "当前 %s → 最新 %s，正在升级…\n", current, latest)
		}
	}

	archive := fmt.Sprintf("sift_%s_%s_%s.tar.gz", latest, goos, goarch)
	tmp, err := os.MkdirTemp("", "sift-update-")
	if err != nil {
		report(stderr, fmt.Errorf("创建临时目录失败：%w", err))
		return 1
	}
	defer os.RemoveAll(tmp) // fail-closed: a bad download/checksum leaves nothing behind

	releaseBase := releaseDownloadBaseURL + "/v" + latest
	archivePath := filepath.Join(tmp, archive)
	if err := downloadFile(releaseBase+"/"+archive, archivePath); err != nil {
		report(stderr, fmt.Errorf("下载 %s 失败：%w", archive, err))
		return 1
	}
	checksumsPath := filepath.Join(tmp, "checksums.txt")
	if err := downloadFile(releaseBase+"/checksums.txt", checksumsPath); err != nil {
		report(stderr, fmt.Errorf("下载 checksums.txt 失败：%w", err))
		return 1
	}
	expected, err := checksumFor(checksumsPath, archive)
	if err != nil {
		report(stderr, err)
		return 1
	}
	actual, err := fileSHA256(archivePath)
	if err != nil {
		report(stderr, fmt.Errorf("计算归档校验和失败：%w", err))
		return 1
	}
	if actual != expected {
		report(stderr, fmt.Errorf("sha256 校验失败：%s 校验和不匹配（预期 %s，实际 %s）；已放弃安装", archive, expected, actual))
		return 1
	}

	if *force {
		// Install refuses to overwrite a live version directory; --force is
		// the documented "remove it first" path (install.go), safe for the
		// running binary because the process holds the old inode.
		if err := os.RemoveAll(filepath.Join(home.Path, install.BinDirName, latest)); err != nil {
			report(stderr, fmt.Errorf("移除旧版本目录失败：%w", err))
			return 1
		}
	}
	installed, err := install.Install(home.Path, archivePath)
	if err != nil {
		report(stderr, err)
		return 1
	}
	if jsonOutput {
		return emitUpdateJSON(stdout, stderr, current, installed, true)
	}
	fmt.Fprintf(stdout, "已升级到 %s\n", installed)
	// Daemon-aware: a running siftd keeps the old binary until restarted
	// (release.md §3: `current` switch never touches the running process).
	if isSocket(filepath.Join(home.Path, "siftd.sock")) {
		fmt.Fprintln(stdout, "守护进程正在运行：运行 `sift service restart` 使新版本生效")
	}
	return 0
}

// updateCompareMessage formats the current-vs-target human message for both
// --check and the no-op install path. The latest<current case must never
// claim "已是最新" (the local build is newer than the release latest); the
// pinned-downgrade path is `sift update --version <版本>`.
func updateCompareMessage(current, latest string, cmp int) string {
	switch {
	case cmp < 0:
		return fmt.Sprintf("当前 %s，最新 %s（有可用更新，运行 sift update 升级）\n", current, latest)
	case cmp == 0:
		return fmt.Sprintf("已是最新 %s\n", current)
	default:
		return fmt.Sprintf("当前 %s 比 release 最新 %s 更新（本地更新）；如需安装指定版本请用 --version <版本>\n", current, latest)
	}
}

// emitUpdateJSON prints the machine-readable {current, latest, updated}
// contract and maps it to the process exit status.
func emitUpdateJSON(stdout, stderr io.Writer, current, latest string, updated bool) int {
	if err := printJSON(stdout, map[string]any{"current": current, "latest": latest, "updated": updated}); err != nil {
		report(stderr, err)
		return 1
	}
	return 0
}

// fetchLatestRelease queries the GitHub releases/latest API and returns the
// tag_name (e.g. "v0.2.0").
func fetchLatestRelease() (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	body, err := fetchBody(client, releaseAPIURL, maxMetadataBytes)
	if err != nil {
		return "", fmt.Errorf("查询最新版本失败：%w", err)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("解析最新版本响应失败：%w", err)
	}
	if rel.TagName == "" {
		return "", errors.New("最新版本响应缺少 tag_name")
	}
	return rel.TagName, nil
}

// releasePlatform maps the running platform to the goreleaser combo naming
// (the install.sh whitelist: darwin/linux × amd64/arm64).
func releasePlatform() (goos, goarch string, err error) {
	switch runtime.GOOS {
	case "darwin", "linux":
		goos = runtime.GOOS
	default:
		return "", "", fmt.Errorf("当前系统 %s 不受支持（仅 darwin/linux）", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		goarch = runtime.GOARCH
	default:
		return "", "", fmt.Errorf("当前架构 %s 不受支持（仅 amd64/arm64）", runtime.GOARCH)
	}
	return goos, goarch, nil
}

// fetchBody GETs a small document (release metadata), retrying transient
// failures like install.sh's `curl --retry 3`.
func fetchBody(client *http.Client, url string, maxBytes int64) ([]byte, error) {
	var buf bytes.Buffer
	var lastErr error
	for attempt := 0; attempt < checksumAttempts; attempt++ {
		buf.Reset()
		lastErr = fetch(client, url, &buf, maxBytes)
		if lastErr == nil {
			return buf.Bytes(), nil
		}
		if attempt < checksumAttempts-1 {
			time.Sleep(updateBackoff(attempt))
		}
	}
	return nil, lastErr
}

// downloadFile GETs an artifact into dest, retrying transient failures. Each
// attempt starts from a fresh file so a retry never appends to a partial body.
func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	var lastErr error
	for attempt := 0; attempt < checksumAttempts; attempt++ {
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("清理旧下载文件失败：%w", err)
		}
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		lastErr = fetch(client, url, f, maxArchiveBytes)
		closeErr := f.Close()
		if lastErr == nil && closeErr != nil {
			lastErr = closeErr
		}
		if lastErr == nil {
			return nil
		}
		if attempt < checksumAttempts-1 {
			time.Sleep(updateBackoff(attempt))
		}
	}
	return lastErr
}

// fetch GETs url and copies the body into w, bounding it to maxBytes and
// failing on any non-200 status. It does not retry; callers own the loop.
func fetch(client *http.Client, url string, w io.Writer, maxBytes int64) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	n, copyErr := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if copyErr != nil {
		return copyErr
	}
	if n > maxBytes {
		return fmt.Errorf("响应超过 %d 字节上限", maxBytes)
	}
	return nil
}

// updateBackoff keeps retry delays small so hermetically-served tests never
// block: 300ms, 900ms.
func updateBackoff(attempt int) time.Duration {
	return time.Duration(attempt+1) * 300 * time.Millisecond
}

// checksumFor extracts the sha256 for archive from the goreleaser
// checksums.txt format (`<hash>  <name>` or `*<name>`), case-insensitively,
// mirroring the install.sh awk contract.
func checksumFor(checksumsPath, archive string) (string, error) {
	raw, err := os.ReadFile(checksumsPath)
	if err != nil {
		return "", fmt.Errorf("读取 checksums.txt 失败：%w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != archive {
			continue
		}
		hash := strings.ToLower(fields[0])
		if !updateChecksumEntry.MatchString(hash) {
			return "", fmt.Errorf("checksums.txt 中 %s 的校验和条目格式非法", archive)
		}
		return hash, nil
	}
	return "", fmt.Errorf("checksums.txt 中没有 %s 的校验和条目", archive)
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
