package catclip

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var errSelectionCancelled = errors.New("selection cancelled")

type scopeResolver struct {
	cfg                  runConfig
	gitCtx               gitContext
	matcher              scopeMatcher
	allowFileSymlinks    bool
	textFileCache        map[string]bool
	useGitIgnore         bool
	includedTargets      map[string]struct{}
	wantedBasenames      map[string]struct{}
	interactiveTargets   []targetMatch
	interactiveTargetsOk bool
	ignoredTargets       []targetMatch
	ignoredTargetsOk     bool
	visibleDirs          visibleDirIndex
	visibleDirsReady     bool
	visibleFiles         visibleFileIndex
	visibleFilesReady    bool
	visibleFileList      []fileEntry
	visibleFileListReady bool
}

func evaluateScope(cfg runConfig, gitCtx gitContext, scopeIndex int, s scope, baseRules []ignoreRule, stderr io.Writer, colors colorPalette) ([]fileEntry, []diagnostic, []string, bool, error) {
	matcher, err := buildScopeMatcher(baseRules, s)
	if err != nil {
		return nil, nil, nil, false, err
	}
	resolver := scopeResolver{
		cfg:               cfg,
		gitCtx:            gitCtx,
		matcher:           matcher,
		allowFileSymlinks: false,
		useGitIgnore:      gitCtx.Enabled,
		includedTargets:   makeIncludedTargetSet(s.IncludedTargets),
		wantedBasenames:   collectWantedBasenames(s.Targets),
	}

	var diagnostics []diagnostic
	var notices []string
	var entries []fileEntry
	hadSelectionCancel := false
	for _, target := range s.Targets {
		discovered, targetDiagnostics, targetNotices, selectionCancelled, err := resolver.resolveAndDiscoverTarget(scopeIndex, target, stderr, colors)
		if err != nil {
			return nil, diagnostics, notices, hadSelectionCancel, err
		}
		diagnostics = append(diagnostics, targetDiagnostics...)
		notices = append(notices, targetNotices...)
		entries = append(entries, discovered...)
		hadSelectionCancel = hadSelectionCancel || selectionCancelled
	}

	entries = dedupeEntriesByPath(entries)

	if gitCtx.Enabled {
		entries, err = filterGitIgnoredEntries(gitCtx, entries)
		if err != nil {
			return nil, diagnostics, notices, hadSelectionCancel, err
		}
	}

	if s.Changed {
		if gitCtx.Enabled {
			entries, err = filterChangedEntries(gitCtx, s, entries)
			if err != nil {
				return nil, diagnostics, notices, hadSelectionCancel, err
			}
		} else {
			diagnostics = append(diagnostics, diagnostic{message: "Warning: --changed/--staged/--unstaged/--untracked require a git repo."})
		}
	}

	if s.Contains != "" {
		entries = ensureEntryAbsPaths(entries, cfg.WorkingDir)
		entries, err = filterEntriesByContent(entries, s.Contains)
		if err != nil {
			return nil, diagnostics, notices, hadSelectionCancel, err
		}
	}

	mode := entryModeFull
	switch {
	case s.Diff:
		mode = entryModeDiff
	case s.Snippet:
		mode = entryModeSnippet
	}
	for i := range entries {
		entries[i].Mode = mode
		entries[i].SnippetPattern = s.Contains
		entries[i].DiffWantStaged = s.Staged
		entries[i].DiffWantUnstaged = s.Unstaged
	}
	entries = ensureEntryAbsPaths(entries, cfg.WorkingDir)
	return entries, diagnostics, dedupePreserveOrder(notices), hadSelectionCancel, nil
}

func makeIncludedTargetSet(targets []string) map[string]struct{} {
	if len(targets) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = normalizeRelPath(target)
		if target == "" {
			continue
		}
		set[target] = struct{}{}
	}
	return set
}

func (r *scopeResolver) targetIncluded(target string) bool {
	if len(r.includedTargets) == 0 {
		return false
	}
	target = normalizeRelPath(target)
	_, ok := r.includedTargets[target]
	return ok
}

func (r *scopeResolver) targetPathExists(relTarget string) (bool, error) {
	_, err := os.Stat(filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(relTarget)))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// canResolveTargetWithoutPrompt mirrors the non-interactive resolver's
// deterministic branches. It returns true only when a target can be handled
// without opening fzf or prompting for ambiguity resolution.
func (r *scopeResolver) canResolveTargetWithoutPrompt(target string) (bool, error) {
	normalizedTarget := normalizeRelPath(target)
	if normalizedTarget == "" {
		normalizedTarget = "."
	}

	exists, err := r.targetPathExists(normalizedTarget)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}

	if strings.Contains(normalizedTarget, "/") {
		return r.canResolveScopedTargetWithoutPrompt(normalizedTarget)
	}

	if resolvedDir, ok, err := r.resolveVisibleDirByExactBasename(".", normalizedTarget); err != nil {
		return false, err
	} else if ok && resolvedDir != "" {
		conflict, err := r.hasVisibleFileBasenameConflict(".", normalizedTarget)
		if err != nil {
			return false, err
		}
		if !conflict {
			return true, nil
		}
	}

	searchedFiles := false
	if prefersDirectFileLookup(normalizedTarget) {
		searchedFiles = true
		discovered, skipped, err := r.resolveVisibleFilesByBasename(".", normalizedTarget)
		if err != nil {
			return false, err
		}
		if len(discovered) > 0 || len(skipped) > 0 {
			return true, nil
		}
		fuzzyFiles, err := r.fuzzySearchFiles(".", normalizedTarget)
		if err != nil {
			return false, err
		}
		switch len(fuzzyFiles) {
		case 0:
		case 1:
			return true, nil
		default:
			return false, nil
		}
	}

	matches, err := r.fuzzySearchDirs(".", normalizedTarget)
	if err != nil {
		return false, err
	}
	if !searchedFiles && len(matches) > 0 {
		fuzzyFiles, err := r.fuzzySearchFiles(".", normalizedTarget)
		if err != nil {
			return false, err
		}
		if len(fuzzyFiles) > 0 {
			combined, err := rankTargetMatches(normalizedTarget, matches, fuzzyFiles)
			if err != nil {
				return false, err
			}
			return len(combined) == 1, nil
		}
	}

	switch len(matches) {
	case 0:
		if searchedFiles {
			return false, nil
		}
		discovered, skipped, err := r.resolveVisibleFilesByBasename(".", normalizedTarget)
		if err != nil {
			return false, err
		}
		if len(discovered) > 0 || len(skipped) > 0 {
			return true, nil
		}
		fuzzyFiles, err := r.fuzzySearchFiles(".", normalizedTarget)
		if err != nil {
			return false, err
		}
		return len(fuzzyFiles) == 1, nil
	case 1:
		return true, nil
	default:
		return false, nil
	}
}

func (r *scopeResolver) canResolveScopedTargetWithoutPrompt(normalizedTarget string) (bool, error) {
	dirPart := path.Dir(normalizedTarget)
	baseName := path.Base(normalizedTarget)

	resolvedDir, ok, err := r.resolveChainedDirWithoutPrompt(dirPart)
	if err != nil || !ok {
		return false, err
	}

	fullRel := normalizeRelPath(path.Join(resolvedDir, baseName))
	exists, err := r.targetPathExists(fullRel)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}

	blockedDir, err := r.blockInfoForDir(resolvedDir)
	if err != nil {
		return false, err
	}
	if blockedDir != nil {
		discovered, err := discoverFilesByBasenameUnder(r.cfg.WorkingDir, filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(resolvedDir)), resolvedDir, baseName, r.matcher, r.classifyTextFile, blockedDir)
		if err != nil {
			return false, err
		}
		if len(discovered) > 0 {
			return true, nil
		}
	} else {
		discovered, skipped, err := r.resolveVisibleFilesByBasename(resolvedDir, baseName)
		if err != nil {
			return false, err
		}
		if len(discovered) > 0 || len(skipped) > 0 {
			return true, nil
		}
	}

	fuzzyFiles, err := r.fuzzySearchFilesUnder(resolvedDir, baseName, blockedDir)
	if err != nil {
		return false, err
	}
	return len(fuzzyFiles) == 1, nil
}

