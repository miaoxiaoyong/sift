# frozen_string_literal: true

# Sift Homebrew formula (DRAFT). Generated from the GitHub Release; the
# published tap lives in its own repo (WBS §8.2 non-scope here). The
# archive name and layout match specs/release.md §2 so a brew install
# and a release-archive install expose the same two binaries. The daemon
# opens no network port (two owner-only Unix sockets).
# Regenerate with: go run ./tools/hosting formula --version <v> --sha256 <h>
class Sift < Formula
  desc "Local multi-agent task orchestration hub"
  homepage "https://github.com/xsift/sift"
  url "https://github.com/xsift/sift/releases/download/v0.1.0-dev/sift_0.1.0-dev_darwin_arm64.tar.gz"
  version "0.1.0-dev"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"

  # The archive carries sift + sift-agent-wrapper + manifest.json for the
  # darwin/arm64 combo; the formula installs both binaries (the daemon
  # resolves its wrapper from its own install directory, DESIGN §8.4).
  def install
    bin.install "sift"
    bin.install "sift-agent-wrapper"
  end

  def caveats
    <<~EOS
      Sift runs as a user-level launchd agent (no system daemon, no port).
      Register it after install with:
        sift service install
      Logs: ~/.sift/logs/
    EOS
  end

  test do
    assert_match(/^0.1.0-dev$/, shell_output("#{bin}/sift --version").strip)
  end
end
