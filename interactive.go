package catclip

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// maybeRunInteractiveBuilder owns the pick-targets-then-type-modifiers flow
// used for TTY-driven sessions with positional targets only.
func maybeRunInteractiveBuilder(args []string, stdout, stderr io.Writer) (bool, error) {
	if !canPromptInteractively() {
		return false, nil
	}

	enabled, err := shouldUseInteractiveBuilder(args)
	if err != nil || !enabled {
		return false, err
	}

	cfg, resolver, err := newInteractiveResolver()
	if err != nil {
		return true, err
	}

	if bypass, err := interactiveCommandCanRunDirectly(resolver, args); err != nil {
		return true, err
	} else if bypass {
		return false, nil
	}

	parsed, err := parseInteractiveInputTokens(args)
	if err != nil {
		return true, err
	}
	if bypass, err := interactiveInputCanBypassBuilder(resolver, parsed); err != nil {
		return true, err
	} else if bypass {
		return false, nil
	}
	resolvedArgs, resolvedTargets, _, err := resolveInteractiveScopeInputs(resolver, parsed.targets, parsed.includeQueries, nil)
	if err != nil {
		if errors.Is(err, errSelectionCancelled) {
			return true, nil
		}
		return true, err
	}
	initialInteractiveArgs := append([]string(nil), resolvedArgs...)
	initialInteractiveArgs = append(initialInteractiveArgs, parsed.modifiers...)
	if parsed.hasThen {
		var nextScopeResolved []string
		if len(parsed.nextScopeTargets) > 0 {
			nextScopeResolved, _, _, err = resolveInteractiveScopeInputs(resolver, parsed.nextScopeTargets, nil, resolvedTargets)
			if err != nil {
				if !errors.Is(err, errSelectionCancelled) {
					return true, err
				}
			}
		} else {
			selected, err := resolver.chooseRootTargetMatches("", "scope> ", false, resolvedTargets)
			if err != nil && !errors.Is(err, errSelectionCancelled) {
				return true, err
			}
			if err == nil && len(selected) > 0 {
				nextScopeResolved = targetMatchArgs(selected)
			}
		}
		if len(nextScopeResolved) > 0 {
			initialInteractiveArgs = append(initialInteractiveArgs, "--then")
			initialInteractiveArgs = append(initialInteractiveArgs, nextScopeResolved...)
		}
	}

	finalArgs, err := promptInteractiveCommandArgs(resolver, initialInteractiveArgs, stderr)
	if err != nil {
		if errors.Is(err, errSelectionCancelled) {
			return true, nil
		}
		return true, err
	}

	cfg, err = parseArgs(finalArgs)
	if err != nil {
		return true, err
	}
	return true, run(cfg, stdout, stderr)
}

func newInteractiveResolver() (runConfig, *scopeResolver, error) {
	cfg, err := parseArgs([]string{"."})
	if err != nil {
		return runConfig{}, nil, err
	}
	gitCtx := detectGitContext(cfg.WorkingDir)
	baseRules, err := loadIgnoreRules()
	if err != nil {
		return runConfig{}, nil, err
	}
	matcher, err := buildScopeMatcher(baseRules, scope{})
	if err != nil {
		return runConfig{}, nil, err
	}
	resolver := &scopeResolver{
		cfg:               cfg,
		gitCtx:            gitCtx,
		matcher:           matcher,
		allowFileSymlinks: false,
		useGitIgnore:      gitCtx.Enabled,
	}
	return cfg, resolver, nil
}

func interactiveInputCanBypassBuilder(resolver *scopeResolver, parsed interactiveInputParse) (bool, error) {
	if parsed.hasThen && len(parsed.modifiers) == 0 && len(parsed.includeQueries) == 0 &&
		len(parsed.nextScopeTargets) == 0 && len(parsed.targets) == 1 &&
		normalizeRelPath(parsed.targets[0]) == "." {
		return true, nil
	}
	if len(parsed.includeQueries) > 0 || len(parsed.modifiers) > 0 || parsed.hasThen {
		return false, nil
	}
	if len(parsed.targets) == 0 {
		return false, nil
	}

	for _, target := range parsed.targets {
		canResolve, err := resolver.canResolveTargetWithoutPrompt(target)
		if err != nil {
			return false, err
		}
		if !canResolve {
			return false, nil
		}
	}

	return true, nil
}

