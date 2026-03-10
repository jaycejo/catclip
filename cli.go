package catclip

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type verboseOutputMetrics struct {
	PayloadBytes           int64
	CleanTrackedCount      int
	ModifiedUntrackedCount int
	GitStateKnown          bool
	GitStateNote           string
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
		discoverySpinnerStop := func() {}
		if !cfg.Quiet {
			discoverySpinnerStop = startLoadingSpinner(spinnerOutputFile(stderr), "Scanning files...")
		}
		var allEntries []fileEntry
		diagnostics := make([]diagnostic, 0, len(cfg.Warnings))
		for _, warning := range cfg.Warnings {
			diagnostics = append(diagnostics, diagnostic{message: warning})
		}
		var notices []string
		hadSelectionCancel := false
		for i, s := range cfg.Scopes {
			scopeStarted := time.Now()
			entries, scopeDiagnostics, scopeNotices, scopeSelectionCancel, err := evaluateScope(cfg, gitCtx, i, s, baseRules, stderr, colors)
			if err != nil {
				return err
			}
			if cfg.Verbose {
				fmt.Fprintf(stderr, "[verbose] scope %d: discovered %d file(s) in %s\n", i+1, len(entries), formatDuration(time.Since(scopeStarted)))
			}
			allEntries = append(allEntries, entries...)
			diagnostics = append(diagnostics, scopeDiagnostics...)
			notices = append(notices, scopeNotices...)
			hadSelectionCancel = hadSelectionCancel || scopeSelectionCancel
		}
		discoverySpinnerStop()
		allEntries = dedupeEntriesByPath(allEntries)
		for _, diag := range diagnostics {
			if diag.isError || !cfg.Quiet {
				fmt.Fprintln(stderr, diag.message)
			}
		}
		if len(allEntries) == 0 {
			if !cfg.Quiet {
				for _, notice := range dedupePreserveOrder(notices) {
					fmt.Fprintln(stderr, notice)
				}
			}
			if err := writeNoFilesMatchedMessage(cfg, stderr, colors, hadSelectionCancel); err != nil {
				return err
			}
			return newExitError(1, "")
		}
		reportStarted := time.Now()
		report, err := buildOutputReport(cfg, gitCtx, allEntries, dedupePreserveOrder(notices))
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
		outputMetrics, err := collectVerboseOutputMetrics(cfg.Verbose, gitCtx, allEntries)
		if err != nil {
			return err
		}
		outputSpinnerStop := func() {}
		if !cfg.Quiet {
			outputSpinnerStop = startLoadingSpinner(spinnerOutputFile(stderr), outputSpinnerMessage(cfg))
		}
		outputStarted := time.Now()
		emitStats, err := emitFullOutput(cfg, gitCtx, allEntries, stdout, colors)
		if err != nil {
			outputSpinnerStop()
			return err
		}
		outputSpinnerStop()
		outputMetrics.PayloadBytes = emitStats.PayloadBytes
		if cfg.Verbose {
			outputDuration := time.Since(outputStarted)
			fmt.Fprintf(stderr, "[verbose] output: %s\n", formatDuration(outputDuration))
			writeVerboseOutputMetrics(stderr, outputMetrics, emitStats, len(allEntries), outputDuration)
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

func collectVerboseOutputMetrics(verbose bool, gitCtx gitContext, entries []fileEntry) (verboseOutputMetrics, error) {
	if !verbose {
		return verboseOutputMetrics{}, nil
	}

	metrics := verboseOutputMetrics{}
	if !gitCtx.Enabled || len(entries) == 0 {
		return metrics, nil
	}

	trackedLines, err := runGitLines(gitCtx.Root, nil, "ls-files")
	if err != nil {
		return verboseOutputMetrics{}, err
	}
	if len(trackedLines) == 0 {
		metrics.GitStateNote = "unavailable (git ls-files returned no tracked files)"
		return metrics, nil
	}
	tracked := make(map[string]struct{}, len(trackedLines))
	for _, repoPath := range trackedLines {
		tracked[normalizeRelPath(repoPath)] = struct{}{}
	}

	changedLines, err := collectChangedRepoPaths(gitCtx, scope{})
	if err != nil {
		return verboseOutputMetrics{}, err
	}
	changed := make(map[string]struct{}, len(changedLines))
	for _, repoPath := range changedLines {
		changed[normalizeRelPath(repoPath)] = struct{}{}
	}

	for _, entry := range entries {
		repoPath := normalizeRelPath(gitCtx.toRepoPath(entry.RelPath))
		if _, ok := tracked[repoPath]; !ok {
			metrics.ModifiedUntrackedCount++
			continue
		}
		if _, ok := changed[repoPath]; ok {
			metrics.ModifiedUntrackedCount++
			continue
		}
		metrics.CleanTrackedCount++
	}
	metrics.GitStateKnown = true
	return metrics, nil
}

func writeVerboseOutputMetrics(w io.Writer, metrics verboseOutputMetrics, emitStats emitStats, fileCount int, outputDuration time.Duration) {
	payloadHuman := formatByteCount(metrics.PayloadBytes)
	avgBytes := int64(0)
	if fileCount > 0 {
		avgBytes = metrics.PayloadBytes / int64(fileCount)
	}
	throughputMiB := 0.0
	if outputDuration > 0 {
		throughputMiB = float64(metrics.PayloadBytes) / (1024 * 1024) / outputDuration.Seconds()
	}

	if emitStats.GenerateDuration > 0 {
		fmt.Fprintf(w, "[verbose] emit generate: %s\n", formatDuration(emitStats.GenerateDuration))
	}
	if emitStats.SinkFinalizeDuration > 0 {
		if emitStats.ClipboardWaitDuration > 0 {
			fmt.Fprintf(w, "[verbose] clipboard flush/close (%s): %s\n", emitStats.SinkName, formatDuration(emitStats.SinkFinalizeDuration))
		} else {
			fmt.Fprintf(w, "[verbose] emit flush (%s): %s\n", emitStats.SinkName, formatDuration(emitStats.SinkFinalizeDuration))
		}
	}
	if emitStats.ClipboardWaitDuration > 0 {
		fmt.Fprintf(w, "[verbose] clipboard wait (%s): %s\n", emitStats.SinkName, formatDuration(emitStats.ClipboardWaitDuration))
	}
	fmt.Fprintf(w, "[verbose] payload: %d bytes (%s), avg/file: %d bytes (%s)\n",
		metrics.PayloadBytes, payloadHuman, avgBytes, formatByteCount(avgBytes))
	if metrics.GitStateKnown {
		fmt.Fprintf(w, "[verbose] git file states: clean-tracked=%d modified/untracked=%d\n",
			metrics.CleanTrackedCount, metrics.ModifiedUntrackedCount)
	} else if metrics.GitStateNote != "" {
		fmt.Fprintf(w, "[verbose] git file states: %s\n", metrics.GitStateNote)
	}
	fmt.Fprintf(w, "[verbose] output throughput: %.2f MiB/s\n", throughputMiB)
}

func formatByteCount(totalBytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)

	switch {
	case totalBytes < kb:
		return fmt.Sprintf("%dB", totalBytes)
	case totalBytes < mb:
		return fmt.Sprintf("%.2fKB", float64(totalBytes)/kb)
	case totalBytes < gb:
		return fmt.Sprintf("%.2fMB", float64(totalBytes)/mb)
	default:
		return fmt.Sprintf("%.2fGB", float64(totalBytes)/gb)
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
	return cfg.Print && cfg.Quiet
}

func activeColorPalette() colorPalette {
	return activeColorPaletteForWriter(os.Stderr)
}

func activeColorPaletteForWriter(w io.Writer) colorPalette {
	file, ok := w.(*os.File)
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
		return 0x40487413, 0x80487414, true
	case "linux":
		return 0x5401, 0x5402, true
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
				return runConfig{}, newUsageError("Error: --include requires a target query.\n  Example: catclip --include node_modules\n  Example: catclip src --include .env")
			}
			i++
			current.Targets = append(current.Targets, args[i])
			current.IncludedTargets = append(current.IncludedTargets, args[i])
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
	return len(b.Targets) > 0 || len(b.Only) > 0 || len(b.Exclude) > 0 ||
		b.Contains != "" || b.Snippet || b.Changed || b.Staged || b.Unstaged ||
		b.Untracked || b.Diff || b.explicitChanged
}

func splitCommaPatterns(value string) []string {
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
		for _, pat := range s.Exclude {
			if !looksLikeDirectoryPattern(cfg.WorkingDir, pat, ignoredDirs) {
				continue
			}
			key := "--exclude:" + pat
			if _, ok := warned[key]; ok {
				continue
			}
			warned[key] = struct{}{}

			if _, err := fmt.Fprintf(stderr, "%sWarning:%s --exclude pattern %s looks like a directory name.\n", colors.Warn, colors.Reset, singleQuoted(pat)); err != nil {
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
	return strings.Contains(pattern, "*") &&
		!strings.Contains(pattern, ".") &&
		!strings.Contains(pattern, "[") &&
		!strings.Contains(pattern, "|") &&
		!strings.Contains(pattern, "\\")
}

func formatScopeSummary(s scope) string {
	parts := []string{
		fmt.Sprintf("targets=%q", s.Targets),
		fmt.Sprintf("included=%q", s.IncludedTargets),
		fmt.Sprintf("only=%q", s.Only),
		fmt.Sprintf("exclude=%q", s.Exclude),
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
