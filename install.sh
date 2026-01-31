#!/usr/bin/env bash
set -euo pipefail

# ------------------------------------------------------------------------------
# install.sh — Interactive installer for catclip
# ------------------------------------------------------------------------------

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
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/catclip"
DEST_CONFIG="$CONFIG_DIR/ignore.yaml"

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
SRC_SCRIPT="$SRC_DIR/catclip"
SRC_CONFIG="$SRC_DIR/ignore.yaml"

# ------------------------------------------------------------------------------
# 2) Helper: Validation Function
# ------------------------------------------------------------------------------
validate_config() {
  local target_file="$1"
  # This AWK script mirrors the logic in the catclip binary
  awk '
    { raw=$0; sub(/#.*/, ""); gsub(/^[ \t]+|[ \t]+$/, ""); }
    length($0) == 0 { next }
    /^ignore_dirs:/  { block="D"; next }
    /^ignore_files:/ { block="F"; next }
    /^- / {
      if (block == "") { print "Orphaned list item (missing header): " raw; exit 1 }
      next
    }
    { print "Unknown syntax line: " raw; exit 1 }
  ' "$target_file" 2>&1
}

# ------------------------------------------------------------------------------
# 3) Sanity Checks
# ------------------------------------------------------------------------------
if [[ ! -f "$SRC_SCRIPT" ]]; then
  echo "${RED}Error: 'catclip' binary missing in $SRC_DIR${RESET}"
  exit 1
fi

if [[ ! -f "$SRC_CONFIG" ]]; then
  echo "${RED}Error: 'ignore.yaml' missing in $SRC_DIR${RESET}"
  exit 1
fi

HAS_LOCAL_CONFIG=true

# ------------------------------------------------------------------------------
# 4) Intro
# ------------------------------------------------------------------------------
clear
echo "${BOLD}Installing catclip...${RESET}"
echo

echo "${CYAN}${BOLD}How Configuration Works:${RESET}"
echo "  It merges ${BOLD}.gitignore${RESET} with a global ${BOLD}ignore.yaml${RESET} 'Safety Filter'."
echo "  The local ${BOLD}ignore.yaml${RESET} is for setup; ${BOLD}it is not used after installation${RESET}."
echo "  Catclip will use the new copy in ${CYAN}$CONFIG_DIR${RESET} instead."
echo
echo "  - ${BOLD}Efficiency:${RESET} Strips high-token noise (node_modules, lockfiles, assets)"
echo "    that Git tracks but LLMs don't need. Keeps context clean and cheap."
echo "  - ${BOLD}Safety:${RESET} Automatically blocks secrets (.env) and credentials by default."
echo "  - ${BOLD}Freedom:${RESET} Use ${RED}--no-ignore${RESET} to disable ALL filters and copy everything."
echo "  - ${BOLD}Fallback:${RESET} Provides search logic in non-Git directories."
echo

# ------------------------------------------------------------------------------
# 5) Initial & Optional Edit Validation
# ------------------------------------------------------------------------------
if [[ "$HAS_LOCAL_CONFIG" == true ]]; then
  # Check if it is broken before we even start
  INITIAL_ERR=$(validate_config "$SRC_CONFIG" || true)
  if [[ -n "$INITIAL_ERR" ]]; then
    echo "${RED}❌ The source ignore.yaml is currently corrupt!${RESET}"
    echo "Error: $INITIAL_ERR"
    read -r -p "Fix it now? [Y/n] " fix_init
    [[ "$fix_init" =~ ^[Nn]$ ]] && { echo "Aborting."; exit 1; }
    
    EDITOR="${EDITOR:-$(command -v nano || command -v vi)}"
    while true; do
      $EDITOR "$SRC_CONFIG"
      ERR=$(validate_config "$SRC_CONFIG" || true)
      [[ -z "$ERR" ]] && break
      echo "${RED}Syntax Error:${RESET} $ERR"
      read -r -p "Try again? [Y/n] " again
      [[ "$again" =~ ^[Nn]$ ]] && exit 1
    done
  fi

  # Optional Edit
  echo "${YELLOW}Would you like to customize the 'ignore.yaml' template?${RESET}"
  echo "The default template is optimized for Javascript, Java, and Python."
  echo "If you work with other languages, we recommend customizing it now."
  read -r -p "Open in editor? [y/N] " open_editor
  if [[ "$open_editor" =~ ^[Yy]$ ]]; then
    EDITOR="${EDITOR:-$(command -v nano || command -v vi)}"
    while true; do
      $EDITOR "$SRC_CONFIG"
      ERR=$(validate_config "$SRC_CONFIG" || true)
      if [[ -z "$ERR" ]]; then
        echo "${GREEN}✔ Valid configuration.${RESET}"
        break
      else
        echo "${RED}❌ Syntax Error:${RESET} $ERR"
        read -r -p "Fix it? [Y/n] " fixit
        [[ "$fixit" =~ ^[Nn]$ ]] && exit 1
      fi
    done
  fi
fi

# ------------------------------------------------------------------------------
# 6) Install Binary
# ------------------------------------------------------------------------------
echo
echo "Installing binary to ${CYAN}$BIN_DIR/catclip${RESET}..."
if [ ! -w "$BIN_DIR" ] && [ "$PREFIX" == "/usr/local" ]; then
    sudo mkdir -p "$BIN_DIR"
    sudo install -m 755 "$SRC_SCRIPT" "$BIN_DIR/catclip"
else
    mkdir -p "$BIN_DIR"
    install -m 755 "$SRC_SCRIPT" "$BIN_DIR/catclip"
fi

# ------------------------------------------------------------------------------
# 7) Install Config (The Final Safety Gate)
# ------------------------------------------------------------------------------
if [[ "$HAS_LOCAL_CONFIG" == true ]]; then
  mkdir -p "$CONFIG_DIR"
  
  COPY_ACTION="none"
  if [[ -f "$DEST_CONFIG" ]]; then
    echo
    echo "${YELLOW}⚠️  Existing config found at $DEST_CONFIG${RESET}"
    read -r -p "Replace with the new template? [y/N] " response
    if [[ "$response" =~ ^[Yy]$ ]]; then
      COPY_ACTION="replace"
    fi
  else
    COPY_ACTION="install"
  fi

  if [[ "$COPY_ACTION" != "none" ]]; then
    # THE FINAL CHECK: Ensure file is valid at the literal moment of copying
    echo -n "Final syntax validation... "
    FINAL_ERR=$(validate_config "$SRC_CONFIG" || true)

    if [[ -z "$FINAL_ERR" ]]; then
      echo "${GREEN}OK ✔${RESET}"
      install -m 644 "$SRC_CONFIG" "$DEST_CONFIG"
      [[ "$COPY_ACTION" == "replace" ]] && echo "${GREEN}Config updated.${RESET}"
      [[ "$COPY_ACTION" == "install" ]] && echo "${GREEN}Config installed to $DEST_CONFIG${RESET}"
    else
      echo "${RED}FAILED ❌${RESET}"
      echo "${RED}Error:${RESET} $FINAL_ERR"
      echo "${RED}Installation of ignore.yaml stopped to prevent system errors.${RESET}"
      exit 1
    fi
  else
    echo "Existing config preserved."
  fi
fi

echo
echo "${GREEN}${BOLD}Done! 🎉${RESET}"
read -r -p "Show the help menu now? [y/N] " show_help
if [[ "$show_help" =~ ^[Yy]$ ]]; then
    # Run using the absolute path to ensure it works even if PATH isn't updated
    "$BIN_DIR/catclip" --help
else
    echo "Run ${CYAN}catclip --help${RESET} anytime to explore features."
fi