func interactiveCommandCanRunDirectly(resolver *scopeResolver, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	cfg, err := parseArgs(args)
	if err != nil {
		return false, nil
	}
	for _, scope := range cfg.Scopes {
		for _, target := range scope.Targets {
			canResolve, err := resolver.canResolveTargetWithoutPrompt(target)
			if err != nil {
				return false, err
			}
			if !canResolve {
				return false, nil
			}
		}
	}
	return true, nil
}

func shouldUseInteractiveBuilder(args []string) (bool, error) {
	seenScopeStart := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--include" {
			seenScopeStart = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if arg == "--then" {
			if !seenScopeStart {
				return false, newUsageError("Error: --then cannot start a command.\n  Add a first scope before --then.\n  Example: catclip src --then tests")
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return false, nil
		}
		if filepath.IsAbs(arg) {
			return false, newUsageError("Error: Absolute paths not allowed: %s\n  Use a relative path from your project root instead.", singleQuoted(arg))
		}
		if containsParentTraversal(arg) {
			return false, newUsageError("Error: Cannot traverse above working directory: %s\n  catclip only operates within the current directory tree.\n  Use a relative path from your project root instead.\n  Example: catclip config/", singleQuoted(arg))
		}
		seenScopeStart = true
	}
	return true, nil
}

func resolveInteractiveScopeInputs(resolver *scopeResolver, targetTokens, includeQueries, alreadySelected []string) ([]string, []string, bool, error) {
	selectedPaths := append([]string(nil), alreadySelected...)
	resolvedArgs := make([]string, 0, len(targetTokens)+len(includeQueries)*2)
	resolvedTargets := make([]string, 0, len(targetTokens)+len(includeQueries))
	usedBuilder := false

	if len(targetTokens) == 0 && len(includeQueries) == 0 {
		resolved, used, err := resolveInteractiveInitialTargets(resolver, nil, selectedPaths)
		if err != nil {
			return nil, nil, true, err
		}
		resolvedArgs = append(resolvedArgs, resolved...)
		resolvedTargets = append(resolvedTargets, interactiveResolvedTargetPaths(resolved)...)
		return resolvedArgs, resolvedTargets, used, nil
	}

	for _, token := range targetTokens {
		resolved, used, err := resolveInteractiveInitialTargets(resolver, []string{token}, selectedPaths)
		if err != nil {
			return nil, nil, true, err
		}
		resolvedArgs = append(resolvedArgs, resolved...)
		resolvedPaths := interactiveResolvedTargetPaths(resolved)
		resolvedTargets = append(resolvedTargets, resolvedPaths...)
		selectedPaths = append(selectedPaths, resolvedPaths...)
		usedBuilder = usedBuilder || used
	}

	for _, query := range includeQueries {
		resolved, err := resolver.resolveInteractiveIncludeTargets(query, selectedPaths)
		if err != nil {
			return nil, nil, true, err
		}
		for _, target := range resolved {
			resolvedArgs = append(resolvedArgs, "--include", target)
			resolvedTargets = append(resolvedTargets, target)
			selectedPaths = append(selectedPaths, target)
		}
		usedBuilder = true
	}

	return resolvedArgs, resolvedTargets, usedBuilder, nil
}

// resolveInteractiveInitialTargets turns the user's initial interactive tokens
// into concrete repo-relative paths by sending them through the root picker.
func resolveInteractiveInitialTargets(resolver *scopeResolver, args []string, alreadySelected []string) ([]string, bool, error) {
	if selectionContainsAll(alreadySelected) {
		return nil, true, nil
	}
	if len(args) == 0 {
		selected, err := resolver.chooseRootTargetMatches("", "pick> ", true, alreadySelected)
		if err != nil {
			return nil, true, err
		}
		if len(selected) == 0 {
			return nil, true, errSelectionCancelled
		}
		return targetMatchArgs(selected), true, nil
	}
	if len(args) == 1 && normalizeRelPath(args[0]) == "." {
		return []string{"."}, false, nil
	}

	resolved := make([]string, 0, len(args))
	usedBuilder := false
	for _, arg := range args {
		query := interactiveTargetQuery(arg)
		hasSlash := strings.Contains(query, "/")
		normalized := normalizeRelPath(arg)
		if normalized == "" {
			normalized = "."
		}
		selectedPaths := append(append([]string(nil), alreadySelected...), resolved...)

		if normalized == "." {
			resolved = append(resolved, normalized)
			continue
		}
		exists, err := resolver.targetPathExists(normalized)
		if err != nil {
			return nil, usedBuilder, err
		}
		if exists && !coveredBySelection(normalized, selectedPaths) && (normalized == "." || hasSlash) {
			resolved = append(resolved, normalized)
			continue
		}
		if hasSlash {
			selected, err := resolver.chooseRootTargetMatches(query, "pick> ", false, selectedPaths)
			if err != nil {
				return nil, true, err
			}
			resolved = append(resolved, targetMatchArgs(selected)...)
			usedBuilder = true
			continue
		}

		selected, err := resolver.chooseRootTargetMatches(normalized, "pick> ", false, selectedPaths)
		if err != nil {
			return nil, true, err
		}
		if len(selected) == 0 {
			return nil, true, errSelectionCancelled
		}
		resolved = append(resolved, targetMatchArgs(selected)...)
		usedBuilder = true
	}
	return resolved, usedBuilder, nil
}

