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
//	porkchop process [rev]   Abridge and cache without opening anything.
//	porkchop hook install    Warm the cache from a post-commit hook.
//
// Inference goes through internal/model, which defaults to Claude on AWS
// Bedrock. Reaching a public API is possible but never implicit: it takes
// -provider anthropic (or openai, or openai-compat for a local server).
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
	"github.com/brandonbosch/porkchop/internal/model"
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

Commands:
  porkchop process [rev|range]   Abridge and cache, printing one line and opening
                                 nothing. The entry point for hooks and agent
                                 devtools; see -json.
  porkchop hook install          Install a post-commit hook that warms the cache
                                 for each new commit in the background, without
                                 delaying or ever failing the commit.
  porkchop hook uninstall        Remove it.
  porkchop hook status           Report whether it is installed.

  Run a command with -h for its own flags. Only "process" and "hook" are reserved;
  a branch literally named either can still be reviewed as "porkchop -- process".
  Note the verbs belong to their command: it is "porkchop hook status", not
  "porkchop status".

porkchop reads a unified diff, asks meat's core to abridge it into a reading
diff, and presents it in a TUI. On a terminal it opens the review screen; when
stdout is redirected it prints plain text so pipes still work. Results are
cached under ~/.meat with the same key meat uses, so a commit processed by
either tool is an instant cache hit for the other.

Per-file "viewed" markers (v in the TUI) persist under the cache directory, keyed
to each file's own content, so they survive a range growing as an agent commits.

Flags:
  -provider name  Inference backend: bedrock (default), anthropic, openai,
                  openai-compat. Bedrock is the default because everything sent
                  to a model here — the diff, and the surrounding source meat's
                  tools read — is assumed to be CUI; a public API takes asking.
  -model string   Model id (default $PORKCHOP_MODEL, then $MEAT_MODEL). Bedrock
                  needs a full inference profile id and has no default, because
                  the id is passed through verbatim and forms the cache key.
  -region string  AWS region for bedrock (default $PORKCHOP_BEDROCK_REGION,
                  $AWS_REGION, or your AWS config). Required when authenticating
                  with a Bedrock API key, which carries no region of its own.
  -base-url url   Endpoint override; required for openai-compat, and the way to
                  reach a Bedrock FIPS or VPC endpoint. The transport pin
                  follows it.
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
  PORKCHOP_PROVIDER                    Default backend (see -provider).
  PORKCHOP_MODEL                       Default model id; read before MEAT_MODEL,
                                       so a machine that also runs plain meat can
                                       point porkchop at a Bedrock profile.
  PORKCHOP_BEDROCK_REGION              Default AWS region for bedrock.
  PORKCHOP_BASE_URL                    Default endpoint for openai-compat.
  AWS_PROFILE / AWS_REGION / ~/.aws    Bedrock credentials, via the standard AWS
                                       chain (SSO, assume-role, env, IMDS).
  AWS_BEARER_TOKEN_BEDROCK             A Bedrock API key, used instead of the
                                       credential chain. Env only — there is no
                                       flag, because a secret on a command line
                                       is visible to every process on the box.
  ANTHROPIC_API_KEY / OPENAI_API_KEY   API key, for those providers only.
  MEAT_MODEL                           Fallback default model id.
  MEAT_CACHE                           Optional cache dir (default ~/.meat; empty disables).