func (r *scopeResolver) resolveAndDiscoverTarget(scopeIndex int, target string, stderr io.Writer, colors colorPalette) ([]fileEntry, []diagnostic, []string, bool, error) {
	var diagnostics []diagnostic
	var notices []string

	if filepath.IsAbs(target) {
		return nil, nil, nil, false, newUsageError("Error: Absolute paths not allowed: %s\n  Use a relative path from your project root instead.", singleQuoted(target))
	}
	if containsParentTraversal(target) {
		return nil, nil, nil, false, newUsageError("Error: Cannot traverse above working directory: %s\n  catclip only operates within the current directory tree.\n  Use a relative path from your project root instead.\n  Example: catclip config/", singleQuoted(target))
	}

	normalizedTarget := normalizeRelPath(target)
	if normalizedTarget == "" {
		normalizedTarget = "."
	}
	if r.targetIncluded(normalizedTarget) {
		discovered, targetDiagnostics, selectionCancelled, err := r.resolveIncludedTarget(target, normalizedTarget, stderr, colors)
		return discovered, targetDiagnostics, notices, selectionCancelled, err
	}

	if discovered, handled, diag, err := r.resolveExactTarget(normalizedTarget, false, colors); handled {
		if diag != nil {
			diagnostics = append(diagnostics, *diag)
		}
		return discovered, diagnostics, notices, false, err
	}

	if strings.Contains(normalizedTarget, "/") {
		dirPart := path.Dir(normalizedTarget)
		baseName := path.Base(normalizedTarget)
		resolvedDir, err := r.resolveChainedDir(dirPart, stderr, colors)
		if err != nil {
			if errors.Is(err, errSelectionCancelled) {
				return nil, diagnostics, notices, true, nil
			}
			return nil, diagnostics, notices, false, err
		}
		fullRel := normalizeRelPath(path.Join(resolvedDir, baseName))
		discovered, handled, diag, err := r.resolveExactTarget(fullRel, true, colors)
		if handled {
			if diag != nil {
				diagnostics = append(diagnostics, *diag)
			}
			return discovered, diagnostics, notices, false, err
		}
		blockedDir, err := r.blockInfoForDir(resolvedDir)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		if blockedDir != nil {
			discovered, err = discoverFilesByBasenameUnder(r.cfg.WorkingDir, filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(resolvedDir)), resolvedDir, baseName, r.matcher, r.classifyTextFile, blockedDir)
		} else {
			var skipped []skippedMatch
			discovered, skipped, err = r.resolveVisibleFilesByBasename(resolvedDir, baseName)
			notices = append(notices, formatSkippedMatchesWarning(skipped)...)
		}
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		if len(discovered) > 0 {
			return withTargetRoot(discovered, resolvedDir), diagnostics, notices, false, nil
		}
		fuzzyFiles, err := r.fuzzySearchFilesUnder(resolvedDir, baseName, blockedDir)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		switch len(fuzzyFiles) {
		case 0:
		case 1:
			discovered, handled, diag, err := r.resolveExactTarget(fuzzyFiles[0], true, colors)
			if diag != nil {
				diagnostics = append(diagnostics, *diag)
			}
			if handled {
				return discovered, diagnostics, notices, false, err
			}
		default:
			selected, err := chooseFileMatch(r.cfg, baseName, resolvedDir, fuzzyFiles, stderr, colors)
			if err != nil {
				if errors.Is(err, errSelectionCancelled) {
					return nil, diagnostics, notices, true, nil
				}
				return nil, diagnostics, notices, false, err
			}
			discovered, handled, diag, err := r.resolveExactTarget(selected, true, colors)
			if diag != nil {
				diagnostics = append(diagnostics, *diag)
			}
			if handled {
				return discovered, diagnostics, notices, false, err
			}
		}
		diagnostics = append(diagnostics, diagnostic{message: targetNotFoundWarning(target, scopeIndex, colors)})
		return nil, diagnostics, notices, false, nil
	}

	searchedFiles := false
	if prefersDirectFileLookup(normalizedTarget) {
		searchedFiles = true
		var skipped []skippedMatch
		discovered, skipped, err := r.resolveVisibleFilesByBasename(".", normalizedTarget)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		notices = append(notices, formatSkippedMatchesWarning(skipped)...)
		if len(discovered) > 0 {
			return discovered, diagnostics, notices, false, nil
		}
		fuzzyFiles, err := r.fuzzySearchFiles(".", normalizedTarget)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		switch len(fuzzyFiles) {
		case 0:
		case 1:
			discovered, handled, diag, err := r.resolveExactTarget(fuzzyFiles[0], false, colors)
			if diag != nil {
				diagnostics = append(diagnostics, *diag)
			}
			if handled {
				return discovered, diagnostics, notices, false, err
			}
		default:
			selected, err := chooseFileMatch(r.cfg, normalizedTarget, ".", fuzzyFiles, stderr, colors)
			if err != nil {
				if errors.Is(err, errSelectionCancelled) {
					return nil, diagnostics, notices, true, nil
				}
				return nil, diagnostics, notices, false, err
			}
			discovered, err := r.resolveTargetMatches([]targetMatch{{Path: selected, Kind: "file"}}, colors)
			if err != nil {
				return nil, diagnostics, notices, false, err
			}
			return discovered, diagnostics, notices, false, nil
		}
	}

	if resolvedDir, ok, err := r.resolveVisibleDirByExactBasename(".", normalizedTarget); err != nil {
		return nil, diagnostics, notices, false, err
	} else if ok && resolvedDir != "" {
		conflict, err := r.hasVisibleFileBasenameConflict(".", normalizedTarget)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		if !conflict {
			discovered, handled, diag, err := r.resolveExactTarget(resolvedDir, false, colors)
			if diag != nil {
				diagnostics = append(diagnostics, *diag)
			}
			if handled {
				return discovered, diagnostics, notices, false, err
			}
		}
	}

	matches, err := r.fuzzySearchDirs(".", normalizedTarget)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if !searchedFiles && len(matches) > 0 {
		fuzzyFiles, err := r.fuzzySearchFiles(".", normalizedTarget)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		if len(fuzzyFiles) > 0 {
			combined, err := rankTargetMatches(normalizedTarget, matches, fuzzyFiles)
			if err != nil {
				return nil, diagnostics, notices, false, err
			}
			if len(combined) == 1 {
				discovered, handled, diag, err := r.resolveTargetMatch(combined[0], colors)
				if diag != nil {
					diagnostics = append(diagnostics, *diag)
				}
				if handled {
					return discovered, diagnostics, notices, false, err
				}
			}
			if !r.cfg.HeadlessStdoutMode() && canPromptInteractively() {
				selected, err := chooseTargetMatch(r.cfg, normalizedTarget, combined, stderr, colors)
				if err != nil {
					if errors.Is(err, errSelectionCancelled) {
						return nil, diagnostics, notices, true, nil
					}
					return nil, diagnostics, notices, false, err
				}
				discovered, err := r.resolveTargetMatches([]targetMatch{selected}, colors)
				if err != nil {
					return nil, diagnostics, notices, false, err
				}
				return discovered, diagnostics, notices, false, nil
			}
		}
	}
	switch len(matches) {
	case 0:
		if !searchedFiles {
			var skipped []skippedMatch
			discovered, skipped, err := r.resolveVisibleFilesByBasename(".", normalizedTarget)
			if err != nil {
				return nil, diagnostics, notices, false, err
			}
			notices = append(notices, formatSkippedMatchesWarning(skipped)...)
			if len(discovered) > 0 {
				return discovered, diagnostics, notices, false, nil
			}
			fuzzyFiles, err := r.fuzzySearchFiles(".", normalizedTarget)
			if err != nil {
				return nil, diagnostics, notices, false, err
			}
			switch len(fuzzyFiles) {
			case 0:
			case 1:
				discovered, handled, diag, err := r.resolveExactTarget(fuzzyFiles[0], false, colors)
				if diag != nil {
					diagnostics = append(diagnostics, *diag)
				}
				if handled {
					return discovered, diagnostics, notices, false, err
				}
			default:
				selected, err := chooseFileMatch(r.cfg, normalizedTarget, ".", fuzzyFiles, stderr, colors)
				if err != nil {
					if errors.Is(err, errSelectionCancelled) {
						return nil, diagnostics, notices, true, nil
					}
					return nil, diagnostics, notices, false, err
				}
				discovered, err := r.resolveTargetMatches([]targetMatch{{Path: selected, Kind: "file"}}, colors)
				if err != nil {
					return nil, diagnostics, notices, false, err
				}
				return discovered, diagnostics, notices, false, nil
			}
		}
		if len(notices) == 0 {
			diagnostics = append(diagnostics, diagnostic{message: targetNotFoundWarning(target, scopeIndex, colors)})
		}
		return nil, diagnostics, notices, false, nil
	case 1:
		files, err := r.discoverVisibleFilesUnder(matches[0])
		return withTargetRoot(files, matches[0]), diagnostics, notices, false, err
	default:
		selected, err := chooseDirectoryMatch(r.cfg, target, ".", matches, stderr, colors)
		if err != nil {
			if errors.Is(err, errSelectionCancelled) {
				return nil, diagnostics, notices, true, nil
			}
			return nil, nil, nil, false, err
		}
		files, err := r.resolveTargetMatches([]targetMatch{{Path: selected, Kind: "dir"}}, colors)
		if err != nil {
			return nil, diagnostics, notices, false, err
		}
		return files, diagnostics, notices, false, nil
	}
}