// promptInteractiveCommandArgs keeps extending the in-progress command with
// more targets, modifiers, and optional --then scopes until execution is ready.
func promptInteractiveCommandArgs(resolver *scopeResolver, initialArgs []string, stderr io.Writer) ([]string, error) {
	args := append([]string(nil), initialArgs...)
	showGuide := true
	previousPromptLines := 0
	ignoreImmediateEmpty := true
	pendingError := ""
	for {
		colors := activeColorPaletteForWriter(stderr)
		display := formatInteractivePromptState(args, colors)
		suffix, ok, renderedLines, err := readInteractiveModifierLine(display, pendingError, stderr, showGuide, previousPromptLines, ignoreImmediateEmpty)
		if err != nil {
			return nil, err
		}
		previousPromptLines = renderedLines
		if !ok {
			return nil, errSelectionCancelled
		}
		showGuide = false
		ignoreImmediateEmpty = false

		tokens, err := splitInteractiveTokens(suffix)
		if err != nil {
			pendingError = err.Error()
			continue
		}
		parsed, err := parseInteractiveInputTokens(tokens)
		if err != nil {
			pendingError = err.Error()
			continue
		}
		pendingError = ""
		candidateArgs := append([]string(nil), args...)
		changedTargets := false

		if len(parsed.targets) > 0 || len(parsed.includeQueries) > 0 {
			selectedTargets := allInteractiveTargetPaths(candidateArgs)
			resolvedArgs, _, _, err := resolveInteractiveScopeInputs(resolver, parsed.targets, parsed.includeQueries, selectedTargets)
			if err != nil {
				if errors.Is(err, errSelectionCancelled) {
					continue
				}
				return nil, err
			}
			candidateArgs = append(candidateArgs, resolvedArgs...)
			changedTargets = true
		}
		if len(parsed.modifiers) == 0 && !parsed.hasThen {
			if len(parsed.targets) == 0 && len(parsed.includeQueries) == 0 {
				if _, err := parseArgs(candidateArgs); err != nil {
					pendingError = err.Error()
					continue
				}
				if err := renderInteractiveExecutionState(stderr, candidateArgs, previousPromptLines); err != nil {
					return nil, err
				}
				return candidateArgs, nil
			}
			if _, err := parseArgs(candidateArgs); err != nil {
				pendingError = err.Error()
				continue
			}
			args = candidateArgs
			if changedTargets {
				ignoreImmediateEmpty = true
			}
			continue
		}
		candidateArgs = append(candidateArgs, parsed.modifiers...)
		if !parsed.hasThen {
			if _, err := parseArgs(candidateArgs); err != nil {
				pendingError = err.Error()
				continue
			}
			if err := renderInteractiveExecutionState(stderr, candidateArgs, previousPromptLines); err != nil {
				return nil, err
			}
			return candidateArgs, nil
		}

		var nextScopeResolved []string
		if len(parsed.nextScopeTargets) > 0 {
			selectedTargets := allInteractiveTargetPaths(candidateArgs)
			resolvedTargets, _, _, err := resolveInteractiveScopeInputs(resolver, parsed.nextScopeTargets, nil, selectedTargets)
			if err != nil {
				if errors.Is(err, errSelectionCancelled) {
					continue
				}
				return nil, err
			}
			nextScopeResolved = resolvedTargets
		} else {
			if selectionContainsAll(allInteractiveTargetPaths(candidateArgs)) {
				pendingError = "Note: '.' already covers all safe targets.\n  Add exact targets after --then or use --include to browse ignored targets."
				continue
			}
			selectedTargets := allInteractiveTargetPaths(candidateArgs)
			selected, err := resolver.chooseRootTargetMatches("", "scope> ", false, selectedTargets)
			if err != nil {
				if errors.Is(err, errSelectionCancelled) {
					continue
				}
				return nil, err
			}
			if len(selected) == 0 {
				continue
			}
			nextScopeResolved = targetMatchArgs(selected)
		}
		candidateArgs = append(candidateArgs, "--then")
		candidateArgs = append(candidateArgs, nextScopeResolved...)
		if _, err := parseArgs(candidateArgs); err != nil {
			pendingError = err.Error()
			continue
		}
		args = candidateArgs
		ignoreImmediateEmpty = true
	}
}

