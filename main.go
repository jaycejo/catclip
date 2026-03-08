package main

// =============================================================================
// catclip — Context Gatherer for LLMs
//
// This is the Go rewrite of the original Bash implementation. The first pass
// is intentionally a single file so parity work stays easy to follow and easy
// to compare against the shell script.
//
// DESIGN:
//   1. Parity First     — preserve current CLI behavior before optimizing
//   2. Single File      — keep all control flow visible until the rewrite works
//   3. Typed Pipeline   — move from shell state to explicit Go structs
//   4. Low Dependency   — use the standard library first, shell out selectively
//   5. Split Later      — extract packages only after behavior is stable
//
// TARGET PIPELINE:
//   1. Parse args
//   2. Build scopes
//   3. Resolve targets
//   4. Discover files
//   5. Apply ignore/include/exclude/only filters
//   6. Apply git selectors
//   7. Apply contains/snippet selectors
//   8. Build preview metadata
//   9. Emit output
//
// CURRENT STATUS:
//   The current rewrite implements the normal full-file path:
//   - parsing and validation
//   - .hiss loading
//   - target resolution and discovery
//   - .gitignore-aware filtering
//   - changed/staged/unstaged/untracked file selection
//   - diff output for git-selected files
//   - ignore/include/exclude/only filtering
//   - plain --contains filtering
//   - snippet block extraction
//   - preview tree + summary
//   - full output emission to stdout or clipboard
//
//   Hiss editing/reset and snippet output are implemented.
// =============================================================================

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// =============================================================================
// Constants and types
// =============================================================================

type action string

const (
	actionRun       action = "run"
	actionHelp      action = "help"
	actionHelpAll   action = "help-all"
	actionVersion   action = "version"
	actionEditHiss  action = "hiss"
	actionResetHiss action = "hiss-reset"
)

type outputMode string

const (
	outputModeClipboard outputMode = "clipboard"
	outputModeStdout    outputMode = "stdout"
)

type runConfig struct {
	Action     action
	Version    string
	Platform   string
	WorkingDir string
	OutputMode outputMode

	Verbose bool
	Quiet   bool
	Yes     bool
	Print   bool
	Preview bool
	NoTree  bool

	Scopes   []scope
	Warnings []string
}

type scope struct {
	Targets   []string
	Only      []string
	Include   []string
	Exclude   []string
	NoIgnore  bool
	Contains  string
	Snippet   bool
	Changed   bool
	Staged    bool
	Unstaged  bool
	Untracked bool
	Diff      bool
}

type scopeBuilder struct {
	scope
	explicitChanged bool
}

type ignoreRuleKind string

const (
	ignoreRuleFile ignoreRuleKind = "file"
	ignoreRuleDir  ignoreRuleKind = "dir"
)

type ignoreRule struct {
	Raw     string
	Kind    ignoreRuleKind
	Pattern string
}

type compiledGlob struct {
	raw string
	re  *regexp.Regexp
}

type compiledDirRule struct {
	raw      string
	segments []*regexp.Regexp
}

type scopeMatcher struct {
	ignoreFiles []compiledGlob
	ignoreDirs  []compiledDirRule
	only        []compiledGlob
	forceFiles  []compiledGlob
	forceDirs   []compiledDirRule
}

type fileEntry struct {
	AbsPath          string
	RelPath          string
	Mode             entryMode
	SnippetPattern   string
	DiffWantStaged   bool
	DiffWantUnstaged bool
}

type gitContext struct {
	Enabled    bool
	Root       string
	WorkPrefix string
	HasHead    bool
}

type colorPalette struct {
	Reset  string
	Bold   string
	Dim    string
	OK     string
	Err    string
	Warn   string
	Dir    string
	Label  string
	Value  string
	Tree   string
	Prompt string
	Git    string
}

type outputReport struct {
	sizes     map[string]int64
	statuses  map[string]string
	modeTags  map[string]string
	landmarks map[string]bool
	humanSize string
	tokens    int64
	fileWord  string
}

type visibleDirIndex struct {
	dirs []string
	set  map[string]struct{}
}

type usageError struct {
	message string
}

type exitError struct {
	message string
	code    int
}

// Diagnostics are collected in encounter order so stderr matches the shell's
// target-by-target flow. Some of them are real "Error:" blocks that should
// still print under --quiet even when soft warnings are suppressed.
type diagnostic struct {
	message string
	isError bool
}

type entryMode string

const (
	entryModeFull    entryMode = "full"
	entryModeSnippet entryMode = "snippet"
	entryModeDiff    entryMode = "diff"
)

const tokenWarnThreshold = 100000

func (e usageError) Error() string {
	return e.message
}

func (e exitError) Error() string {
	return e.message
}

func newUsageError(format string, args ...any) error {
	return usageError{message: fmt.Sprintf(format, args...)}
}

func newExitError(code int, message string) error {
	return exitError{message: message, code: code}
}

// =============================================================================
// Main entrypoint
// =============================================================================

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		exitWithError(err, os.Stderr)
		return
	}

	if err := run(cfg, os.Stdout, os.Stderr); err != nil {
		exitWithError(err, os.Stderr)
	}
}

func run(cfg runConfig, stdout, stderr io.Writer) error {
	switch cfg.Action {
	case actionHelp:
		_, err := io.WriteString(stdout, shortHelpText(cfg.Version, activeColorPaletteForWriter(stdout)))
		return err
	case actionHelpAll:
		_, err := io.WriteString(stdout, fullHelpText(cfg.Version, activeColorPaletteForWriter(stdout)))
		return err
	case actionVersion:
		_, err := fmt.Fprintf(stdout, "catclip %s\n", cfg.Version)
		return err
	case actionEditHiss:
		return runEditHiss(cfg, stderr)
	case actionResetHiss:
		return runResetHiss(cfg, stderr)
	case actionRun:
		if err := validateImplementedFeatureSet(cfg); err != nil {
			return err
		}

		colors := activeColorPalette()
		started := time.Now()
		gitCtx := detectGitContext(cfg.WorkingDir)
		baseRules, err := loadIgnoreRules()
		if err != nil {
			return err
		}
		if proceed, err := warnDirectoryPatternSemantics(cfg, baseRules, stderr, colors); err != nil {
			return err
		} else if !proceed {
			return nil
		}

		if cfg.Verbose {
			fmt.Fprintf(stderr, "[verbose] parsed %d scope(s)\n", len(cfg.Scopes))
			for i, s := range cfg.Scopes {
				fmt.Fprintf(stderr, "[verbose] scope %d: %s\n", i+1, formatScopeSummary(s))
			}
		}
		var allEntries []fileEntry
		// Global parse-time warnings must participate in the same ordered stream
		// as per-target diagnostics, otherwise mixed warning/error cases drift
		// from the shell's stderr order.
		diagnostics := make([]diagnostic, 0, len(cfg.Warnings))
		for _, warning := range cfg.Warnings {
			diagnostics = append(diagnostics, diagnostic{message: warning})
		}
		hadSelectionCancel := false
		for i, s := range cfg.Scopes {
			scopeStarted := time.Now()
			entries, scopeDiagnostics, scopeSelectionCancel, err := evaluateScope(cfg, gitCtx, i, s, baseRules, stderr, colors)
			if err != nil {
				return err
			}
			if cfg.Verbose {
				fmt.Fprintf(stderr, "[verbose] scope %d: discovered %d file(s) in %s\n", i+1, len(entries), formatDuration(time.Since(scopeStarted)))
			}
			allEntries = append(allEntries, entries...)
			diagnostics = append(diagnostics, scopeDiagnostics...)
			hadSelectionCancel = hadSelectionCancel || scopeSelectionCancel
		}
		allEntries = dedupeEntriesByPath(allEntries)
		for _, diag := range diagnostics {
			if diag.isError || !cfg.Quiet {
				fmt.Fprintln(stderr, diag.message)
			}
		}
		if len(allEntries) == 0 {
			if err := writeNoFilesMatchedMessage(cfg, stderr, colors, hadSelectionCancel); err != nil {
				return err
			}
			return newExitError(1, "")
		}
		reportStarted := time.Now()
		report, err := buildOutputReport(cfg, gitCtx, allEntries)
		if err != nil {
			return err
		}
		if cfg.Verbose {
			fmt.Fprintf(stderr, "[verbose] report: %s\n", formatDuration(time.Since(reportStarted)))
		}
		if cfg.Preview {
			renderStarted := time.Now()
			err := renderPreview(cfg, gitCtx, allEntries, report, stdout, stderr, colors)
			if err != nil {
				return err
			}
			if cfg.Verbose {
				fmt.Fprintf(stderr, "[verbose] preview: %s\n", formatDuration(time.Since(renderStarted)))
				fmt.Fprintf(stderr, "[verbose] total: %s\n", formatDuration(time.Since(started)))
			}
			return nil
		}
		diagStarted := time.Now()
		proceed, err := writeNormalDiagnostics(cfg, gitCtx, allEntries, report, stderr, colors)
		if err != nil {
			return err
		}
		if cfg.Verbose {
			fmt.Fprintf(stderr, "[verbose] diagnostics: %s\n", formatDuration(time.Since(diagStarted)))
		}
		if !proceed {
			return nil
		}
		outputStarted := time.Now()
		if err := emitFullOutput(cfg, gitCtx, allEntries, stdout, colors); err != nil {
			return err
		}
		if cfg.Verbose {
			fmt.Fprintf(stderr, "[verbose] output: %s\n", formatDuration(time.Since(outputStarted)))
		}
		if cfg.OutputMode == outputModeClipboard && !cfg.Quiet {
			if err := writeClipboardSuccess(stderr, allEntries, colors); err != nil {
				return err
			}
		}
		if cfg.Verbose {
			fmt.Fprintf(stderr, "[verbose] total: %s\n", formatDuration(time.Since(started)))
		}
		return nil
	default:
		return fmt.Errorf("unknown action %q", cfg.Action)
	}
}

func exitWithError(err error, stderr io.Writer) {
	if err == nil {
		return
	}

	code := 1
	if _, ok := err.(usageError); ok {
		code = 2
	}
	if exitErr, ok := err.(exitError); ok {
		code = exitErr.code
	}
	if msg := strings.TrimSpace(err.Error()); msg != "" {
		fmt.Fprintln(stderr, msg)
	}
	os.Exit(code)
}

func (cfg runConfig) HeadlessStdoutMode() bool {
	// Quiet+print is a special contract in the shell version: machine-readable
	// stdout payload, no prompts, and no non-error stderr noise.
	return cfg.Print && cfg.Quiet
}

func activeColorPalette() colorPalette {
	return activeColorPaletteForWriter(os.Stderr)
}

func activeColorPaletteForWriter(w io.Writer) colorPalette {
	file, ok := w.(*os.File)
	// Match the shell contract: color is keyed off the real stderr TTY and can
	// be disabled globally via NO_COLOR.
	if !ok || os.Getenv("NO_COLOR") != "" || !isTerminalFile(file) {
		return colorPalette{}
	}
	return colorPalette{
		Reset:  "\033[0m",
		Bold:   "\033[1m",
		Dim:    "\033[2m",
		OK:     "\033[32m",
		Err:    "\033[31m",
		Warn:   "\033[33m",
		Dir:    "\033[1;34m",
		Label:  "\033[90m",
		Value:  "\033[1m",
		Tree:   "\033[90m",
		Prompt: "\033[36m",
		Git:    "\033[35m",
	}
}

// =============================================================================
// Interactive actions
// =============================================================================

