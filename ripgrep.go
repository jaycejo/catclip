package catclip

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var errRipgrepUnavailable = errors.New("Error: this catclip install is missing bundled ripgrep.\n  Reinstall catclip with its packaged tools; runtime does not fall back to arbitrary PATH copies.")

type ripgrepFileOptions struct {
	NoIgnore  bool
	Basenames []string
	Paths     []string
}

func ripgrepBinary() (string, bool) {
	return bundledToolBinary("CATCLIP_RG", "rg")
}

func runRipgrepFiles(workingDir string, opts ripgrepFileOptions) ([]string, error) {
	bin, ok := ripgrepBinary()
	if !ok {
		return nil, errRipgrepUnavailable
	}

	// Symlinks are intentionally excluded for now, so keep rg on its default
	// non-following behavior and avoid pulling link paths into candidate lists.
	args := []string{"--files", "--hidden", "-0"}
	if opts.NoIgnore {
		args = append(args, "--no-ignore")
	}
	for _, base := range opts.Basenames {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		args = append(args, "-g", base)
	}
	pathArgs := make([]string, 0, len(opts.Paths))
	for _, rel := range opts.Paths {
		rel = normalizeRelPath(rel)
		if rel == "" || rel == "." {
			continue
		}
		pathArgs = append(pathArgs, rel)
	}
	if len(pathArgs) > 0 {
		args = append(args, "--")
		args = append(args, pathArgs...)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = workingDir
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	paths := splitNullSeparated(out)
	for i, rel := range paths {
		paths[i] = normalizeRelPath(rel)
	}
	sort.Strings(paths)
	return dedupeSortedStrings(paths), nil
}

func runRipgrepMatches(pattern string, absPaths []string) (map[string]struct{}, error) {
	bin, ok := ripgrepBinary()
	if !ok {
		return nil, errRipgrepUnavailable
	}
	if len(absPaths) == 0 {
		return map[string]struct{}{}, nil
	}

	matches := make(map[string]struct{}, len(absPaths))
	for _, chunk := range chunkExecArgs(absPaths, 256, 60*1024) {
		args := []string{"--color=never", "--no-messages", "--files-with-matches", "-0", "-m", "1", "-e", pattern, "--"}
		args = append(args, chunk...)

		cmd := exec.Command(bin, args...)
		out, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue
			}
			return nil, err
		}

		for _, match := range splitNullSeparated(out) {
			match = filepath.Clean(match)
			if match != "" {
				matches[match] = struct{}{}
			}
		}
	}
	return matches, nil
}

func splitNullSeparated(data []byte) []string {
	if len(data) == 0 {
		return nil
	}

	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		out = append(out, string(part))
	}
	return out
}

func chunkExecArgs(paths []string, maxCount, maxBytes int) [][]string {
	if len(paths) == 0 {
		return nil
	}

	chunks := make([][]string, 0, (len(paths)+maxCount-1)/maxCount)
	current := make([]string, 0, maxCount)
	currentBytes := 0

	flush := func() {
		if len(current) == 0 {
			return
		}
		chunk := make([]string, len(current))
		copy(chunk, current)
		chunks = append(chunks, chunk)
		current = current[:0]
		currentBytes = 0
	}

	for _, path := range paths {
		size := len(path) + 1
		if len(current) > 0 && (len(current) >= maxCount || currentBytes+size > maxBytes) {
			flush()
		}
		current = append(current, path)
		currentBytes += size
	}
	flush()
	return chunks
}
