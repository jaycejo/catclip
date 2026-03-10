package catclip

// =============================================================================
// catclip — Context Gatherer for LLMs
//
// This is the Go rewrite of the original Bash implementation.
//
// CURRENT LAYOUT:
//   - cmd/catclip/main.go is the thin binary entrypoint
//   - the implementation lives in package catclip at the module root
//   - files are split by responsibility, but most code still shares one package
//
// PRODUCT RULES:
//   1. Preview Must Be Truthful:
//      the tree and summary must reflect exactly what will be copied, not a
//      looser approximation
//   2. Current Paths Win:
//      the UI should show the current filesystem view and current working-tree
//      paths, even when Git metadata is simplified
//   3. Selectors Drive Status UI:
//      tree badges should optimize for `--changed`, `--staged`,
//      `--unstaged`, and `--untracked`, not raw porcelain fidelity
//   4. Specificity Grants Access:
//      safe discovery stays safe; ignored content only enters through explicit
//      `--include`
//   5. Exact Beats Fuzzy:
//      exact existing paths and exact basename hits should execute directly;
//      genuinely ambiguous fuzzy resolution belongs to fzf, not local
//      heuristics
//   6. One Scope, One Meaning:
//      targets come first, modifiers apply to that scope, and `--then` starts
//      a new scope; scope grammar must stay deterministic, and coverage is
//      path-subtree based rather than "conceptually related" (for example,
//      selecting `src/vs` does not imply that later selecting `src` is
//      redundant)
//   7. Single Writer Integrity:
//      output order must stay deterministic, with one sink writer and loud
//      failure on read/write errors
//   8. Safe Path Is Fast Path:
//      normal visible discovery should stay on the optimized `.gitignore` +
//      `.hiss` path, using rg for Git-visible files and applying `.hiss` in
//      Go; slower ignored/bypass flows are acceptable off the hot path
//   9. Interactive Is a Convenience Layer:
//      catclip is both a scripting CLI and an interactive tool, so complete
//      deterministic commands must remain directly executable; the builder only
//      helps resolve ambiguity or unfinished human input
//  10. Classification Is Product Policy:
//      known text/binary allowlists are intentional behavior, not incidental
//      heuristics
//  11. No Silent Skips:
//      if catclip excludes or bypasses something significant, the user should
//      get a visible reason unless they explicitly asked for quiet behavior
//  12. Warnings Must Be Actionable:
//      diagnostics should tell the user what to do next, not only what went
//      wrong
//  13. Quiet Means Minimal UX, Not Different Semantics:
//      `-q` may suppress presentation, prompts, and tree output, but it should
//      not change what files are selected
//  14. Builder State Must Be Reversible:
//      invalid interactive input must never poison the current scope state or
//      silently mutate the command being built
//  15. Same Payload, Different Sink:
//      stdout and clipboard modes may differ in transport cost, but they
//      should emit the same payload bytes for the same resolved selection
//  16. Bundled Tooling Is Part of the Product:
//      packaged installs must carry private fzf + ripgrep binaries; runtime
//      should not silently fall back to arbitrary PATH copies, because PATH
//      fallback reintroduces version drift, machine-specific behavior, weaker
//      install guarantees, and harder debugging/support
//
// WHY THE ROOT PACKAGE IS STILL BROAD:
//   - resolver, discovery, ignore, git, and rendering still share many internal
//     types and evolve together
//   - tree/preview/rendering behavior is still changing, so freezing package
//     APIs now would create churn without reducing complexity
//   - extracting packages too early would force a lot of exports or awkward
//     shared "types" packages, which is usually a sign the boundary is not ready
//
// CURRENT FILE GROUPS:
//   - cli/help:       arg parsing, entry flow, prompts, help/version output
//   - interactive:    picker-driven command builder
//   - resolver:       target resolution and fzf integration
//   - discovery:      walking, visibility indexes, file classification
//   - content/ripgrep: content filtering and rg-backed helpers
//   - ignore/git:     .hiss loading, git-aware filtering, changed-file logic
//   - preview/emit:   preview tree, summaries, clipboard/stdout output
//   - spinner:        short-lived TTY loading indicators
//
// EXECUTION FLOW:
//   1. Parse args and build scopes
//   2. For interactive TTY runs, decide whether a token is already exact enough
//      to bypass fzf:
//      - exact existing paths win immediately
//      - exact basename file hits can also bypass the picker
//      - only genuinely ambiguous / shorthand queries go to fzf
//   3. Resolve each target into either:
//      - an exact file
//      - an exact directory subtree
//      - an fzf-backed fuzzy selection
//      - an --include-authorized ignored target
//   4. Discover files for resolved targets:
//      - rg is the primary engine for visible file enumeration
//      - exact visible directory targets also use rg-backed subtree discovery
//      - rg is also used for exact basename lookup and --contains matching
//      - Go walks are still used where directory objects matter:
//        ignored-target browsing and some exact ignored / bypassed directory
//        cases
//      - symlinks are currently excluded everywhere by policy
//      - visible directory targets are derived from the visible file set rather
//        than a separate directory walk, so there is no standalone visible-dir
//        walk in the hot path; they inherit both rg/.gitignore visibility and
//        catclip's .hiss filtering
//      - consequence: empty directories, or directories with no surviving text
//        files, are intentionally excluded from the visible picker
//   5. Apply cheap file eligibility checks first:
//      - ignore rules
//      - --only / --exclude
//      - known binary basename/extension denylist
//      - known text basename/extension allowlist
//   6. Fall back to byte sniffing only for unknown file types:
//      - text sniffing is cached for the duration of the run
//      - the same file should only be sniffed once per command
//      - this is also why newer Linux repository runs include more assembly
//        source than older catclip runs: rg only enumerates candidates, but
//        `isLikelyTextFile(...)` now admits `.S` / `.lds.S` through the known
//        text allowlist because extension matching is shell-style and
//        case-insensitive (`.S` normalizes to `s`), instead of relying on the
//        old byte-sniff fallback to recognize them as text
//   7. Keep ripgrep-backed candidate entries lightweight:
//      - picker/index candidates are stored with RelPath first
//      - AbsPath is materialized only when a file survives to real work
//        like --contains, preview sizing, snippets/diffs, or final emission
//   8. Apply git selectors and content selectors
//   9. Build preview metadata and render the tree/summary when needed:
//      - normal `-q` runs skip tree rendering and confirmation entirely
//      - `-q` therefore makes `-y` and `-t` redundant in normal non-preview
//        runs
//      - preview/tree-specific metadata such as git status is only collected
//        when a tree will actually be rendered
//      - preview Git badges are selector-aligned rather than rename-detailed:
//        `[S]`, `[M]`, `[?]`, and `[SM]` are the states that matter because
//        they map to `--staged`, `--unstaged`, `--untracked`, and files that
//        are both staged and unstaged; catclip does not currently show a
//        dedicated rename badge
//      - size/token summary is still computed even without a tree because the
//        Count / Size / Tokens disclaimer depends on it; token counting remains
//        a fast byte-based estimate (`bytes / 4`) on purpose, because exact
//        tokenizers would add noticeable work while the real hot cost here is
//        gathering file sizes, not formatting the final number
//  10. Emit output to stdout or the clipboard sink
//
// GIT / RG PERFORMANCE RULES:
//   - do not reintroduce git check-ignore into the normal visible-file hot path
//   - for safe visible discovery, trust rg's .gitignore handling and then apply
//     .hiss in Go
//   - git check-ignore is reserved for narrow cases only:
//     exact ignored-target diagnostics, ignored-target browsing, and other
//     explicit include/bypass flows
//   - a previous git cat-file --batch fast-path experiment was benchmarked and
//     was substantially slower than direct working-tree streaming for catclip's
//     "wrap and emit many files" workload; do not assume Git blob batching is
//     an optimization here without new measurements
//   - preview/tree badge collection now narrows `git status --porcelain` to
//     selected roots/pathspecs when the path list is small enough, and only
//     falls back to repo-wide porcelain for broad/unsafe path sets or Git
//     command failure; this materially improved tree-enabled scoped runs
//   - drawback/tradeoff of the narrowed porcelain path:
//     boundary-crossing rename/copy cases can be less complete than repo-wide
//     status, but because the tree only cares about staged / unstaged /
//     untracked selector states today, and not a dedicated rename badge, that
//     trade is acceptable; future rename-specific UI should revisit these
//     fallback rules carefully
//
// OUTPUT PIPELINE RULES:
//   - full-file emission uses bounded read-side concurrency, but exactly one
//     goroutine writes to the sink
//   - the default read worker count is 2:
//     tracked-linux benchmarks showed large wins at 2/4/8 workers, but 2 is
//     the safer cross-machine default because it still overlaps reads while
//     being less likely to thrash spinning disks than 4 or 8
//   - benchmark takeaway:
//     2 workers delivered the major jump; 4 and 8 improved things further but
//     with much smaller gains, so higher defaults are harder to justify
//   - integrity rules for future changes:
//     multiple readers are fine, but preserve exactly one writer, complete
//     per-file buffers only, immutable handoff from worker to writer, ordered
//     commit, and loud failure on read error
//   - future output corruption risk comes from:
//     multiple sink writers, shared/reused mutable buffers, out-of-order commit,
//     silent skip/retry logic, or reading files that are being modified mid-run
//   - clipboard note:
//     on macOS, giant clipboard runs are now mostly limited by `pbcopy` /
//     pasteboard wait time, not catclip's own payload generation; this matters
//     for pathological full-repo copies like `catclip .` on Linux repository
//     checkouts, not "Linux the OS". At `vscode-main` scale the clipboard wait
//     was about 1.2s and effectively negligible for normal use.
//
// KNOWN REMAINING COSTS:
//   - exact basename lookup still has its own rg pass separate from the picker
//   - ignored-target browsing still uses a full Go walk by design
//   - preview tree runs still pay per-file size collection, and large or broad
//     tree requests can still fall back to repo-wide git status collection;
//     quiet/no-tree paths avoid that cost
//   - on large clean repos, output emission is currently the dominant cost, not
//     visible-file discovery

