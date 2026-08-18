package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brandonbosch/porkchop/internal/gitx"
	"github.com/brandonbosch/porkchop/internal/store"
	"github.com/brandonbosch/porkchop/meat"
)

// binPath is the porkchop binary under test, built once by TestMain. The workflow
// commands are exercised as a subprocess rather than by calling their functions:
// exit codes and the "a hook must never fail a commit" property are what they are
// for, and neither is observable from inside the process.
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "porkchop-cli")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binPath = filepath.Join(dir, "porkchop")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	build := exec.Command("go", "build", "-o", binPath, "github.com/brandonbosch/porkchop/cmd/porkchop")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building porkchop: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// result is one subprocess run.
type result struct {
	stdout, stderr string
	code           int
}

func (r result) all() string { return r.stdout + r.stderr }

// runIn runs porkchop in dir with the given cache directory, and with every API key
// stripped from the environment. Stripping them is the point: a test that passes
// only because a real model was reachable is not a test, and a cache-hit test is
// only meaningful if a miss would have failed.
func runIn(t *testing.T, dir, cache string, args ...string) result {
	t.Helper()
	return runInEnv(t, dir, cache, nil, args...)
}

// runInEnv is runIn with extra environment entries, for the tests that need to
// describe a broken credential setup rather than merely an absent one.
func runInEnv(t *testing.T, dir, cache string, extraEnv []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	cmd.Env = append(strippedEnv(), "MEAT_CACHE="+cache, "PORKCHOP_MODEL="+testModel)
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running porkchop %v: %v", args, err)
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// testModel is the model id these tests pin. It has to be a real-looking
// Bedrock inference profile because the id is part of the cache key: a cache
// hit means knowing which model produced the entry, even though none of these
// tests can reach one. Nothing here has credentials, so no request is possible.
const testModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"

func strippedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch key := strings.SplitN(kv, "=", 2)[0]; key {
		case "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "MEAT_CACHE", "MEAT_MODEL":
			continue
		default:
			// A developer's own PORKCHOP_* settings would otherwise choose a
			// different backend, and with it a different cache key.
			if strings.HasPrefix(key, "PORKCHOP_") {
				continue
			}
		}
		env = append(env, kv)
	}
	return env
}