func (r *scopeResolver) resolveTargetMatch(match targetMatch, colors colorPalette) ([]fileEntry, bool, *diagnostic, error) {
	if match.Ignored {
		return r.resolveExactTarget(match.Path, false, colors)
	}
	switch match.Kind {
	case "file":
		return r.resolveExactTarget(match.Path, false, colors)
	case "dir":
		files, err := r.discoverVisibleFilesUnder(match.Path)
		if err != nil {
			return nil, true, nil, err
		}
		return withTargetRoot(files, match.Path), true, nil, nil
	default:
		return nil, false, nil, nil
	}
}

func (r *scopeResolver) resolveIncludedTarget(target, normalizedTarget string, stderr io.Writer, colors colorPalette) ([]fileEntry, []diagnostic, bool, error) {
	var diagnostics []diagnostic

	if discovered, handled, diag, err := r.resolveExactTarget(normalizedTarget, false, colors); handled {
		if diag != nil {
			diagnostics = append(diagnostics, *diag)
		}
		return discovered, diagnostics, false, err
	}

	if r.cfg.HeadlessStdoutMode() || !canPromptInteractively() {
		return nil, []diagnostic{{
			message: includeQueryNeedsSelectionMessage(target, colors),
			isError: true,
		}}, false, nil
	}

	matches, err := r.chooseIgnoredTargetMatches(target, "include> ", nil)
	if err != nil {
		if errors.Is(err, errSelectionCancelled) {
			return nil, nil, true, nil
		}
		return nil, nil, false, err
	}
	discovered, err := r.resolveTargetMatches(matches, colors)
	if err != nil {
		return nil, nil, false, err
	}
	return discovered, diagnostics, false, nil
}

func (r *scopeResolver) resolveExactTarget(relTarget string, fromChained bool, colors colorPalette) ([]fileEntry, bool, *diagnostic, error) {
	absTarget := filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(relTarget))
	info, err := os.Lstat(absTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, true, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, true, nil, nil
	}

	if info.IsDir() {
		if ignored, rule := r.matcher.dirIgnored(relTarget); ignored {
			if !r.targetIncluded(relTarget) {
				return nil, true, &diagnostic{message: ignoredDirMessage(relTarget, rule, ".hiss", colors), isError: true}, nil
			}
			files, err := discoverFilesUnder(r.cfg.WorkingDir, absTarget, relTarget, r.matcher, r.classifyTextFile, &blockInfo{Rule: rule, Source: ".hiss"})
			return withTargetRoot(files, relTarget), true, nil, err
		}
		gitIgnored, err := r.gitIgnored(relTarget)
		if err != nil {
			return nil, true, nil, err
		}
		if gitIgnored {
			if !r.targetIncluded(relTarget) {
				return nil, true, &diagnostic{message: ignoredDirMessage(relTarget, ".gitignore", ".gitignore", colors), isError: true}, nil
			}
			files, err := discoverFilesUnder(r.cfg.WorkingDir, absTarget, relTarget, r.matcher, r.classifyTextFile, &blockInfo{Rule: ".gitignore", Source: ".gitignore"})
			return withTargetRoot(files, relTarget), true, nil, err
		}
		files, err := r.discoverVisibleFilesUnder(relTarget)
		return withTargetRoot(files, relTarget), true, nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, true, nil, nil
	}
	if len(r.matcher.only) > 0 && !r.matcher.matchesOnly(relTarget) {
		return nil, true, nil, nil
	}
	if excludedTextLikeAsset(relTarget) {
		return nil, true, nil, nil
	}
	text, err := r.classifyTextFile(relTarget, absTarget)
	if err != nil {
		return nil, true, nil, err
	}
	if !text {
		return nil, true, nil, nil
	}
	entry := fileEntry{
		AbsPath:    absTarget,
		RelPath:    relTarget,
		GitVisible: true,
	}
	if dir := normalizeRelPath(path.Dir(relTarget)); dir != "." {
		entry.TargetRoot = dir
	}
	if ignored, rule := r.matcher.fileIgnored(relTarget); ignored {
		if !r.targetIncluded(relTarget) {
			return nil, true, &diagnostic{message: ignoredFileMessage(relTarget, rule, ".hiss", fromChained, colors), isError: true}, nil
		}
		entry = withBypass(entry, "direct", blockInfo{Rule: rule, Source: ".hiss"})
	} else {
		gitIgnored, err := r.gitIgnored(relTarget)
		if err != nil {
			return nil, true, nil, err
		}
		if gitIgnored {
			if !r.targetIncluded(relTarget) {
				return nil, true, &diagnostic{message: ignoredFileMessage(relTarget, ".gitignore", ".gitignore", fromChained, colors), isError: true}, nil
			}
			entry = withBypass(entry, "direct", blockInfo{Rule: ".gitignore", Source: ".gitignore"})
		}
	}
	return []fileEntry{entry}, true, nil, nil
}

func (r *scopeResolver) gitIgnored(relPath string) (bool, error) {
	if !r.useGitIgnore || relPath == "." || relPath == "" {
		return false, nil
	}
	lines, err := runGitLines(r.gitCtx.Root, []string{r.gitCtx.toRepoPath(relPath)}, "check-ignore", "--stdin")
	if err != nil {
		return false, err
	}
	return len(lines) > 0, nil
}

func (r *scopeResolver) classifyTextFile(relPath, absPath string) (bool, error) {
	if knownTextLikeFile(relPath) {
		return true, nil
	}
	if absPath == "" {
		absPath = filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(relPath))
	}
	absPath = filepath.Clean(absPath)
	if r.textFileCache == nil {
		r.textFileCache = make(map[string]bool)
	}
	if text, ok := r.textFileCache[absPath]; ok {
		return text, nil
	}
	text, err := isProbablyTextFile(absPath)
	if err != nil {
		return false, err
	}
	r.textFileCache[absPath] = text
	return text, nil
}

func (r *scopeResolver) blockInfoForDir(relPath string) (*blockInfo, error) {
	if relPath == "." || relPath == "" {
		return nil, nil
	}
	if ignored, rule := r.matcher.dirIgnored(relPath); ignored {
		return &blockInfo{Rule: rule, Source: ".hiss"}, nil
	}
	gitIgnored, err := r.gitIgnored(relPath)
	if err != nil {
		return nil, err
	}
	if gitIgnored {
		return &blockInfo{Rule: ".gitignore", Source: ".gitignore"}, nil
	}
	return nil, nil
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
			currentAbs = exactAbs
			currentRel = candidateRel
			continue
		}

		resolvedDir, ok, err := r.resolveVisibleDirByExactBasename(currentRel, seg)
		if err != nil {
			return "", err
		}
		if ok && resolvedDir != "" {
			currentRel = resolvedDir
			currentAbs = filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(currentRel))
			continue
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

