// Command meat abridges a code diff into a "reading diff": the same change,
// rewritten to keep only what a senior reviewer actually needs to read —
// mechanical noise (batch field copies, error-message construction, forced
// zero-value returns, generated code) elided, behavior-bearing changes kept.
//
// Usage:
//
//	# summarize the most recent commit in the current repo
//	meat
//
//	# summarize a specific commit or revision
//	meat <sha>
//	meat HEAD~3
//
//	# diff across a commit range
//	meat <sha1>..<sha2>
//	meat main...HEAD
//
//	# staged / unstaged changes
//	meat -staged
//	meat -w
//
//	# abridge any diff piped on stdin
//	git show <sha> | meat
//	git diff main...HEAD | meat
//
// It reads OPENAI_API_KEY or ANTHROPIC_API_KEY from the environment
// (optionally the matching provider base URL, plus MEAT_MODEL / -model).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/brandonbosch/porkchop/meat"
)

const usage = `meat — abridge a diff into a "reading diff"

Usage:
  meat                 Summarize the most recent commit (HEAD) in the current git repo.
  meat <revision>      Summarize a specific commit or revision (e.g. a sha, HEAD~3).
  meat <range>         Diff across a commit range (e.g. sha1..sha2, main...HEAD).
  meat -staged         Abridge the staged (index) changes: git diff --staged.
  meat -w              Abridge the unstaged working-tree changes: git diff.
  git show <sha> | meat   Abridge the diff piped on stdin.
  git diff | meat         Abridge the working-tree diff piped on stdin.

meat reads a unified diff (from stdin, from a named revision or range, or HEAD
when stdin is a terminal), asks an LLM to drop everything not worth reading, and
prints the abridged diff plus a one-line summary.

Results are cached under ~/.meat keyed by the SHA of (rubric/compiler protocol +
model + diff contents), so re-running on an unchanged diff is instant; editing
the diff, switching models, or upgrading meat's rubric or compiler invariants
recomputes.

On an interactive terminal the diff is colored and paged like git show (using
your git pager and color.diff config); piped/redirected output stays plain.

Flags:
  -model string   Model to use (default $MEAT_MODEL or a built-in default).
  -no-cache       Ignore any cached result and recompute (still updates cache).
  -staged         Read the staged changes (git diff --staged).
  -w              Read the unstaged working-tree changes (git diff).
  -json           Emit the result as JSON on stdout (no color, no pager).
  -h, --help      Show this help.

Environment:
  OPENAI_API_KEY       API key for OpenAI models (including the default).
  OPENAI_BASE_URL      Optional. Override the OpenAI API base URL.
  ANTHROPIC_API_KEY    API key for Claude models.
  ANTHROPIC_BASE_URL   Optional. Override the Anthropic API base URL.
  MEAT_MODEL           Optional. Default model id.
  MEAT_CACHE           Optional. Cache directory (default ~/.meat; empty disables).

On an exe.dev VM with an attached "llm" integration, meat uses the managed
LLM gateway automatically — no API key needed. Provider-specific API keys or
base URLs override the gateway.
`

