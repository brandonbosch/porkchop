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
diff, and (from Phase 1 on) presents it in a TUI. Results are cached under
~/.meat with the same key meat uses, so a commit processed by either tool is an
instant cache hit for the other.

Flags:
  -model string   Model to use (default $MEAT_MODEL or a built-in default).
  -no-cache       Ignore any cached result and recompute (still updates cache).
  -staged         Review the staged changes (git diff --staged).
  -w              Review the unstaged working-tree changes (git diff).
  -json           Emit the result as JSON on stdout (no pager, no TUI).
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
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	diff, source, err := gitx.ReadDiff(fs.Args(), *staged, *worktree)
	if err != nil {
		fatal("%v", err)
	}
	if strings.TrimSpace(diff) == "" {
		fatal("no diff to read (%s)", source)
	}

	// Interactive progress is a stopgap for the skeleton: a single overwriting
	// status line on stderr, suppressed in -json mode. Phase 1's TUI feeds this
	// same meat Progress callback into a real spinner.
	progress := func(string) {}
	if !*jsonOut {
		progress = func(msg string) {
			fmt.Fprintf(os.Stderr, "\r\x1b[Kporkchop: %s", msg)
		}
	}

	ctx := context.Background()
	dir := store.Dir()
	key := store.Key(diff, meat.ResolveModel(*model), meat.RubricHash())

	var res *meat.Result
	if !*noCache {
		if cached, ok := store.Load(dir, key); ok {
			res = cached
		}
	}

	if res == nil {
		m, err := meat.NewModelFromEnv(ctx, *model)
		if err != nil {
			fatal("%v", err)
		}
		start := time.Now()
		res, err = meat.Abridge(ctx, m, meat.Request{
			RepoRoot:    gitx.Root(),
			UnifiedDiff: diff,
			Progress:    progress,
		})
		if err != nil {
			if !*jsonOut {
				fmt.Fprint(os.Stderr, "\r\x1b[K")
			}
			fatal("%v", err)
		}
		store.Store(dir, key, res)
		if !*jsonOut {
			fmt.Fprint(os.Stderr, "\r\x1b[K")
			fmt.Fprintf(os.Stderr, "porkchop: tokens in=%d out=%d in %s\n",
				res.InputTokens, res.OutputTokens, time.Since(start).Round(100*time.Millisecond))
		}
	}

	elision := meat.ElisionLine(diff, res.SmartDiff)
	if *jsonOut {
		renderJSON(res, elision)
		return
	}
	renderPlain(res, elision)
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
