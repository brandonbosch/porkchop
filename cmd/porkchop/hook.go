package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonbosch/porkchop/internal/gitx"
)

const hookUsage = `porkchop hook — warm the review cache from a git hook

Usage:
  porkchop hook install     Install the post-commit hook.
  porkchop hook uninstall   Remove it.
  porkchop hook status      Report whether it is installed.

The installed hook runs "porkchop process HEAD" after each commit, so the reading
diff for a change is already computed by the time anyone looks at it. It is written
to behave itself:

  - it never fails a commit: every path in it exits 0;
  - it never delays a commit: the model call is detached and logged, not waited on.

It fires for ordinary commits, for --amend, and for merge commits — all three are
changes worth having a reading diff for. It does not fire during a rebase or a
cherry-pick, because git does not run post-commit for those at all, so replaying
fifty commits costs nothing.

The hook is written into git's configured hooks directory (core.hooksPath and
worktrees are honored) inside a marked block, so an existing post-commit hook keeps
working and "uninstall" removes only porkchop's part.

Flags:
  -force   Install into a post-commit hook porkchop does not recognize as a shell
           script. Read it first.
`

// The hook block is delimited so install is idempotent and uninstall is surgical:
// porkchop rewrites or removes exactly what is between these markers and never
// touches a line of anyone else's hook.
const (
	hookBegin = "# >>> porkchop >>>"
	hookEnd   = "# <<< porkchop <<<"
	hookName  = "post-commit"
	hookLog   = "porkchop-process.log"
)

func runHook(args []string) {
	fs := flag.NewFlagSet("porkchop hook", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, hookUsage) }
	force := fs.Bool("force", false, "install into an unrecognized post-commit hook")
	verbs, err := parseFlexible(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if len(verbs) != 1 {
		fmt.Fprint(os.Stderr, hookUsage)
		os.Exit(2)
	}

	dir, err := gitx.HooksDir()
	if err != nil {
		fatal("%v", err)
	}
	path := filepath.Join(dir, hookName)

	switch cmd := verbs[0]; cmd {
	case "install":
		installHook(path, *force)
	case "uninstall":
		uninstallHook(path)
	case "status":
		hookStatus(path)
	default:
		fatal("unknown hook command %q (want install, uninstall, or status)", cmd)
	}
}

// installHook writes porkchop's block into the post-commit hook, creating the file
// if there isn't one and leaving any other hook content in place.
func installHook(path string, force bool) {
	existing, found := readHook(path)
	body, hadBlock := stripBlock(existing)

	if found && !hadBlock && !looksLikeShell(existing) && !force {
		// Appending shell to a file that is not shell would produce a hook that
		// either does nothing or breaks every commit. Refuse and let a human look.
		fatal("%s exists and is not a shell script porkchop recognizes; inspect it, then re-run with -force", path)
	}

	content := composeHook(body, hookBlock(porkchopBinary()))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("creating %s: %v", filepath.Dir(path), err)
	}
	// 0o755 because git will not run a hook it cannot execute, which is the one
	// failure mode that produces no error message anywhere.
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		fatal("writing %s: %v", path, err)
	}

	switch {
	case hadBlock:
		fmt.Printf("porkchop: updated the hook in %s\n", path)
	case found:
		fmt.Printf("porkchop: added porkchop's block to the existing %s\n", path)
	default:
		fmt.Printf("porkchop: installed %s\n", path)
	}
	if bin := porkchopBinary(); bin != "" {
		fmt.Printf("porkchop: it will run %s process HEAD after each commit\n", bin)
	}
}

// uninstallHook removes porkchop's block, and the file too if the block was all
// that was in it — leaving behind an empty hook would be litter.
func uninstallHook(path string) {
	existing, found := readHook(path)
	if !found {
		fmt.Printf("porkchop: no hook at %s\n", path)
		return
	}
	body, hadBlock := stripBlock(existing)
	if !hadBlock {
		fmt.Printf("porkchop: %s has no porkchop block; leaving it alone\n", path)
		return
	}
	if isEmptyScript(body) {
		if err := os.Remove(path); err != nil {
			fatal("removing %s: %v", path, err)
		}
		fmt.Printf("porkchop: removed %s\n", path)
		return
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		fatal("writing %s: %v", path, err)
	}
	fmt.Printf("porkchop: removed porkchop's block from %s\n", path)
}

// hookStatus reports what is installed and whether it can actually work, since a
// hook that silently does nothing is the failure this command exists to catch.
func hookStatus(path string) {
	existing, found := readHook(path)
	if !found {
		fmt.Printf("porkchop: not installed (no %s)\n", path)
		return
	}
	body, hadBlock := stripBlock(existing)
	if !hadBlock {
		fmt.Printf("porkchop: not installed; %s exists but has no porkchop block\n", path)
		return
	}
	fmt.Printf("porkchop: installed in %s\n", path)
	if !isEmptyScript(body) {
		fmt.Println("porkchop: the hook also contains other content, which uninstall will keep")
	}
	if info, err := os.Stat(path); err == nil && info.Mode()&0o111 == 0 {
		fmt.Println("porkchop: warning — the hook is not executable, so git will not run it")
	}
	if bin := porkchopBinary(); bin != "" {
		fmt.Printf("porkchop: the hook prefers %s, falling back to porkchop on PATH\n", bin)
	} else {
		fmt.Println("porkchop: warning — could not resolve this binary's path; the hook relies on porkchop being on PATH")
	}
	if dir, err := gitx.GitDir(); err == nil {
		log := filepath.Join(dir, hookLog)
		if info, err := os.Stat(log); err == nil {
			fmt.Printf("porkchop: log %s (%d bytes)\n", log, info.Size())
		} else {
			fmt.Printf("porkchop: log %s (not written yet)\n", log)
		}
	}
}