func main() {
	fs := flag.NewFlagSet("meat", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	model := fs.String("model", "", "model to use (default $MEAT_MODEL or built-in default)")
	noCache := fs.Bool("no-cache", false, "ignore any cached result and recompute (still updates the cache)")
	staged := fs.Bool("staged", false, "read the staged changes (git diff --staged)")
	worktree := fs.Bool("w", false, "read the unstaged working-tree changes (git diff)")
	jsonOut := fs.Bool("json", false, "emit the result as JSON on stdout")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag already printed the error and (for -h) the usage.
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	diff, source, err := readDiff(fs.Args(), *staged, *worktree)
	if err != nil {
		fatal("%v", err)
	}
	if strings.TrimSpace(diff) == "" {
		fatal("no diff to read (%s)", source)
	}

	// Progress feedback is strictly interactive: only when BOTH stdout and
	// stderr are terminals (so `meat > file` and `meat 2> log` stay clean),
	// and never in -json mode.
	progress := func(string) {}
	interactive := !*jsonOut && isTerminal(os.Stdout) && isTerminal(os.Stderr)
	if interactive {
		progress = func(msg string) {
			// Overwrite a single status line on stderr; cleared before render.
			fmt.Fprintf(os.Stderr, "\r\x1b[Kmeat: %s", msg)
		}
	}

	// compute produces a fresh result on a cache miss. It is a closure so run()
	// can be unit-tested without an LLM: the real path constructs the selected
	// provider model (which needs credentials/network) only here, AFTER the cache
	// check.
	compute := func(ctx context.Context) (*meat.Result, error) {
		m, err := meat.NewModelFromEnv(ctx, *model)
		if err != nil {
			return nil, err
		}
		// Confine the read-only tools to the repo root, when we're in one, so
		// the agent can inspect surrounding source for clues.
		return meat.Abridge(ctx, m, meat.Request{
			RepoRoot:    gitRoot(),
			UnifiedDiff: diff,
			Progress:    progress,
		})
	}

	render := func(res *meat.Result) {
		if interactive {
			fmt.Fprint(os.Stderr, "\r\x1b[K") // clear the progress line
		}
		elision := meat.ElisionLine(diff, res.SmartDiff)
		if *jsonOut {
			renderJSON(os.Stdout, res, elision)
			return
		}
		// On an interactive terminal, render like `git show`: git's diff colors
		// and git's pager. Otherwise (piped/redirected) print plain text.
		renderResult(os.Stdout, res, elision)
	}

	opts := runOpts{
		diff:     diff,
		model:    meat.ResolveModel(*model),
		rubric:   meat.RubricHash(),
		cacheDir: cacheDir(),
		noCache:  *noCache,
		compute:  compute,
		render:   render,
		stderr:   os.Stderr,
	}
	if err := run(context.Background(), opts); err != nil {
		if interactive {
			fmt.Fprint(os.Stderr, "\r\x1b[K")
		}
		fatal("%v", err)
	}
}

// runOpts carries everything run needs, so the orchestration (cache lookup,
// compute-on-miss, store, print) can be tested in isolation from flag parsing,
// git, and the LLM.
type runOpts struct {
	diff     string
	model    string // already resolved (post $MEAT_MODEL/default)
	rubric   string // rubric hash, part of the cache key
	cacheDir string
	noCache  bool
	// compute produces a fresh result on a cache miss. The real implementation
	// constructs the LLM-backed model; it must only be called on a miss.
	compute func(ctx context.Context) (*meat.Result, error)
	// render emits the result body (summary + diff) to the user.
	render func(*meat.Result)
	stderr io.Writer
}

// run is the cache-aware core: look up by SHA of (model + diff); on a hit,
// print and return without touching opts.compute (so cache hits are instant and
// need no credentials); on a miss, compute, store, and print. -no-cache skips
// the read but still writes through.
func run(ctx context.Context, o runOpts) error {
	key := cacheKey(o.diff, o.model, o.rubric)
	if !o.noCache {
		if res, ok := cacheLoad(o.cacheDir, key); ok {
			o.render(res)
			fmt.Fprintf(o.stderr, "\nmeat: cached (sha %s)\n", key[:12])
			return nil
		}
	}

	start := time.Now()
	res, err := o.compute(ctx)
	if err != nil {
		return err
	}
	elapsed := time.Since(start).Round(100 * time.Millisecond)

	cacheStore(o.cacheDir, key, res)

	o.render(res)
	fmt.Fprintf(o.stderr, "\nmeat: tokens in=%d out=%d in %s\n", res.InputTokens, res.OutputTokens, elapsed)
	return nil
}

// readDiff returns the diff to abridge. Precedence:
//   - -staged / -w: the index or working-tree diff;
//   - an explicit revision argument: `git show` of that revision;
//   - stdin, when piped;
//   - otherwise `git show` of the top commit (HEAD) in the current repo.
//
// The second return value names the source for error messages.
func readDiff(args []string, staged, worktree bool) (string, string, error) {
	if staged && worktree {
		return "", "", fmt.Errorf("-staged and -w are mutually exclusive")
	}
	if (staged || worktree) && len(args) > 0 {
		return "", "", fmt.Errorf("-staged/-w cannot be combined with a revision argument")
	}
	if staged {
		out, err := git("diff", "--staged")
		if err != nil {
			return "", "staged", fmt.Errorf("reading staged changes: %w", err)
		}
		return out, "staged; nothing staged?", nil
	}
	if worktree {
		out, err := git("diff")
		if err != nil {
			return "", "worktree", fmt.Errorf("reading working-tree changes: %w", err)
		}
		return out, "worktree; no unstaged changes?", nil
	}
	if len(args) > 1 {
		return "", "", fmt.Errorf("too many arguments: want at most one revision, got %d", len(args))
	}
	if len(args) == 1 {
		rev := args[0]
		// A range (A..B or A...B) is a diff across commits; a single revision
		// is one commit. `git show` on a range emits per-commit output, not the
		// single aggregate diff we want, so dispatch ranges to `git diff`.
		var out string
		var err error
		if isRevRange(rev) {
			out, err = git("diff", rev)
		} else {
			out, err = gitShow(rev)
		}
		if err != nil {
			return "", rev, fmt.Errorf("reading %q: %w", rev, err)
		}
		return out, rev, nil
	}
	if stdinIsPiped() {
		data, err := readAllStdin()
		if err != nil {
			return "", "stdin", err
		}
		return string(data), "stdin", nil
	}
	// No pipe and no revision: summarize the top commit.
	out, err := gitShow("HEAD")
	if err != nil {
		return "", "HEAD", fmt.Errorf("reading HEAD (are you in a git repo?): %w", err)
	}
	return out, "HEAD", nil
}

// gitShow shows one commit's diff. Plain `git show` on a merge commit emits NO
// diff (so meat would report "no diff to read"); -m --first-parent makes a
// merge show its diff against the first parent — i.e. "what did merging this
// branch change on main" — and leaves regular commits untouched.
func gitShow(rev string) (string, error) {
	return git("show", "--format=fuller", "-m", "--first-parent", rev)
}

// isRevRange reports whether rev uses git's range syntax (A..B or A...B), as
// opposed to a single revision. Such ranges are diffed with `git diff`, not
// summarized with `git show`.
func isRevRange(rev string) bool {
	return strings.Contains(rev, "..")
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// gitRoot returns the repo root, or "" if cwd is not inside a git repo.
func gitRoot() string {
	out, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// Package errors are already prefixed "meat:"; don't double it up.
	if !strings.HasPrefix(msg, "meat:") {
		msg = "meat: " + msg
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
