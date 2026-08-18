package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brandonbosch/porkchop/internal/gitx"
	"github.com/brandonbosch/porkchop/internal/store"
	"github.com/brandonbosch/porkchop/meat"
)

const processUsage = `porkchop process — abridge and cache, without opening anything

Usage:
  porkchop process                 Process the most recent commit (HEAD).
  porkchop process <revision>      Process a specific commit or revision.
  porkchop process <range>         Process a commit range (main...HEAD).
  porkchop process -staged         Process the staged changes.
  porkchop process -w              Process the unstaged working-tree changes.

This is the headless half of porkchop: it reads a diff, asks meat's core to
abridge it, and writes the result to the shared cache. Nothing is displayed and
no pager or TUI opens — a later "porkchop <same rev>" is then an instant cache
hit. It is what "porkchop hook install" runs after each commit, and what an agent
harness should call when it finishes a change.

An empty diff is success, not an error: a commit that changed nothing is a
perfectly good thing for a hook to encounter.

Flags:
  -model string   Model to use (default $MEAT_MODEL or a built-in default).
  -no-cache       Recompute even if a cached result exists (still updates cache).
  -staged         Process the staged changes (git diff --staged).
  -w              Process the unstaged working-tree changes (git diff).
  -json           Report the outcome as JSON on stdout.
  -q              Print nothing on success; errors still go to stderr.
  -h, --help      Show this help.
`

// processReport is what `process` says it did. It is emitted as one human line or,
// with -json, as an object — an agent harness wants the cache key and the token
// spend, and should not have to parse prose for them.
type processReport struct {
	// Source labels what was read: a revision, a range, "staged", or "stdin".
	Source string `json:"source"`
	// Rev is the short sha and subject when the source was a single commit.
	Rev string `json:"rev,omitempty"`
	// Key is the cache key the result is stored under — the same key plain meat
	// would use, so either tool can serve it.
	Key string `json:"key,omitempty"`
	// Cached reports that the work was already done and nothing was spent.
	Cached bool `json:"cached"`
	// Empty reports that there was no diff to read at all.
	Empty bool `json:"empty,omitempty"`
	// Model is the resolved model id, which participates in the cache key.
	Model        string `json:"model,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Elision      string `json:"elision,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	// Seconds is wall-clock time spent in the model, absent on a cache hit.
	Seconds float64 `json:"seconds,omitempty"`
}

// line is the one-line human form, the thing a hook log accumulates.
func (r processReport) line() string {
	what := r.Source
	if r.Rev != "" {
		what = r.Rev
	}
	switch {
	case r.Empty:
		return fmt.Sprintf("porkchop: nothing to read (%s)", what)
	case r.Cached:
		return fmt.Sprintf("porkchop: cached  %s — %s", what, r.Elision)
	default:
		return fmt.Sprintf("porkchop: processed  %s — %s (in=%d out=%d, %.1fs)",
			what, r.Elision, r.InputTokens, r.OutputTokens, r.Seconds)
	}
}

func runProcess(args []string) {
	fs := flag.NewFlagSet("porkchop process", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, processUsage) }
	model := fs.String("model", "", "model to use (default $MEAT_MODEL or built-in default)")
	noCache := fs.Bool("no-cache", false, "recompute even if a cached result exists")
	staged := fs.Bool("staged", false, "process the staged changes (git diff --staged)")
	worktree := fs.Bool("w", false, "process the unstaged working-tree changes (git diff)")
	jsonOut := fs.Bool("json", false, "report the outcome as JSON on stdout")
	quiet := fs.Bool("q", false, "print nothing on success")
	revs, err := parseFlexible(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	diff, source, err := gitx.ReadDiff(revs, *staged, *worktree)
	if err != nil {
		fatalReadDiff(revs, err)
	}
	report := processReport{Source: source}
	if len(revs) == 1 {
		report.Rev = gitx.Describe(revs[0])
	} else if !*staged && !*worktree && source == "HEAD" {
		report.Rev = gitx.Describe("HEAD")
	}

	// An empty diff is a no-op, not a failure. A hook fires on every commit,
	// including the empty ones, and must not start reporting errors for them.
	if strings.TrimSpace(diff) == "" {
		report.Empty = true
		emitProcess(report, *jsonOut, *quiet)
		return
	}

	resolved := meat.ResolveModel(*model)
	report.Model = resolved
	dir := store.Dir()
	report.Key = store.Key(diff, resolved, meat.RubricHash())

	if !*noCache {
		if cached, ok := store.Load(dir, report.Key); ok {
			report.Cached = true
			report.Summary = cached.Summary
			report.Elision = meat.ElisionLine(diff, cached.SmartDiff)
			emitProcess(report, *jsonOut, *quiet)
			return
		}
	}

	// Progress goes to stderr only when a human is watching it. In a hook the
	// destination is a log file, where a carriage-returned status line is noise;
	// the single outcome line below is what a log actually wants.
	progress := func(string) {}
	if !*quiet && isTerminal(os.Stderr) {
		progress = func(msg string) { fmt.Fprintf(os.Stderr, "\r\x1b[Kporkchop: %s", msg) }
	}

	ctx := context.Background()
	m, err := meat.NewModelFromEnv(ctx, *model)
	if err != nil {
		fatal("%v", err)
	}
	start := time.Now()
	res, err := meat.Abridge(ctx, m, meat.Request{
		RepoRoot:    gitx.Root(),
		UnifiedDiff: diff,
		Progress:    progress,
	})
	if isTerminal(os.Stderr) {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
	if err != nil {
		fatal("%v", err)
	}
	store.Store(dir, report.Key, res)

	report.Summary = res.Summary
	report.Elision = meat.ElisionLine(diff, res.SmartDiff)
	report.InputTokens, report.OutputTokens = res.InputTokens, res.OutputTokens
	report.Seconds = time.Since(start).Seconds()
	emitProcess(report, *jsonOut, *quiet)
}

func emitProcess(r processReport, jsonOut, quiet bool) {
	switch {
	case jsonOut:
		// -json wins over -q: a caller asking for a machine-readable answer wants
		// the answer, and silence is not one.
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(r)
	case quiet:
	default:
		fmt.Println(r.line())
	}
}
