package catclip

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"unicode/utf8"
)

func filterEntriesByContent(entries []fileEntry, pattern string) ([]fileEntry, error) {
	if _, err := compileContainsPattern(pattern); err != nil {
		return nil, err
	}

	matched, err := runRipgrepMatches(pattern, entryAbsPaths(entries))
	if err != nil {
		return nil, err
	}

	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := matched[filepath.Clean(entry.AbsPath)]; ok {
			out = append(out, entry)
		}
	}
	return out, nil
}

func entryAbsPaths(entries []fileEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.AbsPath == "" {
			continue
		}
		paths = append(paths, filepath.Clean(entry.AbsPath))
	}
	return paths
}

func ensureEntryAbsPaths(entries []fileEntry, workingDir string) []fileEntry {
	for i := range entries {
		if entries[i].AbsPath != "" {
			continue
		}
		entries[i].AbsPath = filepath.Join(workingDir, filepath.FromSlash(entries[i].RelPath))
	}
	return entries
}

func isLikelyTextFile(relPath, absPath string) (bool, error) {
	if knownTextLikeFile(relPath) {
		return true, nil
	}
	return isProbablyTextFile(absPath)
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