func runEditHiss(cfg runConfig, stderr io.Writer) error {
	path, err := ensureGlobalHiss()
	if err != nil {
		return err
	}

	editor, err := resolveEditorCommand()
	if err != nil {
		return err
	}

	if !cfg.Quiet {
		fmt.Fprintf(stderr, "Opening %s in %s...\n", path, filepath.Base(editor.Path))
	}

	cmd := exec.Command(editor.Path, append(editor.Args, path)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runResetHiss(cfg runConfig, stderr io.Writer) error {
	path, err := ensureGlobalHiss()
	if err != nil {
		return err
	}

	shouldReset := cfg.Yes
	if !shouldReset {
		if !cfg.Quiet {
			fmt.Fprintf(stderr, "This will overwrite %s with defaults.\n", path)
		}
		shouldReset = promptYesNo("Are you sure? [y/N]", false, stderr)
	}

	if !shouldReset {
		if !cfg.Quiet {
			fmt.Fprintln(stderr, "Cancelled.")
		}
		return nil
	}

	if err := os.WriteFile(path, []byte(defaultHissContents), 0o644); err != nil {
		return err
	}
	if !cfg.Quiet {
		fmt.Fprintln(stderr, "Configuration restored.")
	}
	return nil
}

type editorCommand struct {
	Path string
	Args []string
}

func resolveEditorCommand() (editorCommand, error) {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "nano"
	}

	parts := strings.Fields(editor)
	if len(parts) == 0 {
		parts = []string{"nano"}
	}

	path, err := exec.LookPath(parts[0])
	if err != nil {
		if parts[0] != "nano" {
			if nanoPath, nanoErr := exec.LookPath("nano"); nanoErr == nil {
				return editorCommand{Path: nanoPath}, nil
			}
		}
		return editorCommand{}, errors.New("Error: no editor found. Set $EDITOR or install nano.")
	}

	return editorCommand{
		Path: path,
		Args: parts[1:],
	}, nil
}

func promptYesNo(prompt string, defaultYes bool, stderr io.Writer) bool {
	defaultResponse := "n"
	if defaultYes {
		defaultResponse = "y"
	}

	if response, ok := readPromptResponse(prompt, stderr); ok {
		response = strings.TrimSpace(strings.ToLower(response))
		if response == "" {
			response = defaultResponse
		}
		return response == "y" || response == "yes"
	}
	return defaultYes
}

func canPromptInteractively() bool {
	if isTerminalFile(os.Stdin) {
		return true
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	tty.Close()
	return true
}

func readPromptResponse(prompt string, stderr io.Writer) (string, bool) {
	if isTerminalFile(os.Stdin) {
		return readPromptKey(os.Stdin, stderr, prompt)
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer tty.Close()

	return readPromptKey(tty, tty, prompt)
}

func readLineResponse(prompt string, stderr io.Writer) (string, bool) {
	if isTerminalFile(os.Stdin) {
		return readPromptLine(os.Stdin, stderr, prompt)
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer tty.Close()

	return readPromptLine(tty, tty, prompt)
}

func readPromptKey(input *os.File, output io.Writer, prompt string) (string, bool) {
	// Match the shell UX: Y/N prompts should resolve on a single keystroke
	// without waiting for Enter when the user is on a real terminal.
	if response, ok := readPromptByte(input, output, prompt); ok {
		return response, true
	}
	return readPromptLine(input, output, prompt)
}

func readPromptByte(input *os.File, output io.Writer, prompt string) (string, bool) {
	state, err := getTerminalState(input)
	if err != nil {
		return "", false
	}
	raw := *state
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if _, err := fmt.Fprintf(output, "%s ", prompt); err != nil {
		return "", false
	}
	if err := setTerminalState(input, &raw); err != nil {
		return "", false
	}
	defer func() {
		_ = setTerminalState(input, state)
	}()

	var buf [1]byte
	n, readErr := input.Read(buf[:])
	if _, err := fmt.Fprint(output, "\n"); err != nil {
		return "", false
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", false
	}
	if n == 0 {
		return "", true
	}
	return string(buf[:n]), true
}

func readPromptLine(input *os.File, output io.Writer, prompt string) (string, bool) {
	if _, err := fmt.Fprintf(output, "%s ", prompt); err != nil {
		return "", false
	}
	var response string
	_, scanErr := fmt.Fscanln(input, &response)
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		return "", false
	}
	if errors.Is(scanErr, io.EOF) {
		return "", true
	}
	return response, true
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := getTerminalState(f)
	return err == nil
}

func getTerminalState(f *os.File) (*syscall.Termios, error) {
	reqGet, _, ok := terminalIOCTLRequests()
	if !ok {
		return nil, syscall.ENOTTY
	}
	state := &syscall.Termios{}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), reqGet, uintptr(unsafe.Pointer(state)))
	if errno != 0 {
		return nil, errno
	}
	return state, nil
}

func setTerminalState(f *os.File, state *syscall.Termios) error {
	_, reqSet, ok := terminalIOCTLRequests()
	if !ok {
		return syscall.ENOTTY
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), reqSet, uintptr(unsafe.Pointer(state)))
	if errno != 0 {
		return errno
	}
	return nil
}

func terminalIOCTLRequests() (uintptr, uintptr, bool) {
	switch runtime.GOOS {
	case "darwin":
		return 0x40487413, 0x80487414, true // TIOCGETA / TIOCSETA
	case "linux":
		return 0x5401, 0x5402, true // TCGETS / TCSETS
	default:
		return 0, 0, false
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func validateImplementedFeatureSet(cfg runConfig) error {
	return nil
}

// =============================================================================
// Argument parsing and validation
// =============================================================================

// The Bash CLI builds scopes as arguments are consumed. Keeping that parser
// shape in Go makes parity work much easier than forcing a flat flag model.
func parseArgs(args []string) (runConfig, error) {
	wd, err := os.Getwd()
	if err != nil {
		return runConfig{}, err
	}

	cfg := runConfig{
		Action:     actionRun,
		Version:    loadVersion(),
		Platform:   detectPlatform(),
		WorkingDir: wd,
		OutputMode: outputModeClipboard,
	}

	var current scopeBuilder

	finalize := func() error {
		if !current.hasContent() {
			current = scopeBuilder{}
			return nil
		}

		s := current.scope
		hasChangeSelector := current.explicitChanged || s.Staged || s.Unstaged || s.Untracked

		// Keep scope validation here so the parser can preserve the Bash rule that
		// modifiers are validated at scope boundaries, not only after all args.
		if s.Diff && s.Untracked && !current.explicitChanged && !s.Staged && !s.Unstaged {
			return newUsageError("Error: --untracked --diff doesn't make sense (untracked files have no diff).\n  Try: catclip --changed --diff    (includes untracked as full content)\n  Try: catclip --staged --diff     (only staged patches)")
		}
		if s.Snippet && s.Contains == "" {
			return newUsageError("Error: --snippet requires --contains (it extracts blocks around content matches).\n  Example: catclip src --contains 'TODO' --snippet")
		}
		if s.Snippet && s.Diff {
			return newUsageError("Error: --snippet and --diff cannot be combined.\n  Use --snippet to extract blocks around content matches.\n  Use --diff to show unified git patches.")
		}
		if s.Diff && !hasChangeSelector {
			return newUsageError("Error: --diff requires --changed, --staged, --unstaged, or --untracked.\n  Example: catclip src --changed --diff\n  Example: catclip src --staged --diff")
		}
		if s.Staged || s.Unstaged || s.Untracked || current.explicitChanged {
			s.Changed = true
		}
		if len(s.Targets) == 0 {
			// A modifier-only scope still acts on "." in the current CLI.
			s.Targets = []string{"."}
		}

		cfg.Scopes = append(cfg.Scopes, s)
		current = scopeBuilder{}
		return nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch arg {
		case "-h", "--help":
			cfg.Action = actionHelp
			return cfg, nil
		case "--help-all":
			cfg.Action = actionHelpAll
			return cfg, nil
		case "--version", "-V":
			cfg.Action = actionVersion
			return cfg, nil
		case "--hiss":
			// Keep parsing after the action token so global controls like --quiet
			// and --yes still affect interactive hiss flows.
			cfg.Action = actionEditHiss
		case "--hiss-reset":
			cfg.Action = actionResetHiss
		case "-v", "--verbose":
			cfg.Verbose = true
		case "-q", "--quiet":
			cfg.Quiet = true
		case "-y", "--yes":
			cfg.Yes = true
		case "-p", "--print":
			cfg.Print = true
			cfg.OutputMode = outputModeStdout
		case "-t", "--no-tree":
			cfg.NoTree = true
		case "--preview":
			cfg.Preview = true
		case "--changed":
			current.explicitChanged = true
			current.Changed = true
		case "--staged":
			current.Staged = true
		case "--unstaged":
			current.Unstaged = true
		case "--untracked":
			current.Untracked = true
		case "--diff":
			current.Diff = true
		case "--snippet":
			current.Snippet = true
		case "--then":
			if err := finalize(); err != nil {
				return runConfig{}, err
			}
		case "--":
			// Match standard CLI behavior: once "--" is seen, every remaining token
			// is treated as a positional target even if it looks like a flag.
			current.Targets = append(current.Targets, args[i+1:]...)
			i = len(args)
		case "--only":
			if i+1 >= len(args) {
				return runConfig{}, newUsageError("Error: --only requires a pattern.\n  Example: catclip src --only '*.ts'")
			}
			i++
			current.Only = append(current.Only, args[i])
		case "--include":
			if i+1 >= len(args) {
				return runConfig{}, newUsageError("Error: --include requires a pattern.\n  Example: catclip src --include 'tests/'")
			}
			i++
			if args[i] == "*" {
				current.NoIgnore = true
			} else {
				current.Include = append(current.Include, splitCommaPatterns(args[i])...)
			}
		case "--exclude":
			if i+1 >= len(args) {
				return runConfig{}, newUsageError("Error: --exclude requires a pattern.\n  Example: catclip src --exclude '*.test.*'")
			}
			i++
			current.Exclude = append(current.Exclude, splitCommaPatterns(args[i])...)
		case "--contains":
			if i+1 >= len(args) {
				return runConfig{}, newUsageError("Error: --contains requires a regex pattern.\n  Example: catclip src --contains 'TODO'")
			}
			i++
			current.Contains = args[i]
			if looksLikeGlobConfusion(current.Contains) {
				cfg.Warnings = append(cfg.Warnings, "Warning: --contains uses regex, not globs. Did you mean '.*' instead of '*'?\n  Example: --contains 'use.*Context' (not 'use*Context')")
			}
		default:
			switch {
			case strings.HasPrefix(arg, "--contains="):
				return runConfig{}, newUsageError("Error: --contains requires a space before the pattern.\n  Use: catclip src --contains 'pattern'\n  Not: catclip src --contains='pattern'")
			case strings.HasPrefix(arg, "--"):
				return runConfig{}, newUsageError("Error: Unknown option %s\n  Run 'catclip --help' for available options.", singleQuoted(arg))
			case strings.HasPrefix(arg, "-") && len(arg) > 1:
				return runConfig{}, newUsageError("Error: Unknown option %s\n  Run 'catclip --help' for available options.", singleQuoted(arg))
			default:
				current.Targets = append(current.Targets, arg)
			}
		}
	}

	if err := finalize(); err != nil {
		return runConfig{}, err
	}

	if len(cfg.Scopes) == 0 {
		cfg.Scopes = append(cfg.Scopes, scope{Targets: []string{"."}})
	}

	return cfg, nil
}

func (b scopeBuilder) hasContent() bool {
	return len(b.Targets) > 0 || len(b.Only) > 0 || len(b.Include) > 0 || len(b.Exclude) > 0 ||
		b.NoIgnore || b.Contains != "" || b.Snippet || b.Changed || b.Staged || b.Unstaged ||
		b.Untracked || b.Diff || b.explicitChanged
}

func splitCommaPatterns(value string) []string {
	// --include/--exclude keep the existing comma-separated shell UX. Parsing
	// stays narrow here so matching behavior can be implemented separately.
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func warnDirectoryPatternSemantics(cfg runConfig, baseRules []ignoreRule, stderr io.Writer, colors colorPalette) (bool, error) {
	if cfg.Quiet {
		return true, nil
	}

	ignoredDirs := make(map[string]struct{})
	for _, rule := range baseRules {
		if rule.Kind == ignoreRuleDir && !hasGlobChars(rule.Pattern) {
			ignoredDirs[rule.Pattern] = struct{}{}
		}
	}

	warned := make(map[string]struct{})
	for _, s := range cfg.Scopes {
		for _, spec := range []struct {
			name     string
			patterns []string
		}{
			{name: "--include", patterns: s.Include},
			{name: "--exclude", patterns: s.Exclude},
		} {
			for _, pat := range spec.patterns {
				if !looksLikeDirectoryPattern(cfg.WorkingDir, pat, ignoredDirs) {
					continue
				}
				key := spec.name + ":" + pat
				if _, ok := warned[key]; ok {
					continue
				}
				warned[key] = struct{}{}

				if _, err := fmt.Fprintf(stderr, "%sWarning:%s %s pattern %s looks like a directory name.\n", colors.Warn, colors.Reset, spec.name, singleQuoted(pat)); err != nil {
					return false, err
				}
				if _, err := fmt.Fprintf(stderr, "  %sDirectory rules require a trailing slash:%s %s%s/%s\n", colors.Dim, colors.Reset, colors.OK, pat, colors.Reset); err != nil {
					return false, err
				}
				if _, err := fmt.Fprintf(stderr, "  %sWithout '/', it is treated as a file pattern.%s\n", colors.Dim, colors.Reset); err != nil {
					return false, err
				}

				if cfg.Yes || cfg.HeadlessStdoutMode() || !canPromptInteractively() {
					continue
				}
				if !promptYesNo(colors.Prompt+"Continue with file-pattern semantics? [y/N]"+colors.Reset, false, stderr) {
					if _, err := fmt.Fprintf(stderr, "%sAborted.%s\n", colors.Warn, colors.Reset); err != nil {
						return false, err
					}
					return false, nil
				}
			}
		}
	}
	return true, nil
}

func hasGlobChars(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func looksLikeDirectoryPattern(workingDir, pattern string, ignoredDirs map[string]struct{}) bool {
	if pattern == "" || strings.HasSuffix(pattern, "/") || hasGlobChars(pattern) {
		return false
	}

	if info, err := os.Stat(filepath.Join(workingDir, filepath.FromSlash(pattern))); err == nil && info.IsDir() {
		return true
	}
	if _, ok := ignoredDirs[pattern]; ok {
		return true
	}

	// Keep the heuristic intentionally cheap. Bare slug-like names without dots
	// or slashes are commonly intended as directory rules (platform, common,
	// node_modules, tests) and are worth warning about before discovery starts.
	if strings.Contains(pattern, "/") || strings.Contains(pattern, ".") {
		return false
	}
	for i, r := range pattern {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		if i == 0 && r >= 'A' && r <= 'Z' {
			continue
		}
		return false
	}
	return pattern != ""
}

func looksLikeGlobConfusion(pattern string) bool {
	// Preserve the old warning for the common mistake of writing shell-glob style
	// patterns for --contains even though that flag uses regex semantics.
	return strings.Contains(pattern, "*") &&
		!strings.Contains(pattern, ".") &&
		!strings.Contains(pattern, "[") &&
		!strings.Contains(pattern, "|") &&
		!strings.Contains(pattern, "\\")
}

func formatScopeSummary(s scope) string {
	parts := []string{
		fmt.Sprintf("targets=%q", s.Targets),
		fmt.Sprintf("only=%q", s.Only),
		fmt.Sprintf("include=%q", s.Include),
		fmt.Sprintf("exclude=%q", s.Exclude),
	}
	if s.NoIgnore {
		parts = append(parts, "no_ignore=true")
	}
	if s.Contains != "" {
		parts = append(parts, fmt.Sprintf("contains=%q", s.Contains))
	}
	if s.Snippet {
		parts = append(parts, "snippet=true")
	}
	if s.Changed {
		parts = append(parts, "changed=true")
	}
	if s.Staged {
		parts = append(parts, "staged=true")
	}
	if s.Unstaged {
		parts = append(parts, "unstaged=true")
	}
	if s.Untracked {
		parts = append(parts, "untracked=true")
	}
	if s.Diff {
		parts = append(parts, "diff=true")
	}
	return strings.Join(parts, " ")
}

// =============================================================================
// Config and ignore rules
// =============================================================================

const defaultHissContents = `# catclip ignore config — gitignore-inspired syntax
#
# test/test.js  → specific file in specific directory
# *.test.js     → pattern, matches anywhere
# test/         → directory (trailing slash)
# test          → file named test
#
# Edit with: catclip --hiss

# Secrets (never leak these)
.env
.env.local
.env.*
*.pem
*.key
*.p12
*.pfx
id_rsa
id_ed25519
application.properties
application.yml
secrets.yaml
credentials.json

# Version Control
.git/
.svn/
.hg/

# System / Junk
.DS_Store
.AppleDouble
.LSOverride
*.log
*.tmp
*.bak
*.swp
*.swo

# IDEs & Editors
.idea/
.vscode/
.cursor/
.history/

# JavaScript / Node
node_modules/
bower_components/
jspm_packages/
coverage/
.npm/
.yarn/
.pnpm-store/

# Python
__pycache__/
venv/
.venv/
env/
.pytest_cache/
.mypy_cache/
.tox/
htmlcov/

# Java / Build
target/
build/
dist/
out/
bin/
obj/
.gradle/

# Web Frameworks
.next/
.nuxt/
.serverless/
.turbo/

# Test directories (remove these lines to include tests)
test/
tests/
__tests__/
fixtures/
__fixtures__/

# Lockfiles
package-lock.json
yarn.lock
pnpm-lock.yaml
poetry.lock
Pipfile.lock
Gemfile.lock
composer.lock
Cargo.lock
go.sum
`

func loadIgnoreRules() ([]ignoreRule, error) {
	path, err := ensureGlobalHiss()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseHiss(string(data))
}

func ensureGlobalHiss() (string, error) {
	path := globalHissPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(defaultHissContents), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func globalHissPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "catclip", ".hiss")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "catclip", ".hiss")
	}
	return filepath.Join(home, ".config", "catclip", ".hiss")
}

func parseHiss(contents string) ([]ignoreRule, error) {
	lines := strings.Split(contents, "\n")
	rules := make([]ignoreRule, 0, len(lines))
	for _, line := range lines {
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "/") {
			rules = append(rules, ignoreRule{
				Raw:     line,
				Kind:    ignoreRuleDir,
				Pattern: strings.TrimSuffix(line, "/"),
			})
			continue
		}
		rules = append(rules, ignoreRule{
			Raw:     line,
			Kind:    ignoreRuleFile,
			Pattern: line,
		})
	}
	return rules, nil
}

