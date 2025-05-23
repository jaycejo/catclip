#!/usr/bin/env bash
set -euo pipefail

# install.sh — install catclip and its default config
# Usage: PREFIX=/opt/local ./install.sh

# 1) Where to install the binary
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"

# 2) Where to install the default config
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/catclip"
CONFIG_FILE="$CONFIG_DIR/ignore.yaml"

# 3) Path to the script in this repo
#    adjust if your script lives elsewhere, e.g. 'bin/catclip'
SRC_SCRIPT="$(cd "$(dirname "$0")" && pwd)/catclip"

# 4) (Optional) default template in repo
#    create this file in your repo if you want sane defaults!
DEFAULT_IGNORE_TEMPLATE="$(cd "$(dirname "$0")" && pwd)/ignore.yaml"

echo "Installing catclip → $BIN_DIR/catclip"
echo "Config dir         → $CONFIG_FILE"

# Create dirs
mkdir -p "$BIN_DIR"
mkdir -p "$CONFIG_DIR"

# Copy the script
install -m 755 "$SRC_SCRIPT" "$BIN_DIR/catclip"

# Copy default config only if user doesn’t already have one
if [[ ! -e "$CONFIG_FILE" ]]; then
  if [[ -r "$DEFAULT_IGNORE_TEMPLATE" ]]; then
    install -m 644 "$DEFAULT_IGNORE_TEMPLATE" "$CONFIG_FILE"
    echo "Copied default ignore-list to $CONFIG_FILE"
  else
    # create an empty YAML skeleton
    cat > "$CONFIG_FILE" <<EOF
ignore_dirs:
  # - node_modules
ignore_files:
  # - '*.pyc'
EOF
    echo "Created empty default config at $CONFIG_FILE"
  fi
else
  echo "Config already exists at $CONFIG_FILE; skipping"
fi

echo "Done! 🎉"
echo "Run 'catclip --help' to verify."