func readInteractiveModifierLine(display, errorMessage string, stderr io.Writer, showGuide bool, previousPromptLines int, ignoreImmediateEmpty bool) (string, bool, int, error) {
	input := os.Stdin
	output := stderr
	if !isTerminalFile(os.Stdin) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return "", false, 0, err
		}
		defer tty.Close()
		input = tty
		output = tty
	}

	if previousPromptLines > 0 {
		if err := clearInteractivePromptBlock(output, previousPromptLines); err != nil {
			return "", false, 0, err
		}
	}
	colors := activeColorPaletteForWriter(output)
	panel := interactivePromptPanel(display, errorMessage, colors, showGuide)
	if _, err := fmt.Fprintf(output, "%s\n", panel); err != nil {
		return "", false, 0, err
	}
	width := terminalColumns(outputFileForPrompt(output, input))
	panelLines := countDisplayLines(panel, width)
	if isTerminalFile(input) {
		return readInteractiveEditorLine(input, output, colors, ignoreImmediateEmpty, panelLines, width)
	}
	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprint(output, interactiveEditorPrompt(colors)); err != nil {
			return "", false, 0, err
		}
		promptShownAt := time.Now()
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, 0, err
		}
		if errors.Is(err, io.EOF) && line == "" {
			if _, writeErr := fmt.Fprint(output, "\n"); writeErr != nil {
				return "", false, 0, writeErr
			}
			return "", false, panelLines, nil
		}
		line = strings.TrimRight(line, "\r\n")
		if shouldIgnoreImmediateEmptyInteractiveLine(line, promptShownAt, time.Now(), ignoreImmediateEmpty) {
			ignoreImmediateEmpty = false
			continue
		}
		return line, true, panelLines + countDisplayLines(interactiveEditorPrompt(colors)+line, width), nil
	}
}

func readInteractiveEditorLine(input *os.File, output io.Writer, colors colorPalette, ignoreImmediateEmpty bool, panelLines, width int) (string, bool, int, error) {
	state, err := getTerminalState(input)
	if err != nil {
		return "", false, 0, err
	}
	raw := *state
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := setTerminalState(input, &raw); err != nil {
		return "", false, 0, err
	}
	defer func() {
		_ = setTerminalState(input, state)
	}()

	line := ""
	promptShownAt := time.Now()
	if err := redrawInteractiveEditorLine(output, colors, line); err != nil {
		return "", false, 0, err
	}

	var buf [1]byte
	for {
		n, readErr := input.Read(buf[:])
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", false, 0, readErr
		}
		if errors.Is(readErr, io.EOF) || n == 0 {
			if _, err := fmt.Fprint(output, "\n"); err != nil {
				return "", false, 0, err
			}
			return "", false, panelLines, nil
		}

		switch buf[0] {
		case '\r', '\n':
			if shouldIgnoreImmediateEmptyInteractiveLine(line, promptShownAt, time.Now(), ignoreImmediateEmpty) {
				ignoreImmediateEmpty = false
				promptShownAt = time.Now()
				if err := redrawInteractiveEditorLine(output, colors, line); err != nil {
					return "", false, 0, err
				}
				continue
			}
			if _, err := fmt.Fprint(output, "\n"); err != nil {
				return "", false, 0, err
			}
			return line, true, panelLines + countDisplayLines(interactiveEditorPrompt(colors)+line, width), nil
		case 3:
			if _, err := fmt.Fprint(output, "\n"); err != nil {
				return "", false, 0, err
			}
			return "", false, panelLines, errSelectionCancelled
		case 4:
			if line == "" {
				if _, err := fmt.Fprint(output, "\n"); err != nil {
					return "", false, 0, err
				}
				return "", false, panelLines, nil
			}
		case 9:
			completed, changed := applyInteractiveModifierCompletion(line)
			if changed {
				line = completed
			}
		case 127, 8:
			if len(line) > 0 {
				_, size := lastRune(line)
				line = line[:len(line)-size]
			}
		case 27:
			if err := discardEscapeSequence(input); err != nil {
				return "", false, 0, err
			}
		default:
			if buf[0] >= 32 {
				line += string(buf[0])
			}
		}
		if err := redrawInteractiveEditorLine(output, colors, line); err != nil {
			return "", false, 0, err
		}
	}
}

