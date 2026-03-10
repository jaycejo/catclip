package catclip

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

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

// buildScopeMatcher compiles global ignore rules plus per-scope modifiers into
// the matcher used during discovery and direct-target checks.
func buildScopeMatcher(baseRules []ignoreRule, s scope) (scopeMatcher, error) {
	// Match the current flow: start from global .hiss rules, then append
	// additional rules from --exclude for this scope.
	rules := append([]ignoreRule(nil), baseRules...)
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
			compiled.raw = rule.Raw
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
	return matcher, nil
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
	if m.matchesOnly(relPath) {
		return true
	}
	if ignored, _ := m.fileIgnored(relPath); ignored {
		return false
	}
	return len(m.only) == 0
}

func (m scopeMatcher) fileIgnoredByFileRule(relPath string) (bool, string) {
	basename := path.Base(relPath)
	for _, rule := range m.ignoreFiles {
		if rule.re.MatchString(basename) || rule.re.MatchString(relPath) {
			return true, rule.raw
		}
	}
	return false, ""
}

func (m scopeMatcher) dirRuleBlockingFile(relPath string) (bool, string) {
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

func (m scopeMatcher) fileIgnored(relPath string) (bool, string) {
	if ignored, rule := m.fileIgnoredByFileRule(relPath); ignored {
		return true, rule
	}
	if ignored, rule := m.dirRuleBlockingFile(relPath); ignored {
		return true, rule
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
		return false
	}
	basename := path.Base(relPath)
	for _, rule := range m.only {
		if rule.re.MatchString(basename) || rule.re.MatchString(relPath) {
			return true
		}
	}
	return false
}
