// Package gitx turns porkchop's command-line arguments into the raw unified
// diff to abridge, plus the repo root the model's read-only tools are confined
// to.
//
// It is a deliberate, self-contained copy of the git plumbing in meat's own CLI
// (cmd/meat/main.go and cmd/meat/stdin.go). Copying rather than extracting a
// shared package keeps the upstream cmd/meat tree byte-identical, so
// `git merge upstream/main` never conflicts on it. The copied logic is small
// and stable, which is what makes the duplication cheap.
package gitx

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// ReadDiff returns the unified diff to abridge and a short label naming its
// source (used in error messages). Precedence mirrors meat's CLI exactly:
//   - staged / worktree: the index or working-tree diff;
//   - an explicit revision or range argument;
//   - stdin, when piped;
//   - otherwise `git show` of the top commit (HEAD).
func ReadDiff(args []string, staged, worktree bool) (diff, source string, err error) {
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
		// A range (A..B or A...B) is a diff across commits; a single revision is
		// one commit. `git show` on a range emits per-commit output, not the
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
		data, err := io.ReadAll(os.Stdin)
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

// Root returns the repo root, or "" if cwd is not inside a git repo. Porkchop
// passes it as meat.Request.RepoRoot so the model's read-only tools can inspect
// the surrounding source for clues; empty disables those tools.
func Root() string {
	out, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitShow shows one commit's diff. Plain `git show` on a merge commit emits NO
// diff (so we would report "no diff to read"); -m --first-parent makes a merge
// show its diff against the first parent — i.e. "what did merging this branch
// change on main" — and leaves regular commits untouched.
func gitShow(rev string) (string, error) {
	return git("show", "--format=fuller", "-m", "--first-parent", rev)
}

// isRevRange reports whether rev uses git's range syntax (A..B or A...B), as
// opposed to a single revision. Such ranges are diffed with `git diff`.
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

// stdinIsPiped reports whether stdin has data piped/redirected into it (as
// opposed to a terminal). When false, porkchop falls back to summarizing HEAD.
func stdinIsPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	// A char device is an interactive terminal; anything else (pipe, regular
	// file, socket) means data was redirected in.
	return fi.Mode()&os.ModeCharDevice == 0
}
