#!/usr/bin/env bash
set -euo pipefail

# install.sh prefers a local source build when run from a cloned checkout and
# otherwise falls back to installing a prebuilt release binary. Homebrew
# remains the primary path for package-managed installs.

PROGRAM_NAME="catclip"
RELEASE_BASE_URL="${CATCLIP_RELEASE_BASE_URL:-https://github.com/tigreau/catclip/releases}"
INSTALL_VERSION="${CATCLIP_INSTALL_VERSION:-latest}"
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
SHARE_DIR="$PREFIX/share/catclip"

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  RESET=$'\033[0m'
  BOLD=$'\033[1m'
  GREEN=$'\033[32m'
  CYAN=$'\033[36m'
  YELLOW=$'\033[33m'
  RED=$'\033[31m'
else
  RESET=''
  BOLD=''
  GREEN=''
  CYAN=''
  YELLOW=''
  RED=''
fi

die() {
  printf '%sError:%s %s\n' "$RED" "$RESET" "$*" >&2
  exit 1
}

note() {
  printf '%sNote:%s %s\n' "$YELLOW" "$RESET" "$*"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "'$1' is required"
}

need_go_for_source_build() {
  command -v go >/dev/null 2>&1 && return 0
  die "Go is required when installing from a source checkout. Use Homebrew or the release installer if you do not want to build locally."
}

homebrew_manages_catclip() {
  if ! command -v brew >/dev/null 2>&1; then
    return 1
  fi

  brew list --versions "$PROGRAM_NAME" >/dev/null 2>&1 && return 0
  brew list --cask --versions "$PROGRAM_NAME" >/dev/null 2>&1 && return 0
  return 1
}

find_local_source_dir() {
  local script_path script_dir
  script_path="${BASH_SOURCE[0]:-}"
  [[ -n "$script_path" ]] || return 1
  [[ -f "$script_path" ]] || return 1
  script_dir="$(cd "$(dirname "$script_path")" 2>/dev/null && pwd)" || return 1

  # A cloned checkout contains the full Go module. Prefer building that exact
  # source tree so install.sh reflects the checked-out code instead of silently
  # replacing it with whatever release is current.
  if [[ -f "$script_dir/go.mod" && -f "$script_dir/main.go" && -f "$script_dir/VERSION" ]]; then
    printf '%s\n' "$script_dir"
    return 0
  fi

  return 1
}

warn_existing_target_install() {
  local target="$1"
  local version_file="$2"

  if [[ ! -e "$target" ]]; then
    return 0
  fi

  note "Existing catclip installation detected at $target."
  if [[ -f "$version_file" ]]; then
    note "Existing version metadata found at $version_file."
  fi
  note "This install will replace the direct-install binary in place and keep ~/.config/catclip/.hiss."
}

install_file() {
  local mode="$1"
  local src="$2"
  local dest="$3"
  local dest_dir
  dest_dir="$(dirname "$dest")"

  # Prefer a normal user-space install first. This keeps local prefixes like
  # /tmp or ~/.local fast and avoids tripping sudo in writable locations.
  if mkdir -p "$dest_dir" 2>/dev/null; then
    if install -m "$mode" "$src" "$dest" 2>/dev/null; then
      return
    fi
  fi

  if [[ -w "$dest_dir" ]]; then
    install -m "$mode" "$src" "$dest"
    return
  fi

  if ! command -v sudo >/dev/null 2>&1; then
    die "cannot write to $dest_dir; re-run with PREFIX=\"$HOME/.local\" or install sudo"
  fi

  sudo mkdir -p "$dest_dir"
  sudo install -m "$mode" "$src" "$dest"
}

download_file() {
  local url="$1"
  local dest="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
    return
  fi
  die "curl or wget is required to download catclip"
}

try_download_file() {
  local url="$1"
  local dest="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
    return
  fi
  return 1
}

normalize_os() {
  case "$(uname -s)" in
    Darwin) printf '%s\n' "darwin" ;;
    Linux) printf '%s\n' "linux" ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
}

normalize_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf '%s\n' "amd64" ;;
    arm64|aarch64) printf '%s\n' "arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

build_release_url() {
  local asset="$1"

  if [[ "$INSTALL_VERSION" == "latest" ]]; then
    printf '%s\n' "$RELEASE_BASE_URL/latest/download/$asset"
    return
  fi

  printf '%s\n' "$RELEASE_BASE_URL/download/$INSTALL_VERSION/$asset"
}

verify_checksum() {
  local asset_name="$1"
  local archive_path="$2"
  local checksums_path="$3"

  if [[ ! -f "$checksums_path" ]]; then
    printf '%sWarning:%s checksums file missing; skipping verification.\n' "$YELLOW" "$RESET" >&2
    return 0
  fi

  if command -v shasum >/dev/null 2>&1; then
    (
      cd "$(dirname "$archive_path")"
      grep "  $asset_name\$" "$checksums_path" | shasum -a 256 -c -
    ) >/dev/null
    return
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    (
      cd "$(dirname "$archive_path")"
      grep "  $asset_name\$" "$checksums_path" | sha256sum -c -
    ) >/dev/null
    return
  fi

  printf '%sWarning:%s no sha256 verifier found; skipping verification.\n' "$YELLOW" "$RESET" >&2
}