func buildScopeMatcher(baseRules []ignoreRule, s scope) (scopeMatcher, error) {
	if s.NoIgnore {
		baseRules = nil
	}

	// Match the shell flow: start from global .hiss rules, remove exact rules via
	// --include, then append additional rules from --exclude for this scope.
	rules := append([]ignoreRule(nil), baseRules...)
	for _, include := range s.Include {
		rules = removeIgnoreRule(rules, include)
	}
	for _, exclude := range s.Exclude {
		rules = append(rules, makeIgnoreRule(exclude))
	}

	matcher := scopeMatcher{}
	for _, rule := range rules {
		switch rule.Kind {
		case ignoreRuleFile:
			compiled, err := compileGlob(rule.Pattern)
			if err != nil {
				return scopeMatcher{}, newUsageError("Error: invalid ignore glob %q.", rule.Raw)
			}
			matcher.ignoreFiles = append(matcher.ignoreFiles, compiledGlob{raw: rule.Raw, re: compiled})
		case ignoreRuleDir:
			compiled, err := compileDirRule(rule.Pattern)
			if err != nil {
				return scopeMatcher{}, newUsageError("Error: invalid ignore glob %q.", rule.Raw)
			}
			matcher.ignoreDirs = append(matcher.ignoreDirs, compiled)
		}
	}
	for _, only := range s.Only {
		compiled, err := compileGlob(only)
		if err != nil {
			return scopeMatcher{}, newUsageError("Error: invalid --only glob %q.", only)
		}
		matcher.only = append(matcher.only, compiledGlob{raw: only, re: compiled})
	}
	for _, include := range s.Include {
		rule := makeIgnoreRule(include)
		switch rule.Kind {
		case ignoreRuleFile:
			compiled, err := compileGlob(rule.Pattern)
			if err != nil {
				return scopeMatcher{}, newUsageError("Error: invalid --include glob %q.", include)
			}
			matcher.forceFiles = append(matcher.forceFiles, compiledGlob{raw: include, re: compiled})
		case ignoreRuleDir:
			compiled, err := compileDirRule(rule.Pattern)
			if err != nil {
				return scopeMatcher{}, newUsageError("Error: invalid --include glob %q.", include)
			}
			matcher.forceDirs = append(matcher.forceDirs, compiled)
		}
	}
	return matcher, nil
}