func clearInteractivePromptBlock(output io.Writer, lines int) error {
	for i := 0; i < lines; i++ {
		if _, err := fmt.Fprint(output, "\033[1A\r\033[K"); err != nil {
			return err
		}
	}
	return nil
}

func countDisplayLines(text string, width int) int {
	if text == "" {
		return 0
	}
	if width <= 0 {
		width = 80
	}
	total := 0
	for _, line := range strings.Split(text, "\n") {
		runes := utf8.RuneCountInString(stripANSIEscapeCodes(line))
		if runes == 0 {
			total++
			continue
		}
		total += ((runes - 1) / width) + 1
	}
	return total
}

func renderInteractiveExecutionState(output io.Writer, args []string, previousPromptLines int) error {
	colors := activeColorPaletteForWriter(output)
	if previousPromptLines > 0 {
		if err := clearInteractivePromptBlock(output, previousPromptLines); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(output, "%s\n", formatInteractivePromptState(args, colors))
	return err
}

func formatInteractivePromptState(args []string, colors colorPalette) string {
	scopes := interactiveDisplayScopes(args)
	lines := []string{styledLabel(colors, "Targets:")}
	for i, scope := range scopes {
		lines = append(lines, fmt.Sprintf("  %sscope %d:%s %s", colors.Label, i+1, colors.Reset, formatInteractiveDisplayTargets(scope.targets, ".", colors)))
	}
	lines = append(lines, styledLabel(colors, "Modifiers:"))
	for i, scope := range scopes {
		lines = append(lines, fmt.Sprintf("  %sscope %d:%s %s", colors.Label, i+1, colors.Reset, formatInteractiveDisplayTokens(scope.modifiers, "none", colors.Warn, colors)))
	}
	lines = append(lines, "", styledLabel(colors, "Command:"), "  "+colors.Value+formatInteractiveCommand(args)+colors.Reset)
	return strings.Join(lines, "\n")
}

func styledLabel(colors colorPalette, text string) string {
	return colors.Bold + colors.Prompt + text + colors.Reset
}

func interactivePromptPanel(display, errorMessage string, colors colorPalette, showGuide bool) string {
	parts := make([]string, 0, 3)
	if showGuide {
		parts = append(parts, interactiveModifierGuideText(colors))
	}
	if errorMessage != "" {
		parts = append(parts, colors.Err+errorMessage+colors.Reset, "\n\n")
	}
	parts = append(parts, display)
	return strings.Join(parts, "")
}

func interactiveEditorPrompt(colors colorPalette) string {
	return colors.Bold + colors.Prompt + "continue> " + colors.Reset
}

func redrawInteractiveEditorLine(output io.Writer, colors colorPalette, line string) error {
	if _, err := fmt.Fprint(output, "\r\033[K"); err != nil {
		return err
	}
	if _, err := fmt.Fprint(output, interactiveEditorPrompt(colors)); err != nil {
		return err
	}
	if _, err := fmt.Fprint(output, line); err != nil {
		return err
	}
	return nil
}

func discardEscapeSequence(input *os.File) error {
	state, err := getTerminalState(input)
	if err != nil {
		return err
	}
	raw := *state
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1
	if err := setTerminalState(input, &raw); err != nil {
		return err
	}
	defer func() {
		_ = setTerminalState(input, state)
	}()

	var buf [2]byte
	_, err = input.Read(buf[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func formatInteractiveCommand(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "catclip")
	for _, arg := range args {
		parts = append(parts, shellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

type interactiveDisplayScope struct {
	targets   []interactiveDisplayTarget
	modifiers []string
}

type interactiveDisplayTarget struct {
	value    string
	included bool
}

func interactiveDisplayScopes(args []string) []interactiveDisplayScope {
	scopes := []interactiveDisplayScope{{}}
	current := &scopes[0]

	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--then" {
			scopes = append(scopes, interactiveDisplayScope{})
			current = &scopes[len(scopes)-1]
			continue
		}
		if token == "--include" && i+1 < len(args) {
			i++
			current.targets = append(current.targets, interactiveDisplayTarget{value: args[i], included: true})
			continue
		}

		if len(current.modifiers) == 0 && !strings.HasPrefix(token, "-") {
			current.targets = append(current.targets, interactiveDisplayTarget{value: token})
			continue
		}

		current.modifiers = append(current.modifiers, token)
		if interactiveModifierConsumesValue(token) && i+1 < len(args) {
			i++
			current.modifiers = append(current.modifiers, args[i])
		}
	}

	return scopes
}

func interactiveModifierConsumesValue(token string) bool {
	switch token {
	case "--only", "--exclude", "--contains":
		return true
	default:
		return false
	}
}

func formatInteractiveDisplayTargets(targets []interactiveDisplayTarget, empty string, colors colorPalette) string {
	if len(targets) == 0 {
		return colors.Dim + empty + colors.Reset
	}
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		color := interactiveTargetDisplayColor(target.value, target.included, colors)
		parts = append(parts, color+shellQuoteArg(target.value)+colors.Reset)
	}
	return strings.Join(parts, " ")
}

func formatInteractiveDisplayTokens(tokens []string, empty, color string, colors colorPalette) string {
	if len(tokens) == 0 {
		return colors.Dim + empty + colors.Reset
	}
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		parts = append(parts, color+shellQuoteArg(token)+colors.Reset)
	}
	return strings.Join(parts, " ")
}

func interactiveTargetDisplayColor(target string, included bool, colors colorPalette) string {
	if included {
		return colors.Err
	}
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return colors.Dir
	}
	if prefersDirectFileLookup(target) {
		return colors.OK
	}
	return colors.Dir
}

func interactiveModifierGuideText(colors colorPalette) string {
	return strings.Join([]string{
		styledLabel(colors, "Target Actions:"),
		fmt.Sprintf("  %s--include%s  %sbrowse ignored files and directories for this scope%s", colors.Err, colors.Reset, colors.Dim, colors.Reset),
		"",
		styledLabel(colors, "Modifiers:"),
		fmt.Sprintf("  %s--only%s     %snarrow by pattern%s", colors.Warn, colors.Reset, colors.Dim, colors.Reset),
		fmt.Sprintf("  %s--exclude%s  %sskip by pattern%s", colors.Warn, colors.Reset, colors.Dim, colors.Reset),
		fmt.Sprintf("  %s--contains%s %sfilter by content%s", colors.Warn, colors.Reset, colors.Dim, colors.Reset),
		fmt.Sprintf("  %s--changed%s  %sonly git-modified files%s", colors.Warn, colors.Reset, colors.Dim, colors.Reset),
		fmt.Sprintf("  %s--then%s     %snew scope%s", colors.Warn, colors.Reset, colors.Dim, colors.Reset),
		"",
		colors.Dim + "Type more targets, --include queries, modifiers, or --then below." + colors.Reset,
		"",
	}, "\n")
}

func outputFileForPrompt(output io.Writer, fallback *os.File) *os.File {
	if file, ok := output.(*os.File); ok {
		return file
	}
	return fallback
}

type terminalWindowSize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func terminalColumns(file *os.File) int {
	if file != nil {
		size := terminalWindowSize{}
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
		if errno == 0 && size.Col > 0 {
			return int(size.Col)
		}
	}
	if cols, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && cols > 0 {
		return cols
	}
	return 80
}

func stripANSIEscapeCodes(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			out.WriteByte(text[i])
			continue
		}
		if i+1 >= len(text) || text[i+1] != '[' {
			continue
		}
		i += 2
		for ; i < len(text); i++ {
			if text[i] >= '@' && text[i] <= '~' {
				break
			}
		}
	}
	return out.String()
}

