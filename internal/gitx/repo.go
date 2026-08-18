package gitx

import (
	"fmt"
	"path/filepath"
	"strings"
)

// This file is porkchop's own git plumbing, kept apart from gitx.go so the
// diff-reading logic there stays a recognizable copy of meat's and merges cleanly.

// GitDir returns the repository's git directory as an absolute path. In a linked
// worktree this is that worktree's own directory (.git/worktrees/<name>), which is
// the right place for per-worktree state like a process log.
func GitDir() (string, error) {
	return absGitPath("--git-dir")
}

// HooksDir returns the directory git will actually look in for hooks, as an
// absolute path.
//
// It asks git rather than assuming .git/hooks, because core.hooksPath relocates
// them (some teams point every repo at a shared directory) and a linked worktree
// resolves them against the common directory. Writing to a guessed .git/hooks in
// either case installs a hook that git will never run — a silent failure, and the
// worst kind for a tool whose job is to have already done the work.
func HooksDir() (string, error) {
	return absGitPath("--git-path", "hooks")
}

// absGitPath runs `git rev-parse <args>` and resolves the single path it prints
// against the current directory, since --git-path answers relatively.
func absGitPath(args ...string) (string, error) {
	out, err := git(append([]string{"rev-parse"}, args...)...)
	if err != nil {
		return "", fmt.Errorf("not a git repository?: %w", err)
	}
	path := strings.TrimSpace(out)
	if path == "" {
		return "", fmt.Errorf("git rev-parse %s printed nothing", strings.Join(args, " "))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// Describe names a revision for a human: its short sha and subject line. It is
// used in the one line `porkchop process` prints, so a hook log says which commit
// was warmed rather than only that something was. A revision git cannot describe
// (a range, or the working tree) yields "" rather than an error, since the caller
// has a perfectly good label of its own to fall back on.
func Describe(rev string) string {
	if rev == "" || isRevRange(rev) {
		return ""
	}
	out, err := git("show", "-s", "--format=%h %s", rev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
}
