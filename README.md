# catclip - conCATenate to CLIPboard

One command to copy your entire codebase to clipboard for AI assistants.
```bash
catclip src  # That's it.
```

**Why?** Born from the frustration of:

🫩 manual copy‑pasting across folders  
😵‍💫 one‑off commands that aren’t recursive  
😫 commands too long or fiddly to repeat consistently  
🫣 accidentally including binaries or huge artifacts  
😱 paths breaking on spaces or weird characters  
🫥 missing hidden files or sibling folders

...

*You name it.*

All **solved** with `catclip`.

---

## Features

- 🔍 Fuzzy search - `catclip components` finds any nested directory
- 🔗 Chained paths - `catclip shared/components` more specific in case there are multiple `components` directories
- 🧠 Fast exact paths - `catclip src/components/ui/Button.tsx` is perfect with tab-tab completion
- 🧩 Multiple targets - `catclip README.md src docs` in one run
- 🧾 File headers in output - each file is prefixed with `# File: path/to/file`
- 🛡️ Secret protection - Blocks `.env`, keys, credentials
- 🌳 Visual preview - Tree view before copying
- 🙈 Git-aware - Respects `.gitignore`

---

## Installation
```bash
git clone https://github.com/you/catclip.git
cd catclip && ./install.sh
```

**Requirements**: Bash 3.2+, clipboard tool (auto-detected)
- macOS: Built-in ✓
- Linux: `xclip` or `wl-clipboard`
- WSL: Built-in ✓

<details><summary>Manual installation</summary>

```bash
cp catclip /usr/local/bin/
mkdir -p ~/.config/catclip
cp ignore.yaml ~/.config/catclip/ignore.yaml
```
</details>

<details><summary>Updating & Uninstalling</summary>

```bash
# Update
git pull && ./install.sh

# Uninstall
./uninstall.sh
```
</details>

---

## Try It
The repository includes a `dummy-react-project` to experiment with:
```bash
cd dummy-react-project
catclip components          # Fuzzy search
catclip layout/Sidebar.tsx  # Chained path
```

---

## Quick Start
```bash
# Copy source directory:
catclip src

# Fuzzy search:
catclip components              # Finds any 'components' dir

# Specific file via chained path:
catclip hooks/auth/useLogin.ts

# Exact file path (tab-tab from repo root):
catclip src/components/ui/Button.tsx

# Multiple targets at once:
catclip README.md src docs/
```

<details>
<summary><b>More Examples</b></summary>

```bash
# Override filters:
catclip --no-ignore dist

# Copy & Print to stdout:
catclip --print src

# Temporary ignore file (this run only):
catclip src/features/auth --ignore +'LoginForm.tsx'
```

</details>

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
`catclip --ignore +'*.log' -'old.tmp' d+build d-src`

*Adds `*.log` and `build/`, removes `old.tmp` and `src/` from **ignore list**

Tip: Use `--ignore` with targets to apply changes for this run only:
`catclip src --ignore +'main.tsx'`

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
| `-i`, `--ignore <ops>` | Modify ignore list |

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
Permanently un-ignore: `catclip --ignore d-name`

</details>

---

## Contributing

PRs welcome! Keep changes POSIX-compatible and test on macOS.

1. Fork & clone
2. Create branch: `git checkout -b feature/name`
3. Submit PR