var interactiveModifierOptions = []string{
	"--changed",
	"--contains",
	"--diff",
	"--exclude",
	"--include",
	"--no-tree",
	"--only",
	"--preview",
	"--print",
	"--quiet",
	"--snippet",
	"--staged",
	"--then",
	"--unstaged",
	"--untracked",
	"--verbose",
	"--yes",
}

func interactiveModifierHint(line string, colors colorPalette) string {
	hint, _ := interactiveModifierHintInfo(line, colors)
	return hint
}

func interactiveModifierHintInfo(line string, colors colorPalette) (string, int) {
	plain := interactiveModifierHintPlain(line)
	if plain == "" {
		return "", 0
	}
	return colors.Dim + plain + colors.Reset, utf8.RuneCountInString(plain)
}

func interactiveModifierHintPlain(line string) string {
	token := currentInteractiveToken(line)
	if !strings.HasPrefix(token, "--") {
		return ""
	}
	matches := matchingInteractiveModifiers(token)
	if len(matches) == 0 {
		return ""
	}
	if len(matches) == 1 {
		remainder := strings.TrimPrefix(matches[0], token)
		if remainder == "" {
			return ""
		}
		return remainder
	}
	common := longestCommonPrefix(matches)
	if len(common) > len(token) {
		return strings.TrimPrefix(common, token)
	}
	preview := matches
	if len(preview) > 4 {
		preview = preview[:4]
	}
	suffix := strings.Join(preview, " ")
	if len(matches) > len(preview) {
		suffix += fmt.Sprintf(" +%d", len(matches)-len(preview))
	}
	return " [" + suffix + "]"
}