// readHook reads a hook file, reporting whether there was one. An unreadable file
// is fatal rather than treated as absent: overwriting something we could not read
// is exactly the mistake this command must not make.
func readHook(path string) (content string, found bool) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return string(data), true
	case os.IsNotExist(err):
		return "", false
	default:
		fatal("reading %s: %v", path, err)
		return "", false
	}
}

// stripBlock removes porkchop's marked block, returning what is left and whether a
// block was there. An unterminated block (someone deleted the end marker) is
// removed to the end of the file, since anything after it is porkchop's too.
func stripBlock(content string) (rest string, found bool) {
	lines := strings.Split(content, "\n")
	start, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case hookBegin:
			if start < 0 {
				start = i
			}
		case hookEnd:
			end = i
		}
	}
	if start < 0 {
		return content, false
	}
	if end < start {
		end = len(lines) - 1
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n"), true
}

// composeHook puts the block back after whatever else the hook contains, keeping
// the original shebang or supplying one for a new file.
func composeHook(body, block string) string {
	body = strings.TrimRight(body, "\n \t")
	switch {
	case isEmptyScript(body):
		// Preserve an existing shebang even when nothing else survives, in case it
		// names a shell the rest of the team's tooling expects.
		if sh := shebangOf(body); sh != "" {
			body = sh
		} else {
			body = "#!/bin/sh"
		}
	case shebangOf(body) == "":
		// Reachable only via -force, on a hook that has content but no shebang.
		// git will not execute such a file, so supplying one is the difference
		// between a working hook and one that fails every commit.
		body = "#!/bin/sh\n" + body
	}
	return body + "\n\n" + block + "\n"
}

// isEmptyScript reports whether a script has no content beyond a shebang and
// whitespace — i.e. whether removing porkchop's block left nothing worth keeping.
func isEmptyScript(body string) bool {
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (i == 0 && strings.HasPrefix(line, "#!")) {
			continue
		}
		return false
	}
	return true
}

func shebangOf(body string) string {
	first, _, _ := strings.Cut(body, "\n")
	if strings.HasPrefix(strings.TrimSpace(first), "#!") {
		return strings.TrimRight(first, " \t\r")
	}
	return ""
}

// looksLikeShell reports whether a file porkchop is about to append shell to will
// actually be run by a shell.
func looksLikeShell(content string) bool {
	sh := shebangOf(content)
	if sh == "" {
		return false
	}
	// Match the interpreter name, not the whole line, so "#!/usr/bin/env bash" and
	// "#!/bin/sh -e" both pass while "#!/usr/bin/python3" does not.
	for _, name := range []string{"sh", "bash", "zsh", "dash", "ksh", "ash"} {
		for _, field := range strings.Fields(strings.TrimPrefix(sh, "#!")) {
			if filepath.Base(field) == name {
				return true
			}
		}
	}
	return false
}

// porkchopBinary is the absolute path of the running binary, baked into the hook so
// an install from a non-PATH location still works. It is a preference, not a
// requirement: the hook falls back to PATH if the path stops resolving, which is
// what happens after `go install` replaces it elsewhere.
func porkchopBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// Resolve symlinks so a Homebrew-style shim does not bake in a path that a
	// later upgrade repoints.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// hookBlock is the shell porkchop installs. bin is baked in as the preferred
// binary; see porkchopBinary.
func hookBlock(bin string) string {
	return hookBegin + `
# Warm porkchop's review cache for the commit just made, so opening porkchop is
# instant instead of a wait on the model. Written by "porkchop hook install";
# remove it with "porkchop hook uninstall" rather than by hand.
porkchop_warm_cache() {
	porkchop_bin=` + shellQuote(bin) + `
	if [ ! -x "$porkchop_bin" ]; then
		porkchop_bin=$(command -v porkchop 2>/dev/null) || porkchop_bin=''
	fi
	if [ -z "$porkchop_bin" ]; then
		return 0
	fi
	porkchop_git_dir=$(git rev-parse --git-dir 2>/dev/null) || return 0
	# No replay guard here, deliberately. git does not run post-commit during a
	# rebase or a cherry-pick, so there is no storm of model calls to prevent
	# (measured, not assumed). It does run for --amend and for merge commits, and a
	# reading diff is worth having for both.
	porkchop_log="$porkchop_git_dir/` + hookLog + `"
	# The log is a breadcrumb trail, not a record; cap it rather than grow forever.
	if [ -f "$porkchop_log" ] && [ "$(wc -c <"$porkchop_log")" -gt 1048576 ]; then
		: >"$porkchop_log"
	fi
	# Detached, with stdin closed and both streams in the log: git waits for the
	# hook, so anything still holding the terminal would hold up the commit.
	( nohup "$porkchop_bin" process HEAD >>"$porkchop_log" 2>&1 & ) </dev/null
	return 0
}
# Never let warming the cache fail a commit.
porkchop_warm_cache || true
` + hookEnd
}

// shellQuote renders s as a single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
