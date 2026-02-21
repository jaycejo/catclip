#!/usr/bin/env bash
set -euo pipefail

# ------------------------------------------------------------------------------
# install.sh — Interactive installer for catclip
# ------------------------------------------------------------------------------

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  clear
fi

# Colors for UX
RESET=$'\033[0m'
BOLD=$'\033[1m'
GREEN=$'\033[32m'
YELLOW=$'\033[33m'
CYAN=$'\033[36m'
RED=$'\033[31m'

# 1) Paths
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
SRC_SCRIPT="$SRC_DIR/catclip"
SRC_VERSION="$SRC_DIR/VERSION"

# ------------------------------------------------------------------------------
# 2) Sanity Checks
# ------------------------------------------------------------------------------
if [[ ! -f "$SRC_SCRIPT" ]]; then
  echo "${RED}Error: 'catclip' binary missing in $SRC_DIR${RESET}"
  exit 1
fi

if [[ ! -f "$SRC_VERSION" ]]; then
  echo "${RED}Error: 'VERSION' missing in $SRC_DIR${RESET}"
  exit 1
fi

# ------------------------------------------------------------------------------
# 3) Intro
# ------------------------------------------------------------------------------
echo "${BOLD}Installing catclip...${RESET}"
echo
echo "${CYAN}${BOLD}How Ignore Configuration Works:${RESET}"
echo "  catclip uses ${CYAN}~/.config/catclip/.hiss${RESET} (gitignore-compatible syntax)."
echo "  In Git repos, ${CYAN}.gitignore${RESET} patterns are also respected."
echo
echo "  - ${BOLD}Efficiency:${RESET} Strips high-token noise (node_modules, lockfiles, assets)"
echo "    that Git tracks but LLMs don't need. Keeps context clean and cheap."
echo "  - ${BOLD}Safety:${RESET} Blocks secrets (.env) and credentials by default."
echo "  - ${BOLD}Freedom:${RESET} Use ${RED}--include \"*\"${RESET} to disable ALL filters and copy everything."
echo "  - ${BOLD}Cross-Platform:${RESET} Works on macOS, Linux, and WSL."
echo

# ------------------------------------------------------------------------------
# 4) Clipboard Tool Check
# ------------------------------------------------------------------------------
detect_os() {
  case "$(uname -s)" in
    Darwin) echo "macos" ;;
    Linux)
      if [[ -f /proc/version ]] && grep -qiE '(microsoft|wsl)' /proc/version 2>/dev/null; then
        echo "wsl"
      else
        echo "linux"
      fi
      ;;
    *) echo "unknown" ;;
  esac
}

OS_TYPE="$(detect_os)"
HAS_CLIPBOARD=false

case "$OS_TYPE" in
  macos)
    command -v pbcopy &>/dev/null && HAS_CLIPBOARD=true
    ;;
  wsl)
    command -v clip.exe &>/dev/null && HAS_CLIPBOARD=true
    ;;
esac

# Check common Linux clipboard tools as fallback
if [[ "$HAS_CLIPBOARD" == false ]]; then
  command -v xclip &>/dev/null && HAS_CLIPBOARD=true
  command -v xsel &>/dev/null && HAS_CLIPBOARD=true
  command -v wl-copy &>/dev/null && HAS_CLIPBOARD=true
fi

if [[ "$HAS_CLIPBOARD" == false ]]; then
  echo "${YELLOW}Warning: No clipboard tool detected.${RESET}"
  case "$OS_TYPE" in
    macos)
      echo "  pbcopy should be available on macOS. Check your PATH."
      ;;
    wsl)
      echo "  clip.exe should be available in WSL. You can also install xclip."
      ;;
    linux)
      echo "  Install a clipboard tool to enable copy functionality:"
      # Detect display server
      if [[ "${XDG_SESSION_TYPE:-}" == "wayland" ]] || [[ -n "${WAYLAND_DISPLAY:-}" ]]; then
        echo "  ${CYAN}Wayland detected.${RESET} Install wl-clipboard:"
        echo "    Ubuntu/Debian: sudo apt install wl-clipboard"
        echo "    Fedora/RHEL:   sudo dnf install wl-clipboard"
        echo "    Arch:          sudo pacman -S wl-clipboard"
      else
        echo "  ${CYAN}X11 detected.${RESET} Install xclip:"
        echo "    Ubuntu/Debian: sudo apt install xclip"
        echo "    Fedora/RHEL:   sudo dnf install xclip"
        echo "    Arch:          sudo pacman -S xclip"
      fi
      ;;
  esac
  echo
  if [[ ! -t 0 ]]; then
    echo "Non-interactive shell; cannot prompt. Aborting."
    exit 1
  fi
  read -r -p "Continue anyway? [y/N] " continue_install
  [[ ! "$continue_install" =~ ^[Yy]$ ]] && { echo "Aborting."; exit 1; }
fi

# ------------------------------------------------------------------------------
# 5) Required Tools Check
# ------------------------------------------------------------------------------
if ! command -v install &>/dev/null; then
  echo "${RED}Error: 'install' command not found.${RESET}"
  exit 1
fi

# ------------------------------------------------------------------------------
# 6) Install Binary
# ------------------------------------------------------------------------------
echo
echo "Installing binary to ${CYAN}$BIN_DIR/catclip${RESET}..."
if [[ ! -w "$BIN_DIR" ]] && [[ "$PREFIX" == "/usr/local" ]]; then
    if command -v sudo &>/dev/null; then
        sudo mkdir -p "$BIN_DIR"
        sudo install -m 755 "$SRC_SCRIPT" "$BIN_DIR/catclip"
    else
        echo "${RED}Error: Cannot write to $BIN_DIR and sudo is not available.${RESET}"
        echo "Try: PREFIX=~/.local ./install.sh"
        exit 1
    fi
else
    mkdir -p "$BIN_DIR"
    install -m 755 "$SRC_SCRIPT" "$BIN_DIR/catclip"
fi

# ------------------------------------------------------------------------------
# 7) Install Version File (share/catclip)
# ------------------------------------------------------------------------------
SHARE_DIR="$PREFIX/share/catclip"
if [[ ! -w "$BIN_DIR" ]] && [[ "$PREFIX" == "/usr/local" ]]; then
    sudo mkdir -p "$SHARE_DIR"
    sudo install -m 644 "$SRC_VERSION" "$SHARE_DIR/VERSION"
else
    mkdir -p "$SHARE_DIR"
    install -m 644 "$SRC_VERSION" "$SHARE_DIR/VERSION"
fi

echo
echo "${GREEN}${BOLD}Done!${RESET}"
echo "  Binary:  ${CYAN}$BIN_DIR/catclip${RESET}"
echo "  Config:  ${CYAN}~/.config/catclip/.hiss${RESET} (created on first run)"
if [[ ! -t 0 ]]; then
  exit 0
fi
read -r -p "Show the help menu now? [y/N] " show_help
if [[ "$show_help" =~ ^[Yy]$ ]]; then
    # Run using the absolute path to ensure it works even if PATH isn't updated
    "$BIN_DIR/catclip" --help
else
    echo "Run ${CYAN}catclip --help${RESET} anytime to explore features."
fi