func removeIgnoreRule(rules []ignoreRule, include string) []ignoreRule {
	want := makeIgnoreRule(include)
	out := rules[:0]
	for _, rule := range rules {
		if rule.Kind == want.Kind && rule.Raw == want.Raw {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func makeIgnoreRule(pattern string) ignoreRule {
	if strings.HasSuffix(pattern, "/") {
		return ignoreRule{Raw: pattern, Kind: ignoreRuleDir, Pattern: normalizeRulePattern(strings.TrimSuffix(pattern, "/"))}
	}
	return ignoreRule{Raw: pattern, Kind: ignoreRuleFile, Pattern: normalizeRulePattern(pattern)}
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	// Preserve the shell-style glob subset used by catclip matching:
	// * ? and [...] . The translator keeps "*" matching across path separators,
	// which is closer to the original Bash case semantics than filepath.Match.
	return compileGlobWithStar(pattern, ".*")
}

func normalizeRulePattern(pattern string) string {
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	pattern = strings.TrimPrefix(pattern, "./")
	pattern = path.Clean(pattern)
	if pattern == "." {
		return ""
	}
	return pattern
}

func compileSegmentGlob(pattern string) (*regexp.Regexp, error) {
	// Directory rules are matched segment-by-segment, so "*" and "?" must stay
	// inside one path segment instead of spanning slashes.
	return compileGlobWithStar(pattern, "[^/]*")
}

func compileGlobWithStar(pattern, star string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			b.WriteString(star)
		case '?':
			if star == "[^/]*" {
				b.WriteString("[^/]")
			} else {
				b.WriteByte('.')
			}
		case '[':
			end := i + 1
			for end < len(pattern) && pattern[end] != ']' {
				end++
			}
			if end >= len(pattern) {
				b.WriteString("\\[")
				continue
			}
			class := pattern[i : end+1]
			if class == "[]" {
				b.WriteString("\\[\\]")
			} else {
				b.WriteString(class)
			}
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func compileDirRule(pattern string) (compiledDirRule, error) {
	normalized := normalizeRulePattern(pattern)
	if normalized == "" {
		return compiledDirRule{}, newUsageError("Error: invalid empty directory glob.")
	}

	parts := strings.Split(normalized, "/")
	segments := make([]*regexp.Regexp, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		compiled, err := compileSegmentGlob(part)
		if err != nil {
			return compiledDirRule{}, err
		}
		segments = append(segments, compiled)
	}
	if len(segments) == 0 {
		return compiledDirRule{}, newUsageError("Error: invalid empty directory glob.")
	}
	return compiledDirRule{raw: pattern, segments: segments}, nil
}

func matchDirRule(relPath string, rule compiledDirRule) bool {
	if relPath == "." || relPath == "" {
		return false
	}

	parts := strings.Split(normalizeRelPath(relPath), "/")
	if len(parts) < len(rule.segments) {
		return false
	}
	for start := 0; start+len(rule.segments) <= len(parts); start++ {
		matched := true
		for i, segment := range rule.segments {
			if !segment.MatchString(parts[start+i]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (m scopeMatcher) fileAllowed(relPath string) bool {
	if ignored, _ := m.fileIgnored(relPath); ignored {
		return false
	}
	return m.matchesOnly(relPath)
}

func (m scopeMatcher) fileIgnored(relPath string) (bool, string) {
	basename := path.Base(relPath)
	// Preserve the shell matcher behavior: file rules are checked against both
	// basename and full relative path. Directory rules are path-aware and can
	// match contiguous multi-segment fragments like foo/bar/.
	for _, rule := range m.ignoreFiles {
		if rule.re.MatchString(basename) || rule.re.MatchString(relPath) {
			return true, rule.raw
		}
	}

	dirPart := path.Dir(relPath)
	if dirPart == "." || dirPart == "" {
		return false, ""
	}
	for _, rule := range m.ignoreDirs {
		if matchDirRule(dirPart, rule) {
			return true, rule.raw
		}
	}
	return false, ""
}

func (m scopeMatcher) dirIgnored(relPath string) (bool, string) {
	if relPath == "." || relPath == "" {
		return false, ""
	}
	for _, rule := range m.ignoreDirs {
		if matchDirRule(relPath, rule) {
			return true, rule.raw
		}
	}
	return false, ""
}

func (m scopeMatcher) matchesOnly(relPath string) bool {
	if len(m.only) == 0 {
		return true
	}
	basename := path.Base(relPath)
	for _, rule := range m.only {
		if rule.re.MatchString(basename) || rule.re.MatchString(relPath) {
			return true
		}
	}
	return false
}

func (m scopeMatcher) forceIncluded(relPath string) bool {
	basename := path.Base(relPath)
	for _, rule := range m.forceFiles {
		if rule.re.MatchString(basename) || rule.re.MatchString(relPath) {
			return true
		}
	}

	dirPart := path.Dir(relPath)
	if dirPart == "." || dirPart == "" {
		return false
	}
	for _, rule := range m.forceDirs {
		if matchDirRule(dirPart, rule) {
			return true
		}
	}
	return false
}

func (m scopeMatcher) dirForceIncluded(relPath string) bool {
	if relPath == "." || relPath == "" {
		return false
	}
	for _, rule := range m.forceDirs {
		if matchDirRule(relPath, rule) {
			return true
		}
	}
	return false
}

// =============================================================================
// Target resolution and discovery
// =============================================================================

var errSelectionCancelled = errors.New("selection cancelled")

type scopeResolver struct {
	cfg               runConfig
	gitCtx            gitContext
	matcher           scopeMatcher
	allowFileSymlinks bool
	useGitIgnore      bool
	visibleDirs       visibleDirIndex
	visibleDirsReady  bool
}

func evaluateScope(cfg runConfig, gitCtx gitContext, scopeIndex int, s scope, baseRules []ignoreRule, stderr io.Writer, colors colorPalette) ([]fileEntry, []diagnostic, bool, error) {
	matcher, err := buildScopeMatcher(baseRules, s)
	if err != nil {
		return nil, nil, false, err
	}
	resolver := scopeResolver{
		cfg:               cfg,
		gitCtx:            gitCtx,
		matcher:           matcher,
		allowFileSymlinks: gitCtx.Enabled,
		useGitIgnore:      gitCtx.Enabled && !s.NoIgnore,
	}

	// A scope still resolves targets first even in the Go rewrite. That keeps
	// warnings, fuzzy prompts, and ignored-target errors tied to the user's
	// original target list instead of being inferred later from discovered files.
	var diagnostics []diagnostic
	var relPaths []string
	hadSelectionCancel := false
	for _, target := range s.Targets {
		discovered, targetDiagnostics, selectionCancelled, err := resolver.resolveAndDiscoverTarget(scopeIndex, target, stderr, colors)
		if err != nil {
			return nil, diagnostics, hadSelectionCancel, err
		}
		diagnostics = append(diagnostics, targetDiagnostics...)
		relPaths = append(relPaths, discovered...)
		hadSelectionCancel = hadSelectionCancel || selectionCancelled
	}

	sort.Strings(relPaths)
	relPaths = dedupeSortedStrings(relPaths)

	if gitCtx.Enabled && !s.NoIgnore {
		relPaths, err = filterGitIgnoredPaths(gitCtx, matcher, relPaths)
		if err != nil {
			return nil, diagnostics, hadSelectionCancel, err
		}
	}

	if s.Changed {
		if gitCtx.Enabled {
			relPaths, err = filterChangedPaths(gitCtx, s, relPaths)
			if err != nil {
				return nil, diagnostics, hadSelectionCancel, err
			}
		} else {
			diagnostics = append(diagnostics, diagnostic{message: "Warning: --changed/--staged/--unstaged/--untracked require a git repo."})
		}
	}

	if s.Contains != "" {
		relPaths, err = filterPathsByContent(cfg.WorkingDir, relPaths, s.Contains)
		if err != nil {
			return nil, diagnostics, hadSelectionCancel, err
		}
	}

	entries := make([]fileEntry, 0, len(relPaths))
	for _, rel := range relPaths {
		mode := entryModeFull
		switch {
		case s.Diff:
			mode = entryModeDiff
		case s.Snippet:
			mode = entryModeSnippet
		}
		entries = append(entries, fileEntry{
			AbsPath:          filepath.Join(cfg.WorkingDir, filepath.FromSlash(rel)),
			RelPath:          rel,
			Mode:             mode,
			SnippetPattern:   s.Contains,
			DiffWantStaged:   s.Staged,
			DiffWantUnstaged: s.Unstaged,
		})
	}
	return entries, diagnostics, hadSelectionCancel, nil
}

func (r *scopeResolver) resolveAndDiscoverTarget(scopeIndex int, target string, stderr io.Writer, colors colorPalette) ([]string, []diagnostic, bool, error) {
	var diagnostics []diagnostic

	if filepath.IsAbs(target) {
		return nil, nil, false, newUsageError("Error: Absolute paths not allowed: %s\n  Use a relative path from your project root instead.", singleQuoted(target))
	}
	if containsParentTraversal(target) {
		return nil, nil, false, newUsageError("Error: Cannot traverse above working directory: %s\n  catclip only operates within the current directory tree.\n  Use a relative path from your project root instead.\n  Example: catclip config/", singleQuoted(target))
	}

	normalizedTarget := normalizeRelPath(target)
	if normalizedTarget == "" {
		normalizedTarget = "."
	}

	// Exact targets get first refusal. That preserves the shell distinction
	// between "this path exists but is ignored/non-text" and "this target does
	// not exist", which drives different guidance text.
	if discovered, handled, diag, err := r.resolveExactTarget(normalizedTarget, false, colors); handled {
		if diag != nil {
			diagnostics = append(diagnostics, *diag)
		}
		return discovered, diagnostics, false, err
	}

	if strings.Contains(normalizedTarget, "/") {
		dirPart := path.Dir(normalizedTarget)
		baseName := path.Base(normalizedTarget)
		// Chained targets fuzzy-resolve only the directory prefix. The final path
		// still goes back through exact resolution so ignored-file and ignored-dir
		// messaging stays consistent with direct targets.
		resolvedDir, err := r.resolveChainedDir(dirPart, stderr, colors)
		if err != nil {
			if errors.Is(err, errSelectionCancelled) {
				return nil, diagnostics, true, nil
			}
			return nil, diagnostics, false, err
		}
		fullRel := normalizeRelPath(path.Join(resolvedDir, baseName))
		discovered, handled, diag, err := r.resolveExactTarget(fullRel, true, colors)
		if handled {
			if diag != nil {
				diagnostics = append(diagnostics, *diag)
			}
			return discovered, diagnostics, false, err
		}
		diagnostics = append(diagnostics, diagnostic{message: targetNotFoundWarning(target, scopeIndex, colors)})
		return nil, diagnostics, false, nil
	}

	matches, err := r.fuzzySearchDirs(".", normalizedTarget)
	if err != nil {
		return nil, nil, false, err
	}
	switch len(matches) {
	case 0:
		diagnostics = append(diagnostics, diagnostic{message: targetNotFoundWarning(target, scopeIndex, colors)})
		return nil, diagnostics, false, nil
	case 1:
		files, err := discoverFilesUnder(r.cfg.WorkingDir, filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(matches[0])), matches[0], r.matcher, r.allowFileSymlinks)
		return files, diagnostics, false, err
	default:
		selected, err := chooseDirectoryMatch(r.cfg, target, ".", matches, stderr, colors)
		if err != nil {
			if errors.Is(err, errSelectionCancelled) {
				return nil, diagnostics, true, nil
			}
			return nil, nil, false, err
		}
		files, err := discoverFilesUnder(r.cfg.WorkingDir, filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(selected)), selected, r.matcher, r.allowFileSymlinks)
		return files, diagnostics, false, err
	}
}

func (r *scopeResolver) resolveExactTarget(relTarget string, fromChained bool, colors colorPalette) ([]string, bool, *diagnostic, error) {
	absTarget := filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(relTarget))
	info, err := os.Stat(absTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, true, nil, err
	}

	if info.IsDir() {
		if ignored, _ := r.matcher.dirIgnored(relTarget); ignored {
			return nil, true, &diagnostic{message: ignoredDirMessage(relTarget, colors), isError: true}, nil
		}
		files, err := discoverFilesUnder(r.cfg.WorkingDir, absTarget, relTarget, r.matcher, r.allowFileSymlinks)
		return files, true, nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, true, nil, nil
	}
	if ignored, rule := r.matcher.fileIgnored(relTarget); ignored {
		return nil, true, &diagnostic{message: ignoredFileMessage(relTarget, rule, fromChained, colors), isError: true}, nil
	}
	if !r.matcher.matchesOnly(relTarget) {
		return nil, true, nil, nil
	}
	if excludedTextLikeAsset(relTarget) {
		return nil, true, nil, nil
	}
	text, err := isProbablyTextFile(absTarget)
	if err != nil {
		return nil, true, nil, err
	}
	if !text {
		return nil, true, nil, nil
	}
	return []string{relTarget}, true, nil, nil
}

func (r *scopeResolver) resolveChainedDir(relPath string, stderr io.Writer, colors colorPalette) (string, error) {
	currentAbs := r.cfg.WorkingDir
	currentRel := "."

	for _, seg := range strings.Split(relPath, "/") {
		if seg == "" || seg == "." {
			continue
		}

		exactAbs := filepath.Join(currentAbs, seg)
		info, err := os.Stat(exactAbs)
		if err == nil && info.IsDir() {
			candidateRel := normalizeRelPath(path.Join(currentRel, seg))
			visible, err := r.dirVisible(candidateRel)
			if err != nil {
				return "", err
			}
			if visible {
				currentAbs = exactAbs
				currentRel = candidateRel
				continue
			}
		}

		matches, err := r.fuzzySearchDirs(currentRel, seg)
		if err != nil {
			return "", err
		}
		switch len(matches) {
		case 0:
			return "", fmt.Errorf("Error: No directory matching %s found in %s.\n  Check the spelling, or use --hiss to see if it's excluded.", singleQuoted(seg), currentRel)
		case 1:
			currentRel = matches[0]
		default:
			selected, err := chooseDirectoryMatch(r.cfg, seg, currentRel, matches, stderr, colors)
			if err != nil {
				return "", err
			}
			currentRel = selected
		}
		currentAbs = filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(currentRel))
	}

	return currentRel, nil
}

func (r *scopeResolver) dirVisible(relPath string) (bool, error) {
	if relPath == "." || relPath == "" {
		return true, nil
	}
	if ignored, _ := r.matcher.dirIgnored(relPath); ignored {
		return false, nil
	}
	if !r.useGitIgnore {
		return true, nil
	}
	if err := r.buildVisibleDirIndex(); err != nil {
		return false, err
	}
	_, ok := r.visibleDirs.set[relPath]
	return ok, nil
}

func (r *scopeResolver) buildVisibleDirIndex() error {
	if r.visibleDirsReady {
		return nil
	}

	dirs := make([]string, 0, 256)
	err := filepath.WalkDir(r.cfg.WorkingDir, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if current == r.cfg.WorkingDir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(r.cfg.WorkingDir, current)
		if err != nil {
			return err
		}
		rel = normalizeRelPath(rel)
		if ignored, _ := r.matcher.dirIgnored(rel); ignored {
			return fs.SkipDir
		}

		dirs = append(dirs, rel)
		return nil
	})
	if err != nil {
		return err
	}

	sort.Strings(dirs)
	if r.useGitIgnore {
		dirs, err = r.filterGitIgnoredDirs(dirs)
		if err != nil {
			return err
		}
	}

	r.visibleDirs = visibleDirIndex{
		dirs: dirs,
		set:  make(map[string]struct{}, len(dirs)),
	}
	for _, rel := range dirs {
		r.visibleDirs.set[rel] = struct{}{}
	}
	r.visibleDirsReady = true
	return nil
}

func (r *scopeResolver) filterGitIgnoredDirs(dirs []string) ([]string, error) {
	if len(dirs) == 0 {
		return dirs, nil
	}

	repoPaths := make([]string, 0, len(dirs))
	for _, rel := range dirs {
		repoPaths = append(repoPaths, r.gitCtx.toRepoPath(rel))
	}

	ignoredRepoPaths, err := runGitLines(r.gitCtx.Root, repoPaths, "check-ignore", "--stdin")
	if err != nil {
		return nil, err
	}
	ignored := make(map[string]struct{}, len(ignoredRepoPaths))
	for _, repoPath := range ignoredRepoPaths {
		ignored[normalizeRelPath(repoPath)] = struct{}{}
	}

	out := make([]string, 0, len(dirs))
	for _, rel := range dirs {
		repoPath := normalizeRelPath(r.gitCtx.toRepoPath(rel))
		if _, ok := ignored[repoPath]; ok && !r.matcher.dirForceIncluded(rel) {
			continue
		}
		out = append(out, rel)
	}
	return out, nil
}

func (r *scopeResolver) fuzzySearchDirs(baseRel, needle string) ([]string, error) {
	if err := r.buildVisibleDirIndex(); err != nil {
		return nil, err
	}

	baseRel = normalizeRelPath(baseRel)
	if baseRel == "" {
		baseRel = "."
	}
	prefix := ""
	if baseRel != "." {
		prefix = baseRel + "/"
	}
	needle = strings.ToLower(needle)

	matches := make([]string, 0, 16)
	for _, rel := range r.visibleDirs.dirs {
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			continue
		}
		if strings.Contains(strings.ToLower(path.Base(rel)), needle) {
			matches = append(matches, rel)
		}
	}
	return matches, nil
}

func chooseDirectoryMatch(cfg runConfig, needle, currentRel string, matches []string, stderr io.Writer, colors colorPalette) (string, error) {
	if cfg.HeadlessStdoutMode() || !canPromptInteractively() {
		if currentRel == "." {
			return "", fmt.Errorf("Error: Multiple directories match %s.\n  Use a more specific path segment to disambiguate.", singleQuoted(needle))
		}
		return "", fmt.Errorf("Error: Multiple directories match %s in %s.\n  Use a more specific path segment to disambiguate.", singleQuoted(needle), currentRel)
	}

	if _, err := fmt.Fprintf(stderr, "%sMultiple matches for %s:%s\n", colors.Bold, singleQuoted(needle), colors.Reset); err != nil {
		return "", err
	}
	for i, match := range matches {
		if _, err := fmt.Fprintf(stderr, "  %s%2d)%s %s%s%s\n", colors.Bold, i+1, colors.Reset, colors.Dir, match, colors.Reset); err != nil {
			return "", err
		}
	}
	if len(matches) > 5 {
		if _, err := fmt.Fprintf(stderr, "\n%sTip:%s %sToo many matches. Narrow with path segments:%s\n  %scatclip parent/%s%s\n", colors.Warn, colors.Reset, colors.Dim, colors.Reset, colors.OK, needle, colors.Reset); err != nil {
			return "", err
		}
	}

	response, ok := readLineResponse(fmt.Sprintf("Choose [1-%d]:", len(matches)), stderr)
	if !ok {
		return "", errSelectionCancelled
	}
	response = strings.TrimSpace(response)
	choice, err := strconv.Atoi(response)
	if err != nil || choice < 1 || choice > len(matches) {
		if _, err := fmt.Fprintf(stderr, "%sInvalid selection.%s\n", colors.Err, colors.Reset); err != nil {
			return "", err
		}
		return "", errSelectionCancelled
	}
	return matches[choice-1], nil
}

func targetNotFoundWarning(target string, scopeIndex int, colors colorPalette) string {
	if looksLikeFileTarget(path.Base(target)) || strings.Contains(path.Base(target), ".") {
		return fmt.Sprintf("%sWarning:%s %s not found at project root.\n\n  %sTo find it in subdirectories:%s\n    %scatclip . --only %q%s         %s# all matches%s\n    %scatclip src --only %q%s       %s# within src/%s",
			colors.Warn, colors.Reset, singleQuoted(target),
			colors.Dim, colors.Reset,
			colors.OK, target, colors.Reset, colors.Dim, colors.Reset,
			colors.OK, target, colors.Reset, colors.Dim, colors.Reset)
	}
	return fmt.Sprintf("%sWarning:%s No file or directory %s found (scope %d).\n\n  %sIf it's a file in a subdirectory:%s\n    %scatclip . --only %q%s\n\n  %sIf it's inside an ignored directory:%s\n    %scatclip . --include \"dir/\"%s",
		colors.Warn, colors.Reset, singleQuoted(target), scopeIndex+1,
		colors.Dim, colors.Reset,
		colors.OK, target, colors.Reset,
		colors.Dim, colors.Reset,
		colors.OK, colors.Reset)
}

func ignoredDirMessage(relTarget string, colors colorPalette) string {
	includeRule := path.Base(strings.TrimSuffix(relTarget, "/")) + "/"
	return fmt.Sprintf("\n%sError: %s is ignored by .hiss%s\n\n  %sTo include it this run:%s   %scatclip . --include %q%s\n  %sTo include everything:%s   %scatclip . --include \"*\"%s\n  %sTo remove permanently:%s   %scatclip --hiss%s %s(delete the rule)%s",
		colors.Err, singleQuoted(relTarget), colors.Reset,
		colors.Dim, colors.Reset, colors.OK, includeRule, colors.Reset,
		colors.Dim, colors.Reset, colors.OK, colors.Reset,
		colors.Dim, colors.Reset, colors.OK, colors.Reset, colors.Dim, colors.Reset)
}

func ignoredFileMessage(relTarget, rule string, fromChained bool, colors colorPalette) string {
	// The shell prints a slightly different footer for chained paths: it omits
	// the permanent-removal hint because the user may have resolved into a file
	// via fuzzy segments rather than naming the ignored file directly.
	message := fmt.Sprintf("\n%sError: %s is ignored by rule %s in .hiss%s\n\n  %sTo include it this run:%s   %scatclip . --include %q%s\n  %sTo include everything:%s   %scatclip . --include \"*\"%s",
		colors.Err, singleQuoted(relTarget), singleQuoted(rule), colors.Reset,
		colors.Dim, colors.Reset, colors.OK, rule, colors.Reset,
		colors.Dim, colors.Reset, colors.OK, colors.Reset)
	if fromChained {
		return message
	}
	return message + fmt.Sprintf("\n  %sTo remove permanently:%s   %scatclip --hiss%s %s(delete the rule)%s",
		colors.Dim, colors.Reset, colors.OK, colors.Reset, colors.Dim, colors.Reset)
}

func looksLikeFileTarget(base string) bool {
	if strings.Contains(base, ".") {
		return true
	}
	switch strings.ToLower(base) {
	case "makefile", "dockerfile", "containerfile", "jenkinsfile", "procfile",
		"gemfile", "rakefile", "guardfile", "vagrantfile", "cmakelists.txt",
		"configure", "configure.ac", ".gitignore", ".gitattributes", ".gitmodules",
		".gitkeep", ".keep", ".editorconfig", ".npmrc", ".yarnrc", ".nvmrc":
		return true
	default:
		return false
	}
}

func singleQuoted(value string) string {
	return "'" + value + "'"
}

func writeNoFilesMatchedMessage(cfg runConfig, stderr io.Writer, colors colorPalette, hadSelectionCancel bool) error {
	if hadSelectionCancel {
		// If the user cancelled an ambiguity prompt, the shell exits quietly
		// instead of stacking a misleading "No text files found" footer below it.
		return nil
	}

	anyChanged := false
	hasStaged := false
	hasUnstaged := false
	hasUntracked := false
	for _, s := range cfg.Scopes {
		anyChanged = anyChanged || s.Changed
		hasStaged = hasStaged || s.Staged
		hasUnstaged = hasUnstaged || s.Unstaged
		hasUntracked = hasUntracked || s.Untracked
	}

	if anyChanged {
		flags := "--changed"
		if hasStaged || hasUnstaged || hasUntracked {
			var parts []string
			if hasStaged {
				parts = append(parts, "--staged")
			}
			if hasUnstaged {
				parts = append(parts, "--unstaged")
			}
			if hasUntracked {
				parts = append(parts, "--untracked")
			}
			flags = strings.Join(parts, "/")
		}

		if _, err := fmt.Fprintf(stderr, "%sNo %s files found.%s\n", colors.Warn, flags, colors.Reset); err != nil {
			return err
		}
		switch {
		case hasStaged && !hasUnstaged && !hasUntracked:
			_, _ = fmt.Fprintf(stderr, "  %sNo files are staged for commit. Use 'git add' to stage changes.%s\n", colors.Dim, colors.Reset)
		case hasUnstaged && !hasStaged && !hasUntracked:
			_, _ = fmt.Fprintf(stderr, "  %sNo tracked files have uncommitted modifications.%s\n", colors.Dim, colors.Reset)
		case hasUntracked && !hasStaged && !hasUnstaged:
			_, _ = fmt.Fprintf(stderr, "  %sNo new untracked files in the target directories.%s\n", colors.Dim, colors.Reset)
		default:
			_, _ = fmt.Fprintf(stderr, "  %sYour working tree may be clean, or the target has no modifications.%s\n", colors.Dim, colors.Reset)
		}
		_, err := fmt.Fprintf(stderr, "  %sRun without %s to copy all files.%s\n", colors.Dim, flags, colors.Reset)
		return err
	}

	if _, err := fmt.Fprintf(stderr, "%sNo text files found matching your criteria.%s\n", colors.Warn, colors.Reset); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "\n  %sPossible causes:%s\n", colors.Dim, colors.Reset); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "  %s  1. Directory is empty or contains only binary files%s\n", colors.Dim, colors.Reset); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "  %s  2. All files matched by ignore rules%s\n", colors.Dim, colors.Reset); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "  %s  3. Typo in target name%s\n", colors.Dim, colors.Reset); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "\n  %sTry: catclip --hiss                   # view/edit ignore rules%s\n", colors.Dim, colors.Reset); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stderr, "  %s     catclip . --include \"*\"          # disable all ignore rules%s\n", colors.Dim, colors.Reset)
	return err
}