// PATH / PICKER RULES:
//   - `catclip` with no args is the explicit interactive entrypoint
//   - exact existing targets like `.`, `src`, or `dir/file` should run
//     directly instead of opening fzf
//   - slashless shorthand like `common`, `btn`, or `node` is picker territory
//   - trailing slash has no special picker meaning:
//     overloading `dir/` as "directories only" or treating `src/`
//     differently from `src` was a bad plan and is intentionally rejected;
//     we also rejected the idea that `dir/` should scope the picker to "all
//     files under that dir" as a special interactive mode; exact paths should
//     stay exact, scoped path targets like `layout/Footer.tsx` still use
//     normal resolution, and fuzzy file/directory discovery should be handled
//     by fzf rather than by slash punctuation; if directory-only or
//     directory-first picker modes return later, they should use explicit
//     flags or picker toggles instead of path punctuation
//   - startup `--then` continuation stays in the interactive builder; it
//     should behave like the builder flow, not fall back to the normal CLI path
//   - the normal picker is visible-only
//   - ignored targets require explicit `--include` authorization; in the picker
//     flow they are reached through the ignored-target path rather than mixed
//     into the safe list by default
//   - packaged installs are expected to resolve fzf/rg from app-private paths;
//     env overrides remain for tests and developer runs, but there is no normal
//     user-facing PATH fallback
//   - bare `--include` opens ignored-target selection for the current scope
//   - `.` means "all safe targets" and suppresses further safe-target picking,
//     but it must not suppress ignored-target browsing
//   - scope coverage is literal and subtree-based:
//     selecting `src/vs` covers only `src/vs/...`, not all of `src/...`, so a
//     later `--then src` is valid and should remain available
//   - exact overlapping scopes are allowed in scripting mode even when a later
//     scope is already covered by an earlier one; final payload is still
//     deduped by path, but the command should keep the user's literal scope
//     structure
//   - current interactive continuation exclusion is target-based, not
//     result-set-based:
//     later pickers exclude previously selected target paths/subtrees, but do
//     not evaluate prior-scope modifiers like `--only`, `--exclude`,
//     `--contains`, `--changed`, `--snippet`, or `--diff` before deciding what
//     counts as "already covered"
//   - consequence of that simplification:
//     `src --only "*.ts"` still makes later picker logic treat all of `src` as
//     covered, and prior `.` still means "all safe targets are covered" for
//     continuation purposes, even if that scope would later be narrowed by
//     modifiers
//   - the old same-scope `[done] use selected targets` continuation picker was
//     removed; same-scope multi-targeting now belongs to the builder flow
//   - deferred interaction design decision:
//     the product direction above is not fully implemented yet: the current
//     builder still treats incomplete value-taking modifiers like `--only`,
//     `--exclude`, and `--contains` as hard errors instead of interactive
//     continuation points; keep that strict behavior for now unless the builder
//     is deliberately expanded, and do not mistake the current simplification
//     for the intended long-term UX; likewise, target-based continuation
//     exclusion is a deliberate temporary simplification, not the ideal final
//     interactive model
//
// NOTES FOR FUTURE CODEX/CLAUDE PASSES:
//   - preserve user-facing semantics over "cleaner" abstractions; if a refactor
//     changes target meaning, preview truthfulness, or builder behavior, it is
//     probably the wrong refactor
//   - only extract an internal package when it can own a small API without
//     exporting half the app's internals; if a split mainly moves files around,
//     it is premature
//   - if tree/render UX is still in flux, keep it close to the rest of the app
//     until the behavior settles
//   - benchmark explicit binaries, not just whatever `catclip` in PATH points
//     to; old installed binaries can silently invalidate before/after results
//   - benchmark the path you actually changed: tree/porcelain optimizations
//     must be measured with tree-enabled commands, not `-q -t` runs that skip
//     that work entirely
//   - when file counts or tree contents change, investigate selection and
//     classification first; output-path changes should not change what files
//     are selected
//   - for Git-performance work, use real tracked clones as the primary testbed;
//     odd, partially detached, or effectively untracked trees can hide or
//     distort Git costs
//   - before adding a Git-based "optimization", compare it against the current
//     rg + direct-filesystem baseline on a real tracked clone, not only on odd
//     working trees; previous `git cat-file --batch` experiments lost badly
//   - clipboard benchmarks must separate catclip generation time from clipboard
//     backend wait; large macOS runs were dominated by `pbcopy` wait, not by
//     catclip's own payload generation
//   - interactive input must be validated on a candidate copy before mutating
//     builder state; invalid lines should surface an in-panel error without
//     poisoning the current scope or command
//   - incomplete modifier commands may eventually grow specialized builder
//     dialogs/continuations instead of hard errors, but only if they preserve
//     deterministic CLI semantics and keep interactive state reversible
// =============================================================================

