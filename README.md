# catclip

`catclip` is a simple Bash utility for macOS that concatenates all files under one or more target directories (with `# File: <path>` headers), applies customizable ignore lists, and copies the result to the clipboard with a single command. It was born out of the frustration of running:

```bash
echo "$(cat *)" | pbcopy
```

—and wanting an easy way to grab *entire* directory contents (recursively) without manual filtering.

---

## Features

* **Recursive concatenation** of files under one or more targets
* Automatic **`# File: <full-path>`** headers for clarity
* **Ignore lists** (files & directories) stored in YAML
* Built‑in commands for listing and updating ignore patterns
* Optionally **print** the concatenated output
* Reports which directories contributed files
* macOS clipboard integration via `pbcopy`

---

## Prerequisites

* macOS (relies on `pbcopy`)
* Bash (with `set -euo pipefail` semantics)
* Tools: `find`, `realpath`, `mktemp`

---

## Installation

You can install using the provided `install.sh` script:

```bash
# Clone the repo:
git clone https://github.com/<you>/catclip.git
cd catclip

# (Optional) Install to a custom prefix:
PREFIX=/opt/local ./install.sh

# Default install:
./install.sh
```

This will copy `catclip` into `$(PREFIX:-/usr/local)/bin` and initialize a default `ignore.yaml` in your `~/.config/catclip/` directory.

You can also install manually:

```bash
cp catclip /usr/local/bin/
mkdir -p ~/.config/catclip
cp ignore.yaml ~/.config/catclip/ignore.yaml
```

---

## Usage

```bash
catclip [OPTIONS] [TARGETS...]
```

* If no `TARGETS` are provided, `.` is used (current directory).
* The output is copied directly to the macOS clipboard via `pbcopy`.
* Use `--print` to also echo the output to stdout.

### Options

| Flag             | Description                                           |
| ---------------- | ----------------------------------------------------- |
| `-h`, `--help`   | Show help and usage information.                      |
| `--no-ignore`    | Ignore your YAML filters and copy *all* files.        |
| `--list-ignores` | Display current ignore directories and file patterns. |
| `--ignore <ops>` | Modify the YAML ignore lists and exit. Ops can be:    |
|                  | • `+pattern` add a file-pattern to ignore             |
|                  | • `-pattern` remove a file-pattern                    |
|                  | • `d+dirname` add a directory to ignore               |
|                  | • `d-dirname` remove a directory from ignore          |
| `--print`        | Also print the concatenated output to stdout.         |

### Examples

```bash
# Copy all .c and .h files from src/, ignoring node_modules:
catclip src

# Copy everything (no ignores):
catclip --no-ignore ~/projects/foo

# Show current ignore rules:
catclip --list-ignores

# Add a new ignore pattern and exit:
catclip --ignore +*.log -temp.txt d+build

# Copy and print to terminal:
catclip --print docs/
```

---

## Configuration

The default ignore lists live in:

```
$XDG_CONFIG_HOME/catclip/ignore.yaml
# usually ~/.config/catclip/ignore.yaml
```

```yaml
ignore_dirs:
  - node_modules
  - .git
  # …
ignore_files:
  - '*.pyc'
  - '*.class'
  # …
```

Use `catclip --ignore` to add or remove patterns without editing by hand.

---

## Contributing

1. Fork the repo & clone.
2. Create a branch: `git checkout -b feature/your-change`.
3. Make your changes & test on macOS.
4. Submit a Pull Request.

Please keep changes small and scripts POSIX‑compatible where possible.

---

## License

This project is released under the [MIT License](LICENSE).
