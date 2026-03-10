package catclip

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type helpRow struct {
	Left  string
	Right string
}

func writeAlignedHelpRows(b *strings.Builder, indent string, style func(string) string, rows []helpRow) {
	max := 0
	for _, row := range rows {
		if len(row.Left) > max {
			max = len(row.Left)
		}
	}
	for _, row := range rows {
		b.WriteString(indent)
		b.WriteString(style(row.Left))
		b.WriteString(strings.Repeat(" ", max-len(row.Left)+2))
		b.WriteString(row.Right)
		b.WriteByte('\n')
	}
}

func shortHelpText(version string, colors colorPalette) string {
	var b strings.Builder
	cmd := func(s string) string { return colors.OK + s + colors.Reset }
	bold := func(s string) string { return colors.Bold + s + colors.Reset }
	dim := func(s string) string { return colors.Dim + s + colors.Reset }

	fmt.Fprintf(&b, "%scatclip v%s — Copy code context for AI prompts%s\n\n", colors.Bold, version, colors.Reset)
	b.WriteString("Usage:  catclip [options] [target ...]\n\n")
	b.WriteString("Examples:\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip", Right: "Open target picker (TTY) or copy current directory"},
		{Left: "catclip src lib", Right: "Copy specific directories"},
		{Left: "catclip Button.tsx", Right: "Near-instant exact filename lookup across the repo"},
		{Left: "catclip --include tests", Right: "Authorize an ignored directory for this run"},
		{Left: "catclip --include .env.production", Right: "Authorize an ignored file for this run"},
		{Left: `catclip src --only "*.ts"`, Right: "Only TypeScript files in src"},
		{Left: `catclip src --exclude "*.test.*"`, Right: "Skip test files"},
		{Left: `catclip --include coverage --only "*.json"`, Right: "Authorize a blocked target, then narrow"},
		{Left: "catclip src --changed", Right: "Only git-modified files in src"},
		{Left: "catclip src --contains TODO", Right: "Only files whose contents match regex"},
	})

	fmt.Fprintf(&b, "\n%s\n", bold("Options:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "-h, --help", Right: "Show this help"},
		{Left: "--help-all", Right: "Show full manual (scopes, patterns, all options)"},
		{Left: "--version", Right: "Show version"},
		{Left: "-v, --verbose", Right: "Show phase timings and debug info, even with -q"},
		{Left: "-q, --quiet", Right: "Suppress normal stderr output; in non-preview runs this also skips the tree and confirmation prompt"},
		{Left: "-p, --print", Right: "Output payload to stdout instead of clipboard"},
		{Left: "-y, --yes", Right: "Skip confirmation for large copies (redundant with -q)"},
		{Left: "-t, --no-tree", Right: "Skip tree preview (redundant with -q in non-preview runs)"},
		{Left: "--hiss", Right: "Open ignore config in editor"},
		{Left: "--hiss-reset", Right: "Restore ignore config to defaults"},
		{Left: "--preview", Right: "Show file tree and token count, skip copying"},
		{Left: "--include QUERY", Right: "Authorize an exact ignored target or browse ignored files/dirs for this scope"},
	})

	fmt.Fprintf(&b, "\n%s %s for headless stdout output (no prompts, no stderr hints, no clipboard writes)\n", dim("Machine mode:"), cmd("-q -p"))
	fmt.Fprintf(&b, "%s\n", dim("In normal copy runs, -q already behaves like a non-interactive yes-all and does not start tree rendering."))
	fmt.Fprintf(&b, "%s\n", dim("Exception: with --preview, -t still matters because preview mode can render the tree even when -q is set."))

	fmt.Fprintf(&b, "\n%s (apply to the current scope)\n", bold("Scope Modifiers:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "--exclude GLOBS", Right: "Add skip rules this run (comma-separated; trailing / = directory)"},
		{Left: "--only GLOB", Right: "Filter files matching shell glob within selected targets"},
		{Left: "--changed", Right: "Only git-modified files"},
		{Left: "--staged", Right: "Only staged files (git index)"},
		{Left: "--unstaged", Right: "Only unstaged tracked modifications"},
		{Left: "--untracked", Right: "Only new untracked files"},
		{Left: "--diff", Right: "With change-selection flags: emit unified diff instead of full file"},
		{Left: "--contains PATTERN", Right: "Only files whose contents match regex pattern"},
		{Left: "--snippet", Right: "With --contains: emit only the matched block (blank-line bounded)"},
		{Left: "--then", Right: "Start a new scope (separate targets with different modifiers)"},
	})

	fmt.Fprintf(&b, "\n%s\n", dim("Patterns use shell glob syntax (*, ?, [...]), not regex."))
	fmt.Fprintf(&b, "%s\n", dim("Exception: --contains uses regex syntax."))
	fmt.Fprintf(&b, "%s\n", dim("Packaged installs include private fzf + ripgrep binaries. Exact paths and exact basenames bypass fzf; bundled fzf is only used when shorthand still has multiple viable matches."))
	fmt.Fprintf(&b, "%s\n", dim("Run 'catclip --help-all' for the full manual."))
	return b.String()
}