func (r *scopeResolver) resolveChainedDirWithoutPrompt(relPath string) (string, bool, error) {
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
			currentAbs = exactAbs
			currentRel = candidateRel
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return "", false, err
		}

		resolvedDir, ok, err := r.resolveVisibleDirByExactBasename(currentRel, seg)
		if err != nil {
			return "", false, err
		}
		if ok && resolvedDir != "" {
			currentRel = resolvedDir
			currentAbs = filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(currentRel))
			continue
		}

		matches, err := r.fuzzySearchDirs(currentRel, seg)
		if err != nil {
			return "", false, err
		}
		if len(matches) != 1 {
			return "", false, nil
		}
		currentRel = matches[0]
		currentAbs = filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(currentRel))
	}

	return currentRel, true, nil
}

func (r *scopeResolver) resolveVisibleDirByExactBasename(baseRel, basename string) (string, bool, error) {
	if basename == "" || basename == "." {
		return "", false, nil
	}
	if err := r.buildVisibleDirIndex(); err != nil {
		return "", false, err
	}

	baseRel = normalizeRelPath(baseRel)
	if baseRel == "" {
		baseRel = "."
	}
	prefix := ""
	if baseRel != "." {
		prefix = baseRel + "/"
	}

	var match string
	for _, rel := range r.visibleDirs.dirs {
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			continue
		}
		if path.Base(rel) != basename {
			continue
		}
		if match != "" {
			return "", false, nil
		}
		match = rel
	}
	if match == "" {
		return "", false, nil
	}
	return match, true, nil
}

func (r *scopeResolver) hasVisibleFileBasenameConflict(baseRel, needle string) (bool, error) {
	if needle == "" || needle == "." {
		return false, nil
	}
	if err := r.buildVisibleFileList(); err != nil {
		return false, err
	}

	baseRel = normalizeRelPath(baseRel)
	if baseRel == "" {
		baseRel = "."
	}
	prefix := ""
	if baseRel != "." {
		prefix = baseRel + "/"
	}

	for _, entry := range r.visibleFileList {
		if prefix != "" && !strings.HasPrefix(entry.RelPath, prefix) {
			continue
		}
		base := path.Base(entry.RelPath)
		if base == needle {
			return true, nil
		}
		if strings.TrimSuffix(base, path.Ext(base)) == needle {
			return true, nil
		}
	}
	return false, nil
}

func (r *scopeResolver) chooseRootTargetMatches(query, prompt string, includeCopyAll bool, selectedPaths []string) ([]targetMatch, error) {
	query = normalizeInteractivePickerQuery(query)
	if selectionContainsAll(selectedPaths) {
		return nil, errSelectionCancelled
	}
	stopSpinner := func() {}
	if !r.interactiveTargetsOk {
		stopSpinner = startLoadingSpinner(os.Stderr, "Loading targets...")
	}
	allTargets, err := r.allVisibleTargets()
	stopSpinner()
	if err != nil {
		return nil, err
	}
	options := make([]targetMatch, 0, len(allTargets))
	for _, target := range allTargets {
		if coveredBySelection(target.Path, selectedPaths) {
			continue
		}
		options = append(options, target)
	}
	if includeCopyAll {
		options = append([]targetMatch{{Path: ".", Kind: "all"}}, options...)
	}
	if len(options) == 0 {
		return nil, errSelectionCancelled
	}
	if match, ok := exactInteractiveTargetMatch(options, query); ok {
		return []targetMatch{match}, nil
	}

	path, err := fuzzyResolverBinary()
	if err != nil {
		return nil, err
	}

	labels, index := targetMatchLabels(options)
	currentQuery := query
	browsingIgnored := false
	for {
		if browsingIgnored {
			ignored, err := r.chooseIgnoredTargetMatches(currentQuery, "include> ", selectedPaths)
			if err != nil {
				if errors.Is(err, errSelectionCancelled) {
					browsingIgnored = false
					continue
				}
				return nil, err
			}
			return ignored, nil
		}

		result, err := chooseManyWithFzfControl(path, currentQuery, prompt, "1,2", safeTargetPickerHeader(), labels, "ctrl-o")
		if err != nil {
			return nil, err
		}
		currentQuery = result.Query
		if result.Key == "ctrl-o" {
			browsingIgnored = true
			continue
		}

		selected := make([]targetMatch, 0, len(result.Matches))
		for _, label := range result.Matches {
			match, ok := index[label]
			if ok {
				if match.Kind == "all" {
					return []targetMatch{match}, nil
				}
				selected = append(selected, match)
			}
		}
		if len(selected) == 0 {
			return nil, errSelectionCancelled
		}
		return selected, nil
	}
}

func (r *scopeResolver) chooseIgnoredTargetMatches(query, prompt string, selectedPaths []string) ([]targetMatch, error) {
	query = normalizeInteractivePickerQuery(query)
	stopSpinner := func() {}
	if !r.ignoredTargetsOk {
		stopSpinner = startLoadingSpinner(os.Stderr, "Loading ignored targets...")
	}
	allTargets, err := r.allIgnoredTargets()
	stopSpinner()
	if err != nil {
		return nil, err
	}
	options := filterRedundantTargetMatches(allTargets, selectionPathsForIgnoredTargets(selectedPaths))
	if len(options) == 0 {
		return nil, errSelectionCancelled
	}
	if match, ok := exactTargetPathMatch(options, query); ok {
		return []targetMatch{match}, nil
	}

	path, err := fuzzyResolverBinary()
	if err != nil {
		return nil, err
	}
	labels, index := targetMatchLabels(options)
	selectedLabels, err := chooseManyWithFzfHeader(path, query, prompt, ignoredTargetPickerHeader(), labels)
	if err != nil {
		return nil, err
	}

	selected := make([]targetMatch, 0, len(selectedLabels))
	for _, label := range selectedLabels {
		match, ok := index[label]
		if ok {
			selected = append(selected, match)
		}
	}
	if len(selected) == 0 {
		return nil, errSelectionCancelled
	}
	return selected, nil
}

func selectionPathsForIgnoredTargets(selectedPaths []string) []string {
	filtered := make([]string, 0, len(selectedPaths))
	for _, selected := range selectedPaths {
		if normalizeRelPath(selected) == "." {
			continue
		}
		filtered = append(filtered, selected)
	}
	return filtered
}

func (r *scopeResolver) resolveInteractiveIncludeTargets(query string, selectedPaths []string) ([]string, error) {
	matches, err := r.chooseIgnoredTargetMatches(query, "include> ", selectedPaths)
	if err != nil {
		return nil, err
	}
	return targetMatchPaths(matches), nil
}

func exactInteractiveTargetMatch(options []targetMatch, query string) (targetMatch, bool) {
	if !shouldAutoAcceptInteractiveQuery(query) {
		return targetMatch{}, false
	}
	return exactTargetPathMatch(options, query)
}

func exactTargetPathMatch(options []targetMatch, query string) (targetMatch, bool) {
	trimmed := strings.TrimSuffix(query, "/")
	want := normalizeRelPath(trimmed)
	if want == "" || want == "." {
		return targetMatch{}, false
	}
	for _, option := range options {
		if option.Kind == "all" {
			continue
		}
		if option.Path == want {
			if strings.HasSuffix(query, "/") && option.Kind != "dir" {
				continue
			}
			return option, true
		}
	}
	return targetMatch{}, false
}

func shouldAutoAcceptInteractiveQuery(query string) bool {
	trimmed := strings.TrimSuffix(query, "/")
	if trimmed == "" || trimmed == "." {
		return false
	}
	return strings.Contains(trimmed, "/")
}

func normalizeInteractivePickerQuery(query string) string {
	if strings.TrimSpace(query) == "*" {
		return ""
	}
	return query
}