func applyInteractiveModifierCompletion(line string) (string, bool) {
	token, start := currentInteractiveTokenWithStart(line)
	if !strings.HasPrefix(token, "--") {
		return line, false
	}
	matches := matchingInteractiveModifiers(token)
	if len(matches) == 0 {
		return line, false
	}
	replacement := token
	switch {
	case len(matches) == 1:
		replacement = matches[0]
		if !strings.HasSuffix(line, " ") {
			replacement += " "
		}
	default:
		common := longestCommonPrefix(matches)
		if len(common) <= len(token) {
			return line, false
		}
		replacement = common
	}
	return line[:start] + replacement, true
}

func currentInteractiveToken(line string) string {
	token, _ := currentInteractiveTokenWithStart(line)
	return token
}

func currentInteractiveTokenWithStart(line string) (string, int) {
	start := strings.LastIndexAny(line, " \t")
	if start == -1 {
		return line, 0
	}
	return line[start+1:], start + 1
}

func matchingInteractiveModifiers(prefix string) []string {
	matches := make([]string, 0, len(interactiveModifierOptions))
	for _, option := range interactiveModifierOptions {
		if strings.HasPrefix(option, prefix) {
			matches = append(matches, option)
		}
	}
	slices.Sort(matches)
	return matches
}

func longestCommonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, value := range values[1:] {
		for !strings.HasPrefix(value, prefix) && prefix != "" {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	return prefix
}

func lastRune(text string) (rune, int) {
	return utf8.DecodeLastRuneInString(text)
}

func shellQuoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n\"'\\*?[]{}()$&;|<>") {
		return arg
	}
	return strconv.Quote(arg)
}

func interactiveTargetQuery(raw string) string {
	value := strings.ReplaceAll(raw, "\\", "/")
	return normalizeRelPath(value)
}

func shouldIgnoreImmediateEmptyInteractiveLine(line string, promptShownAt, now time.Time, enabled bool) bool {
	if !enabled || line != "" {
		return false
	}
	return now.Sub(promptShownAt) < 150*time.Millisecond
}

func splitInteractiveTokens(line string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, r := range line {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				current.WriteRune(r)
			}
		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			default:
				current.WriteRune(r)
			}
		default:
			switch r {
			case '\\':
				escaped = true
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case ' ', '\t':
				flush()
			default:
				current.WriteRune(r)
			}
		}
	}

	if escaped || inSingle || inDouble {
		return nil, newUsageError("Error: unterminated quote or escape in modifier input.")
	}
	flush()
	return tokens, nil
}

type interactiveInputParse struct {
	targets          []string
	includeQueries   []string
	modifiers        []string
	nextScopeTargets []string
	hasThen          bool
}

