#!/usr/bin/env bash
# Install the Sift release for the current machine.
# Intended for: curl -fsSL https://raw.githubusercontent.com/miaoxiaoyong/sift/main/scripts/install.sh | bash
set -euo pipefail

readonly RELEASE_API_URL="https://api.github.com/repos/miaoxiaoyong/sift/releases/latest"
readonly RELEASE_DOWNLOAD_BASE_URL="https://github.com/miaoxiaoyong/sift/releases/download"
readonly INSTALL_ROOT="${SIFT_INSTALL_ROOT:-${HOME}/.sift}"

fail() {
  printf 'sift installer: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf 'Usage: install.sh [--version VERSION] [--add-to-path]\n' >&2
  printf '  --add-to-path   append the PATH export to your shell rc file (opt-in;\n' >&2
  printf '                  default is to only print the next-steps hint)\n' >&2
}

version="${SIFT_VERSION:-}"
add_to_path=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || { usage; fail '--version requires a value'; }
      [ -z "$version" ] || [ "$version" = "${SIFT_VERSION:-}" ] || fail 'version specified more than once'
      version="$2"
      shift 2
      ;;
    --version=*)
      [ -z "$version" ] || [ "$version" = "${SIFT_VERSION:-}" ] || fail 'version specified more than once'
      version="${1#*=}"
      shift
      ;;
    --add-to-path)
      add_to_path=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      fail "unknown argument: $1"
      ;;
  esac
done

normalize_version() {
  local candidate="$1"
  candidate="${candidate#v}"
  case "$candidate" in
    ''|*[!0-9A-Za-z.-]*|*/*|*'..'*|.*|*.) fail "invalid release version: $1" ;;
  esac
  # Release directories and URLs only accept canonical-looking SemVer values.
  [[ "$candidate" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || fail "invalid release version: $1"
  printf '%s' "$candidate"
}

if [ -z "$version" ]; then
  api_response="$(curl -fsSL "$RELEASE_API_URL")" || fail 'could not query the latest release'
  version="$(printf '%s\n' "$api_response" | sed -nE 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' | head -n 1)"
  [ -n "$version" ] || fail 'latest release response did not contain tag_name'
fi
version="$(normalize_version "$version")"

os="$(uname -s)"
case "$os" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported operating system: $os (only darwin and linux are supported)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail "unsupported architecture: $arch (only amd64 and arm64 are supported)" ;;
esac

command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v tar >/dev/null 2>&1 || fail 'tar is required'
if command -v sha256sum >/dev/null 2>&1; then
  checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  checksum_tool=shasum
else
  fail 'sha256sum or shasum is required'
fi

archive="sift_${version}_${os}_${arch}.tar.gz"
release_url="${RELEASE_DOWNLOAD_BASE_URL}/v${version}"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/sift-install.XXXXXX")"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT

curl -fsSL --retry 3 -o "$tmp_dir/$archive" "$release_url/$archive" || fail "could not download $archive"
curl -fsSL --retry 3 -o "$tmp_dir/checksums.txt" "$release_url/checksums.txt" || fail 'could not download checksums.txt'

checksum_line="$(awk -v name="$archive" '$2 == name || $2 == "*" name { print; found=1; exit } END { if (!found) exit 1 }' "$tmp_dir/checksums.txt")" || fail "checksums.txt has no entry for $archive"
case "$checksum_line" in
  [0-9a-fA-F][0-9a-fA-F]*) ;;
  *) fail "invalid checksum entry for $archive" ;;
esac
printf '%s\n' "$checksum_line" >"$tmp_dir/checksum-entry"
if [ "$checksum_tool" = sha256sum ]; then
  (cd "$tmp_dir" && sha256sum --check --status checksum-entry) || fail "sha256 checksum failed for $archive"
else
  expected="$(printf '%s' "$checksum_line" | awk '{print $1}')"
  actual="$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')"
  [ "$expected" = "$actual" ] || fail "sha256 checksum failed for $archive"
fi

# Reject absolute paths, parent traversal, and link members before extraction.
archive_members="$tmp_dir/archive-members"
if ! tar -tzf "$tmp_dir/$archive" >"$archive_members"; then
  fail 'could not inspect archive'
fi
while IFS= read -r member; do
  case "$member" in
    ''|/*|../*|*/../*|*/..|*/|.|./*) fail "unsafe archive member: $member" ;;
  esac
done <"$archive_members"
archive_details="$tmp_dir/archive-details"
if ! tar -tvzf "$tmp_dir/$archive" >"$archive_details"; then
  fail 'could not inspect archive'
fi
if awk '$1 ~ /^[lh]/ { found=1; exit } END { exit found ? 0 : 1 }' "$archive_details"; then
  fail 'archive contains symlink or hardlink member'
fi

bin_root="$INSTALL_ROOT/bin"
version_dir="$bin_root/$version"
mkdir -p "$bin_root"
staging="$bin_root/.staging-${version}.$$"
[ ! -e "$staging" ] || fail 'staging path already exists'
mkdir "$staging"
if ! tar -xzf "$tmp_dir/$archive" -C "$staging"; then
  rm -rf "$staging"
  fail 'could not extract archive'
fi
[ -f "$staging/sift" ] || { rm -rf "$staging"; fail 'archive does not contain sift'; }
[ -f "$staging/sift-agent-wrapper" ] || { rm -rf "$staging"; fail 'archive does not contain sift-agent-wrapper'; }

if [ -e "$version_dir" ] || [ -L "$version_dir" ]; then
  rm -rf "$staging"
else
  mv "$staging" "$version_dir"
fi

current="$bin_root/current"
if [ -e "$current" ] && [ ! -L "$current" ]; then
  fail "$current exists and is not a symlink"
fi
ln -sfn "$version" "$current"

"$current/sift" --version || fail 'installed sift failed --version verification'
printf 'Installed Sift %s at %s\n' "$version" "$version_dir"

# --- Onboarding ---------------------------------------------------------
# rc detection follows $SHELL (zsh -> ~/.zshrc, bash -> ~/.bashrc); any other
# shell only gets the printed hint, never a file write.
path_line="export PATH=\"$bin_root/current:\$PATH\""
rc_file=""
rc_hint="add '$path_line' to your shell rc file"
case "${SHELL:-}" in
  */zsh)
    rc_file="${HOME}/.zshrc"
    rc_hint="echo '$path_line' >> ~/.zshrc && source ~/.zshrc"
    ;;
  */bash)
    rc_file="${HOME}/.bashrc"
    rc_hint="echo '$path_line' >> ~/.bashrc && source ~/.bashrc"
    ;;