func filterRedundantTargetMatches(candidates []targetMatch, selectedPaths []string) []targetMatch {
	if len(selectedPaths) == 0 {
		return candidates
	}
	filtered := make([]targetMatch, 0, len(candidates))
	for _, candidate := range candidates {
		if coveredBySelection(candidate.Path, selectedPaths) {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func coveredBySelection(path string, selectedPaths []string) bool {
	for _, selected := range selectedPaths {
		selected = normalizeRelPath(selected)
		switch {
		case selected == ".":
			return true
		case path == selected:
			return true
		case selected != "" && strings.HasPrefix(path, selected+"/"):
			return true
		}
	}
	return false
}

func selectionContainsAll(selectedPaths []string) bool {
	for _, selected := range selectedPaths {
		if normalizeRelPath(selected) == "." {
			return true
		}
	}
	return false
}

func (r *scopeResolver) allVisibleTargets() ([]targetMatch, error) {
	if r.interactiveTargetsOk {
		return append([]targetMatch(nil), r.interactiveTargets...), nil
	}
	if err := r.buildVisibleDirIndex(); err != nil {
		return nil, err
	}
	if err := r.buildVisibleFileList(); err != nil {
		return nil, err
	}

	targets := make([]targetMatch, 0, len(r.visibleDirs.dirs)+len(r.visibleFileList))
	for _, rel := range r.visibleDirs.dirs {
		targets = append(targets, targetMatch{Path: rel, Kind: "dir"})
	}
	for _, entry := range r.visibleFileList {
		targets = append(targets, targetMatch{Path: entry.RelPath, Kind: "file"})
	}

	r.interactiveTargets = targets
	r.interactiveTargetsOk = true
	return append([]targetMatch(nil), targets...), nil
}

func (r *scopeResolver) allIgnoredTargets() ([]targetMatch, error) {
	if r.ignoredTargetsOk {
		return append([]targetMatch(nil), r.ignoredTargets...), nil
	}

	dirPaths := make([]string, 0, 256)
	filePaths := make([]string, 0, 512)
	err := filepath.WalkDir(r.cfg.WorkingDir, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if current == r.cfg.WorkingDir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		rel, err := filepath.Rel(r.cfg.WorkingDir, current)
		if err != nil {
			return err
		}
		rel = normalizeRelPath(rel)

		if d.IsDir() {
			dirPaths = append(dirPaths, rel)
			return nil
		}

		info, err := os.Stat(current)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if excludedTextLikeAsset(rel) {
			return nil
		}
		text, err := r.classifyTextFile(rel, current)
		if err != nil {
			return err
		}
		if text {
			filePaths = append(filePaths, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	relPaths := make([]string, 0, len(dirPaths)+len(filePaths))
	relPaths = append(relPaths, dirPaths...)
	relPaths = append(relPaths, filePaths...)

	gitIgnored := map[string]gitIgnoreMatch{}
	if r.useGitIgnore {
		gitIgnored, err = collectGitIgnoreMatchesForRelPaths(r.gitCtx, relPaths)
		if err != nil {
			return nil, err
		}
	}

	targets := make([]targetMatch, 0, len(dirPaths)+len(filePaths))
	for _, rel := range dirPaths {
		match := targetMatch{Path: rel, Kind: "dir"}
		if ignored, _ := r.matcher.dirIgnored(rel); ignored {
			match.Ignored = true
			match.IgnoreSource = ".hiss"
		} else if _, ok := gitIgnored[rel]; ok {
			match.Ignored = true
			match.IgnoreSource = ".gitignore"
		}
		if match.Ignored {
			targets = append(targets, match)
		}
	}
	for _, rel := range filePaths {
		match := targetMatch{Path: rel, Kind: "file"}
		if ignored, _ := r.matcher.fileIgnored(rel); ignored {
			match.Ignored = true
			match.IgnoreSource = ".hiss"
		} else if _, ok := gitIgnored[rel]; ok {
			match.Ignored = true
			match.IgnoreSource = ".gitignore"
		}
		if match.Ignored {
			targets = append(targets, match)
		}
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return targets[i].Kind < targets[j].Kind
		}
		if targets[i].IgnoreSource != targets[j].IgnoreSource {
			return targets[i].IgnoreSource < targets[j].IgnoreSource
		}
		return targets[i].Path < targets[j].Path
	})

	r.ignoredTargets = targets
	r.ignoredTargetsOk = true
	return append([]targetMatch(nil), targets...), nil
}

func (r *scopeResolver) resolveTargetMatches(matches []targetMatch, colors colorPalette) ([]fileEntry, error) {
	entries := make([]fileEntry, 0, len(matches))
	for _, match := range matches {
		if match.Kind == "done" {
			continue
		}
		discovered, handled, _, err := r.resolveTargetMatch(match, colors)
		if err != nil {
			return nil, err
		}
		if handled {
			entries = append(entries, discovered...)
		}
	}
	return dedupeEntriesByPath(entries), nil
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
	if err := r.buildVisibleFileList(); err != nil {
		return err
	}

	dirSet := make(map[string]struct{}, len(r.visibleFileList))
	for _, entry := range r.visibleFileList {
		dir := path.Dir(entry.RelPath)
		for dir != "." && dir != "" {
			dirSet[dir] = struct{}{}
			dir = path.Dir(dir)
		}
	}

	dirs := make([]string, 0, len(dirSet))
	for rel := range dirSet {
		dirs = append(dirs, rel)
	}
	sort.Strings(dirs)

	r.visibleDirs = visibleDirIndex{
		dirs:        dirs,
		set:         make(map[string]struct{}, len(dirs)),
		symlinkDirs: nil,
	}
	for _, rel := range dirs {
		r.visibleDirs.set[rel] = struct{}{}
	}
	r.visibleDirsReady = true
	return nil
}

func (r *scopeResolver) buildVisibleFileIndex() error {
	if r.visibleFilesReady {
		return nil
	}
	if len(r.wantedBasenames) == 0 {
		r.visibleFiles = visibleFileIndex{
			byBase:        map[string][]fileEntry{},
			skippedByBase: map[string][]skippedMatch{},
		}
		r.visibleFilesReady = true
		return nil
	}

	paths, err := runRipgrepFiles(r.cfg.WorkingDir, ripgrepFileOptions{
		NoIgnore:  true,
		Basenames: sortedStringSet(r.wantedBasenames),
	})
	if err != nil {
		return err
	}
	candidates, err := r.textEntriesFromRipgrepPaths(paths, false)
	if err != nil {
		return err
	}

	gitIgnored := map[string]gitIgnoreMatch{}
	if r.useGitIgnore {
		gitIgnored, err = collectGitIgnoreMatches(r.gitCtx, candidates)
		if err != nil {
			return err
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RelPath < candidates[j].RelPath
	})

	byBase := make(map[string][]fileEntry, len(candidates))
	skippedByBase := make(map[string][]skippedMatch, len(candidates))
	for _, entry := range candidates {
		base := path.Base(entry.RelPath)
		if ignored, rule := r.matcher.dirRuleBlockingFile(entry.RelPath); ignored {
			skippedByBase[base] = append(skippedByBase[base], skippedMatch{
				RelPath:     entry.RelPath,
				BlockRule:   rule,
				BlockSource: ".hiss",
				BlockKind:   "directory",
			})
			continue
		}
		if gitMatch, ok := gitIgnored[entry.RelPath]; ok && gitMatch.DirRule {
			skippedByBase[base] = append(skippedByBase[base], skippedMatch{
				RelPath:     entry.RelPath,
				BlockRule:   gitMatch.Rule,
				BlockSource: ".gitignore",
				BlockKind:   "directory",
			})
			continue
		}
		if ignored, rule := r.matcher.fileIgnoredByFileRule(entry.RelPath); ignored {
			skippedByBase[base] = append(skippedByBase[base], skippedMatch{
				RelPath:     entry.RelPath,
				BlockRule:   rule,
				BlockSource: ".hiss",
				BlockKind:   "file",
			})
			continue
		}
		if gitMatch, ok := gitIgnored[entry.RelPath]; ok {
			skippedByBase[base] = append(skippedByBase[base], skippedMatch{
				RelPath:     entry.RelPath,
				BlockRule:   gitMatch.Rule,
				BlockSource: ".gitignore",
				BlockKind:   "file",
			})
			continue
		}
		entry.GitVisible = true
		byBase[base] = append(byBase[base], entry)
	}

	r.visibleFiles = visibleFileIndex{
		byBase:        byBase,
		skippedByBase: skippedByBase,
	}
	r.visibleFilesReady = true
	return nil
}

func (r *scopeResolver) buildVisibleFileList() error {
	if r.visibleFileListReady {
		return nil
	}
	paths, err := runRipgrepFiles(r.cfg.WorkingDir, ripgrepFileOptions{})
	if err != nil {
		return err
	}
	entries, err := r.textEntriesFromRipgrepPaths(paths, true)
	if err != nil {
		return err
	}
	entries = markEntriesGitVisible(entries)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelPath < entries[j].RelPath
	})
	r.visibleFileList = entries
	r.visibleFileListReady = true
	return nil
}

func (r *scopeResolver) textEntriesFromRipgrepPaths(relPaths []string, applyIgnore bool) ([]fileEntry, error) {
	entries := make([]fileEntry, 0, len(relPaths))
	for _, rel := range relPaths {
		rel = normalizeRelPath(rel)
		if rel == "" || rel == "." || coveredBySelection(rel, r.visibleDirs.symlinkDirs) {
			continue
		}
		if applyIgnore {
			if ignored, _ := r.matcher.fileIgnored(rel); ignored {
				continue
			}
		}
		if len(r.matcher.only) > 0 && !r.matcher.matchesOnly(rel) {
			continue
		}
		if excludedTextLikeAsset(rel) {
			continue
		}

		text, err := r.classifyTextFile(rel, "")
		if err != nil {
			return nil, err
		}
		if !text {
			continue
		}

		entries = append(entries, fileEntry{RelPath: rel})
	}
	return entries, nil
}

func (r *scopeResolver) discoverVisibleFilesUnder(rootRel string) ([]fileEntry, error) {
	rootRel = normalizeRelPath(rootRel)
	opts := ripgrepFileOptions{}
	if rootRel != "." && rootRel != "" {
		opts.Paths = []string{rootRel}
	}
	paths, err := runRipgrepFiles(r.cfg.WorkingDir, opts)
	if err != nil {
		return nil, err
	}
	entries, err := r.textEntriesFromRipgrepPaths(paths, true)
	if err != nil {
		return nil, err
	}
	return markEntriesGitVisible(entries), nil
}

func sortedStringSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	for value := range values {
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (r *scopeResolver) resolveVisibleFilesByBasename(baseRel, baseName string) ([]fileEntry, []skippedMatch, error) {
	if err := r.buildVisibleFileIndex(); err != nil {
		return nil, nil, err
	}

	candidates := ensureEntryAbsPaths(append([]fileEntry(nil), r.visibleFiles.byBase[baseName]...), r.cfg.WorkingDir)
	skipped := r.visibleFiles.skippedByBase[baseName]

	baseRel = normalizeRelPath(baseRel)
	if baseRel == "." || baseRel == "" {
		return candidates, append([]skippedMatch(nil), skipped...), nil
	}

	prefix := baseRel + "/"
	matches := make([]fileEntry, 0, len(candidates))
	for _, entry := range candidates {
		if strings.HasPrefix(entry.RelPath, prefix) {
			matches = append(matches, entry)
		}
	}
	blocked := make([]skippedMatch, 0, len(skipped))
	for _, match := range skipped {
		if strings.HasPrefix(match.RelPath, prefix) {
			blocked = append(blocked, match)
		}
	}
	return matches, blocked, nil
}

func (r *scopeResolver) lookupVisibleFilesByExactBasename(baseName string) ([]fileEntry, []skippedMatch, error) {
	clone := *r
	clone.wantedBasenames = map[string]struct{}{baseName: {}}
	clone.visibleFiles = visibleFileIndex{}
	clone.visibleFilesReady = false
	return clone.resolveVisibleFilesByBasename(".", baseName)
}

func collectGitIgnoredPaths(gitCtx gitContext, entries []fileEntry) (map[string]struct{}, error) {
	if !gitCtx.Enabled || len(entries) == 0 {
		return nil, nil
	}

	repoPaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		repoPaths = append(repoPaths, gitCtx.toRepoPath(entry.RelPath))
	}

	ignoredRepoPaths, err := runGitLines(gitCtx.Root, repoPaths, "check-ignore", "--stdin")
	if err != nil {
		return nil, err
	}
	ignored := make(map[string]struct{}, len(ignoredRepoPaths))
	for _, repoPath := range ignoredRepoPaths {
		workPath := gitCtx.toWorkPath(repoPath)
		if workPath == "" {
			workPath = normalizeRelPath(repoPath)
		}
		ignored[normalizeRelPath(workPath)] = struct{}{}
	}
	return ignored, nil
}

func collectGitIgnoreMatches(gitCtx gitContext, entries []fileEntry) (map[string]gitIgnoreMatch, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	relPaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		relPaths = append(relPaths, entry.RelPath)
	}
	return collectGitIgnoreMatchesForRelPaths(gitCtx, relPaths)
}

func collectGitIgnoreMatchesForRelPaths(gitCtx gitContext, relPaths []string) (map[string]gitIgnoreMatch, error) {
	if !gitCtx.Enabled || len(relPaths) == 0 {
		return nil, nil
	}

	repoPaths := make([]string, 0, len(relPaths))
	seen := make(map[string]struct{}, len(relPaths))
	for _, relPath := range relPaths {
		relPath = normalizeRelPath(relPath)
		if relPath == "" || relPath == "." {
			continue
		}
		repoPath := gitCtx.toRepoPath(relPath)
		if _, ok := seen[repoPath]; ok {
			continue
		}
		seen[repoPath] = struct{}{}
		repoPaths = append(repoPaths, repoPath)
	}
	if len(repoPaths) == 0 {
		return nil, nil
	}

	cmd := exec.Command("git", "check-ignore", "-v", "--stdin")
	cmd.Dir = gitCtx.Root
	cmd.Stdin = strings.NewReader(strings.Join(repoPaths, "\n") + "\n")
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

	matches := make(map[string]gitIgnoreMatch)
	for _, line := range strings.Split(text, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		meta := parts[0]
		repoPath := normalizeRelPath(parts[1])
		workPath := gitCtx.toWorkPath(repoPath)
		if workPath == "" {
			workPath = repoPath
		}

		metaParts := strings.SplitN(meta, ":", 3)
		if len(metaParts) != 3 {
			continue
		}
		rule := metaParts[2]
		matches[normalizeRelPath(workPath)] = gitIgnoreMatch{
			Rule:    rule,
			DirRule: strings.HasSuffix(rule, "/"),
		}
	}
	return matches, nil
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

	matches := make([]string, 0, 16)
	for _, rel := range r.visibleDirs.dirs {
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			continue
		}
		matches = append(matches, rel)
	}
	return fuzzyFilterCandidates(needle, matches)
}