`

func main() {
	args := os.Args[1:]
	// Subcommand dispatch happens before flag parsing, so it must be exact: only a
	// bare "process" or "hook" in first position is a command. Everything else —
	// including a flag, a sha, or a range — is the review command, which is what
	// porkchop is for and what meat's arguments mean. "--" forces review, for the
	// unlucky repo with a branch named after a command.
	if len(args) > 0 {
		switch args[0] {
		case "process":
			runProcess(args[1:])
			return
		case "hook":
			runHook(args[1:])
			return
		case "help":
			fmt.Fprint(os.Stdout, usage)
			return
		case "--":
			args = args[1:]
		}
	}
	runReview(args)
}

// fatalReadDiff reports a failure to read the diff, adding a hint when the argument
// that failed is one of porkchop's own subcommand verbs.
//
// `porkchop status` is a natural thing to type and is not a subcommand — only
// `porkchop hook status` is — so it is read as a revision, and git answers with a
// wall of "ambiguous argument" text that says nothing about what the reviewer
// actually wanted. Reserving the verb instead would shadow anyone's branch of that
// name; hinting costs nothing and shadows nothing.
//
// The hint is only ever reached because the lookup failed, which means the word is
// not a revision in this repo — so it is safe even after an explicit `--`.
func fatalReadDiff(args []string, err error) {
	if hint := subcommandHint(args); hint != "" {
		fatal("%v\nporkchop: %s", err, hint)
	}
	fatal("%v", err)
}

// subcommandHint names the command a verb belongs to, or "" if it is not one.
func subcommandHint(args []string) string {
	if len(args) != 1 {
		return ""
	}
	verb := args[0]
	switch verb {
	case "install", "uninstall", "status":
		return fmt.Sprintf("%q is not a revision — did you mean `porkchop hook %s`?", verb, verb)
	}
	return ""
}

// parseFlexible parses fs over args, accepting flags before or after the positional
// arguments, and returns the positionals in order.
//
// Go's flag package stops parsing at the first non-flag word, which makes
// "porkchop hook install -force" and "porkchop process HEAD -json" — the orders a
// person actually types — into silent usage errors. The workflow commands are new
// and owe nothing to anyone's muscle memory, so they accept either order. The review
// command deliberately does not: it has to accept meat's arguments exactly as meat
// does, and a revision is allowed to look like anything.
func parseFlexible(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// runReview is porkchop proper: read a diff, abridge it, and open the review
// screen (or print it, when stdout is not a terminal).
func runReview(args []string) {
	fs := flag.NewFlagSet("porkchop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	backend := addBackendFlags(fs)
	noCache := fs.Bool("no-cache", false, "ignore any cached result and recompute (still updates the cache)")
	staged := fs.Bool("staged", false, "read the staged changes (git diff --staged)")
	worktree := fs.Bool("w", false, "read the unstaged working-tree changes (git diff)")
	jsonOut := fs.Bool("json", false, "emit the result as JSON on stdout")
	plain := fs.Bool("plain", false, "force plain-text output instead of the TUI")
	readingDiffFile := fs.String("reading-diff", "", "offline/dev: render a pre-computed reading diff from a file, skipping meat and the LLM")
	if err := fs.Parse(args); err != nil {
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
		diff, res = computeResult(fs.Args(), *staged, *worktree, backend.config(), *noCache, *jsonOut)
	}

	elision := meat.ElisionLine(diff, res.SmartDiff)
	switch {
	case *jsonOut:
		renderJSON(res, elision)
	case !*plain && isTerminal(os.Stdout):
		if err := ui.Run(ui.Input{
			Summary:     res.Summary,
			Elision:     elision,
			ReadingDiff: res.SmartDiff,
			RawDiff:     diff,
			Viewed:      openViewed(),
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
func computeResult(args []string, staged, worktree bool, backend model.Config, noCache, jsonOut bool) (string, *meat.Result) {
	diff, source, err := gitx.ReadDiff(args, staged, worktree)
	if err != nil {
		fatalReadDiff(args, err)
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
	// Resolve the backend before touching the cache: the model id is part of
	// the key, so a Bedrock inference profile and a public claude-opus-4-8 are
	// distinct entries and cannot collide. Resolution is offline — no
	// credentials are touched until there is actually work to do.
	backend, err = model.Resolve(backend)
	if err != nil {
		fatal("%v", err)
	}
	dir := store.Dir()
	key := store.Key(diff, backend.Model, meat.RubricHash())

	if !noCache {
		if cached, ok := store.Load(dir, key); ok {
			return diff, cached
		}
	}

	m, err := model.Open(ctx, backend)
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
