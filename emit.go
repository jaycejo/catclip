package catclip

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	fileCloseTag            = []byte("</file>\n\n")
	fileCloseTagWithNewline = []byte("\n</file>\n\n")
)

type emitStats struct {
	PayloadBytes          int64
	GenerateDuration      time.Duration
	SinkFinalizeDuration  time.Duration
	ClipboardWaitDuration time.Duration
	SinkName              string
}

type countingWriter struct {
	w io.Writer
	n int64
}

type emitPrefetchResult struct {
	index      int
	data       []byte
	prefetched bool
	err        error
}

type emitPrefetcher struct {
	done      chan struct{}
	results   chan emitPrefetchResult
	pending   map[int]emitPrefetchResult
	closeOnce sync.Once
	closed    chan struct{}
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// emitFullOutput streams every selected entry in the requested payload format,
// either to stdout or through the platform clipboard command.
func emitFullOutput(cfg runConfig, gitCtx gitContext, entries []fileEntry, stdout io.Writer, colors colorPalette) (emitStats, error) {
	return withPayloadWriter(cfg, stdout, colors, func(w io.Writer) error {
		prefetcher := startEmitPrefetch(entries)
		if prefetcher != nil {
			defer prefetcher.Close()
		}
		for i, entry := range entries {
			if err := emitEntry(w, gitCtx, entry, i, prefetcher); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitEntry(w io.Writer, gitCtx gitContext, entry fileEntry, index int, prefetcher *emitPrefetcher) error {
	switch entry.Mode {
	case entryModeDiff:
		return emitDiffEntry(w, gitCtx, entry)
	case entryModeSnippet:
		return emitSnippetEntry(w, entry)
	default:
		return emitFile(w, entry, index, prefetcher)
	}
}

func emitFile(w io.Writer, entry fileEntry, index int, prefetcher *emitPrefetcher) error {
	if prefetcher != nil {
		result, err := prefetcher.Wait(index)
		if err != nil {
			return err
		}
		if result.err != nil {
			return result.err
		}
		if result.prefetched {
			return emitWrappedFile(w, entry.RelPath, "", result.data)
		}
	}
	return emitFileFromDisk(w, entry.RelPath, "", entry.AbsPath)
}

func emitFileFromDisk(w io.Writer, relPath, typeAttr, absPath string) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	return emitWrappedReader(w, relPath, typeAttr, f)
}

func emitWrappedFile(w io.Writer, relPath, typeAttr string, data []byte) error {
	return emitWrappedReader(w, relPath, typeAttr, bytes.NewReader(data))
}

func emitWrappedReader(w io.Writer, relPath, typeAttr string, r io.Reader) error {
	if _, err := w.Write(buildFileOpenTag(relPath, typeAttr)); err != nil {
		return err
	}
	readBufSize := readBufferSize()
	var (
		buf      = make([]byte, readBufSize)
		lastByte byte
		wroteAny bool
	)
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			wroteAny = true
			lastByte = buf[n-1]
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("failed while writing %s: %w", relPath, writeErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed while streaming %s: %w", relPath, err)
		}
	}
	if wroteAny && lastByte != '\n' {
		_, err := w.Write(fileCloseTagWithNewline)
		return err
	}
	_, err := w.Write(fileCloseTag)
	return err
}

func startEmitPrefetch(entries []fileEntry) *emitPrefetcher {
	workers := emitReadWorkerCount()
	capBytes := emitPrefetchFileCap()
	if workers <= 1 || capBytes <= 0 {
		return nil
	}

	type job struct {
		index int
		entry fileEntry
	}

	done := make(chan struct{})
	jobs := make(chan job, workers)
	results := make(chan emitPrefetchResult, workers)
	closed := make(chan struct{})

	var workerWG sync.WaitGroup
	workerWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workerWG.Done()
			for current := range jobs {
				data, prefetched, err := readPrefetchCandidate(current.entry, capBytes)
				result := emitPrefetchResult{
					index:      current.index,
					data:       data,
					prefetched: prefetched,
					err:        err,
				}
				select {
				case results <- result:
				case <-done:
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i, entry := range entries {
			if !entryUsesFullOutput(entry) {
				continue
			}
			select {
			case jobs <- job{index: i, entry: entry}:
			case <-done:
				return
			}
		}
	}()

	go func() {
		workerWG.Wait()
		close(results)
		close(closed)
	}()

	return &emitPrefetcher{
		done:    done,
		results: results,
		pending: make(map[int]emitPrefetchResult, workers),
		closed:  closed,
	}
}

func (p *emitPrefetcher) Wait(index int) (emitPrefetchResult, error) {
	if p == nil {
		return emitPrefetchResult{}, nil
	}
	if result, ok := p.pending[index]; ok {
		delete(p.pending, index)
		return result, nil
	}
	for {
		result, ok := <-p.results
		if !ok {
			return emitPrefetchResult{}, fmt.Errorf("prefetch pipeline closed before result %d", index)
		}
		if result.index == index {
			return result, nil
		}
		p.pending[result.index] = result
	}
}

func (p *emitPrefetcher) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.done)
	})
	<-p.closed
}

