package catclip

import (
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

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

func filterGitIgnoredEntries(gitCtx gitContext, entries []fileEntry) ([]fileEntry, error) {
	if len(entries) == 0 {
		return entries, nil
	}

	pending := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Bypassed || entry.GitVisible {
			continue
		}
		pending = append(pending, entry)
	}
	if len(pending) == 0 {
		return entries, nil
	}

	ignored, err := collectGitIgnoredPaths(gitCtx, pending)
	if err != nil {
		return nil, err
	}
	if len(ignored) == 0 {
		return entries, nil
	}

	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := ignored[entry.RelPath]; ok && !entry.Bypassed {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func filterChangedEntries(gitCtx gitContext, s scope, entries []fileEntry) ([]fileEntry, error) {
	changedRepoPaths, err := collectChangedRepoPaths(gitCtx, s)
	if err != nil {
		return nil, err
	}

	changed := make(map[string]struct{}, len(changedRepoPaths))
	for _, repoPath := range changedRepoPaths {
		changed[normalizeRelPath(repoPath)] = struct{}{}
	}

	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := changed[normalizeRelPath(gitCtx.toRepoPath(entry.RelPath))]; ok {
			out = append(out, entry)
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
