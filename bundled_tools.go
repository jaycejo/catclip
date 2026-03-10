package catclip

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Packaged installs are expected to carry private rg/fzf binaries under
// share/catclip/bin. Env overrides remain available for tests and developer
// runs, but runtime should not silently fall back to arbitrary PATH copies.
func bundledToolBinary(envVar, toolName string) (string, bool) {
	if override := strings.TrimSpace(os.Getenv(envVar)); override != "" {
		if strings.ContainsRune(override, os.PathSeparator) {
			if info, err := os.Stat(override); err == nil && !info.IsDir() {
				return override, true
			}
			return "", false
		}
		path, err := exec.LookPath(override)
		return path, err == nil
	}

	for _, candidate := range bundledToolCandidates(toolName) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func bundledToolCandidates(toolName string) []string {
	name := platformToolBinaryName(toolName)
	var candidates []string
	for _, dir := range executableCandidateDirs() {
		candidates = append(candidates,
			filepath.Join(dir, name),
			filepath.Join(dir, "bin", name),
			filepath.Join(dir, "..", "share", "catclip", "bin", name),
		)
	}
	return dedupePreserveOrder(candidates)
}

func executableCandidateDirs() []string {
	if exe, err := os.Executable(); err == nil {
		dirs := []string{filepath.Dir(exe)}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			dirs = append(dirs, filepath.Dir(resolved))
		}
		return dedupePreserveOrder(dirs)
	}
	return nil
}

func platformToolBinaryName(toolName string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(toolName), ".exe") {
		return toolName + ".exe"
	}
	return toolName
}