func (r *scopeResolver) fuzzySearchFiles(baseRel, needle string) ([]string, error) {
	if err := r.buildVisibleFileList(); err != nil {
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

	candidates := make([]string, 0, len(r.visibleFileList))
	for _, entry := range r.visibleFileList {
		if prefix != "" && !strings.HasPrefix(entry.RelPath, prefix) {
			continue
		}
		candidates = append(candidates, entry.RelPath)
	}
	return fuzzyFilterCandidates(needle, candidates)
}

func (r *scopeResolver) fuzzySearchFilesUnder(baseRel, needle string, rootBypass *blockInfo) ([]string, error) {
	if rootBypass == nil {
		return r.fuzzySearchFiles(baseRel, needle)
	}

	rootAbs := filepath.Join(r.cfg.WorkingDir, filepath.FromSlash(baseRel))
	entries, err := discoverFilesUnder(r.cfg.WorkingDir, rootAbs, baseRel, r.matcher, r.classifyTextFile, rootBypass)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		candidates = append(candidates, entry.RelPath)
	}
	return fuzzyFilterCandidates(needle, candidates)
}

func chooseDirectoryMatch(cfg runConfig, needle, currentRel string, matches []string, stderr io.Writer, colors colorPalette) (string, error) {
	if cfg.HeadlessStdoutMode() || !canPromptInteractively() {
		if currentRel == "." {
			return "", fmt.Errorf("Error: Multiple directories match %s.\n  Use a more specific path segment to disambiguate.", singleQuoted(needle))
		}
		return "", fmt.Errorf("Error: Multiple directories match %s in %s.\n  Use a more specific path segment to disambiguate.", singleQuoted(needle), currentRel)
	}

	path, err := fuzzyResolverBinary()
	if err != nil {
		return "", err
	}
	return chooseWithFzf(path, needle, "dir> ", matches)
}

