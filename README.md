# catclip - conCATenate to CLIPboard

One command to copy your entire codebase to clipboard for AI assistants.
```bash
catclip src  # That's it.
```
---

## Features

- ⚡ **Instant** - Zero setup, smart defaults, copies 500+ files in seconds
- 🔍 **Fuzzy search** - `catclip components` finds any nested directory
- 🔗 **Chained paths** - `catclip shared/components` more specific in case there are multiple `components` directories
- 🧩 **Multiple targets** - `catclip README.md src docs` in one run
- 🧾 **File headers in output** - each file is prefixed with `# File: path/to/file`
- 🌳 **Visual preview** - Tree view with file count, size, and token estimate before copying
- 🙈 **Git-aware** - Respects `.gitignore` and supports diff-only context with `--changed`
- 🎛️ **Flexible ignores** - `--ignore +'*.css' d-build` for one-off changes, or `--ignore-always` to persist
- 🛡️ **Secret protection** - Blocks `.env`, keys, credentials

---

## Installation

### Homebrew (Recommended)
```bash
brew tap tigreau/catclip && brew install catclip
```
Note: Homebrew installs the CLI only. The example project is available when you clone the repo.

### From Source (Git)
```bash
git clone https://github.com/tigreau/catclip.git
cd catclip && ./install.sh
```

**Requirements**: Bash 3.2+, clipboard tool (auto-detected)
- macOS: Built-in ✓
- Linux: `xclip` or `wl-clipboard`
- WSL: Built-in ✓

<details><summary>Manual install (no script)</summary>

```bash
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
SHARE_DIR="$PREFIX/share/catclip"

mkdir -p "$BIN_DIR" "$SHARE_DIR" ~/.config/catclip
cp catclip "$BIN_DIR/"
cp VERSION "$SHARE_DIR/VERSION"
cp ignore.yaml ~/.config/catclip/ignore.yaml
```

If you prefer a user-local install (no sudo):
```bash
PREFIX="$HOME/.local"
mkdir -p "$PREFIX/bin" "$PREFIX/share/catclip" ~/.config/catclip
cp catclip "$PREFIX/bin/"
cp VERSION "$PREFIX/share/catclip/VERSION"
cp ignore.yaml ~/.config/catclip/ignore.yaml
```
</details>

<details><summary>Updating & Uninstalling</summary>
Use the section that matches how you installed catclip.

```bash
# Homebrew
brew upgrade catclip
brew uninstall catclip

# From Source (Git)
git pull && ./install.sh
./uninstall.sh
```
</details>

---

## Try It
The repository includes a `dummy-react-project` to experiment with (clone the repo to access it):
```bash
cd dummy-react-project
catclip components          # Copy directory
catclip layout/Sidebar.tsx  # Copy file
```
\
You don't even have to type the full directory name, `com` is enough:

<img width="1300" height="835" alt="image" src="https://github.com/user-attachments/assets/c2d2fb10-310a-4cd6-aa6d-d5bea0fbf2d0" />



---

## Quick Start
```bash
# Copy source directory:
catclip src

# Fuzzy search:
catclip components              # Finds any 'components' dir

# Specific file via chained path:
catclip auth/hooks/useLogin.ts

# Exact file path (tab-tab from repo root):
catclip src/components/ui/Button.tsx

# Multiple targets at once:
catclip README.md src docs
```

<details>
<summary><b>More Examples</b></summary>

```bash
# Override filters:
catclip --no-ignore dist

# Copy & Print to stdout:
catclip --print src

# Temporary ignore (this run only):
catclip src/features/authentication --ignore +'LoginForm.tsx'
```

</details>

## Changed files (Git only)
```
catclip --changed
catclip src --changed
```
Copies only the files that differ from `HEAD`, including staged changes and untracked files. Runs only inside a Git repository. Optional targets limit the scope (for example, `src` only).

---

## Configuration

Default location: `~/.config/catclip/ignore.yaml`
```yaml
ignore_dirs:
  - node_modules
  - .git
ignore_files:
  - '*.log'
  - .env
```

Quick config:
`catclip --ignore-always +'*.log' -'old.tmp' d+build d-src`

Adds `*.log` and `build/`, removes `old.tmp` and `src/` from **ignore list**

Tip: Use `--ignore` with targets to apply changes for this run only:
`catclip src --ignore +'main.tsx'`
If you omit targets, `catclip` defaults to current directory (`.`).

---

## Options

| Flag | Description |
|------|-------------|
| `-h`, `--help` | Show help |
| `-y`, `--yes` | Skip confirmation |
| `-n`, `--no-ignore` | Include ignored files |
| `-p`, `--print` | Print to terminal in addition to clipboard |
| `-l`, `--list-ignores` | Show ignore rules |
| `-t`, `--no-tree` | Skip tree rendering |
| `-r`, `--reset-config` | Restore default ignore config |
| `-i`, `--ignore <ops>` | Temporary ignores for this run only |
| `--ignore-always <ops>` | Modify ignore list |
| `--changed` | Copy files changed since the last commit (requires Git repo; optional targets scope results). |

Full docs: `catclip --help`

---

## Troubleshooting

<details>
<summary><b>No clipboard tool found</b></summary>

Install for your platform:
```bash
# Ubuntu/Debian
sudo apt install xclip  # or wl-clipboard for Wayland

# Fedora
sudo dnf install xclip # or wl-clipboard for Wayland

# Arch
sudo pacman -S xclip # or wl-clipboard for Wayland
```
Or fallback to stdout: `catclip --print src > code.txt`

</details>

<details>
<summary><b>Directory ignored</b></summary>

Check: `catclip --list-ignores`
Bypass once: `catclip --no-ignore name`
or
Permanently un-ignore: `catclip --ignore-always d-name`

</details>

---

## Contributing

PRs welcome! Keep changes POSIX-compatible and test on macOS.

1. Fork & clone
2. Create branch: `git checkout -b feature/name`
3. Submit PR