func readPrefetchCandidate(entry fileEntry, capBytes int64) ([]byte, bool, error) {
	info, err := os.Stat(entry.AbsPath)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if size := info.Size(); size < 0 || size > capBytes {
		return nil, false, nil
	}

	data, err := os.ReadFile(entry.AbsPath)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func entryUsesFullOutput(entry fileEntry) bool {
	return entry.Mode == "" || entry.Mode == entryModeFull
}

func buildFileOpenTag(relPath, typeAttr string) []byte {
	if typeAttr == "" {
		tag := make([]byte, 0, len(relPath)+16)
		tag = append(tag, "<file path=\""...)
		tag = append(tag, relPath...)
		tag = append(tag, "\">\n"...)
		return tag
	}

	tag := make([]byte, 0, len(relPath)+len(typeAttr)+25)
	tag = append(tag, "<file path=\""...)
	tag = append(tag, relPath...)
	tag = append(tag, "\" type=\""...)
	tag = append(tag, typeAttr...)
	tag = append(tag, "\">\n"...)
	return tag
}

type snippetRange struct {
	Start int
	End   int
}

// emitSnippetEntry writes only the blank-line-bounded blocks that matched the
// scope's --contains pattern.
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

// emitDiffEntry emits git patches for tracked files and falls back to full
// content for untracked files, matching the shell tool's diff UX.
func emitDiffEntry(w io.Writer, gitCtx gitContext, entry fileEntry) error {
	if !gitCtx.Enabled {
		return emitFileFromDisk(w, entry.RelPath, "untracked", entry.AbsPath)
	}

	repoPath := gitCtx.toRepoPath(entry.RelPath)
	trackedOutput, err := runGitCapture(gitCtx.Root, "ls-files", "--", repoPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(trackedOutput) == "" {
		// Untracked files have no unified diff. The shell falls back to full file
		// content tagged as type="untracked", and the rewrite preserves that UX.
		return emitFileFromDisk(w, entry.RelPath, "untracked", entry.AbsPath)
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

func withPayloadWriter(cfg runConfig, stdout io.Writer, colors colorPalette, fn func(io.Writer) error) (emitStats, error) {
	bufferSize := outputBufferSize()

	if cfg.OutputMode == outputModeStdout {
		counted := &countingWriter{w: stdout}
		buffered := bufio.NewWriterSize(counted, bufferSize)
		generateStarted := time.Now()
		if err := fn(buffered); err != nil {
			return emitStats{}, err
		}
		generateDuration := time.Since(generateStarted)
		finalizeStarted := time.Now()
		if err := buffered.Flush(); err != nil {
			return emitStats{}, err
		}
		return emitStats{
			PayloadBytes:         counted.n,
			GenerateDuration:     generateDuration,
			SinkFinalizeDuration: time.Since(finalizeStarted),
			SinkName:             "stdout",
		}, nil
	}

	// Clipboard mode streams directly into the platform tool. That keeps the Go
	// path closer to the shell's "generate once, send to sink" behavior and
	// avoids buffering a second full copy of the payload in memory.
	cmd, err := clipboardCommand(cfg.Platform, colors)
	if err != nil {
		return emitStats{}, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return emitStats{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return emitStats{}, err
	}

	counted := &countingWriter{w: stdin}
	buffered := bufio.NewWriterSize(counted, bufferSize)
	generateStarted := time.Now()
	writeErr := fn(buffered)
	generateDuration := time.Since(generateStarted)
	finalizeStarted := time.Now()
	flushErr := buffered.Flush()
	closeErr := stdin.Close()
	finalizeDuration := time.Since(finalizeStarted)
	waitStarted := time.Now()
	waitErr := cmd.Wait()
	waitDuration := time.Since(waitStarted)

	if writeErr != nil {
		return emitStats{}, writeErr
	}
	if flushErr != nil {
		return emitStats{}, flushErr
	}
	if closeErr != nil {
		return emitStats{}, closeErr
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return emitStats{}, fmt.Errorf("Error: clipboard command failed: %s", msg)
		}
		return emitStats{}, fmt.Errorf("Error: clipboard command failed: %w", waitErr)
	}
	return emitStats{
		PayloadBytes:          counted.n,
		GenerateDuration:      generateDuration,
		SinkFinalizeDuration:  finalizeDuration,
		ClipboardWaitDuration: waitDuration,
		SinkName:              filepath.Base(cmd.Path),
	}, nil
}

func outputBufferSize() int {
	const defaultSize = 64 * 1024

	raw := strings.TrimSpace(os.Getenv("CATCLIP_OUTPUT_BUFFER_KIB"))
	if raw == "" {
		return defaultSize
	}
	kib, err := strconv.Atoi(raw)
	if err != nil || kib <= 0 {
		return defaultSize
	}
	return kib * 1024
}

func readBufferSize() int {
	const defaultSize = 32 * 1024

	raw := strings.TrimSpace(os.Getenv("CATCLIP_READ_BUFFER_KIB"))
	if raw == "" {
		return defaultSize
	}
	kib, err := strconv.Atoi(raw)
	if err != nil || kib <= 0 {
		return defaultSize
	}
	return kib * 1024
}

func emitReadWorkerCount() int {
	const defaultWorkers = 2

	raw := strings.TrimSpace(os.Getenv("CATCLIP_READ_WORKERS"))
	if raw == "" {
		return defaultWorkers
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count <= 0 {
		return defaultWorkers
	}
	return count
}

func emitPrefetchFileCap() int64 {
	const defaultSize = 4 * 1024 * 1024

	raw := strings.TrimSpace(os.Getenv("CATCLIP_PREFETCH_FILE_KIB"))
	if raw == "" {
		return defaultSize
	}
	kib, err := strconv.Atoi(raw)
	if err != nil || kib <= 0 {
		return defaultSize
	}
	return int64(kib) * 1024
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
