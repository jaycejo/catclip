package catclip

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func buildOutputReport(cfg runConfig, gitCtx gitContext, entries []fileEntry, notices []string) (outputReport, error) {
	sizes, totalBytes, err := collectFileSizes(entries)
	if err != nil {
		return outputReport{}, err
	}

	report := outputReport{
		sizes:   sizes,
		notices: append(buildBypassNotices(entries), notices...),
	}

	if needsTreeRender(cfg) {
		if gitCtx.Enabled {
			report.statuses, err = collectGitStatusMap(gitCtx, entries)
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

func needsTreeRender(cfg runConfig) bool {
	if cfg.NoTree {
		return false
	}
	if cfg.Preview {
		return true
	}
	return !cfg.Quiet
}

// renderPreview writes the user-facing preview path: filter summary, notices,
// optional tree, and the final size/token summary.
func renderPreview(cfg runConfig, gitCtx gitContext, entries []fileEntry, report outputReport, stdout, stderr io.Writer, colors colorPalette) error {
	if !cfg.Quiet {
		if err := writeFilterSummary(stderr, cfg, gitCtx, colors); err != nil {
			return err
		}
		if err := writeReportNotices(stderr, report, colors); err != nil {
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
		if err := writeReportNotices(stderr, report, colors); err != nil {
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

func writeReportNotices(w io.Writer, report outputReport, colors colorPalette) error {
	for _, notice := range report.notices {
		if _, err := fmt.Fprintf(w, "  %s%s%s\n", colors.Warn, notice, colors.Reset); err != nil {
			return err
		}
	}
	return nil
}

func buildBypassNotices(entries []fileEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	notices := make([]string, 0, 4)
	for _, entry := range entries {
		if !entry.Bypassed {
			continue
		}
		key := entry.BypassKind + "\x00" + entry.BlockSource + "\x00" + entry.BlockRule
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		rule := entry.BlockRule
		if rule == "" {
			rule = entry.BlockSource
		}
		reason := "including because directly targeted"
		if entry.BypassKind != "" && entry.BypassKind != "direct" {
			reason = "including because specifically targeted"
		}
		if entry.BlockSource != "" {
			notices = append(notices, fmt.Sprintf("Warning: bypassing ignore rule %s from %s - %s", singleQuoted(rule), entry.BlockSource, reason))
			continue
		}
		notices = append(notices, fmt.Sprintf("Warning: bypassing ignore rule %s - %s", singleQuoted(rule), reason))
	}
	return notices
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
		// Lstat keeps preview accounting faithful to the on-disk entry size.
		// Symlinks are currently excluded before preview generation, so this is
		// effectively regular-file size accounting today.
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

func collectGitStatusMap(gitCtx gitContext, entries []fileEntry) (map[string]string, error) {
	pathspecs := previewGitStatusPathspecs(gitCtx, entries)
	out, err := collectGitStatusOutput(gitCtx, pathspecs)
	if err != nil {
		if len(pathspecs) > 0 {
			out, err = collectGitStatusOutput(gitCtx, nil)
		}
		if err != nil {
			return nil, err
		}
	}
	return parseGitStatusMap(gitCtx, string(out)), nil
}

func collectGitStatusOutput(gitCtx gitContext, pathspecs []string) ([]byte, error) {
	args := []string{"status", "--porcelain"}
	if len(pathspecs) > 0 && canScopePreviewGitStatus(pathspecs) {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = gitCtx.Root
	return cmd.Output()
}

func canScopePreviewGitStatus(pathspecs []string) bool {
	if len(pathspecs) == 0 {
		return false
	}
	if len(pathspecs) > 256 {
		return false
	}
	total := 0
	for _, pathspec := range pathspecs {
		total += len(pathspec) + 1
	}
	return total <= 32768
}

func parseGitStatusMap(gitCtx gitContext, output string) map[string]string {
	statuses := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(output), "\n")
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

		if xy == "??" {
			statuses[workPath] = "?"
			continue
		}

		staged := len(xy) >= 1 && xy[0] != ' ' && xy[0] != '?'
		unstaged := len(xy) >= 2 && xy[1] != ' ' && xy[1] != '?'
		switch {
		case staged && unstaged:
			statuses[workPath] = "SM"
		case staged:
			statuses[workPath] = "S"
		case unstaged:
			statuses[workPath] = "M"
		}
	}
	return statuses
}

func previewGitStatusPathspecs(gitCtx gitContext, entries []fileEntry) []string {
	set := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		repoPath := ""
		if entry.TargetRoot != "" && entry.TargetRoot != "." {
			repoPath = gitCtx.toRepoPath(entry.TargetRoot)
		} else {
			repoPath = gitCtx.toRepoPath(entry.RelPath)
		}
		repoPath = normalizeRelPath(repoPath)
		if repoPath == "" || repoPath == "." {
			continue
		}
		set[repoPath] = struct{}{}
	}

	pathspecs := make([]string, 0, len(set))
	for repoPath := range set {
		pathspecs = append(pathspecs, repoPath)
	}
	sort.Strings(pathspecs)
	return pathspecs
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

// printPreviewTree renders the compact directory tree shown before clipboard or
// stdout emission, including bypass coloring and target path hints.
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
			targetHint := shouldShowTargetPathHint(entry.TargetRoot, accum)
			if i > 0 && (targetHint || landmarks[accum] || lineCount >= 24) {
				label += " " + colors.Dim + "(" + accum + "/)" + colors.Reset
				lineCount = 0
			}
			dirColor := colors.Dir
			if bypassesDirectoryLabel(entry, accum) {
				dirColor = colors.Err
			}
			if _, err := fmt.Fprintf(w, "%s%s%s%s\n", prefix, dirColor, label, colors.Reset); err != nil {
				return err
			}
			lineCount++
		}

		filePrefix := treeIndent(fileIndex, colors)
		if fileIndex == 0 {
			filePrefix = colors.Tree + "├── " + colors.Reset
		}
		nameColor := ""
		nameReset := ""
		if entry.Bypassed {
			nameColor = colors.Err
			nameReset = colors.Reset
		}
		fileLine := filePrefix + nameColor + parts[fileIndex] + nameReset
		if size, ok := sizes[entry.RelPath]; ok {
			if entry.Bypassed {
				fileLine += " " + colors.Err + "(" + formatInlineSize(size) + ")" + colors.Reset
			} else {
				fileLine += " " + styleSize(formatInlineSize(size), size, colors)
			}
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

func shouldShowTargetPathHint(targetRoot, accum string) bool {
	targetRoot = normalizeRelPath(targetRoot)
	if targetRoot == "." || targetRoot == "" {
		return false
	}
	if !strings.Contains(targetRoot, "/") {
		return false
	}
	return targetRoot == accum
}

func bypassesDirectoryLabel(entry fileEntry, relDir string) bool {
	if !entry.Bypassed {
		return false
	}
	targetRoot := normalizeRelPath(entry.TargetRoot)
	if targetRoot != "" && targetRoot != "." && targetRoot != entry.RelPath {
		if relDir == targetRoot || strings.HasPrefix(relDir, targetRoot+"/") {
			return true
		}
	}
	if entry.BlockRule == "" || !strings.HasSuffix(entry.BlockRule, "/") {
		return false
	}
	ruleName := path.Base(strings.TrimSuffix(entry.BlockRule, "/"))
	return path.Base(relDir) == ruleName
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
	case "SM":
		return colors.Warn + "[SM]" + colors.Reset
	case "M":
		return colors.Warn + "[M]" + colors.Reset
	case "S":
		return colors.OK + "[S]" + colors.Reset
	case "?":
		return colors.Git + "[?]" + colors.Reset
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