func discoverFilesUnder(workingDir, rootAbs, rootRel string, matcher scopeMatcher, allowFileSymlinks bool) ([]string, error) {
	// Walk the filesystem directly and apply .hiss-style filtering in process.
	// .gitignore is layered afterwards via the git phase so filesystem walking
	// and repository filtering stay separable.
	var files []string
	err := filepath.WalkDir(rootAbs, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(current)
			if err != nil {
				return nil
			}
			if info.IsDir() {
				return fs.SkipDir
			}
			if !allowFileSymlinks {
				return nil
			}
		}

		rel, err := filepath.Rel(workingDir, current)
		if err != nil {
			return err
		}
		rel = normalizeRelPath(rel)

		if d.IsDir() {
			if rel != normalizeRelPath(rootRel) {
				if ignored, _ := matcher.dirIgnored(rel); ignored {
					return fs.SkipDir
				}
			}
			return nil
		}
		info, err := os.Stat(current)
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if !matcher.fileAllowed(rel) {
			return nil
		}
		// The shell implementation excludes some text-formatted image assets from
		// broad discovery even though they are UTF-8. Keep that output parity here
		// instead of treating every text-like payload as copyable source context.
		if excludedTextLikeAsset(rel) {
			return nil
		}

		text, err := isProbablyTextFile(current)
		if err != nil {
			return err
		}
		if text {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func excludedTextLikeAsset(relPath string) bool {
	if _, blocked := knownBinaryBasenames[strings.ToLower(path.Base(relPath))]; blocked {
		return true
	}
	_, blocked := knownBinaryExts[shellStyleExtension(relPath)]
	return blocked
}

func shellStyleExtension(relPath string) string {
	base := strings.ToLower(path.Base(relPath))
	lastDot := strings.LastIndexByte(base, '.')
	if lastDot <= 0 || lastDot == len(base)-1 {
		return ""
	}
	return base[lastDot+1:]
}

// Keep a shell-parity hard block for extensions the Bash version rejects
// immediately, even when the payload is technically UTF-8 text.
// These include some text-encoded asset/config formats that surfaced in large
// parity sweeps (for example linux-master: .inf, .pbm, .ppm).
var knownBinaryBasenames = map[string]struct{}{
	".ds_store":              {},
	"thumbs.db":              {},
	"bad-nonprintable.bconf": {},
}

var knownBinaryExts = map[string]struct{}{
	"png": {}, "jpg": {}, "jpeg": {}, "gif": {}, "bmp": {}, "ico": {}, "svg": {}, "webp": {}, "tif": {}, "tiff": {}, "psd": {}, "xcf": {}, "heic": {}, "raw": {},
	"pdf": {}, "docx": {}, "doc": {}, "xlsx": {}, "xls": {}, "pptx": {}, "ppt": {}, "odt": {}, "ods": {}, "odp": {}, "rtf": {},
	"zip": {}, "tar": {}, "gz": {}, "bz2": {}, "xz": {}, "7z": {}, "rar": {}, "dmg": {}, "iso": {}, "img": {}, "vmdk": {}, "qcow2": {},
	"exe": {}, "dll": {}, "so": {}, "dylib": {}, "a": {}, "lib": {}, "o": {}, "obj": {}, "pdb": {},
	"class": {}, "jar": {}, "war": {}, "ear": {}, "pyc": {}, "pyo": {}, "pyd": {}, "wasm": {}, "beam": {}, "rlib": {},
	"apk": {}, "aab": {}, "ipa": {}, "msi": {}, "cab": {}, "deb": {}, "rpm": {},
	"pt": {}, "pth": {}, "ckpt": {}, "safetensors": {}, "onnx": {}, "gguf": {}, "h5": {}, "pkl": {}, "parquet": {}, "arrow": {},
	"mp3": {}, "mp4": {}, "mov": {}, "avi": {}, "mkv": {}, "webm": {}, "flv": {}, "wmv": {}, "m4a": {}, "wav": {}, "flac": {}, "ogg": {}, "3gp": {},
	"ttf": {}, "otf": {}, "woff": {}, "woff2": {}, "eot": {},
	"blend": {}, "glb": {}, "fbx": {}, "3ds": {},
	"db": {}, "sqlite": {}, "sqlite3": {}, "bin": {}, "dat": {}, "hex": {}, "dump": {}, "map": {}, "lockb": {},
	"pack": {}, "eslintcache": {}, "inf": {}, "pbm": {}, "ppm": {},
	"icns": {}, "xpm": {}, "scpt": {},
}

func containsParentTraversal(value string) bool {
	normalized := strings.ReplaceAll(value, "\\", "/")
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func normalizeRelPath(value string) string {
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\\", "/")
	value = path.Clean(value)
	value = strings.TrimPrefix(value, "./")
	if value == "." || value == "/" {
		return "."
	}
	return value
}

func dedupeSortedStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func dedupeEntriesByPath(entries []fileEntry) []fileEntry {
	if len(entries) == 0 {
		return entries
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].RelPath != entries[j].RelPath {
			return entries[i].RelPath < entries[j].RelPath
		}
		return entryModePriority(entries[i].Mode) > entryModePriority(entries[j].Mode)
	})

	out := entries[:1]
	for _, entry := range entries[1:] {
		last := &out[len(out)-1]
		if entry.RelPath != last.RelPath {
			out = append(out, entry)
			continue
		}
		if entryModePriority(entry.Mode) > entryModePriority(last.Mode) {
			*last = entry
			continue
		}
		if entry.Mode == entryModeDiff && last.Mode == entryModeDiff {
			last.DiffWantStaged = last.DiffWantStaged || entry.DiffWantStaged
			last.DiffWantUnstaged = last.DiffWantUnstaged || entry.DiffWantUnstaged
		}
	}
	return out
}

func entryModePriority(mode entryMode) int {
	switch mode {
	case entryModeDiff:
		return 2
	case entryModeSnippet:
		return 1
	default:
		return 0
	}
}

// =============================================================================
// Content filtering and text detection
// =============================================================================

// Git is layered on top of raw filesystem discovery so the non-repo path stays
// simple. The git phase removes .gitignored files and optionally narrows the
// scope to changed/staged/unstaged/untracked paths.
func detectGitContext(workingDir string) gitContext {
	root, err := runGitCapture(workingDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitContext{}
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return gitContext{}
	}

	prefix, err := runGitCapture(workingDir, "rev-parse", "--show-prefix")
	if err != nil {
		prefix = ""
	}
	hasHead := runGitNoOutput(root, "rev-parse", "--verify", "HEAD") == nil

	return gitContext{
		Enabled:    true,
		Root:       filepath.Clean(root),
		WorkPrefix: normalizeGitPrefix(prefix),
		HasHead:    hasHead,
	}
}

func normalizeGitPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.TrimSuffix(prefix, "/")
	prefix = strings.TrimPrefix(prefix, "./")
	return strings.ReplaceAll(prefix, "\\", "/")
}

