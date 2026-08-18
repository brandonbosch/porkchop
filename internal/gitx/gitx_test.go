package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a throwaway repo and chdir's into it, since gitx reads the diff by
// running git in the current directory.
//
// History: main has two commits touching a.txt, then a branch adds two commits
// touching b.txt while main moves on again. That shape is what makes the two range
// spellings differ, which is the thing range review actually depends on.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)

	run(t, "-c", "init.defaultBranch=main", "init", "-q")
	run(t, "config", "user.email", "test@example.com")
	run(t, "config", "user.name", "Test")
	run(t, "config", "commit.gpgsign", "false")

	commit(t, "a.txt", "one\n", "first")
	commit(t, "a.txt", "one\ntwo\n", "second")

	run(t, "checkout", "-q", "-b", "agent-session")
	commit(t, "b.txt", "alpha\n", "agent one")
	commit(t, "b.txt", "alpha\nbeta\n", "agent two")

	run(t, "checkout", "-q", "main")
	commit(t, "c.txt", "meanwhile\n", "main moved on")
	run(t, "checkout", "-q", "agent-session")
	return dir
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func commit(t *testing.T, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "add", ".")
	run(t, "commit", "-q", "-m", msg)
}

func TestReadDiffHEAD(t *testing.T) {
	repo(t)
	diff, source, err := ReadDiff(nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != "HEAD" {
		t.Errorf("source = %q, want HEAD", source)
	}
	// `git show` of one commit: the commit's own change, and nothing else's.
	if !strings.Contains(diff, "b/b.txt") {
		t.Errorf("HEAD's diff does not mention b.txt:\n%s", diff)
	}
	if strings.Contains(diff, "b/a.txt") {
		t.Errorf("HEAD's diff leaked an earlier commit's file:\n%s", diff)
	}
	if !strings.Contains(diff, "+beta") {
		t.Errorf("HEAD's diff does not contain its added line:\n%s", diff)
	}
}

func TestReadDiffExplicitRevision(t *testing.T) {
	repo(t)
	diff, source, err := ReadDiff([]string{"HEAD~1"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != "HEAD~1" {
		t.Errorf("source = %q, want HEAD~1", source)
	}
	if !strings.Contains(diff, "+alpha") {
		t.Errorf("HEAD~1's diff is not the commit asked for:\n%s", diff)
	}
	if strings.Contains(diff, "+beta") {
		t.Errorf("HEAD~1's diff includes HEAD's change:\n%s", diff)
	}
}

// TestReadDiffRangeSpellings is the substance of range review. `..` diffs the two
// tips, so anything main gained since the branch point shows up inverted — c.txt
// appears as a deletion, which is not something the agent did. `...` diffs from the
// merge base, which is "what this session changed". Both must work, and they must
// not be the same diff, or the distinction porkchop offers is imaginary.
func TestReadDiffRangeSpellings(t *testing.T) {
	repo(t)

	threeDot, source, err := ReadDiff([]string{"main...HEAD"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != "main...HEAD" {
		t.Errorf("source = %q, want the range", source)
	}
	// The whole session, in one diff.
	for _, want := range []string{"+alpha", "+beta", "b/b.txt"} {
		if !strings.Contains(threeDot, want) {
			t.Errorf("main...HEAD is missing %q:\n%s", want, threeDot)
		}
	}
	if strings.Contains(threeDot, "c.txt") {
		t.Errorf("main...HEAD included main's own later commit:\n%s", threeDot)
	}

	twoDot, _, err := ReadDiff([]string{"main..HEAD"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(twoDot, "c.txt") {
		t.Errorf("main..HEAD does not show main's divergence, so it is not a tip-to-tip diff:\n%s", twoDot)
	}
	if twoDot == threeDot {
		t.Error("the two range spellings produced identical diffs on diverged history")
	}

	// A range must be one aggregate diff, not git show's per-commit output — which
	// would repeat file headers and confuse everything downstream.
	if got := strings.Count(threeDot, "diff --git a/b.txt"); got != 1 {
		t.Errorf("main...HEAD emitted %d headers for b.txt, want 1 (per-commit output?)", got)
	}
	if strings.Contains(threeDot, "commit ") {
		t.Errorf("a range diff carries commit metadata:\n%s", threeDot)
	}
}

// TestReadDiffMergeCommit covers the case plain `git show` gets wrong: a merge commit
// shows no diff at all without -m, so porkchop would report "nothing to read" for a
// merge — exactly the commit a reviewer most wants explained.
func TestReadDiffMergeCommit(t *testing.T) {
	repo(t)
	run(t, "checkout", "-q", "main")
	run(t, "merge", "-q", "--no-ff", "agent-session", "-m", "merge the session")

	diff, _, err := ReadDiff(nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(diff) == "" {
		t.Fatal("a merge commit produced an empty diff")
	}
	if !strings.Contains(diff, "b/b.txt") {
		t.Errorf("the merge's diff against its first parent is missing b.txt:\n%s", diff)
	}
}

func TestReadDiffStagedAndWorktree(t *testing.T) {
	repo(t)

	// Clean tree: both selections are empty, and empty is not an error here — the
	// caller decides what to do about it.
	for _, tc := range []struct{ staged, worktree bool }{{true, false}, {false, true}} {
		diff, _, err := ReadDiff(nil, tc.staged, tc.worktree)
		if err != nil {
			t.Fatalf("staged=%v worktree=%v: %v", tc.staged, tc.worktree, err)
		}
		if strings.TrimSpace(diff) != "" {
			t.Errorf("staged=%v worktree=%v on a clean tree returned:\n%s", tc.staged, tc.worktree, diff)
		}
	}

	if err := os.WriteFile("b.txt", []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree, _, err := ReadDiff(nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(worktree, "+gamma") {
		t.Errorf("-w did not see the unstaged edit:\n%s", worktree)
	}
	staged, _, err := ReadDiff(nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(staged, "+gamma") {
		t.Errorf("-staged saw an unstaged edit:\n%s", staged)
	}

	run(t, "add", "b.txt")
	staged, _, err = ReadDiff(nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(staged, "+gamma") {
		t.Errorf("-staged did not see the staged edit:\n%s", staged)
	}
	worktree, _, err = ReadDiff(nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(worktree, "+gamma") {
		t.Errorf("-w still saw the edit after it was staged:\n%s", worktree)
	}
}

func TestReadDiffArgumentErrors(t *testing.T) {
	repo(t)
	tests := map[string]struct {
		args             []string
		staged, worktree bool
	}{
		"staged and worktree together": {nil, true, true},
		"staged with a revision":       {[]string{"HEAD"}, true, false},
		"worktree with a revision":     {[]string{"HEAD"}, false, true},
		"two revisions":                {[]string{"HEAD", "HEAD~1"}, false, false},
	}
	for name, tc := range tests {
		if _, _, err := ReadDiff(tc.args, tc.staged, tc.worktree); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A revision that does not exist is an error naming it, not a panic or silence.
	_, _, err := ReadDiff([]string{"no-such-rev"}, false, false)
	if err == nil {
		t.Fatal("a nonexistent revision produced no error")
	}
	if !strings.Contains(err.Error(), "no-such-rev") {
		t.Errorf("the error does not name the revision: %v", err)
	}
}

func TestIsRevRange(t *testing.T) {
	ranges := []string{"a..b", "a...b", "main...HEAD", "HEAD~3..HEAD"}
	for _, r := range ranges {
		if !isRevRange(r) {
			t.Errorf("%q not treated as a range", r)
		}
	}
	singles := []string{"HEAD", "HEAD~3", "main", "abc123", "v1.2.3", "refs/heads/main"}
	for _, s := range singles {
		if isRevRange(s) {
			t.Errorf("%q wrongly treated as a range", s)
		}
	}
}

func TestRootAndGitPaths(t *testing.T) {
	dir := repo(t)

	// macOS hands out /var symlinks for temp dirs, so compare resolved paths.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(Root())
	if err != nil {
		t.Fatalf("Root() = %q: %v", Root(), err)
	}
	if got != want {
		t.Errorf("Root() = %q, want %q", got, want)
	}

	gitDir, err := GitDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(gitDir) {
		t.Errorf("GitDir() = %q, want an absolute path", gitDir)
	}
	if _, err := os.Stat(gitDir); err != nil {
		t.Errorf("GitDir() = %q, which does not exist: %v", gitDir, err)
	}

	hooks, err := HooksDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(hooks) {
		t.Errorf("HooksDir() = %q, want an absolute path", hooks)
	}
	if filepath.Base(hooks) != "hooks" {
		t.Errorf("HooksDir() = %q, want it to end in hooks", hooks)
	}

	// The point of asking git rather than assuming .git/hooks.
	run(t, "config", "core.hooksPath", ".myhooks")
	relocated, err := HooksDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(relocated) != ".myhooks" {
		t.Errorf("HooksDir() = %q, want the configured .myhooks", relocated)
	}
}

func TestRootOutsideARepo(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := Root(); got != "" {
		t.Errorf("Root() = %q outside a repo, want empty", got)
	}
	if _, err := GitDir(); err == nil {
		t.Error("GitDir() succeeded outside a repo")
	}
	if _, err := HooksDir(); err == nil {
		t.Error("HooksDir() succeeded outside a repo")
	}
}

func TestDescribe(t *testing.T) {
	repo(t)
	got := Describe("HEAD")
	if got == "" {
		t.Fatal("Describe(HEAD) is empty")
	}
	if !strings.Contains(got, "agent two") {
		t.Errorf("Describe(HEAD) = %q, want it to name the subject", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("Describe returned more than one line: %q", got)
	}
	// A range and a bad revision have nothing to describe, and must say so quietly
	// rather than erroring — the caller has its own label to fall back on.
	for _, rev := range []string{"main...HEAD", "no-such-rev", ""} {
		if got := Describe(rev); got != "" {
			t.Errorf("Describe(%q) = %q, want empty", rev, got)
		}
	}
}
