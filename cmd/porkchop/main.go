// Command porkchop abridges a code diff into a "reading diff" and (soon)
// presents it in a TUI built for how a reviewer actually reads.
//
// It is a fork of meat.dev: meat's abridging core is vendored untouched under
// meat/, and porkchop replaces only the presentation and workflow around it.
// This Phase-0 skeleton is intentionally a thin wrapper — it accepts the same
// arguments as meat, shares meat's ~/.meat cache, and prints the same abridged
// result as plain text. Phase 1 replaces the plain render below with a Bubble
// Tea TUI (side-by-side panes, keyboard stepping, fold expansion).
//
// Usage:
//
//	porkchop                 Review the most recent commit (HEAD).
//	porkchop <revision>      Review a specific commit or revision (sha, HEAD~3).
//	porkchop <range>         Review a commit range (sha1..sha2, main...HEAD).
//	porkchop -staged         Review the staged (index) changes.
//	porkchop -w              Review the unstaged working-tree changes.
//	git show <sha> | porkchop   Review the diff piped on stdin.
//
// It reads OPENAI_API_KEY or ANTHROPIC_API_KEY from the environment (optionally
// the matching provider base URL, plus MEAT_MODEL / -model), exactly like meat.
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
	"github.com/brandonbosch/porkchop/internal/ui"
	"github.com/brandonbosch/porkchop/meat"
)

const usage = `porkchop — a reviewer-first reading-diff viewer (a fork of meat)

Usage:
  porkchop                 Review the most recent commit (HEAD) in the current git repo.
  porkchop <revision>      Review a specific commit or revision (e.g. a sha, HEAD~3).
  porkchop <range>         Review a commit range (e.g. sha1..sha2, main...HEAD).
  porkchop -staged         Review the staged (index) changes: git diff --staged.
  porkchop -w              Review the unstaged working-tree changes: git diff.
  git show <sha> | porkchop   Review the diff piped on stdin.

porkchop reads a unified diff, asks meat's core to abridge it into a reading
diff, and presents it in a TUI. On a terminal it opens the review screen; when
stdout is redirected it prints plain text so pipes still work. Results are
cached under ~/.meat with the same key meat uses, so a commit processed by
either tool is an instant cache hit for the other.

Flags:
  -model string   Model to use (default $MEAT_MODEL or a built-in default).
  -no-cache       Ignore any cached result and recompute (still updates cache).
  -staged         Review the staged changes (git diff --staged).
  -w              Review the unstaged working-tree changes (git diff).
  -json           Emit the result as JSON on stdout (no pager, no TUI).
  -plain          Force plain-text output instead of the TUI.
  -reading-diff f Offline/dev: render a pre-computed reading diff from file f,
                  skipping meat and the LLM (used to develop against goldens).
                  A sibling <name>.diff, if present, supplies the elision/size
                  manifest.
  -h, --help      Show this help.

Environment:
  OPENAI_API_KEY / ANTHROPIC_API_KEY   API key for the selected provider.
  OPENAI_BASE_URL / ANTHROPIC_BASE_URL Optional provider base-URL overrides.
  MEAT_MODEL                           Optional default model id.
  MEAT_CACHE                           Optional cache dir (default ~/.meat; empty disables).
`