func filterGitIgnoredPaths(gitCtx gitContext, matcher scopeMatcher, relPaths []string) ([]string, error) {
	if len(relPaths) == 0 {
		return relPaths, nil
	}

	repoPaths := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		repoPaths = append(repoPaths, gitCtx.toRepoPath(relPath))
	}

	ignoredRepoPaths, err := runGitLines(gitCtx.Root, repoPaths, "check-ignore", "--stdin")
	if err != nil {
		return nil, err
	}
	ignored := make(map[string]struct{}, len(ignoredRepoPaths))
	for _, repoPath := range ignoredRepoPaths {
		ignored[normalizeRelPath(repoPath)] = struct{}{}
	}

	out := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		repoPath := normalizeRelPath(gitCtx.toRepoPath(relPath))
		if _, ok := ignored[repoPath]; ok && !matcher.forceIncluded(relPath) {
			continue
		}
		out = append(out, relPath)
	}
	return out, nil
}

func filterChangedPaths(gitCtx gitContext, s scope, relPaths []string) ([]string, error) {
	changedRepoPaths, err := collectChangedRepoPaths(gitCtx, s)
	if err != nil {
		return nil, err
	}

	changed := make(map[string]struct{}, len(changedRepoPaths))
	for _, repoPath := range changedRepoPaths {
		changed[normalizeRelPath(repoPath)] = struct{}{}
	}

	out := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		if _, ok := changed[normalizeRelPath(gitCtx.toRepoPath(relPath))]; ok {
			out = append(out, relPath)
		}
	}
	return out, nil
}

func collectChangedRepoPaths(gitCtx gitContext, s scope) ([]string, error) {
	wantStaged, wantUnstaged, wantUntracked := changeSelection(s)
	set := make(map[string]struct{})

	if wantStaged {
		lines, err := runGitLines(gitCtx.Root, nil, "diff", "--name-only", "--cached")
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			set[normalizeRelPath(line)] = struct{}{}
		}
	}
	if wantUnstaged {
		lines, err := runGitLines(gitCtx.Root, nil, "diff", "--name-only")
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			set[normalizeRelPath(line)] = struct{}{}
		}
	}
	if wantUntracked {
		lines, err := runGitLines(gitCtx.Root, nil, "ls-files", "--others", "--exclude-standard")
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			set[normalizeRelPath(line)] = struct{}{}
		}
	}
	if wantStaged && wantUnstaged && wantUntracked && gitCtx.HasHead {
		lines, err := runGitLines(gitCtx.Root, nil, "diff", "--name-only", "HEAD")
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			set[normalizeRelPath(line)] = struct{}{}
		}
	}

	out := make([]string, 0, len(set))
	for line := range set {
		if workPath := gitCtx.toWorkPath(line); workPath != "" {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out, nil
}

func changeSelection(s scope) (wantStaged, wantUnstaged, wantUntracked bool) {
	if s.Staged || s.Unstaged || s.Untracked {
		return s.Staged, s.Unstaged, s.Untracked
	}
	return true, true, true
}

func runGitCapture(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func runGitNoOutput(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

func runGitLines(dir string, stdin []string, args ...string) ([]string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if len(stdin) > 0 {
		cmd.Stdin = strings.NewReader(strings.Join(stdin, "\n") + "\n")
	}

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = normalizeRelPath(line)
	}
	return lines, nil
}

func diffAgainstHeadOrIndex(gitCtx gitContext, repoPath string) (string, error) {
	if gitCtx.HasHead {
		return runGitCapture(gitCtx.Root, "diff", "HEAD", "--", repoPath)
	}

	stagedDiff, stagedErr := runGitCapture(gitCtx.Root, "diff", "--cached", "--", repoPath)
	if stagedErr != nil {
		return "", stagedErr
	}
	unstagedDiff, unstagedErr := runGitCapture(gitCtx.Root, "diff", "--", repoPath)
	if unstagedErr != nil {
		return "", unstagedErr
	}

	switch {
	case stagedDiff == "":
		return unstagedDiff, nil
	case unstagedDiff == "":
		return stagedDiff, nil
	default:
		return stagedDiff + "\n" + unstagedDiff, nil
	}
}

func (g gitContext) toRepoPath(workRel string) string {
	workRel = normalizeRelPath(workRel)
	if g.WorkPrefix == "" {
		return workRel
	}
	return normalizeRelPath(path.Join(g.WorkPrefix, workRel))
}

func (g gitContext) toWorkPath(repoRel string) string {
	repoRel = normalizeRelPath(repoRel)
	if g.WorkPrefix == "" {
		return repoRel
	}
	prefix := normalizeRelPath(g.WorkPrefix)
	if repoRel == prefix {
		return "."
	}
	if strings.HasPrefix(repoRel, prefix+"/") {
		return normalizeRelPath(strings.TrimPrefix(repoRel, prefix+"/"))
	}
	return ""
}

func filterPathsByContent(workingDir string, relPaths []string, pattern string) ([]string, error) {
	// Match on logical lines instead of whole-file blobs so behavior stays close
	// to grep -E and snippet extraction can reuse the same regex semantics.
	re, err := compileContainsPattern(pattern)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		match, err := fileHasMatchingLine(filepath.Join(workingDir, filepath.FromSlash(relPath)), re)
		if err != nil {
			return nil, err
		}
		if match {
			out = append(out, relPath)
		}
	}
	return out, nil
}

func compileContainsPattern(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, newUsageError("Error: invalid --contains regex: %v.", err)
	}
	return re, nil
}

func fileHasMatchingLine(absPath string, re *regexp.Regexp) (bool, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false, err
	}
	for _, line := range splitLogicalLines(data) {
		if re.MatchString(line) {
			return true, nil
		}
	}
	return false, nil
}

func splitLogicalLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}

	// Keep the shell/grep notion of "logical lines": split on '\n' only, do not
	// invent a trailing empty record for files that end with a newline.
	lines := make([]string, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for start < len(data) {
		offset := bytes.IndexByte(data[start:], '\n')
		if offset < 0 {
			lines = append(lines, string(data[start:]))
			break
		}
		end := start + offset
		lines = append(lines, string(data[start:end]))
		start = end + 1
	}
	return lines
}

func isProbablyTextFile(path string) (bool, error) {
	const sniffSize = 8192

	// First pass heuristic: reject NUL-containing files immediately, accept valid
	// UTF-8, and treat high-control-byte payloads as likely binary. This is
	// intentionally simpler than the shell script's file(1)+extension pipeline.
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, sniffSize)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if n == 0 {
		return true, nil
	}
	buf = buf[:n]
	if bytes.IndexByte(buf, 0) >= 0 {
		return false, nil
	}
	if utf8.Valid(buf) {
		return true, nil
	}

	control := 0
	for _, b := range buf {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' && b != '\f' {
			control++
		}
	}
	return control <= len(buf)/10, nil
}

// =============================================================================
// Preview rendering
// =============================================================================

func buildOutputReport(cfg runConfig, gitCtx gitContext, entries []fileEntry) (outputReport, error) {
	sizes, totalBytes, err := collectFileSizes(entries)
	if err != nil {
		return outputReport{}, err
	}

	report := outputReport{
		sizes: sizes,
	}

	if !cfg.NoTree {
		if gitCtx.Enabled {
			report.statuses, err = collectGitStatusMap(gitCtx)
			if err != nil {
				return outputReport{}, err
			}
		}
		report.modeTags = buildPreviewModeTags(entries, report.statuses)
		report.landmarks = detectLandmarkDirs(entries)
	}

	report.humanSize, report.tokens = formatSizeAndTokens(totalBytes, len(entries))
	report.fileWord = "files"
	if len(entries) == 1 {
		report.fileWord = "file"
	}
	return report, nil
}

func renderPreview(cfg runConfig, gitCtx gitContext, entries []fileEntry, report outputReport, stdout, stderr io.Writer, colors colorPalette) error {
	if !cfg.Quiet {
		if err := writeFilterSummary(stderr, cfg, gitCtx, colors); err != nil {
			return err
		}
	}

	if !cfg.NoTree {
		if err := printPreviewTree(stdout, entries, report.sizes, report.statuses, report.modeTags, report.landmarks, colors); err != nil {
			return err
		}
	}

	return writeSummary(stdout, report, colors)
}

func writeNormalDiagnostics(cfg runConfig, gitCtx gitContext, entries []fileEntry, report outputReport, stderr io.Writer, colors colorPalette) (bool, error) {
	if !cfg.Quiet {
		if err := writeFilterSummary(stderr, cfg, gitCtx, colors); err != nil {
			return false, err
		}
		if !cfg.NoTree {
			if err := printPreviewTree(stderr, entries, report.sizes, report.statuses, report.modeTags, report.landmarks, colors); err != nil {
				return false, err
			}
		}
		if err := writeSummary(stderr, report, colors); err != nil {
			return false, err
		}
		if report.tokens > tokenWarnThreshold {
			if _, err := fmt.Fprintf(stderr, "  %s~%d tokens may exceed some LLM context windows.%s\n", colors.Warn, report.tokens, colors.Reset); err != nil {
				return false, err
			}
		}
	}

	if report.tokens <= tokenWarnThreshold || cfg.Yes || cfg.Quiet || cfg.HeadlessStdoutMode() {
		return true, nil
	}

	if promptYesNo(colors.Prompt+"Proceed? [y/N]"+colors.Reset, false, stderr) {
		return true, nil
	}
	if _, err := fmt.Fprintf(stderr, "%sAborted.%s\n", colors.Warn, colors.Reset); err != nil {
		return false, err
	}
	return false, nil
}

