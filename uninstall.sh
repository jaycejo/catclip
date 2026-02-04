#!/usr/bin/env bash
set -euo pipefail

# ------------------------------------------------------------------------------
# uninstall.sh — Uninstall catclip
# ------------------------------------------------------------------------------

# Colors
RESET=$'\033[0m'
BOLD=$'\033[1m'
RED=$'\033[31m'
GREEN=$'\033[32m'
YELLOW=$'\033[33m'
CYAN=$'\033[36m'

# Paths
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
TARGET="$BIN_DIR/catclip"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/catclip"

echo "${BOLD}Uninstalling catclip...${RESET}"
echo

# 1. Remove Binary
if [[ -f "$TARGET" ]]; then
  echo "Removing binary: ${CYAN}$TARGET${RESET}"
  
  if [[ -w "$BIN_DIR" ]]; then
    rm "$TARGET"
    echo "${GREEN}✔ Binary removed.${RESET}"
  else
    echo "${YELLOW}Permission denied. Trying with sudo...${RESET}"
    if sudo rm "$TARGET"; then
        echo "${GREEN}✔ Binary removed.${RESET}"
    else
        echo "${RED}❌ Failed to remove binary.${RESET}"
        exit 1
    fi
  fi
else
  echo "${YELLOW}Binary not found at $TARGET. Skipping.${RESET}"
fi

# 2. Remove Config (Optional)
if [[ -d "$CONFIG_DIR" ]]; then
  echo
  echo "Configuration directory found at: ${CYAN}$CONFIG_DIR${RESET}"
  read -r -p "Do you want to remove the configuration files? [Y/n] " remove_config
  if [[ ! "$remove_config" =~ ^[Nn]$ ]]; then
    rm -rf "$CONFIG_DIR"
    echo "${GREEN}✔ Configuration removed.${RESET}"
  else
    echo "Configuration preserved."
  fi
fi

echo
echo "${GREEN}${BOLD}Uninstallation complete.${RESET}"