// newRepo builds a throwaway repo with two commits on main and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "-c", "init.defaultBranch=main", "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	write(t, dir, "a.txt", "one\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "first")

	write(t, dir, "a.txt", "one\ntwo\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "second")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedCache stores a result under the exact key porkchop will compute for the given
// selection, so the rest of a test can run with no model reachable. The diff is read
// through gitx itself rather than by reconstructing git's command line, so the key
// cannot drift from the one the binary derives.
func seedCache(t *testing.T, repo, cache string, args []string) (key, smart string) {
	t.Helper()
	t.Chdir(repo)
	diff, source, err := gitx.ReadDiff(args, false, false)
	if err != nil {
		t.Fatalf("reading the diff for %v: %v", args, err)
	}
	if strings.TrimSpace(diff) == "" {
		t.Fatalf("no diff for %v (%s)", args, source)
	}
	smart = "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1,1 +1,2 @@\n+SEEDED-READING-DIFF\n"
	key = store.Key(diff, testModel, meat.RubricHash())
	store.Store(cache, key, &meat.Result{Summary: "SEEDED-SUMMARY", SmartDiff: smart})
	if _, ok := store.Load(cache, key); !ok {
		t.Fatal("seeding the cache did not take")
	}
	return key, smart
}

// TestProcessEmptyDiffIsSuccess is the property a hook depends on. A commit that
// changed nothing, or a clean worktree, must not make the workflow command fail —
// while the interactive review command still says there is nothing to look at,
// because a human who asked to read a diff should be told there isn't one.
func TestProcessEmptyDiffIsSuccess(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()

	got := runIn(t, repo, cache, "process", "-w")
	if got.code != 0 {
		t.Errorf("process on a clean worktree exited %d: %s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "nothing to read") {
		t.Errorf("process did not say there was nothing to read: %q", got.stdout)
	}

	review := runIn(t, repo, cache, "-w")
	if review.code == 0 {
		t.Error("the review command succeeded with no diff to review")
	}
}

// TestProcessCacheHitCostsNothing is the whole point of the command: after the work
// is done once, it is free — and provably so, since no model is reachable here.
func TestProcessCacheHitCostsNothing(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()

	// With nothing cached and no credentials, process must fail. This is the
	// control: it proves the pass below is a cache hit and not a live call.
	miss := runIn(t, repo, cache, "process")
	if miss.code == 0 {
		t.Fatalf("process succeeded with an empty cache and no credentials: %s", miss.all())
	}

	key, _ := seedCache(t, repo, cache, nil)

	hit := runIn(t, repo, cache, "process")
	if hit.code != 0 {
		t.Fatalf("process on a cache hit exited %d: %s", hit.code, hit.all())
	}
	if !strings.Contains(hit.stdout, "cached") {
		t.Errorf("process did not report a cache hit: %q", hit.stdout)
	}
	// The line a hook log accumulates should name the commit it warmed.
	if !strings.Contains(hit.stdout, "second") {
		t.Errorf("process did not name the commit: %q", hit.stdout)
	}

	js := runIn(t, repo, cache, "process", "-json")
	if js.code != 0 {
		t.Fatalf("process -json exited %d: %s", js.code, js.all())
	}
	var report processReport
	if err := json.Unmarshal([]byte(js.stdout), &report); err != nil {
		t.Fatalf("process -json emitted unparseable output %q: %v", js.stdout, err)
	}
	if !report.Cached {
		t.Error("-json did not report the cache hit")
	}
	if report.Key != key {
		t.Errorf("-json key = %q, want %q", report.Key, key)
	}
	if report.Summary != "SEEDED-SUMMARY" {
		t.Errorf("-json summary = %q, want the cached one", report.Summary)
	}
	if report.Model == "" {
		t.Error("-json did not report the resolved model, which participates in the key")
	}

	// -q is for hooks that only want to hear about problems.
	quiet := runIn(t, repo, cache, "process", "-q")
	if quiet.code != 0 || quiet.stdout != "" {
		t.Errorf("process -q printed %q and exited %d", quiet.stdout, quiet.code)
	}
}

// TestReviewRangeFromCache is range review end to end: a range is one aggregate diff,
// it is cached under its own key, and reviewing it renders the reading diff. This is
// the shape an agent session is reviewed in — one range across many commits.
func TestReviewRangeFromCache(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()

	// A branch with two more commits, so main...HEAD spans more than one commit and
	// is a genuinely different diff from either commit alone.
	runGit(t, repo, "checkout", "-q", "-b", "agent-session")
	write(t, repo, "b.txt", "alpha\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "agent commit one")
	write(t, repo, "b.txt", "alpha\nbeta\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "agent commit two")

	const rangeArg = "main...HEAD"
	_, smart := seedCache(t, repo, cache, []string{rangeArg})

	// -plain ahead of the range: Go's flag package stops at the first non-flag
	// argument, the same as meat's CLI.
	got := runIn(t, repo, cache, "-plain", rangeArg)
	if got.code != 0 {
		t.Fatalf("reviewing %s exited %d: %s", rangeArg, got.code, got.all())
	}
	if !strings.Contains(got.stdout, "SEEDED-SUMMARY") {
		t.Errorf("range review did not print the summary:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "SEEDED-READING-DIFF") {
		t.Errorf("range review did not print the reading diff:\n%s", got.stdout)
	}
	_ = smart

	// The range's key must not be the key of its tip commit, or reviewing one would
	// silently serve the other.
	tipKey, _ := seedCache(t, repo, cache, []string{"HEAD"})
	rangeKey, _ := seedCache(t, repo, cache, []string{rangeArg})
	if tipKey == rangeKey {
		t.Error("a range and its tip commit share a cache key")
	}

	// A range with two dots is the other spelling and must also work.
	if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
		twoDot := runIn(t, repo, cache, "process", "-json", "main..HEAD")
		if twoDot.code != 0 {
			// No cache entry for this one, so a failure is expected — but it must be
			// a model failure, not an argument-parsing one.
			if strings.Contains(twoDot.all(), "too many arguments") || strings.Contains(twoDot.all(), "reading \"main..HEAD\"") {
				t.Errorf("two-dot range was not understood: %s", twoDot.all())
			}
		}
	}
}

// TestSubcommandDispatch checks the reserved words do not shadow revisions beyond
// repair, and that each command has its own help.
func TestSubcommandDispatch(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()

	for _, args := range [][]string{{"process", "-h"}, {"hook", "-h"}} {
		got := runIn(t, repo, cache, args...)
		if got.code != 0 {
			t.Errorf("porkchop %v exited %d: %s", args, got.code, got.all())
		}
	}

	// "hook" with no verb is a usage error, not a silent no-op.
	if got := runIn(t, repo, cache, "hook"); got.code == 0 {
		t.Errorf("bare `hook` succeeded: %s", got.all())
	}

	// A branch really named "process" is reachable behind --. It does not exist
	// here, so what matters is that porkchop tried to read it as a revision rather
	// than running the subcommand.
	got := runIn(t, repo, cache, "--", "process")
	if got.code == 0 {
		t.Errorf("reviewing a nonexistent revision succeeded: %s", got.all())
	}
	if !strings.Contains(got.all(), "process") {
		t.Errorf("-- did not route `process` to the review command: %s", got.all())
	}
	if strings.Contains(got.all(), "nothing to read") {
		t.Errorf("-- still ran the process subcommand: %s", got.all())
	}
}

// TestHookLifecycle covers install, re-install, status and uninstall, including
// coexistence with someone else's hook — the case that decides whether this command
// is safe to run in a repo you did not set up.
func TestHookLifecycle(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()
	hookPath := filepath.Join(repo, ".git", "hooks", "post-commit")

	if got := runIn(t, repo, cache, "hook", "status"); !strings.Contains(got.stdout, "not installed") {
		t.Errorf("status on a fresh repo: %q", got.all())
	}

	if got := runIn(t, repo, cache, "hook", "install"); got.code != 0 {
		t.Fatalf("install exited %d: %s", got.code, got.all())
	}
	body := readFile(t, hookPath)
	if !strings.Contains(body, hookBegin) {
		t.Fatalf("installed hook has no porkchop block:\n%s", body)
	}
	assertShellParses(t, hookPath)
	if info, err := os.Stat(hookPath); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("hook is not executable (%v, %v); git would ignore it", info.Mode(), err)
	}
	if got := runIn(t, repo, cache, "hook", "status"); !strings.Contains(got.stdout, "installed in") {
		t.Errorf("status after install: %q", got.all())
	}

	// Re-installing must not stack blocks.
	runIn(t, repo, cache, "hook", "install")
	if got := strings.Count(readFile(t, hookPath), hookBegin); got != 1 {
		t.Errorf("re-install left %d blocks, want 1", got)
	}

	// A porkchop-only hook is removed entirely rather than left as an empty stub.
	if got := runIn(t, repo, cache, "hook", "uninstall"); got.code != 0 {
		t.Fatalf("uninstall exited %d: %s", got.code, got.all())
	}
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Errorf("uninstall left %s behind", hookPath)
	}
	// Uninstalling twice is not an error.
	if got := runIn(t, repo, cache, "hook", "uninstall"); got.code != 0 {
		t.Errorf("second uninstall exited %d: %s", got.code, got.all())
	}

	// Now the case that matters: an existing hook someone else wrote.
	const theirs = "#!/bin/bash\n# their hook\necho THEIR-HOOK-RAN\n"
	if err := os.WriteFile(hookPath, []byte(theirs), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runIn(t, repo, cache, "hook", "install"); got.code != 0 {
		t.Fatalf("install over an existing hook exited %d: %s", got.code, got.all())
	}
	assertShellParses(t, hookPath)
	if !strings.Contains(readFile(t, hookPath), "THEIR-HOOK-RAN") {
		t.Error("install destroyed the existing hook")
	}
	// And their hook still runs.
	write(t, repo, "a.txt", "one\ntwo\nthree\n")
	runGit(t, repo, "add", ".")
	if out := runGit(t, repo, "commit", "-m", "third"); !strings.Contains(out, "THEIR-HOOK-RAN") {
		t.Errorf("the existing hook stopped running after install:\n%s", out)
	}
	if got := runIn(t, repo, cache, "hook", "uninstall"); got.code != 0 {
		t.Fatalf("uninstall exited %d: %s", got.code, got.all())
	}
	if left := readFile(t, hookPath); !strings.Contains(left, "THEIR-HOOK-RAN") || strings.Contains(left, "porkchop") {
		t.Errorf("uninstall did not leave exactly their hook behind:\n%s", left)
	}

	// A hook porkchop cannot recognize as shell is refused rather than corrupted.
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env python3\nprint('hi')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := runIn(t, repo, cache, "hook", "install")
	if got.code == 0 {
		t.Error("install into a python hook succeeded")
	}
	if !strings.Contains(got.stderr, "-force") {
		t.Errorf("the refusal does not say how to override it: %q", got.stderr)
	}
	if !strings.Contains(readFile(t, hookPath), "print('hi')") {
		t.Error("the refused install modified the file anyway")
	}

	// The realistic -force case is a hook with no shebang, which is shell in spirit
	// but which git cannot execute as written. Forcing must produce a hook that
	// actually runs, keeping what was there — including the flag after the verb,
	// which is the order a person types.
	if err := os.WriteFile(hookPath, []byte("echo THEIR-BARE-HOOK\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runIn(t, repo, cache, "hook", "install", "-force"); got.code != 0 {
		t.Fatalf("-force install exited %d: %s", got.code, got.all())
	}
	forced := readFile(t, hookPath)
	if !strings.HasPrefix(forced, "#!") {
		t.Errorf("-force left a hook git cannot execute:\n%s", forced)
	}
	if !strings.Contains(forced, "THEIR-BARE-HOOK") {
		t.Errorf("-force discarded the existing content:\n%s", forced)
	}
	assertShellParses(t, hookPath)
}

// TestHookHonorsHooksPath checks porkchop asks git where hooks live instead of
// assuming .git/hooks. Guessing installs a hook git will never run, which fails
// silently — the worst outcome for a tool whose job is to have already finished.
func TestHookHonorsHooksPath(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()
	runGit(t, repo, "config", "core.hooksPath", ".githooks")

	if got := runIn(t, repo, cache, "hook", "install"); got.code != 0 {
		t.Fatalf("install exited %d: %s", got.code, got.all())
	}
	if _, err := os.Stat(filepath.Join(repo, ".githooks", "post-commit")); err != nil {
		t.Errorf("hook not written to the configured hooks path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "hooks", "post-commit")); err == nil {
		t.Error("hook was written to .git/hooks, which git is configured to ignore")
	}
	if got := runIn(t, repo, cache, "hook", "status"); !strings.Contains(got.stdout, ".githooks") {
		t.Errorf("status does not report the configured path: %q", got.all())
	}
}

// TestHookNeitherDelaysNorFailsTheCommit is the load-bearing behavioural test. A
// review tool that slows down or breaks `git commit` gets uninstalled inside a week,
// so both properties are checked against a stand-in binary that is slow and then one
// that fails.
func TestHookNeitherDelaysNorFailsTheCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installed hook is a POSIX shell script")
	}
	repo, cache := newRepo(t), t.TempDir()
	if got := runIn(t, repo, cache, "hook", "install"); got.code != 0 {
		t.Fatalf("install exited %d: %s", got.code, got.all())
	}
	hookPath := filepath.Join(repo, ".git", "hooks", "post-commit")

	// A stand-in for porkchop that takes far longer than any commit should wait, and
	// then fails. If the hook waits on it, the commit takes seconds; if the hook
	// propagates its status, the commit fails.
	slow := filepath.Join(t.TempDir(), "slow-porkchop")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 10\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, hookPath)
	patched := strings.Replace(body, shellQuote(porkchopBinaryFromHook(t, body)), shellQuote(slow), 1)
	if patched == body {
		t.Fatal("could not point the hook at the stand-in binary")
	}
	if err := os.WriteFile(hookPath, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
	assertShellParses(t, hookPath)

	write(t, repo, "a.txt", "one\ntwo\nthree\n")
	runGit(t, repo, "add", ".")
	cmd := exec.Command("git", "commit", "-m", "with a slow hook")
	cmd.Dir = repo
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("the hook failed the commit: %v\n%s", err, out)
	}
	// The stand-in sleeps 10s; anything under a couple of seconds proves the model
	// call was detached rather than waited on.
	if elapsed > 3*time.Second {
		t.Errorf("the commit waited %s on the hook", elapsed.Round(time.Millisecond))
	}
	if head := strings.TrimSpace(runGit(t, repo, "log", "-1", "--format=%s")); head != "with a slow hook" {
		t.Errorf("the commit did not land: HEAD is %q", head)
	}
}

// porkchopBinaryFromHook pulls the baked binary path back out of an installed hook,
// so a test can repoint it without knowing where the build put it.
func porkchopBinaryFromHook(t *testing.T, body string) string {
	t.Helper()
	const prefix = "porkchop_bin='"
	i := strings.Index(body, prefix)
	if i < 0 {
		t.Fatalf("no baked binary path in the hook:\n%s", body)
	}
	rest := body[i+len(prefix):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatalf("unterminated binary path in the hook:\n%s", body)
	}
	return rest[:j]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertShellParses runs the hook through `sh -n`, since a syntax error in generated
// shell would only ever surface as a broken commit on someone else's machine.
func assertShellParses(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Errorf("generated hook is not valid shell: %v\n%s\n%s", err, out, readFile(t, path))
	}
}

// TestOpenViewed covers the adapter between the marker file and the review screen —
// the one piece of the viewed-marker path that neither package's tests can see.
func TestOpenViewed(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("MEAT_CACHE", cache)

	// Outside a repo there is no per-repo file to key markers to, and the screen must
	// be handed nil rather than a store that silently drops everything.
	t.Chdir(t.TempDir())
	if got := openViewed(); got != nil {
		t.Errorf("openViewed outside a repo = %v, want nil", got)
	}

	repo := newRepo(t)
	t.Chdir(repo)
	vs := openViewed()
	if vs == nil {
		t.Fatal("openViewed inside a repo returned nil")
	}
	if vs.Has("a.txt", "digest-1") {
		t.Error("a fresh repo reports a file as viewed")
	}
	vs.Set("a.txt", "digest-1", true)
	vs.Save()

	// A second open is a different process's worth of state: the marker must be there,
	// and must not match a different version of the same file.
	again := openViewed()
	if !again.Has("a.txt", "digest-1") {
		t.Error("the marker did not survive reopening the store")
	}
	if again.Has("a.txt", "digest-2") {
		t.Error("the marker matched a different version of the file")
	}

	// Caching disabled means markers have nowhere to live at all.
	t.Setenv("MEAT_CACHE", "")
	if got := openViewed(); got != nil {
		t.Errorf("openViewed with caching disabled = %v, want nil", got)
	}
}

// TestSubcommandVerbHint covers the diagnostic for a real thing people type. The
// verbs belong to their command — it is `porkchop hook status`, not `porkchop
// status` — so a bare verb is read as a revision and git answers with a wall of
// "ambiguous argument" text that says nothing about what was actually wanted.
// Reserving the verbs would shadow branches of those names; hinting shadows nothing.
func TestSubcommandVerbHint(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()

	for _, verb := range []string{"status", "install", "uninstall"} {
		got := runIn(t, repo, cache, verb)
		if got.code == 0 {
			t.Errorf("porkchop %s succeeded as a revision: %s", verb, got.all())
			continue
		}
		want := fmt.Sprintf("porkchop hook %s", verb)
		if !strings.Contains(got.stderr, want) {
			t.Errorf("porkchop %s does not suggest %q:\n%s", verb, want, got.stderr)
		}
		if !strings.Contains(got.stderr, "not a revision") {
			t.Errorf("porkchop %s does not explain why it failed:\n%s", verb, got.stderr)
		}
	}

	// The same hint reaches the process command, which takes a revision too.
	if got := runIn(t, repo, cache, "process", "status"); !strings.Contains(got.stderr, "porkchop hook status") {
		t.Errorf("process does not hint on a subcommand verb:\n%s", got.stderr)
	}

	// A revision that is simply wrong gets no hint — an unconditional suggestion
	// would be noise on every typo.
	got := runIn(t, repo, cache, "no-such-rev")
	if got.code == 0 {
		t.Fatal("a nonexistent revision succeeded")
	}
	if strings.Contains(got.stderr, "did you mean") {
		t.Errorf("hinted on an ordinary bad revision:\n%s", got.stderr)
	}

	// A branch really named "status" resolves, and is reviewed rather than hinted at.
	runGit(t, repo, "branch", "status")
	seedCache(t, repo, cache, []string{"status"})
	if got := runIn(t, repo, cache, "-plain", "status"); got.code != 0 {
		t.Errorf("a branch named status could not be reviewed: %s", got.all())
	}
}

// TestHelpVerb checks `porkchop help` works, since it is what someone types before
// they know that -h is the flag. It goes to stdout so it can be piped to a pager.
// TestBedrockWithoutCredentialsRefusesRatherThanFallingBack exercises the whole
// wiring, not just internal/model: a real binary, a real cache miss, an
// ANTHROPIC_API_KEY sitting in the environment the way a laptop used for home
// projects would have one, and no AWS credentials at all.
//
// fantasy's Bedrock path swallows a credential-loading error and leaves a plain
// Anthropic client aimed at api.anthropic.com, so the failure this guards
// against is not a crash — it is a run that looks like it worked while sending
// a CUI diff to a public API. The assertion is therefore about what did *not*
// happen: a non-zero exit, and nothing written to the cache.
func TestBedrockWithoutCredentialsRefusesRatherThanFallingBack(t *testing.T) {
	repo := newRepo(t)
	cache := t.TempDir()
	awsHome := t.TempDir()

	res := runInEnv(t, repo, cache, []string{
		"ANTHROPIC_API_KEY=sk-ant-this-must-never-be-used",
		"AWS_CONFIG_FILE=" + filepath.Join(awsHome, "config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(awsHome, "credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_REGION=us-east-1",
		"AWS_ACCESS_KEY_ID=",
		"AWS_SECRET_ACCESS_KEY=",
		"AWS_PROFILE=",
	}, "process", "HEAD", "-provider", "bedrock")

	if res.code == 0 {
		t.Fatalf("process succeeded with no AWS credentials; that means it reached something else:\n%s", res.all())
	}
	if !strings.Contains(res.all(), "bedrock") {
		t.Errorf("error does not name bedrock:\n%s", res.all())
	}
	if strings.Contains(res.all(), "sk-ant-") {
		t.Errorf("the API key leaked into the error:\n%s", res.all())
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cache has %d entries, want none: a refused run must not store a result", len(entries))
	}
}

func TestHelpVerb(t *testing.T) {
	repo, cache := newRepo(t), t.TempDir()
	got := runIn(t, repo, cache, "help")
	if got.code != 0 {
		t.Fatalf("porkchop help exited %d: %s", got.code, got.all())
	}
	if !strings.Contains(got.stdout, "Commands:") {
		t.Errorf("help does not list the commands:\n%s", got.stdout)
	}
	if got.stderr != "" {
		t.Errorf("help wrote to stderr: %q", got.stderr)
	}
}