func writeFilterSummary(w io.Writer, cfg runConfig, gitCtx gitContext, colors colorPalette) error {
	anyNoIgnore := false
	for _, s := range cfg.Scopes {
		if s.NoIgnore {
			anyNoIgnore = true
			break
		}
	}
	if anyNoIgnore {
		_, err := fmt.Fprintf(w, "  %sSafety filters disabled (+\"*\")%s\n", colors.Warn, colors.Reset)
		return err
	}
	shortConfig := shortPath(globalHissPath())
	if gitCtx.Enabled && repoHasGitIgnore(gitCtx) {
		_, err := fmt.Fprintf(w, "  %sFiltered by .gitignore + %s%s\n", colors.Git, shortConfig, colors.Reset)
		return err
	}
	if gitCtx.Enabled {
		_, err := fmt.Fprintf(w, "  %sGit repo, but no .gitignore; only %s applies.%s\n", colors.Warn, shortConfig, colors.Reset)
		return err
	}
	_, err := fmt.Fprintf(w, "  %sNot a git repo; only %s applies.%s\n", colors.Warn, shortConfig, colors.Reset)
	return err
}

func writeSummary(w io.Writer, report outputReport, colors colorPalette) error {
	if _, err := fmt.Fprintf(w, "\n  %s%-8s%s %s%d %s%s\n", colors.Label, "Count:", colors.Reset, colors.Value, len(report.sizes), report.fileWord, colors.Reset); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  %s%-8s%s %s%s%s\n", colors.Label, "Size:", colors.Reset, colors.Value, report.humanSize, colors.Reset); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "  %s%-8s%s %s~%d%s\n", colors.Label, "Tokens:", colors.Reset, colors.Value, report.tokens, colors.Reset)
	return err
}

func writeClipboardSuccess(w io.Writer, entries []fileEntry, colors colorPalette) error {
	if len(entries) == 0 {
		return nil
	}
	first := entries[0].RelPath
	last := entries[len(entries)-1].RelPath
	fileWord := "files"
	if len(entries) == 1 {
		fileWord = "file"
	}

	switch {
	case len(entries) == 1:
		_, err := fmt.Fprintf(w, "\n%sCopied%s %s%s%s %sto clipboard%s\n", colors.OK, colors.Reset, colors.Bold, first, colors.Reset, colors.OK, colors.Reset)
		return err
	case first == last:
		_, err := fmt.Fprintf(w, "\n%sCopied%s %s%d %s%s %sto clipboard%s\n", colors.OK, colors.Reset, colors.Bold, len(entries), fileWord, colors.Reset, colors.OK, colors.Reset)
		return err
	default:
		_, err := fmt.Fprintf(w, "\n%sCopied%s %s%d %s%s %sto clipboard%s %s(%s ... %s)%s\n", colors.OK, colors.Reset, colors.Bold, len(entries), fileWord, colors.Reset, colors.OK, colors.Reset, colors.Dim, first, last, colors.Reset)
		return err
	}
}

func repoHasGitIgnore(gitCtx gitContext) bool {
	if !gitCtx.Enabled {
		return false
	}
	info, err := os.Stat(filepath.Join(gitCtx.Root, ".gitignore"))
	return err == nil && !info.IsDir()
}

func shortPath(value string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return value
	}
	home = filepath.Clean(home)
	value = filepath.Clean(value)
	if value == home {
		return "~"
	}
	if strings.HasPrefix(value, home+string(os.PathSeparator)) {
		return "~" + string(os.PathSeparator) + strings.TrimPrefix(value, home+string(os.PathSeparator))
	}
	return value
}

func collectFileSizes(entries []fileEntry) (map[string]int64, int64, error) {
	sizes := make(map[string]int64, len(entries))
	var total int64
	for _, entry := range entries {
		// Preview parity follows the shell script's stat-based accounting, which
		// counts file symlinks by link size rather than dereferenced target size.
		info, err := os.Lstat(entry.AbsPath)
		if err != nil {
			return nil, 0, err
		}
		size := info.Size()
		sizes[entry.RelPath] = size
		total += size
	}
	return sizes, total, nil
}

func collectGitStatusMap(gitCtx gitContext) (map[string]string, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = gitCtx.Root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	statuses := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" || len(line) < 4 {
			continue
		}
		xy := line[:2]
		pathPart := line[3:]
		if strings.Contains(pathPart, " -> ") {
			parts := strings.Split(pathPart, " -> ")
			pathPart = parts[len(parts)-1]
		}
		repoPath := normalizeRelPath(pathPart)
		workPath := gitCtx.toWorkPath(repoPath)
		if workPath == "" {
			continue
		}

		switch {
		case xy == "??":
			statuses[workPath] = "?"
		case len(xy) >= 1 && xy[0] == 'A':
			statuses[workPath] = "A"
		case len(xy) >= 1 && xy[0] != ' ' && xy[0] != '?':
			statuses[workPath] = "S"
		case len(xy) >= 2 && (xy[1] == 'M' || xy[1] == 'D'):
			statuses[workPath] = "M"
		}
	}
	return statuses, nil
}

func detectLandmarkDirs(entries []fileEntry) map[string]bool {
	nameCounts := map[string]map[string]struct{}{}
	for _, entry := range entries {
		dir := path.Dir(entry.RelPath)
		if dir == "." {
			continue
		}
		accum := ""
		for _, segment := range strings.Split(dir, "/") {
			if segment == "" || segment == "." {
				continue
			}
			if accum == "" {
				accum = segment
			} else {
				accum = accum + "/" + segment
			}
			if nameCounts[segment] == nil {
				nameCounts[segment] = map[string]struct{}{}
			}
			nameCounts[segment][accum] = struct{}{}
		}
	}

	landmarks := map[string]bool{}
	for _, paths := range nameCounts {
		if len(paths) <= 1 {
			continue
		}
		for full := range paths {
			landmarks[full] = true
		}
	}
	return landmarks
}

func buildPreviewModeTags(entries []fileEntry, statuses map[string]string) map[string]string {
	tags := make(map[string]string)
	for _, entry := range entries {
		switch entry.Mode {
		case entryModeDiff:
			if statuses[entry.RelPath] == "?" {
				continue
			}
			tags[entry.RelPath] = "diff only"
		case entryModeSnippet:
			tags[entry.RelPath] = "snippet only"
		}
	}
	return tags
}

func printPreviewTree(w io.Writer, entries []fileEntry, sizes map[string]int64, statuses map[string]string, modeTags map[string]string, landmarks map[string]bool, colors colorPalette) error {
	lastParts := []string{}
	lineCount := 0
	for _, entry := range entries {
		parts := strings.Split(entry.RelPath, "/")
		fileIndex := len(parts) - 1

		common := 0
		for common < fileIndex && common < len(lastParts) && lastParts[common] == parts[common] {
			common++
		}

		accum := ""
		for i := 0; i < fileIndex; i++ {
			if accum == "" {
				accum = parts[i]
			} else {
				accum += "/" + parts[i]
			}
			if i < common {
				continue
			}

			prefix := treeIndent(i, colors)
			label := parts[i] + "/"
			if i > 0 && (landmarks[accum] || lineCount >= 24) {
				label += " " + colors.Dim + "(" + accum + "/)" + colors.Reset
				lineCount = 0
			}
			if _, err := fmt.Fprintf(w, "%s%s%s%s\n", prefix, colors.Dir, label, colors.Reset); err != nil {
				return err
			}
			lineCount++
		}

		filePrefix := treeIndent(fileIndex, colors)
		if fileIndex == 0 {
			filePrefix = colors.Tree + "├── " + colors.Reset
		}
		fileLine := filePrefix + parts[fileIndex]
		if size, ok := sizes[entry.RelPath]; ok {
			fileLine += " " + styleSize(formatInlineSize(size), size, colors)
		}
		if status := statuses[entry.RelPath]; status != "" {
			fileLine += " " + styleStatus(status, colors)
		}
		if tag := modeTags[entry.RelPath]; tag != "" {
			fileLine += " " + colors.Git + "[" + tag + "]" + colors.Reset
		}
		if _, err := fmt.Fprintln(w, fileLine); err != nil {
			return err
		}
		lineCount++
		lastParts = parts[:fileIndex]
	}
	return nil
}

func treeIndent(depth int, colors colorPalette) string {
	if depth <= 0 {
		return ""
	}
	return strings.Repeat(colors.Tree+"│   "+colors.Reset, depth) + colors.Tree + "├── " + colors.Reset
}

func styleSize(label string, size int64, colors colorPalette) string {
	switch {
	case size < 40000:
		return colors.Dim + "(" + label + ")" + colors.Reset
	case size < 200000:
		return colors.Warn + "(" + label + ")" + colors.Reset
	default:
		return colors.Err + "(" + label + ")" + colors.Reset
	}
}

func styleStatus(status string, colors colorPalette) string {
	switch status {
	case "M":
		return colors.Warn + "[M]" + colors.Reset
	case "S":
		return colors.OK + "[S]" + colors.Reset
	case "?":
		return colors.Git + "[?]" + colors.Reset
	case "A":
		return colors.OK + "[A]" + colors.Reset
	default:
		return "[" + status + "]"
	}
}

func formatInlineSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}

func formatSizeAndTokens(totalBytes int64, fileCount int) (string, int64) {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)

	totalBytes += int64(fileCount) * 30
	tokens := totalBytes / 4

	switch {
	case totalBytes < kb:
		return fmt.Sprintf("%.2fB", float64(totalBytes)), tokens
	case totalBytes < mb:
		return fmt.Sprintf("%.2fKB", float64(totalBytes)/kb), tokens
	case totalBytes < gb:
		return fmt.Sprintf("%.2fMB", float64(totalBytes)/mb), tokens
	default:
		return fmt.Sprintf("%.2fGB", float64(totalBytes)/gb), tokens
	}
}

// =============================================================================
// Output and clipboard
// =============================================================================