need_cmd tar
need_cmd install

OS_NAME="$(normalize_os)"
ARCH_NAME="$(normalize_arch)"
ASSET_NAME="${PROGRAM_NAME}_${OS_NAME}_${ARCH_NAME}.tar.gz"
CHECKSUMS_NAME="checksums.txt"

TMP_ROOT=''
cleanup() {
  if [[ -n "$TMP_ROOT" && -d "$TMP_ROOT" ]]; then
    rm -rf "$TMP_ROOT"
  fi
}
trap cleanup EXIT

printf '%sInstalling catclip...%s\n' "$BOLD" "$RESET"
printf 'Target:   %s%s/%s%s\n' "$CYAN" "$OS_NAME" "$ARCH_NAME" "$RESET"

if homebrew_manages_catclip; then
  die "catclip appears to be managed by Homebrew; use 'brew upgrade catclip' instead."
fi

warn_existing_target_install "$BIN_DIR/$PROGRAM_NAME" "$SHARE_DIR/VERSION"

TMP_ROOT="$(mktemp -d)"

if SOURCE_DIR="$(find_local_source_dir)"; then
  need_go_for_source_build

  printf 'Source:   %s%s%s\n' "$CYAN" "$SOURCE_DIR" "$RESET"
  note "Building from your local checkout so the installed binary matches the code you have checked out."
  note "This avoids replacing your in-progress source tree with the latest published release."

  VERSION_FILE="$SOURCE_DIR/VERSION"
  BINARY_FILE="$TMP_ROOT/$PROGRAM_NAME"
  VERSION="$(tr -d '\r' < "$VERSION_FILE" | head -n 1)"
  [[ -n "$VERSION" ]] || die "VERSION file is empty"

  printf 'Building %s%s%s from source\n' "$CYAN" "$PROGRAM_NAME" "$RESET"
  (
    cd "$SOURCE_DIR"
    go build -trimpath -o "$BINARY_FILE" .
  )
else
  ARCHIVE_PATH="$TMP_ROOT/$ASSET_NAME"
  CHECKSUMS_PATH="$TMP_ROOT/$CHECKSUMS_NAME"

  printf 'Downloading %s%s%s\n' "$CYAN" "$ASSET_NAME" "$RESET"
  download_file "$(build_release_url "$ASSET_NAME")" "$ARCHIVE_PATH"

  if [[ "${CATCLIP_SKIP_VERIFY:-0}" != "1" ]]; then
    printf 'Downloading %s%s%s\n' "$CYAN" "$CHECKSUMS_NAME" "$RESET"
    if try_download_file "$(build_release_url "$CHECKSUMS_NAME")" "$CHECKSUMS_PATH"; then
      printf 'Verifying checksum...\n'
      verify_checksum "$ASSET_NAME" "$ARCHIVE_PATH" "$CHECKSUMS_PATH"
    else
      printf '%sWarning:%s failed to download checksums; skipping verification.\n' "$YELLOW" "$RESET" >&2
    fi
  fi

  tar -xzf "$ARCHIVE_PATH" -C "$TMP_ROOT"

  VERSION_FILE="$TMP_ROOT/VERSION"
  BINARY_FILE="$TMP_ROOT/$PROGRAM_NAME"
  [[ -f "$VERSION_FILE" ]] || die "release archive is missing VERSION"
  [[ -f "$BINARY_FILE" ]] || die "release archive is missing $PROGRAM_NAME"

  VERSION="$(tr -d '\r' < "$VERSION_FILE" | head -n 1)"
  [[ -n "$VERSION" ]] || die "VERSION file is empty"
fi

install_file 755 "$BINARY_FILE" "$BIN_DIR/$PROGRAM_NAME"
install_file 644 "$VERSION_FILE" "$SHARE_DIR/VERSION"

printf '%sDone.%s\n' "$GREEN" "$RESET"
printf '  Binary:  %s%s%s\n' "$CYAN" "$BIN_DIR/$PROGRAM_NAME" "$RESET"
printf '  Version: %s%s%s\n' "$CYAN" "$VERSION" "$RESET"
printf '  Config:  %s%s%s\n' "$CYAN" '~/.config/catclip/.hiss' "$RESET"

if [[ "$BIN_DIR" == "$HOME/.local/bin" ]]; then
  case ":${PATH}:" in
    *":$BIN_DIR:"*) ;;
    *) printf '%sNote:%s add %s to PATH if it is not already exported.\n' "$YELLOW" "$RESET" "$BIN_DIR" ;;
  esac
fi
