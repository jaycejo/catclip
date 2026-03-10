#!/usr/bin/env bash
set -euo pipefail

# uninstall.sh removes the standalone catclip install, including its private
# bundled rg/fzf binaries under share/catclip/bin. Config removal stays opt-in.

PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
SHARE_DIR="$PREFIX/share/catclip"
TOOLS_DIR="$SHARE_DIR/bin"
TARGET="$BIN_DIR/catclip"
VERSION_FILE="$SHARE_DIR/VERSION"
RG_FILE="$TOOLS_DIR/rg"
FZF_FILE="$TOOLS_DIR/fzf"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/catclip"

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

homebrew_manages_catclip() {
  if ! command -v brew >/dev/null 2>&1; then
    return 1
  fi

  brew list --versions catclip >/dev/null 2>&1 && return 0
  brew list --cask --versions catclip >/dev/null 2>&1 && return 0
  return 1
}

remove_path() {
  local path="$1"
  if [[ ! -e "$path" ]]; then
    return 0
  fi

  if [[ -w "$(dirname "$path")" ]]; then
    rm -f "$path"
    return 0
  fi

  if command -v sudo >/dev/null 2>&1; then
    sudo rm -f "$path"
    return 0
  fi

  printf '%sError:%s cannot remove %s without sudo\n' "$RED" "$RESET" "$path" >&2
  exit 1
}

remove_dir_if_empty() {
  local path="$1"
  if [[ ! -d "$path" ]]; then
    return 0
  fi

  if [[ -w "$(dirname "$path")" ]]; then
    rmdir "$path" 2>/dev/null || true
    return 0
  fi

  if command -v sudo >/dev/null 2>&1; then
    sudo rmdir "$path" 2>/dev/null || true
  fi
}

printf '%sUninstalling catclip...%s\n' "$BOLD" "$RESET"

if homebrew_manages_catclip; then
  die "catclip appears to be managed by Homebrew; use 'brew uninstall catclip' instead."
fi

if [[ -e "$TARGET" ]]; then
  printf 'Removing %s%s%s\n' "$CYAN" "$TARGET" "$RESET"
  remove_path "$TARGET"
else
  printf '%sNotice:%s %s not found\n' "$YELLOW" "$RESET" "$TARGET"
fi

if [[ -e "$VERSION_FILE" ]]; then
  printf 'Removing %s%s%s\n' "$CYAN" "$VERSION_FILE" "$RESET"
  remove_path "$VERSION_FILE"
fi

for tool in "$RG_FILE" "$FZF_FILE"; do
  if [[ -e "$tool" ]]; then
    printf 'Removing %s%s%s\n' "$CYAN" "$tool" "$RESET"
    remove_path "$tool"
  fi
done

remove_dir_if_empty "$TOOLS_DIR"
remove_dir_if_empty "$SHARE_DIR"

if [[ -d "$CONFIG_DIR" && -t 0 ]]; then
  printf 'Remove config at %s%s%s? [y/N] ' "$CYAN" "$CONFIG_DIR" "$RESET"
  read -r reply
  if [[ "$reply" =~ ^[Yy]$ ]]; then
    rm -rf "$CONFIG_DIR"
    printf '%sRemoved config.%s\n' "$GREEN" "$RESET"
  fi
fi

printf '%sDone.%s\n' "$GREEN" "$RESET"