func emitFullOutput(cfg runConfig, gitCtx gitContext, entries []fileEntry, stdout io.Writer, colors colorPalette) error {
	return withPayloadWriter(cfg, stdout, colors, func(w io.Writer) error {
		for _, entry := range entries {
			if err := emitEntry(w, gitCtx, entry); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitEntry(w io.Writer, gitCtx gitContext, entry fileEntry) error {
	switch entry.Mode {
	case entryModeDiff:
		return emitDiffEntry(w, gitCtx, entry)
	case entryModeSnippet:
		return emitSnippetEntry(w, entry)
	default:
		return emitFile(w, entry)
	}
}

func emitFile(w io.Writer, entry fileEntry) error {
	data, err := os.ReadFile(entry.AbsPath)
	if err != nil {
		return err
	}
	return emitWrappedFile(w, entry.RelPath, "", data)
}

func emitWrappedFile(w io.Writer, relPath, typeAttr string, data []byte) error {
	if typeAttr == "" {
		if _, err := fmt.Fprintf(w, "<file path=\"%s\">\n", relPath); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "<file path=\"%s\" type=\"%s\">\n", relPath, typeAttr); err != nil {
			return err
		}
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			return err
		}
		if data[len(data)-1] != '\n' {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(w, "</file>\n\n")
	return err
}

type snippetRange struct {
	Start int
	End   int
}

func emitSnippetEntry(w io.Writer, entry fileEntry) error {
	ranges, lines, err := extractSnippetRanges(entry.AbsPath, entry.SnippetPattern)
	if err != nil {
		return err
	}
	for _, r := range ranges {
		if _, err := fmt.Fprintf(w, "<file path=\"%s\" snippet=\"%d-%d\">\n", entry.RelPath, r.Start, r.End); err != nil {
			return err
		}
		for i := r.Start - 1; i < r.End; i++ {
			if _, err := io.WriteString(w, lines[i]); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "</file>\n\n"); err != nil {
			return err
		}
	}
	return nil
}

func extractSnippetRanges(absPath, pattern string) ([]snippetRange, []string, error) {
	re, err := compileContainsPattern(pattern)
	if err != nil {
		return nil, nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, err
	}
	lines := splitLogicalLines(data)
	if len(lines) == 0 {
		return nil, lines, nil
	}

	matchedLines := make([]int, 0)
	for i, line := range lines {
		if re.MatchString(line) {
			matchedLines = append(matchedLines, i+1)
		}
	}
	if len(matchedLines) == 0 {
		return nil, lines, nil
	}

	// Expand each hit to the surrounding blank-line-delimited block. Hits inside
	// the same block collapse to one snippet range, matching the shell emitter.
	ranges := make([]snippetRange, 0, len(matchedLines))
	seen := make(map[string]struct{}, len(matchedLines))
	total := len(lines)
	for _, matchLine := range matchedLines {
		start := matchLine
		for start > 1 && lines[start-2] != "" {
			start--
		}

		end := matchLine
		for end < total && lines[end] != "" {
			end++
		}

		key := fmt.Sprintf("%d:%d", start, end)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		ranges = append(ranges, snippetRange{Start: start, End: end})
	}
	return ranges, lines, nil
}

func emitDiffEntry(w io.Writer, gitCtx gitContext, entry fileEntry) error {
	if !gitCtx.Enabled {
		data, err := os.ReadFile(entry.AbsPath)
		if err != nil {
			return err
		}
		return emitWrappedFile(w, entry.RelPath, "untracked", data)
	}

	repoPath := gitCtx.toRepoPath(entry.RelPath)
	trackedOutput, err := runGitCapture(gitCtx.Root, "ls-files", "--", repoPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(trackedOutput) == "" {
		// Untracked files have no unified diff. The shell falls back to full file
		// content tagged as type="untracked", and the rewrite preserves that UX.
		data, err := os.ReadFile(entry.AbsPath)
		if err != nil {
			return err
		}
		return emitWrappedFile(w, entry.RelPath, "untracked", data)
	}

	var diffOutput string
	var diffType string
	switch {
	case entry.DiffWantStaged && !entry.DiffWantUnstaged:
		diffOutput, err = runGitCapture(gitCtx.Root, "diff", "--cached", "--", repoPath)
		diffType = "staged-diff"
	case entry.DiffWantUnstaged && !entry.DiffWantStaged:
		diffOutput, err = runGitCapture(gitCtx.Root, "diff", "--", repoPath)
		diffType = "unstaged-diff"
	default:
		diffOutput, err = diffAgainstHeadOrIndex(gitCtx, repoPath)
		diffType = "diff"
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(diffOutput) == "" {
		return nil
	}
	return emitWrappedFile(w, entry.RelPath, diffType, []byte(diffOutput))
}

func withPayloadWriter(cfg runConfig, stdout io.Writer, colors colorPalette, fn func(io.Writer) error) error {
	if cfg.OutputMode == outputModeStdout {
		return fn(stdout)
	}

	// Clipboard mode streams directly into the platform tool. That keeps the Go
	// path closer to the shell's "generate once, send to sink" behavior and
	// avoids buffering a second full copy of the payload in memory.
	cmd, err := clipboardCommand(cfg.Platform, colors)
	if err != nil {
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	writeErr := fn(stdin)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("Error: clipboard command failed: %s", msg)
		}
		return fmt.Errorf("Error: clipboard command failed: %w", waitErr)
	}
	return nil
}

func clipboardCommand(platform string, colors colorPalette) (*exec.Cmd, error) {
	switch platform {
	case "macos":
		if _, err := exec.LookPath("pbcopy"); err != nil {
			return nil, fmt.Errorf("Error: No clipboard tool found.\n%s", clipboardInstallHint(platform, colors))
		}
		return exec.Command("pbcopy"), nil
	case "wsl":
		if path, err := exec.LookPath("clip.exe"); err == nil {
			return exec.Command(path), nil
		}
		if path, err := exec.LookPath("clip"); err == nil {
			return exec.Command(path), nil
		}
		return nil, fmt.Errorf("Error: No clipboard tool found.\n%s", clipboardInstallHint(platform, colors))
	default:
		if isWaylandSession() {
			if _, err := exec.LookPath("wl-copy"); err == nil {
				return exec.Command("wl-copy"), nil
			}
		}
		if _, err := exec.LookPath("xclip"); err == nil {
			return exec.Command("xclip", "-selection", "clipboard"), nil
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			return exec.Command("xsel", "--clipboard", "--input"), nil
		}
		return nil, fmt.Errorf("Error: No clipboard tool found.\n%s", clipboardInstallHint(platform, colors))
	}
}

func isWaylandSession() bool {
	return strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") || os.Getenv("WAYLAND_DISPLAY") != ""
}

func clipboardInstallHint(platform string, colors colorPalette) string {
	switch platform {
	case "macos":
		return fmt.Sprintf("  %sEnsure pbcopy is in PATH (ships with macOS).%s", colors.Dim, colors.Reset)
	case "wsl":
		return fmt.Sprintf("  %sEnsure clip.exe is available (ships with WSL).%s", colors.Dim, colors.Reset)
	default:
		if isWaylandSession() {
			return fmt.Sprintf("  Wayland detected. Install wl-clipboard:\n    sudo apt install wl-clipboard    %s# Debian/Ubuntu%s\n    sudo pacman -S wl-clipboard      %s# Arch%s",
				colors.Dim, colors.Reset, colors.Dim, colors.Reset)
		}
		return fmt.Sprintf("  X11 detected. Install xclip or xsel:\n    sudo apt install xclip xsel      %s# Debian/Ubuntu%s\n    sudo pacman -S xclip xsel        %s# Arch%s",
			colors.Dim, colors.Reset, colors.Dim, colors.Reset)
	}
}

// =============================================================================
// Help text
// =============================================================================

type helpRow struct {
	Left  string
	Right string
}

func writeAlignedHelpRows(b *strings.Builder, indent string, style func(string) string, rows []helpRow) {
	// Help intentionally keeps shell wording/colors but not shell spacing.
	// This helper standardizes the right column so the text stays readable while
	// preserving the user's requested "spacing can differ" carve-out.
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
		{Left: "catclip", Right: "Copy all files in current directory"},
		{Left: "catclip src lib", Right: "Copy specific directories"},
		{Left: `catclip src --only "*.ts"`, Right: "Only TypeScript files in src/"},
		{Left: `catclip src --exclude "*.test.*"`, Right: "Skip test files"},
		{Left: `catclip src --include "tests/,*.test.ts"`, Right: "Remove rules, include tests"},
		{Left: `catclip . --include "*"`, Right: "Disable all ignore rules"},
		{Left: "catclip src --changed", Right: "Only git-modified files in src/"},
		{Left: "catclip src --contains TODO", Right: "Only files whose contents match regex"},
	})

	fmt.Fprintf(&b, "\n%s\n", bold("Options:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "-h, --help", Right: "Show this help"},
		{Left: "--help-all", Right: "Show full manual (scopes, patterns, all options)"},
		{Left: "--version", Right: "Show version"},
		{Left: "-v, --verbose", Right: "Show phase timings and debug info"},
		{Left: "-q, --quiet", Right: "Suppress non-error stderr output"},
		{Left: "-p, --print", Right: "Output payload to stdout instead of clipboard"},
		{Left: "-y, --yes", Right: "Skip confirmation for large copies"},
		{Left: "-t, --no-tree", Right: "Skip tree preview"},
		{Left: "--hiss", Right: "Open ignore config in editor"},
		{Left: "--hiss-reset", Right: "Restore ignore config to defaults"},
		{Left: "--preview", Right: "Show file tree and token count, skip copying"},
	})

	fmt.Fprintf(&b, "\n%s %s for headless stdout output (no prompts, no stderr hints, no clipboard writes)\n", dim("Machine mode:"), cmd("-q -p"))

	fmt.Fprintf(&b, "\n%s (apply to the current scope)\n", bold("Scope Modifiers:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "--include RULES", Right: `Override ignore rules (.hiss + .gitignore) this run (comma-separated; "*" = all)`},
		{Left: "--exclude GLOBS", Right: "Add skip rules this run (comma-separated; trailing / = directory)"},
		{Left: "--only GLOB", Right: "Include only files matching shell glob (OR across repeats)"},
		{Left: "--changed", Right: "Only git-modified files"},
		{Left: "--staged", Right: "Only staged files (git index)"},
		{Left: "--unstaged", Right: "Only unstaged tracked modifications"},
		{Left: "--untracked", Right: "Only new untracked files"},
		{Left: "--diff", Right: "With --changed/--staged/--unstaged: emit unified diff instead of full file"},
		{Left: "--contains PATTERN", Right: "Only files whose contents match regex pattern"},
		{Left: "--snippet", Right: "With --contains: emit only the matched block (blank-line bounded)"},
		{Left: "--then", Right: "Start a new scope (separate targets with different modifiers)"},
	})

	fmt.Fprintf(&b, "\n%s\n", dim("Patterns use shell glob syntax (*, ?, [...]), not regex."))
	fmt.Fprintf(&b, "%s\n", dim("Exception: --contains uses regex (grep -E extended regex)."))
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
	fmt.Fprintf(&b, "  %s\n", dim("  Scope 1: src/ — TypeScript files, skipping tests"))
	fmt.Fprintf(&b, "  %s\n\n", dim("  Scope 2: features/ — TSX files only"))
	fmt.Fprintf(&b, "  Without %s, all targets share the same modifiers:\n", cmd("--then"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip src lib", Right: dim("Both use default rules")},
		{Left: `catclip src lib --only "*.ts"`, Right: dim("Both filtered to .ts files")},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("Target Resolution:"))
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "catclip auth", Right: "Fuzzy directory search"},
		{Left: "catclip shared/components/Badge.tsx", Right: "Chained path resolution"},
		{Left: "catclip src/Button.tsx", Right: "Direct file path"},
	})
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s\n", bold("--include (override ignore rules):"))
	b.WriteString("  Overrides both .hiss and .gitignore for this run only.\n")
	b.WriteString("  Comma-separated patterns. Trailing / = directory rule.\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: `--include "tests/"`, Right: "Include tests/ even if gitignored"},
		{Left: `--include "tests/,*.test.ts"`, Right: "Include tests/ AND test files"},
		{Left: `--include "*"`, Right: "Remove ALL rules (full scan, slower)"},
		{Left: `catclip . --include "*" --only "*.log"`, Right: "All rules off, only .log files"},
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
	fmt.Fprintf(&b, "  Git projects also respect %s via %s.\n\n", cmd(".gitignore"), dim("git check-ignore"))

	fmt.Fprintf(&b, "%s\n", bold("Example .hiss:"))
	fmt.Fprintf(&b, "  %s\n", dim("# Ignore build output"))
	fmt.Fprintf(&b, "  %s\n", dim("dist/"))
	fmt.Fprintf(&b, "  %s\n", dim("*.min.js"))
	fmt.Fprintf(&b, "  %s\n", dim("# Ignore specific file"))
	fmt.Fprintf(&b, "  %s\n\n", dim("test/fixtures.json"))

	fmt.Fprintf(&b, "%s\n", bold("--contains (content search):"))
	b.WriteString("  Filters to files whose contents match a regex pattern (grep -E).\n")
	writeAlignedHelpRows(&b, "  ", cmd, []helpRow{
		{Left: "--contains TODO", Right: `Files containing "TODO"`},
		{Left: `--contains "useState|useEffect"`, Right: "Files matching either hook"},
		{Left: `--contains '\$store'`, Right: "Escaped special characters"},
	})
	fmt.Fprintf(&b, "  %s\n\n", dim("Plain text works for most searches. Use single quotes for special chars."))
	fmt.Fprintf(&b, "  %s\n", bold("Pattern types:"))
	writeAlignedHelpRows(&b, "    ", dim, []helpRow{
		{Left: "--only, --exclude, --include", Right: "→ shell globs (*, ?, [...]) match filenames"},
		{Left: "--contains", Right: "→ regex (grep -E) matches file contents"},
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
		"Load .hiss into memory",
		"Apply --include (remove rules from blocklist)",
		"Apply --exclude (add rules to blocklist)",
		"Discover files (find/git ls-files with modified blocklist)",
		"Apply --only filter",
		"Apply --contains filter (grep content match)",
		"Binary exclusion (always, cannot be overridden)",
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

// =============================================================================
// Platform and version helpers
// =============================================================================

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
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
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