func chooseFileMatch(cfg runConfig, needle, currentRel string, matches []string, stderr io.Writer, colors colorPalette) (string, error) {
	if cfg.HeadlessStdoutMode() || !canPromptInteractively() {
		if currentRel == "." {
			return "", fmt.Errorf("Error: Multiple files match %s.\n  Use a more specific name or path to disambiguate.", singleQuoted(needle))
		}
		return "", fmt.Errorf("Error: Multiple files match %s in %s.\n  Use a more specific name or path to disambiguate.", singleQuoted(needle), currentRel)
	}

	path, err := fuzzyResolverBinary()
	if err != nil {
		return "", err
	}
	return chooseWithFzf(path, needle, "file> ", matches)
}

func chooseTargetMatch(cfg runConfig, needle string, matches []targetMatch, stderr io.Writer, colors colorPalette) (targetMatch, error) {
	if cfg.HeadlessStdoutMode() || !canPromptInteractively() {
		return targetMatch{}, fmt.Errorf("Error: Multiple files and directories match %s.\n  Use a more specific name or path to disambiguate.", singleQuoted(needle))
	}

	path, err := fuzzyResolverBinary()
	if err != nil {
		return targetMatch{}, err
	}
	labels, index := targetMatchLabels(matches)
	selected, err := chooseWithFzf(path, needle, "target> ", labels)
	if err != nil {
		return targetMatch{}, err
	}
	match, ok := index[selected]
	if !ok {
		return targetMatch{}, errSelectionCancelled
	}
	return match, nil
}

func fzfBinary() (string, bool) {
	return bundledToolBinary("CATCLIP_FZF", "fzf")
}

func fuzzyResolverBinary() (string, error) {
	path, ok := fzfBinary()
	if ok {
		return path, nil
	}
	return "", fmt.Errorf("Error: this catclip install is missing bundled fzf.\n  Reinstall catclip with its packaged tools; runtime does not fall back to arbitrary PATH copies.")
}

func fuzzyFilterCandidates(query string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	path, err := fuzzyResolverBinary()
	if err != nil {
		return nil, err
	}
	return runFzfFilter(path, query, candidates)
}

func runFzfFilter(bin, query string, candidates []string) ([]string, error) {
	cmd := exec.Command(bin, "--delimiter", "\t", "--nth", "1,2", "--filter", query)
	cmd.Stdin = strings.NewReader(strings.Join(formatFzfCandidates(candidates), "\n") + "\n")
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
	return parseFzfMatches(text), nil
}

func chooseWithFzf(bin, query, prompt string, candidates []string) (string, error) {
	cmd := exec.Command(bin, "--ansi", "--layout=default", "--info=inline-right", "--delimiter", "\t", "--with-nth", "1,2", "--query", query, "--prompt", prompt)
	cmd.Stdin = strings.NewReader(strings.Join(formatFzfCandidates(candidates), "\n") + "\n")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 130) {
			return "", errSelectionCancelled
		}
		return "", err
	}
	selected := strings.TrimSpace(string(out))
	if selected == "" {
		return "", errSelectionCancelled
	}
	lines := parseFzfMatches(selected)
	if len(lines) == 0 {
		return "", errSelectionCancelled
	}
	return lines[0], nil
}

func chooseManyWithFzf(bin, query, prompt string, candidates []string) ([]string, error) {
	return chooseManyWithFzfNth(bin, query, prompt, "1,2", candidates)
}

func chooseManyWithFzfNth(bin, query, prompt, nth string, candidates []string) ([]string, error) {
	return chooseManyWithFzfOptions(bin, query, prompt, nth, "", candidates)
}

func chooseManyWithFzfHeader(bin, query, prompt, header string, candidates []string) ([]string, error) {
	return chooseManyWithFzfOptions(bin, query, prompt, "1,2", header, candidates)
}

type fzfChooseResult struct {
	Query   string
	Key     string
	Matches []string
}

func chooseManyWithFzfControl(bin, query, prompt, nth, header string, candidates []string, expectedKeys ...string) (fzfChooseResult, error) {
	cmd := exec.Command(bin, "--ansi", "--layout=default", "--info=inline-right", "--multi", "--delimiter", "\t", "--with-nth", "1,2", "--query", query, "--prompt", prompt, "--print-query")
	if nth != "" {
		cmd.Args = append(cmd.Args, "--nth", nth)
	}
	if header != "" {
		cmd.Args = append(cmd.Args, "--header", header, "--header-border=rounded")
	}
	if len(expectedKeys) > 0 {
		cmd.Args = append(cmd.Args, "--expect", strings.Join(expectedKeys, ","))
	}
	cmd.Stdin = strings.NewReader(strings.Join(formatFzfCandidates(candidates), "\n") + "\n")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 130) {
			return fzfChooseResult{}, errSelectionCancelled
		}
		return fzfChooseResult{}, err
	}
	result := parseFzfChooseResult(string(out), expectedKeys)
	if result.Key == "" && len(result.Matches) == 0 {
		return fzfChooseResult{}, errSelectionCancelled
	}
	return result, nil
}

func chooseManyWithFzfOptions(bin, query, prompt, nth, header string, candidates []string) ([]string, error) {
	cmd := exec.Command(bin, "--ansi", "--layout=default", "--info=inline-right", "--multi", "--delimiter", "\t", "--with-nth", "1,2", "--query", query, "--prompt", prompt)
	if nth != "" {
		cmd.Args = append(cmd.Args, "--nth", nth)
	}
	if header != "" {
		cmd.Args = append(cmd.Args, "--header", header, "--header-border=rounded")
	}
	cmd.Stdin = strings.NewReader(strings.Join(formatFzfCandidates(candidates), "\n") + "\n")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 130) {
			return nil, errSelectionCancelled
		}
		return nil, err
	}
	selected := strings.TrimSpace(string(out))
	if selected == "" {
		return nil, errSelectionCancelled
	}
	lines := parseFzfMatches(selected)
	if len(lines) == 0 {
		return nil, errSelectionCancelled
	}
	return lines, nil
}

func parseFzfChooseResult(text string, expectedKeys []string) fzfChooseResult {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return fzfChooseResult{}
	}

	lines := strings.Split(text, "\n")
	result := fzfChooseResult{Query: lines[0]}
	lines = lines[1:]
	if len(lines) == 0 {
		return result
	}

	keySet := make(map[string]struct{}, len(expectedKeys))
	for _, key := range expectedKeys {
		keySet[key] = struct{}{}
	}
	if _, ok := keySet[lines[0]]; ok {
		result.Key = lines[0]
		lines = lines[1:]
	}
	if len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return result
	}
	result.Matches = parseFzfMatches(strings.Join(lines, "\n"))
	return result
}

func formatFzfCandidates(candidates []string) []string {
	lines := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		lines = append(lines, path.Base(candidate)+"\t"+candidate)
	}
	return lines
}

func parseFzfMatches(text string) []string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			out = append(out, parts[1])
			continue
		}
		out = append(out, line)
	}
	return out
}

func rankTargetMatches(query string, dirs, files []string) ([]targetMatch, error) {
	matches := make([]targetMatch, 0, len(dirs)+len(files))
	for _, dir := range dirs {
		matches = append(matches, targetMatch{Path: dir, Kind: "dir"})
	}
	for _, file := range files {
		matches = append(matches, targetMatch{Path: file, Kind: "file"})
	}
	if len(matches) == 0 {
		return nil, nil
	}
	path, err := fuzzyResolverBinary()
	if err != nil {
		return nil, err
	}
	labels, index := targetMatchLabels(matches)
	filtered, err := runFzfFilter(path, query, labels)
	if err != nil {
		return nil, err
	}
	ranked := make([]targetMatch, 0, len(filtered))
	for _, label := range filtered {
		match, ok := index[label]
		if ok {
			ranked = append(ranked, match)
		}
	}
	return ranked, nil
}