// parseInteractiveInputTokens enforces the builder grammar:
// [targets|--include [query]]* [modifiers] [--then [next-scope-targets...]].
func parseInteractiveInputTokens(tokens []string) (interactiveInputParse, error) {
	parsed := interactiveInputParse{
		targets:          make([]string, 0, len(tokens)),
		includeQueries:   make([]string, 0, len(tokens)),
		modifiers:        make([]string, 0, len(tokens)),
		nextScopeTargets: make([]string, 0, len(tokens)),
	}
	seenModifier := false

	for i := 0; i < len(tokens); i++ {
		if !seenModifier && tokens[i] == "--then" {
			parsed.hasThen = true
			parsed.nextScopeTargets = append(parsed.nextScopeTargets, tokens[i+1:]...)
			for _, token := range parsed.nextScopeTargets {
				if strings.HasPrefix(token, "-") {
					return interactiveInputParse{}, newUsageError("Error: --then expects next-scope targets here.\n  Example: --then tests")
				}
			}
			return parsed, nil
		}
		if !seenModifier && tokens[i] == "--include" {
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
				i++
				parsed.includeQueries = append(parsed.includeQueries, tokens[i])
				continue
			}
			parsed.includeQueries = append(parsed.includeQueries, "")
			continue
		}
		if !seenModifier && !strings.HasPrefix(tokens[i], "-") {
			parsed.targets = append(parsed.targets, tokens[i])
			continue
		}

		seenModifier = true
		switch tokens[i] {
		case "-v", "--verbose", "-q", "--quiet", "-y", "--yes", "-p", "--print", "-t", "--no-tree",
			"--preview", "--changed", "--staged", "--unstaged", "--untracked", "--diff", "--snippet":
			parsed.modifiers = append(parsed.modifiers, tokens[i])
		case "--only":
			if i+1 >= len(tokens) {
				return interactiveInputParse{}, newUsageError("Error: --only requires a pattern.\n  Example: catclip src --only '*.ts'")
			}
			parsed.modifiers = append(parsed.modifiers, tokens[i], tokens[i+1])
			i++
		case "--exclude":
			if i+1 >= len(tokens) {
				return interactiveInputParse{}, newUsageError("Error: --exclude requires a pattern.\n  Example: catclip src --exclude '*.test.*'")
			}
			parsed.modifiers = append(parsed.modifiers, tokens[i], tokens[i+1])
			i++
		case "--contains":
			if i+1 >= len(tokens) {
				return interactiveInputParse{}, newUsageError("Error: --contains requires a regex pattern.\n  Example: catclip src --contains 'TODO'")
			}
			parsed.modifiers = append(parsed.modifiers, tokens[i], tokens[i+1])
			i++
		case "--include":
			return interactiveInputParse{}, newUsageError("Error: --include must come before modifiers.\n  Use it while selecting targets for the current scope.")
		case "--then":
			parsed.hasThen = true
			parsed.nextScopeTargets = append(parsed.nextScopeTargets, tokens[i+1:]...)
			for _, token := range parsed.nextScopeTargets {
				if strings.HasPrefix(token, "-") {
					return interactiveInputParse{}, newUsageError("Error: --then expects next-scope targets here.\n  Example: --then tests")
				}
			}
			return parsed, nil
		case "--":
			return interactiveInputParse{}, newUsageError("Error: modifier input cannot include positional targets.\n  Use --then to start a new scope.")
		default:
			switch {
			case strings.HasPrefix(tokens[i], "--contains="):
				return interactiveInputParse{}, newUsageError("Error: --contains requires a space before the pattern.\n  Use: --contains 'pattern'")
			case strings.HasPrefix(tokens[i], "--"):
				return interactiveInputParse{}, newUsageError("Error: Unknown option %s\n  Run 'catclip --help' for available options.", singleQuoted(tokens[i]))
			case strings.HasPrefix(tokens[i], "-") && len(tokens[i]) > 1:
				return interactiveInputParse{}, newUsageError("Error: Unknown option %s\n  Run 'catclip --help' for available options.", singleQuoted(tokens[i]))
			default:
				return interactiveInputParse{}, newUsageError("Error: positional targets must come before modifiers.\n  Add targets first, use --include, or use --then for a new scope.")
			}
		}
	}
	return parsed, nil
}

func targetMatchPaths(matches []targetMatch) []string {
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		if match.Kind == "done" {
			continue
		}
		paths = append(paths, match.Path)
	}
	return paths
}

func targetMatchArgs(matches []targetMatch) []string {
	args := make([]string, 0, len(matches)*2)
	for _, match := range matches {
		if match.Kind == "done" {
			continue
		}
		if match.Ignored {
			args = append(args, "--include", match.Path)
			continue
		}
		args = append(args, match.Path)
	}
	return args
}

func interactiveResolvedTargetPaths(args []string) []string {
	paths := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--include" {
			if i+1 < len(args) {
				paths = append(paths, args[i+1])
				i++
			}
			continue
		}
		paths = append(paths, args[i])
	}
	return paths
}

func currentInteractiveScopeTargetPaths(args []string) []string {
	cfg, err := parseArgs(args)
	if err != nil || len(cfg.Scopes) == 0 {
		return nil
	}
	return append([]string(nil), cfg.Scopes[len(cfg.Scopes)-1].Targets...)
}

func allInteractiveTargetPaths(args []string) []string {
	cfg, err := parseArgs(args)
	if err != nil || len(cfg.Scopes) == 0 {
		return nil
	}
	paths := make([]string, 0, len(cfg.Scopes)*2)
	for _, scope := range cfg.Scopes {
		paths = append(paths, scope.Targets...)
	}
	return paths
}