import (
	"fmt"
	"os"
	"regexp"
)

// =============================================================================
// Constants and types
// =============================================================================

type action string

const (
	actionRun       action = "run"
	actionHelp      action = "help"
	actionHelpAll   action = "help-all"
	actionVersion   action = "version"
	actionEditHiss  action = "hiss"
	actionResetHiss action = "hiss-reset"
)

type outputMode string

const (
	outputModeClipboard outputMode = "clipboard"
	outputModeStdout    outputMode = "stdout"
)

type runConfig struct {
	Action     action
	Version    string
	Platform   string
	WorkingDir string
	OutputMode outputMode

	Verbose bool
	Quiet   bool
	Yes     bool
	Print   bool
	Preview bool
	NoTree  bool

	Scopes   []scope
	Warnings []string
}

type scope struct {
	Targets         []string
	IncludedTargets []string
	Only            []string
	Exclude         []string
	Contains        string
	Snippet         bool
	Changed         bool
	Staged          bool
	Unstaged        bool
	Untracked       bool
	Diff            bool
}

type scopeBuilder struct {
	scope
	explicitChanged bool
}

type ignoreRuleKind string

const (
	ignoreRuleFile ignoreRuleKind = "file"
	ignoreRuleDir  ignoreRuleKind = "dir"
)