func targetMatchLabels(matches []targetMatch) ([]string, map[string]targetMatch) {
	labels := make([]string, 0, len(matches))
	index := make(map[string]targetMatch, len(matches))
	for _, match := range matches {
		label := fmt.Sprintf("[%s] %s", match.Kind, match.Path)
		if match.Kind == "all" {
			plain := "[copy all files]"
			label = "\x1b[1m" + plain + "\x1b[0m"
			index[plain] = match
		} else if match.Ignored {
			source := strings.TrimSpace(match.IgnoreSource)
			if source == "" {
				source = "ignored"
			}
			label = fmt.Sprintf("[ignored %s %s] %s", match.Kind, source, match.Path)
		}
		labels = append(labels, label)
		index[label] = match
	}
	return labels, index
}

func targetMatchKey(match targetMatch) string {
	return match.Kind + "\x00" + match.Path
}

func safeTargetPickerHeader() string {
	return "Type part of a directory or file name to filter the list.\nUse Up/Down arrow keys to move, Enter to confirm, Tab to multi-select, Ctrl-O to browse ignored targets."
}

func ignoredTargetPickerHeader() string {
	return "Type part of an ignored directory or file name to filter the list.\nUse Up/Down arrow keys to move, Enter to add as --include, Tab to multi-select, Esc to cancel."
}

func targetNotFoundWarning(target string, scopeIndex int, colors colorPalette) string {
	if strings.Contains(target, "/") {
		return fmt.Sprintf("%sWarning:%s Target %s not found (scope %d).\n\n  %sIf the parent directory is ignored, use --include to authorize it first.%s\n  %sExample:%s %scatclip --include %s --only %s%s",
			colors.Warn, colors.Reset, singleQuoted(target), scopeIndex+1,
			colors.Dim, colors.Reset,
			colors.Dim, colors.Reset,
			colors.OK, singleQuoted(path.Dir(target)), singleQuoted(path.Base(target)), colors.Reset)
	}
	if prefersDirectFileLookup(target) {
		return fmt.Sprintf("%sWarning:%s No file named %s found (scope %d).\n\n  %sDirect file targets use exact basenames first. Non-exact file shorthand is resolved by fzf across safe directories.%s\n\n  %sIf an ignored rule is hiding it, use --include to authorize the blocked file or directory first.%s",
			colors.Warn, colors.Reset, singleQuoted(target), scopeIndex+1,
			colors.Dim, colors.Reset,
			colors.Dim, colors.Reset)
	}
	return fmt.Sprintf("%sWarning:%s No file or directory %s found (scope %d).\n\n  %sDirectory shorthand is resolved by fzf. File targets use exact basenames first, then fzf across safe directories.%s\n\n  %sIf the thing you want is ignored, use --include to browse blocked targets for this scope.%s",
		colors.Warn, colors.Reset, singleQuoted(target), scopeIndex+1,
		colors.Dim, colors.Reset,
		colors.Dim, colors.Reset)
}

func ignoredDirMessage(relTarget, rule, source string, colors colorPalette) string {
	return fmt.Sprintf("\n%sError: %s is ignored by rule %s in %s%s\n\n  %sUse --include to authorize it for this run.%s\n  %sExample:%s %scatclip --include %s%s\n  %sTo narrow inside it:%s   %scatclip --include %s --only \"*.ext\"%s\n  %sTo remove permanently:%s   %scatclip --hiss%s %s(delete the rule)%s",
		colors.Err, singleQuoted(relTarget), singleQuoted(rule), source, colors.Reset,
		colors.Dim, colors.Reset,
		colors.Dim, colors.Reset, colors.OK, singleQuoted(relTarget), colors.Reset,
		colors.Dim, colors.Reset, colors.OK, singleQuoted(relTarget), colors.Reset,
		colors.Dim, colors.Reset, colors.OK, colors.Reset, colors.Dim, colors.Reset)
}

func ignoredFileMessage(relTarget, rule, source string, fromChained bool, colors colorPalette) string {
	message := fmt.Sprintf("\n%sError: %s is ignored by rule %s in %s%s\n\n  %sUse --include to authorize it for this run.%s\n  %sExample:%s %scatclip --include %s%s",
		colors.Err, singleQuoted(relTarget), singleQuoted(rule), source, colors.Reset,
		colors.Dim, colors.Reset,
		colors.Dim, colors.Reset, colors.OK, singleQuoted(relTarget), colors.Reset)
	if fromChained {
		return message
	}
	return message + fmt.Sprintf("\n  %sTo remove permanently:%s   %scatclip --hiss%s %s(delete the rule)%s",
		colors.Dim, colors.Reset, colors.OK, colors.Reset, colors.Dim, colors.Reset)
}

func ignoredTargetNeedsIncludeMessage(resolvedPath, query string, colors colorPalette) string {
	if normalizeRelPath(query) == normalizeRelPath(resolvedPath) {
		return fmt.Sprintf("\n%sError: %s is ignored.%s\n\n  %sUse --include to authorize it for this run.%s\n  %sExample:%s %scatclip --include %s%s",
			colors.Err, singleQuoted(resolvedPath), colors.Reset,
			colors.Dim, colors.Reset,
			colors.Dim, colors.Reset, colors.OK, singleQuoted(resolvedPath), colors.Reset)
	}
	return fmt.Sprintf("\n%sError: %s only matches ignored targets.%s\n\n  %sUse --include to browse blocked files and directories for this scope.%s\n  %sExample:%s %scatclip --include %s%s",
		colors.Err, singleQuoted(query), colors.Reset,
		colors.Dim, colors.Reset,
		colors.Dim, colors.Reset, colors.OK, singleQuoted(query), colors.Reset)
}

func includeQueryNeedsSelectionMessage(query string, colors colorPalette) string {
	return fmt.Sprintf("\n%sError: %s needs an ignored-target selection.%s\n\n  %sUse --include with an exact ignored path, or run it in a TTY so catclip can open the ignored picker.%s",
		colors.Err, singleQuoted(query), colors.Reset,
		colors.Dim, colors.Reset)
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

func prefersDirectFileLookup(target string) bool {
	base := path.Base(target)
	return looksLikeFileTarget(base) || strings.Contains(base, ".")
}

func withBypass(entry fileEntry, kind string, block blockInfo) fileEntry {
	entry.Bypassed = true
	entry.BypassKind = kind
	entry.BlockRule = block.Rule
	entry.BlockSource = block.Source
	return entry
}

func withTargetRoot(entries []fileEntry, targetRoot string) []fileEntry {
	targetRoot = normalizeRelPath(targetRoot)
	if targetRoot == "." || targetRoot == "" {
		return entries
	}
	for i := range entries {
		entries[i].TargetRoot = targetRoot
	}
	return entries
}

func markEntriesGitVisible(entries []fileEntry) []fileEntry {
	for i := range entries {
		entries[i].GitVisible = true
	}
	return entries
}

func collectWantedBasenames(targets []string) map[string]struct{} {
	wanted := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		normalized := normalizeRelPath(target)
		if normalized == "" || normalized == "." {
			continue
		}
		if !strings.Contains(normalized, "/") && !prefersDirectFileLookup(normalized) {
			continue
		}
		base := path.Base(normalized)
		if base == "" || base == "." {
			continue
		}
		wanted[base] = struct{}{}
	}
	return wanted
}

func formatSkippedMatchesWarning(matches []skippedMatch) []string {
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].RelPath < matches[j].RelPath
	})

	label := "matches"
	if len(matches) == 1 {
		label = "match"
	}
	lines := []string{fmt.Sprintf("Warning: %d %s skipped by ignore rules:", len(matches), label)}
	for _, match := range matches {
		rule := match.BlockRule
		if rule == "" {
			rule = match.BlockSource
		}
		lines = append(lines, fmt.Sprintf("  %s  [%s]", match.RelPath, rule))
	}
	return []string{strings.Join(lines, "\n")}
}

func singleQuoted(value string) string {
	return "'" + value + "'"
}

func writeNoFilesMatchedMessage(cfg runConfig, stderr io.Writer, colors colorPalette, hadSelectionCancel bool) error {
	if hadSelectionCancel {
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
	if _, err := fmt.Fprintf(stderr, "\n  %sTry: catclip --hiss                        # view/edit ignore rules%s\n", colors.Dim, colors.Reset); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stderr, "  %s     catclip --include blocked-dir        # browse blocked dirs/files for this run%s\n", colors.Dim, colors.Reset)
	return err
}