func fullHelpText(version string, colors colorPalette) string {
	var b strings.Builder
	cmd := func(s string) string { return colors.OK + s + colors.Reset }
	bold := func(s string) string { return colors.Bold + s + colors.Reset }
	dim := func(s string) string { return colors.Dim + s + colors.Reset }
	errText := func(s string) string { return colors.Err + s + colors.Reset }

	b.WriteString(shortHelpText(version, colors))
	fmt.Fprintf(&b, "\n\n%s\n\n", bold("━━━ Full Manual ━━━"))

	fmt.Fprintf(&b, "%s\n", bold("Scope System:"))
	fmt.Fprintf(&b, "  Use %s to separate scopes with different modifiers.\n", cmd("--then"))
	fmt.Fprintf(&b, "  Layout: %s\n\n", dim("TARGETS [MODIFIERS...] --then TARGETS [MODIFIERS...] ..."))
	fmt.Fprintf(&b, "  %s\n", cmd(`catclip src --only "*.ts" --exclude "*.test.ts" --then features --only "*.tsx"`))
	fmt.Fprintf(&b, "  %s\n", dim("  Scope 1: src — TypeScript files, skipping tests"))
	fmt.Fprintf(&b, "  %s\n\n", dim("  Scope 2: features — TSX files only"))
	fmt.Fprintf(&b, "  %s\n", dim("  Overlapping scopes are allowed; final copied files are deduplicated by path."))
	fmt.Fprintf(&b, "  Without %s, all targets share the same modifiers:\n", cmd("--then"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip src lib", Right: dim("Both use default rules")},
		{Left: `catclip src lib --only "*.ts"`, Right: dim("Both filtered to .ts files")},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("Target Resolution:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip", Right: "Interactive picker on TTY, then modifier prompt"},
		{Left: "catclip auth", Right: "Directory shorthand; resolves directly when unique, otherwise fzf"},
		{Left: "catclip Button.tsx", Right: "Near-instant exact basename lookup across safe files"},
		{Left: "catclip layout/Footer.tsx", Right: "Scoped shorthand; resolves directly when unique"},
		{Left: "catclip btn.tsx", Right: "File shorthand; resolves directly when unique, otherwise fzf"},
		{Left: "catclip src/components/ui/Button.tsx", Right: "Exact nested file path"},
	})
	fmt.Fprintf(&b, "  %s\n", dim("Plain targets stay independent: 'catclip src Button.tsx docs' does not bind Button.tsx to src."))
	fmt.Fprintf(&b, "  %s\n\n", dim("catclip handles exact targets directly; bundled fzf is only used for shorthand and fuzzy disambiguation when needed."))

	fmt.Fprintf(&b, "%s\n", bold("Safe By Default:"))
	b.WriteString("  Default discovery stays safe and respects .hiss + .gitignore.\n")
	b.WriteString("  Ignored files and directories require --include authorization.\n")
	b.WriteString("  --only narrows the selected targets; it does not authorize on its own.\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: `catclip --include tests`, Right: "Authorize tests/ for this run"},
		{Left: `catclip --include .env.production`, Right: "Authorize a blocked filename"},
		{Left: `catclip --include coverage --only "*.json"`, Right: "Authorize via include, then narrow"},
		{Left: `catclip index.js`, Right: "Safe basename search; blocked dir hits are skipped"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("--exclude (add rules):"))
	b.WriteString("  Adds temporary skip rules for this run only.\n")
	b.WriteString("  Comma-separated patterns. Trailing / = directory.\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: `--exclude "*.test.*"`, Right: "Skip test files"},
		{Left: `--exclude "*.test.*,*.snap"`, Right: "Skip tests and snapshots"},
		{Left: `--exclude "build/"`, Right: "Skip build directory"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("Editing Ignore Rules:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip --hiss", Right: "Open ignore config in editor"},
		{Left: "catclip --hiss-reset", Right: "Restore defaults"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("Ignore System:"))
	fmt.Fprintf(&b, "  Global config: %s (gitignore-inspired syntax)\n", cmd(displayPath(globalHissPath())))
	fmt.Fprintf(&b, "  First run is still safe: catclip creates the default %s and applies it immediately.\n", cmd(".hiss"))
	fmt.Fprintf(&b, "  Normal visible discovery combines %s and %s, using bundled ripgrep for Git-visible files and applying %s in Go.\n\n", cmd(".gitignore"), cmd(".hiss"), cmd(".hiss"))

	fmt.Fprintf(&b, "%s\n", bold("Example .hiss:"))
	fmt.Fprintf(&b, "  %s\n", dim("# Ignore build output"))
	fmt.Fprintf(&b, "  %s\n", dim("dist/"))
	fmt.Fprintf(&b, "  %s\n", dim("*.min.js"))
	fmt.Fprintf(&b, "  %s\n", dim("# Ignore specific file"))
	fmt.Fprintf(&b, "  %s\n\n", dim("test/fixtures.json"))

	fmt.Fprintf(&b, "%s\n", bold("--contains (content search):"))
	b.WriteString("  Filters to files whose contents match a regex pattern.\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "--contains TODO", Right: `Files containing "TODO"`},
		{Left: `--contains "useState|useEffect"`, Right: "Files matching either hook"},
		{Left: `--contains '\$store'`, Right: "Escaped special characters"},
	})
	fmt.Fprintf(&b, "  %s\n\n", dim("Plain text works for most searches. Use single quotes for special chars."))
	fmt.Fprintf(&b, "  %s\n", bold("Pattern types:"))
	writeAlignedHelpRows(&b, "    ", dim, []helpRow{
		{Left: "--only, --exclude", Right: "→ shell globs (*, ?, [...]) match filenames"},
		{Left: "--contains", Right: "→ regex syntax matches file contents"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("--snippet (block extraction):"))
	b.WriteString("  Requires --contains. Instead of the full file, emits only the semantic blocks\n")
	b.WriteString("  (blank-line-bounded) surrounding each match. Dramatically reduces token usage.\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip src --contains TODO --snippet", Right: "Blocks around each TODO"},
		{Left: `catclip . --contains "useState" --snippet`, Right: "React hook call-sites only"},
	})
	fmt.Fprintf(&b, "  Output: %s\n\n", dim(`<file path="..." snippet="42-57">...block...</file>`))

	fmt.Fprintf(&b, "%s\n", bold("--staged / --unstaged / --untracked (git filters):"))
	b.WriteString("  Composable alternatives to --changed (which is shorthand for all three).\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "--staged", Right: "Files in the git index (staged for commit)"},
		{Left: "--unstaged", Right: "Tracked modifications in working tree"},
		{Left: "--untracked", Right: "New files not yet tracked by git"},
	})
	fmt.Fprintf(&b, "  %s\n\n", "  These flags imply --changed; they can be combined:")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip src --staged --untracked", Right: "Staged + new, skip WIP edits"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("--diff (unified diff output):"))
	b.WriteString("  Requires a change-selection flag (--changed, --staged, --unstaged, or --untracked).\n")
	b.WriteString("  Emits the unified git diff instead of full file contents.\n")
	b.WriteString("  Untracked files have no diff and are emitted with their full content (type=\"untracked\").\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip --changed --diff", Right: "All modified files as patches"},
		{Left: "catclip --staged --diff", Right: "Staged changes only — ideal for commit review"},
		{Left: "catclip --unstaged --diff", Right: "WIP edits — what you're actively changing"},
	})
	fmt.Fprintf(&b, "  Output types: %s %s %s %s\n\n", dim(`type="staged-diff"`), dim(`type="unstaged-diff"`), dim(`type="diff"`), dim(`type="untracked"`))

	fmt.Fprintf(&b, "%s\n", bold("Evaluation Order (per scope):"))
	for i, line := range []string{
		"Load .hiss and merge --exclude into this scope's ignore rules",
		"Resolve targets (--include authorizes ignored ones)",
		"Discover candidate text files in the authorized scope",
		"Apply git visibility and change selectors",
		"Apply --contains content filtering",
		"Choose output mode (full file, snippet, or diff)",
		"Binary exclusion and text classification happen during discovery",
	} {
		fmt.Fprintf(&b, "  %s %s\n", dim(fmt.Sprintf("%d.", i+1)), line)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "%s\n", bold("Pattern Matching (shell globs, not regex):"))
	fmt.Fprintf(&b, "  Globs match against both basename and full path:\n    %s  %s  %s\n", dim("*.ts"), dim("test/*.ts"), dim("**/test/*.ts"))
	fmt.Fprintf(&b, "  Supported wildcards: %s (any chars), %s (single char), %s (char class)\n\n", dim("*"), dim("?"), dim("[...]"))

	fmt.Fprintf(&b, "%s\n", bold("Output Format:"))
	fmt.Fprintf(&b, "  Each file is wrapped in %s\n\n", dim(`<file path="path/to/file">`))

	fmt.Fprintf(&b, "%s\n", errText("Not Allowed:"))
	writeAlignedHelpRows(&b, "  ", dim, []helpRow{
		{Left: "catclip ../parent", Right: "Cannot go above working directory"},
		{Left: "catclip /abs/path", Right: "Absolute paths not allowed"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s %s  %s\n", bold("Config:"), dim(displayPath(globalHissPath())), dim("(catclip --hiss to edit)"))
	return b.String()
}

func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + string(filepath.Separator) + strings.TrimPrefix(p, home+string(filepath.Separator))
	}
	return p
}

func detectPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "linux":
		if data, err := os.ReadFile("/proc/version"); err == nil {
			version := strings.ToLower(string(data))
			if strings.Contains(version, "microsoft") || strings.Contains(version, "wsl") {
				return "wsl"
			}
		}
		return "linux"
	case "windows":
		return "wsl"
	default:
		return runtime.GOOS
	}
}

func loadVersion() string {
	const fallback = "dev"

	candidates := []string{"VERSION"}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "VERSION"))
	}
	for _, dir := range executableCandidateDirs() {
		candidates = append(candidates,
			filepath.Join(dir, "VERSION"),
			filepath.Join(dir, "..", "share", "catclip", "VERSION"),
		)
	}

	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		version := strings.TrimSpace(string(data))
		if version != "" {
			return version
		}
	}

	return fallback
}