type ignoreRule struct {
	Raw     string
	Kind    ignoreRuleKind
	Pattern string
}

type compiledGlob struct {
	raw string
	re  *regexp.Regexp
}

type compiledDirRule struct {
	raw      string
	segments []*regexp.Regexp
}

type scopeMatcher struct {
	ignoreFiles []compiledGlob
	ignoreDirs  []compiledDirRule
	only        []compiledGlob
}

type fileEntry struct {
	AbsPath          string
	RelPath          string
	TargetRoot       string
	GitVisible       bool
	Mode             entryMode
	SnippetPattern   string
	DiffWantStaged   bool
	DiffWantUnstaged bool
	Bypassed         bool
	BypassKind       string
	BlockRule        string
	BlockSource      string
}

type gitContext struct {
	Enabled    bool
	Root       string
	WorkPrefix string
	HasHead    bool
}

type colorPalette struct {
	Reset  string
	Bold   string
	Dim    string
	OK     string
	Err    string
	Warn   string
	Dir    string
	Label  string
	Value  string
	Tree   string
	Prompt string
	Git    string
}

type outputReport struct {
	sizes     map[string]int64
	statuses  map[string]string
	modeTags  map[string]string
	landmarks map[string]bool
	humanSize string
	tokens    int64
	fileWord  string
	notices   []string
}

