package catclip

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type textClassifier func(relPath, absPath string) (bool, error)

func discoverFilesUnder(workingDir, rootAbs, rootRel string, matcher scopeMatcher, classifyText textClassifier, rootBypass *blockInfo) ([]fileEntry, error) {
	rootRel = normalizeRelPath(rootRel)
	var files []fileEntry
	err := filepath.WalkDir(rootAbs, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		rel, err := filepath.Rel(workingDir, current)
		if err != nil {
			return err
		}
		rel = normalizeRelPath(rel)

		if d.IsDir() {
			if rootBypass == nil && rel != rootRel {
				if ignored, _ := matcher.dirIgnored(rel); ignored {
					return fs.SkipDir
				}
			}
			return nil
		}
		info, err := os.Stat(current)
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		entry := fileEntry{
			AbsPath: current,
			RelPath: rel,
		}

		if rootBypass == nil {
			if ignored, _ := matcher.fileIgnored(rel); ignored {
				return nil
			}
		}
		if len(matcher.only) > 0 && !matcher.matchesOnly(rel) {
			return nil
		}

		if rootBypass != nil {
			entry = withBypass(entry, "direct", *rootBypass)
		}
		if excludedTextLikeAsset(rel) {
			return nil
		}

		text, err := classifyText(rel, current)
		if err != nil {
			return err
		}
		if text {
			files = append(files, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func discoverFilesByBasenameUnder(workingDir, rootAbs, rootRel, baseName string, matcher scopeMatcher, classifyText textClassifier, rootBypass *blockInfo) ([]fileEntry, error) {
	rootRel = normalizeRelPath(rootRel)
	var files []fileEntry
	err := filepath.WalkDir(rootAbs, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		rel, err := filepath.Rel(workingDir, current)
		if err != nil {
			return err
		}
		rel = normalizeRelPath(rel)

		if d.IsDir() {
			if rootBypass == nil && rel != rootRel {
				if ignored, _ := matcher.dirIgnored(rel); ignored {
					return fs.SkipDir
				}
			}
			return nil
		}
		if path.Base(rel) != baseName {
			return nil
		}

		info, err := os.Stat(current)
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if rootBypass == nil {
			if ignored, _ := matcher.fileIgnored(rel); ignored {
				return nil
			}
		}
		if len(matcher.only) > 0 && !matcher.matchesOnly(rel) {
			return nil
		}
		if excludedTextLikeAsset(rel) {
			return nil
		}

		text, err := classifyText(rel, current)
		if err != nil {
			return err
		}
		if !text {
			return nil
		}

		entry := fileEntry{
			AbsPath: current,
			RelPath: rel,
		}
		if rootBypass != nil {
			entry = withBypass(entry, "direct", *rootBypass)
		}
		files = append(files, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func excludedTextLikeAsset(relPath string) bool {
	if _, blocked := knownBinaryBasenames[strings.ToLower(path.Base(relPath))]; blocked {
		return true
	}
	_, blocked := knownBinaryExts[shellStyleExtension(relPath)]
	return blocked
}

func knownTextLikeFile(relPath string) bool {
	if _, ok := knownTextBasenames[strings.ToLower(path.Base(relPath))]; ok {
		return true
	}
	_, ok := knownTextExts[shellStyleExtension(relPath)]
	return ok
}

func shellStyleExtension(relPath string) string {
	base := strings.ToLower(path.Base(relPath))
	lastDot := strings.LastIndexByte(base, '.')
	if lastDot <= 0 || lastDot == len(base)-1 {
		return ""
	}
	return base[lastDot+1:]
}

var knownBinaryBasenames = map[string]struct{}{
	".ds_store":              {},
	"thumbs.db":              {},
	"bad-nonprintable.bconf": {},
}

var knownTextBasenames = map[string]struct{}{
	"makefile": {}, "gemfile": {}, "rakefile": {}, "guardfile": {}, "vagrantfile": {}, "berksfile": {}, "capfile": {},
	"dockerfile": {}, "containerfile": {}, "jenkinsfile": {}, "procfile": {},
	"cmakelists.txt": {}, "configure": {}, "configure.ac": {},
	".gitignore": {}, ".gitattributes": {}, ".gitmodules": {}, ".gitkeep": {}, ".keep": {},
	".git-blame-ignore-revs": {},
	".dockerignore":          {}, ".helmignore": {}, ".slugignore": {},
	".vscodeignore": {}, ".npmignore": {}, ".eslintignore": {},
	".editorconfig": {}, ".eslintrc": {}, ".prettierrc": {}, ".stylelintrc": {},
	".babelrc": {}, ".npmrc": {}, ".yarnrc": {}, ".nvmrc": {}, ".browserslistrc": {},
	".flake8": {}, ".pylintrc": {}, ".rubocop.yml": {},
	".htaccess": {}, ".mailmap": {}, ".sequelizerc": {},
	"license": {}, "licence": {}, "authors": {}, "contributors": {}, "changelog": {}, "todo": {},
	"codeowners": {}, "version": {}, "readme": {},
}

var knownBinaryExts = map[string]struct{}{
	"png": {}, "jpg": {}, "jpeg": {}, "gif": {}, "bmp": {}, "ico": {}, "svg": {}, "webp": {}, "tif": {}, "tiff": {}, "psd": {}, "xcf": {}, "heic": {}, "raw": {},
	"pdf": {}, "docx": {}, "doc": {}, "xlsx": {}, "xls": {}, "pptx": {}, "ppt": {}, "odt": {}, "ods": {}, "odp": {}, "rtf": {},
	"zip": {}, "tar": {}, "gz": {}, "bz2": {}, "xz": {}, "7z": {}, "rar": {}, "dmg": {}, "iso": {}, "img": {}, "vmdk": {}, "qcow2": {},
	"exe": {}, "dll": {}, "so": {}, "dylib": {}, "a": {}, "lib": {}, "o": {}, "obj": {}, "pdb": {},
	"class": {}, "jar": {}, "war": {}, "ear": {}, "pyc": {}, "pyo": {}, "pyd": {}, "wasm": {}, "beam": {}, "rlib": {},
	"apk": {}, "aab": {}, "ipa": {}, "msi": {}, "cab": {}, "deb": {}, "rpm": {},
	"pt": {}, "pth": {}, "ckpt": {}, "safetensors": {}, "onnx": {}, "gguf": {}, "h5": {}, "pkl": {}, "parquet": {}, "arrow": {},
	"mp3": {}, "mp4": {}, "mov": {}, "avi": {}, "mkv": {}, "webm": {}, "flv": {}, "wmv": {}, "m4a": {}, "wav": {}, "flac": {}, "ogg": {}, "3gp": {},
	"ttf": {}, "otf": {}, "woff": {}, "woff2": {}, "eot": {},
	"blend": {}, "glb": {}, "fbx": {}, "3ds": {},
	"db": {}, "sqlite": {}, "sqlite3": {}, "bin": {}, "dat": {}, "hex": {}, "dump": {}, "map": {}, "lockb": {},
	"pack": {}, "eslintcache": {}, "inf": {}, "pbm": {}, "ppm": {},
	"icns": {}, "xpm": {}, "scpt": {},
}

var knownTextExts = map[string]struct{}{
	"html": {}, "htm": {}, "css": {}, "scss": {}, "sass": {}, "less": {}, "js": {}, "jsx": {}, "mjs": {}, "cjs": {}, "ts": {}, "tsx": {}, "mts": {}, "cts": {},
	"json": {}, "yaml": {}, "yml": {}, "xml": {}, "toml": {}, "ini": {}, "cfg": {}, "conf": {}, "properties": {}, "env": {}, "lock": {},
	"md": {}, "markdown": {}, "txt": {}, "text": {}, "rst": {}, "adoc": {},
	"py": {}, "pyw": {}, "pyi": {}, "ipynb": {}, "rb": {}, "erb": {}, "haml": {}, "pl": {}, "pm": {}, "lua": {}, "sh": {}, "bash": {}, "zsh": {}, "fish": {}, "bat": {}, "cmd": {}, "ps1": {}, "psm1": {}, "psd1": {},
	"c": {}, "cc": {}, "cpp": {}, "cxx": {}, "h": {}, "hh": {}, "hpp": {}, "hxx": {}, "go": {}, "rs": {}, "swift": {}, "kt": {}, "kts": {}, "scala": {}, "cs": {}, "fs": {}, "vb": {}, "vbs": {}, "java": {}, "jsp": {}, "php": {}, "sql": {},
	"gd": {}, "godot": {}, "shader": {}, "unity": {}, "qml": {},
	"mk": {}, "cmake": {}, "gradle": {},
	"groovy": {}, "gvy": {}, "tf": {}, "hcl": {},
	"sln": {}, "csproj": {}, "vbproj": {}, "fsproj": {},
	"r": {}, "rmd": {}, "clj": {}, "cljs": {}, "ex": {}, "exs": {}, "erl": {}, "hrl": {}, "elm": {}, "nim": {}, "zig": {}, "v": {}, "d": {}, "m": {}, "mm": {},
	"hs": {}, "lhs": {}, "jl": {}, "cl": {}, "lisp": {}, "scm": {}, "ss": {}, "rkt": {}, "asm": {}, "s": {},
	"csv": {}, "tsv": {}, "graphql": {}, "gql": {}, "proto": {}, "sol": {}, "patch": {}, "diff": {},
	"vim": {}, "dart": {}, "vue": {}, "svelte": {}, "astro": {}, "tex": {}, "j2": {}, "ejs": {}, "hbs": {}, "mustache": {}, "liquid": {}, "pug": {}, "jade": {},
	"tsbuildinfo": {}, "info": {}, "local": {}, "development": {}, "production": {}, "staging": {}, "test": {}, "example": {},
	"log": {}, "out": {}, "err": {}, "pid": {}, "seed": {}, "snap": {},
	"code-snippets": {}, "code-workspace": {}, "tmlanguage": {},
	"desktop": {}, "template": {}, "spec": {}, "ps1xml": {},
}

func containsParentTraversal(value string) bool {
	normalized := strings.ReplaceAll(value, "\\", "/")
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func normalizeRelPath(value string) string {
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\\", "/")
	value = path.Clean(value)
	value = strings.TrimPrefix(value, "./")
	if value == "." || value == "/" {
		return "."
	}
	return value
}

func dedupeSortedStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func dedupeEntriesByPath(entries []fileEntry) []fileEntry {
	if len(entries) == 0 {
		return entries
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].RelPath != entries[j].RelPath {
			return entries[i].RelPath < entries[j].RelPath
		}
		return entryModePriority(entries[i].Mode) > entryModePriority(entries[j].Mode)
	})

	out := entries[:1]
	for _, entry := range entries[1:] {
		last := &out[len(out)-1]
		if entry.RelPath != last.RelPath {
			out = append(out, entry)
			continue
		}
		if entryModePriority(entry.Mode) > entryModePriority(last.Mode) {
			*last = entry
			continue
		}
		if entry.Mode == entryModeDiff && last.Mode == entryModeDiff {
			last.DiffWantStaged = last.DiffWantStaged || entry.DiffWantStaged
			last.DiffWantUnstaged = last.DiffWantUnstaged || entry.DiffWantUnstaged
		}
		if entry.GitVisible && !last.GitVisible {
			last.GitVisible = true
		}
		if entry.Bypassed && !last.Bypassed {
			last.Bypassed = true
			last.BypassKind = entry.BypassKind
			last.BlockRule = entry.BlockRule
			last.BlockSource = entry.BlockSource
		}
		if last.TargetRoot == "" && entry.TargetRoot != "" {
			last.TargetRoot = entry.TargetRoot
		}
	}
	return out
}

func dedupePreserveOrder(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func entryModePriority(mode entryMode) int {
	switch mode {
	case entryModeDiff:
		return 2
	case entryModeSnippet:
		return 1
	default:
		return 0
	}
}
