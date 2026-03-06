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
- 🧾 **File headers in output** - each file is wrapped in `<file path="path/to/file">` tags
- 🌳 **Visual preview** - Tree view with file count, size, and token estimate before copying
- 🙈 **Git-aware** - Respects `.gitignore`, filters by staged/unstaged/untracked, and can output diffs instead of full files
- 🎛️ **Flexible ignores** - `--exclude "*.css"` to skip, `--include "tests/"` to restore, `--include "*"` to disable all rules
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

mkdir -p "$BIN_DIR" "$SHARE_DIR"
cp catclip "$BIN_DIR/"
cp VERSION "$SHARE_DIR/VERSION"
```

If you prefer a user-local install (no sudo):
```bash
PREFIX="$HOME/.local"
mkdir -p "$PREFIX/bin" "$PREFIX/share/catclip"
cp catclip "$PREFIX/bin/"
cp VERSION "$PREFIX/share/catclip/VERSION"
```

The global config (`~/.config/catclip/.hiss`) is created automatically on first run.
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
# Remove rules for this run (include tests):
catclip . --include "tests/,*.test.ts"

# Disable all ignore rules (full scan):
catclip . --include "*"

# Output to screen (stdout) instead of clipboard:
catclip --print src

# Preview what would be copied (fast dry-run):
catclip src --preview

# Skip files (this run only):
catclip src --exclude "LoginForm.tsx"

# Only files containing a pattern (regex):
catclip src --contains "TODO"

# Only blocks around TODO matches (not full files):
catclip src --contains "TODO" --snippet

# Staged changes as unified diff (great for commit review):
catclip --staged --diff

# All changes as patches + architecture reference:
catclip --changed --diff --then src/api/reference.ts
```

</details>

## Git-Aware Context

### Changed files
```bash
catclip --changed              # All modified: staged + unstaged + untracked
catclip src --changed          # Scoped to src/
```

### Composable git filters
Use specific filters instead of `--changed` to narrow what you grab:
```bash
catclip --staged               # Files in the git index (staged for commit)
catclip --unstaged             # Tracked files with uncommitted modifications
catclip --untracked            # New files not yet tracked by git
catclip --staged --untracked   # Combine: staged + new, skip WIP edits
```
`--changed` is shorthand for all three. Each flag implies `--changed` automatically.

### Diff output
Replace full file content with unified git patches:
```bash
catclip --changed --diff       # All modified files as patches
catclip --staged --diff        # Staged changes only — ideal for commit review
catclip --unstaged --diff      # WIP edits — what you're actively changing
```
Untracked files have no diff and are included with their full content.
The tree preview shows `[diff only]` or `[snippet only]` on files with partial output.

### Snippet extraction
With `--contains`, extract only the blank-line-bounded blocks around each match instead of the full file:
```bash
catclip src --contains "TODO" --snippet        # Blocks around each TODO
catclip . --contains "useState" --snippet      # React hook call-sites only
```

## Scopes
Use `--then` to apply different modifiers to different targets:
```bash
catclip src --only "*.ts" --then tests --only "*.test.ts"
#   Scope 1: src/ — only TypeScript source files
#   Scope 2: tests/ — only test files
```
Without `--then`, all targets share the same modifiers:
```bash
catclip src lib --only "*.ts"   # Both filtered to .ts files
```

---

## Configuration

catclip uses `~/.config/catclip/.hiss` (gitignore-inspired syntax, created on first run) plus `.gitignore` in Git repos.

```
# Example .hiss file (trailing / = directory)
node_modules/
*.log
.env
```

Edit config:
```bash
catclip --hiss             # open ignore config in editor
catclip --hiss-reset       # restore defaults
```

For this run only:
```bash
catclip src --exclude "*.test.*"           # skip test files
catclip . --include ".env"                 # remove .env rule, then discover
catclip . --include "*"                    # disable all ignore rules (full scan)
catclip src --include "tests/" --exclude "*.snap"  # combine both
```

---

## Options

| Flag | Description |
|------|-------------|
| `-h`, `--help` | Show help |
| `-y`, `--yes` | Skip confirmation |
| `-q`, `--quiet` | Suppress all informational output (errors only) |
| `-v`, `--verbose` | Show phase timings and debug info |
| `--include RULES` | Remove rules from blocklist this run (comma-separated; `"*"` = all) |
| `--exclude GLOBS` | Add skip rules this run (comma-separated; trailing `/` = directory) |
| `-p`, `--print` | Output to screen (stdout) instead of clipboard |
| `--hiss` | Open ignore config in editor |
| `-t`, `--no-tree` | Skip tree rendering |
| `--hiss-reset` | Restore default ignore config |
| `--only GLOB` | Include only files matching shell glob (OR across repeats) |
| `--changed` | All modified files: staged + unstaged + untracked (requires Git) |
| `--staged` | Only staged files (git index) |
| `--unstaged` | Only unstaged tracked modifications |
| `--untracked` | Only new untracked files |
| `--diff` | Emit unified diff instead of full file (requires a change-selection flag) |
| `--contains PATTERN` | Only files whose contents match regex pattern |
| `--snippet` | With `--contains`: emit only matched blocks (blank-line bounded) |
| `--then` | Start a new scope (separate targets with different modifiers) |
| `--preview` | Show file tree and token count without copying |

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
Or output to screen (stdout): `catclip --print src > code.txt`

</details>

<details>
<summary><b>Directory ignored</b></summary>

Check: `catclip --hiss`
Include this run: `catclip . --include "name/"` or `catclip . --include "*"`
Remove permanently: `catclip --hiss` (delete the line from the config)

</details>

---

## Contributing

PRs welcome! Keep changes POSIX-compatible and test on macOS.

1. Fork & clone
2. Create branch: `git checkout -b feature/name`
3. Submit PR