type visibleDirIndex struct {
	dirs        []string
	set         map[string]struct{}
	symlinkDirs []string
}

type visibleFileIndex struct {
	byBase        map[string][]fileEntry
	skippedByBase map[string][]skippedMatch
}

type blockInfo struct {
	Rule   string
	Source string
}

type skippedMatch struct {
	RelPath     string
	BlockRule   string
	BlockSource string
	BlockKind   string
}

type gitIgnoreMatch struct {
	Rule    string
	DirRule bool
}

type targetMatch struct {
	Path         string
	Kind         string
	Ignored      bool
	IgnoreSource string
}

type usageError struct {
	message string
}

type exitError struct {
	message string
	code    int
}

// Diagnostics are collected in encounter order so stderr matches the shell's
// target-by-target flow. Some of them are real "Error:" blocks that should
// still print under --quiet even when soft warnings are suppressed.
type diagnostic struct {
	message string
	isError bool
}

type entryMode string

const (
	entryModeFull    entryMode = "full"
	entryModeSnippet entryMode = "snippet"
	entryModeDiff    entryMode = "diff"
)

const tokenWarnThreshold = 100000

func (e usageError) Error() string {
	return e.message
}

func (e exitError) Error() string {
	return e.message
}

func newUsageError(format string, args ...any) error {
	return usageError{message: fmt.Sprintf(format, args...)}
}

func newExitError(code int, message string) error {
	return exitError{message: message, code: code}
}

// =============================================================================
// Main entrypoint
// =============================================================================

// Main routes into the interactive builder when appropriate, otherwise parses
// the CLI normally and runs the selected action.
func Main() {
	args := os.Args[1:]
	if handled, err := maybeRunInteractiveBuilder(args, os.Stdout, os.Stderr); handled {
		if err != nil {
			exitWithError(err, os.Stderr)
		}
		return
	}

	cfg, err := parseArgs(args)
	if err != nil {
		exitWithError(err, os.Stderr)
		return
	}

	if err := run(cfg, os.Stdout, os.Stderr); err != nil {
		exitWithError(err, os.Stderr)
	}
}
