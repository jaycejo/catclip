# catclip - conCATenate to CLIPboard

One command to copy your entire codebase to clipboard for AI assistants.
```bash
catclip src  # That's it.
```

**Why?** Born from the frustration of `echo "$(cat *)" | pbcopy` and wanting 
recursive directory copying without manual filtering.

---

## Features

- 🔍 Fuzzy search - `catclip components` finds any nested directory
- 🔗 Chained paths - `catclip auth/components/Login.tsx` more specific in case there are multiple directories with the same name
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
The repository includes an `example-react` project to experiment with:
```bash
cd example-react
catclip components       # Fuzzy search
catclip hooks/useAuth.ts # Chained path
```

---

## Quick Start
```bash
# Copy source directory:
catclip src

# Fuzzy search:
catclip components              # Finds any 'components' dir

# Specific file via chained path:
catclip auth/components/Login.tsx
```

<details>
<summary><b>More Examples</b></summary>

```bash
# Copy multiple targets:
catclip README.md src config/

# Override filters:
catclip --no-ignore dist

# Copy & Print to stdout:
catclip --print src
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

*Adds `*.log` and `build/`, removes `old.tmp` and `src/` from ignore list*

---

## Options

| Flag | Description |
|------|-------------|
| `-h` | Show help |
| `-y` | Skip confirmation |
| `--no-ignore` | Include ignored files |
| `--list-ignores` | Show ignore rules |
| `--ignore <ops>` | Modify ignore list |

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