func main() {
	fs := flag.NewFlagSet("porkchop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	model := fs.String("model", "", "model to use (default $MEAT_MODEL or built-in default)")
	noCache := fs.Bool("no-cache", false, "ignore any cached result and recompute (still updates the cache)")
	staged := fs.Bool("staged", false, "read the staged changes (git diff --staged)")
	worktree := fs.Bool("w", false, "read the unstaged working-tree changes (git diff)")
	jsonOut := fs.Bool("json", false, "emit the result as JSON on stdout")
	plain := fs.Bool("plain", false, "force plain-text output instead of the TUI")
	readingDiffFile := fs.String("reading-diff", "", "offline/dev: render a pre-computed reading diff from a file, skipping meat and the LLM")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	// diff is the raw pre-abridgement unified diff; res is the abridged result.
	// Both the live path (git → cache/meat) and the offline path (a golden file)
	// produce this same pair, after which rendering is identical.
	var (
		diff string
		res  *meat.Result
	)
	if *readingDiffFile != "" {
		diff, res = loadReadingDiff(*readingDiffFile)
	} else {
		diff, res = computeResult(fs.Args(), *staged, *worktree, *model, *noCache, *jsonOut)
	}

	elision := meat.ElisionLine(diff, res.SmartDiff)
	switch {
	case *jsonOut:
		renderJSON(res, elision)
	case !*plain && isTerminal(os.Stdout):
		if err := ui.Run(ui.Input{
			Summary:      res.Summary,
			Elision:      elision,
			ReadingDiff:  res.SmartDiff,
			RawDiffBytes: len(diff),
		}); err != nil {
			fatal("%v", err)
		}
	default:
		renderPlain(res, elision)
	}
}

// computeResult reads the raw diff for the given selection and returns it with
// meat's abridged result, hitting the shared ~/.meat cache first and computing
// via the LLM (needs credentials) only on a miss.
func computeResult(args []string, staged, worktree bool, model string, noCache, jsonOut bool) (string, *meat.Result) {
	diff, source, err := gitx.ReadDiff(args, staged, worktree)
	if err != nil {
		fatal("%v", err)
	}
	if strings.TrimSpace(diff) == "" {
		fatal("no diff to read (%s)", source)
	}

	// A single overwriting status line on stderr, suppressed in -json mode.
	progress := func(string) {}
	if !jsonOut {
		progress = func(msg string) {
			fmt.Fprintf(os.Stderr, "\r\x1b[Kporkchop: %s", msg)
		}
	}

	ctx := context.Background()
	dir := store.Dir()
	key := store.Key(diff, meat.ResolveModel(model), meat.RubricHash())

	if !noCache {
		if cached, ok := store.Load(dir, key); ok {
			return diff, cached
		}
	}

	m, err := meat.NewModelFromEnv(ctx, model)
	if err != nil {
		fatal("%v", err)
	}
	start := time.Now()
	res, err := meat.Abridge(ctx, m, meat.Request{
		RepoRoot:    gitx.Root(),
		UnifiedDiff: diff,
		Progress:    progress,
	})
	if err != nil {
		if !jsonOut {
			fmt.Fprint(os.Stderr, "\r\x1b[K")
		}
		fatal("%v", err)
	}
	store.Store(dir, key, res)
	if !jsonOut {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
		fmt.Fprintf(os.Stderr, "porkchop: tokens in=%d out=%d in %s\n",
			res.InputTokens, res.OutputTokens, time.Since(start).Round(100*time.Millisecond))
	}
	return diff, res
}

// loadReadingDiff builds a Result from a pre-computed reading-diff file, with no
// git, cache, LLM, or credentials — the offline path for developing the TUI
// against meat/testdata goldens. A sibling raw diff (the golden's <name>.diff,
// i.e. the ".golden" segment dropped) supplies the raw text so the size and
// elision manifest are real; without it those stats are simply omitted.
func loadReadingDiff(path string) (string, *meat.Result) {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("%v", err)
	}
	res := &meat.Result{SmartDiff: string(data)}

	// meat/testdata pairs "<name>.golden.diff" (reading diff) with "<name>.diff"
	// (the raw input). When rendering a golden, pick up its raw sibling so the
	// size and elision stats are real rather than omitted.
	if base, ok := strings.CutSuffix(path, ".golden.diff"); ok {
		if raw, err := os.ReadFile(base + ".diff"); err == nil {
			return string(raw), res
		}
	}
	return "", res
}

// isTerminal reports whether f is an interactive terminal (not a pipe or file),
// so porkchop can fall back to plain text when its output is redirected.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// jsonResult is the -json wire form: meat's Result plus the locally computed
// elision manifest. It matches meat's -json shape for drop-in tooling.
type jsonResult struct {
	meat.Result
	Elision string `json:"elision,omitempty"`
}

func renderJSON(res *meat.Result, elision string) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(jsonResult{Result: *res, Elision: elision})
}

// renderPlain writes summary + elision manifest + reading diff as plain text.
// Phase 1 replaces this with the Bubble Tea review screen; keeping it trivial
// here avoids building color/pager machinery the TUI will obsolete.
func renderPlain(res *meat.Result, elision string) {
	if res.Summary != "" {
		fmt.Printf("# %s\n", res.Summary)
	}
	if elision != "" {
		fmt.Printf("# %s\n", elision)
	}
	if res.Summary != "" || elision != "" {
		fmt.Println()
	}
	diff := strings.TrimRight(res.SmartDiff, "\n")
	if strings.TrimSpace(diff) == "" {
		fmt.Println("(no meaningful change to read)")
		return
	}
	fmt.Println(diff)
}

func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// meat's package errors are already prefixed "meat:"; add a porkchop prefix
	// only when there isn't one already, to avoid "porkchop: meat: ...".
	if !strings.HasPrefix(msg, "meat:") && !strings.HasPrefix(msg, "porkchop:") {
		msg = "porkchop: " + msg
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
