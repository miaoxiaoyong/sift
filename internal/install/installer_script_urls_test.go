package install

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installScriptPath locates scripts/install.sh from the repo root. Tests never
// execute the installer or touch the network; they only assert the URL
// constants embedded in the shipped script so a stray legacy owner does not
// regress silently after a repository transfer.
func installScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	return filepath.Join(root, "scripts", "install.sh")
}

func readInstallScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatalf("read scripts/install.sh: %v", err)
	}
	return string(raw)
}

// canonicalRepos are the post-transfer owner/repo identities that must never
// appear in the installer, regardless of which alias the repo ever lived under.
var legacyRepoOwners = []string{
	"github.com/miaoxiaoyong/sift",
	"github.com/hexai-cn/sift",
}

// TestInstallerScriptCanonicalURLs guards the canonical xsift identity for the
// three install-time URL surfaces (release API, release download, docs) plus
// the raw install entry point, analogous to the hosting URL assertions.
func TestInstallerScriptCanonicalURLs(t *testing.T) {
	s := readInstallScript(t)

	wantPresent := []string{
		"https://api.github.com/repos/xsift/sift/releases/latest",
		"https://github.com/xsift/sift/releases/download",
		"https://github.com/xsift/sift#readme",
		"https://raw.githubusercontent.com/xsift/sift/main/scripts/install.sh",
	}
	for _, url := range wantPresent {
		if !strings.Contains(s, url) {
			t.Errorf("scripts/install.sh does not reference canonical URL %q", url)
		}
	}
}

// TestInstallerScriptNoLegacyOwnerURLs asserts the installer carries no
// pre-transfer owner in any URL surface.
func TestInstallerScriptNoLegacyOwnerURLs(t *testing.T) {
	s := readInstallScript(t)
	for _, legacy := range legacyRepoOwners {
		if strings.Contains(s, legacy) {
			t.Errorf("scripts/install.sh retains legacy repository identity %q", legacy)
		}
	}
}