esac

# Opt-in auto PATH: SIFT_AUTO_PATH=1 or --add-to-path. Idempotent: never
# append a line that is already present verbatim.
if [ "$add_to_path" -eq 1 ] || [ "${SIFT_AUTO_PATH:-}" = "1" ]; then
  if [ -z "$rc_file" ]; then
    printf 'Sift installer: unsupported shell (%s); not modifying any rc file\n' "${SHELL:-unset}" >&2
    printf 'Add Sift to PATH manually: %s\n' "$path_line"
  elif [ -f "$rc_file" ] && grep -Fqx "$path_line" "$rc_file"; then
    printf 'Sift PATH entry already present in %s (no change)\n' "$rc_file"
  elif printf '\n# Added by the Sift installer (SIFT_AUTO_PATH)\n%s\n' "$path_line" >>"$rc_file" 2>/dev/null; then
    printf 'Added Sift to PATH in %s\n' "$rc_file"
  else
    printf 'Sift installer: warning: could not write %s; ' "$rc_file" >&2
    printf 'add to PATH manually: %s\n' "$path_line" >&2
  fi
fi

# The daemon refuses startup when SIFT_HOME grants group/other access
# (docs/specs/config.md §2.1); the installer-created root must be owner-only.
if ! chmod 700 "$INSTALL_ROOT" 2>/dev/null; then
  printf 'Sift installer: warning: could not set %s to owner-only (0700); ' "$INSTALL_ROOT" >&2
  printf 'run chmod 700 %s before starting sift daemon\n' "$INSTALL_ROOT" >&2
fi

cat <<EOF

Next steps
----------
1) Add Sift to PATH
   For this session:
     export PATH="$bin_root/current:\$PATH"
   Permanent (per \$SHELL):
     $rc_hint
   Or rerun the installer with SIFT_AUTO_PATH=1 to append it automatically.

2) Log in to your forge CLI (Sift drives the official CLI and never manages
   credentials itself):
     gh auth login       # GitHub
     glab auth login     # GitLab

3) Initialize config (wizard writes and validates ~/.sift/config.yaml):
     sift init
   Non-interactive: sift init --agent claude --project . --forge github
   Minimal hand-written example (operators/agents/projects): docs/specs/config.md §3.1-3.3
   Walkthrough:                                docs/guides/installation.md
   If present, config.yaml must be owner-only:
     chmod 600 ~/.sift/config.yaml

4) Verify and start:
     sift doctor --offline   # read-only check; exit 0 = healthy
     sift daemon             # foreground; or: sift service install (autostart)

Docs: https://github.com/miaoxiaoyong/sift#readme
EOF
