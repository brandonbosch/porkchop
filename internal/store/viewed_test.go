package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestViewedRoundTrip(t *testing.T) {
	dir, repo := t.TempDir(), "/src/project"

	v := LoadViewed(dir, repo)
	if v.Has("a.py", "d1") {
		t.Fatal("a fresh store reports a file as viewed")
	}
	v.Set("a.py", "d1", true)
	v.Set("b.py", "d2", true)
	StoreViewed(dir, repo, v)

	got := LoadViewed(dir, repo)
	if !got.Has("a.py", "d1") || !got.Has("b.py", "d2") {
		t.Fatalf("markers did not survive a round trip: %+v", got.Files)
	}

	// Unmarking has to persist too, or a mis-click is permanent.
	got.Set("a.py", "d1", false)
	StoreViewed(dir, repo, got)
	if LoadViewed(dir, repo).Has("a.py", "d1") {
		t.Error("unmarked file came back viewed")
	}
}

// TestViewedIsPerDigest is the behaviour the whole design exists for: a marker is
// against one version of a file, so the file changing un-checks it while leaving
// every other file alone.
func TestViewedIsPerDigest(t *testing.T) {
	dir, repo := t.TempDir(), "/src/project"
	v := LoadViewed(dir, repo)
	v.Set("a.py", "old", true)
	v.Set("untouched.py", "same", true)
	StoreViewed(dir, repo, v)

	// The agent commits again: a.py's section of the diff moves, untouched.py's
	// does not.
	got := LoadViewed(dir, repo)
	if got.Has("a.py", "new") {
		t.Error("a changed file is still reported as viewed")
	}
	if !got.Has("untouched.py", "same") {
		t.Error("an unchanged file lost its marker when the diff grew")
	}

	// Re-reading it replaces the mark rather than accumulating versions.
	got.Set("a.py", "new", true)
	if got.Has("a.py", "old") {
		t.Error("the superseded digest still matches")
	}
	if !got.Has("a.py", "new") {
		t.Error("re-viewing did not take")
	}
	if len(got.Files) != 2 {
		t.Errorf("want 2 entries after re-viewing, got %d", len(got.Files))
	}
}

// TestViewedRejectsEmptyDigest pins the no-original degradation: with nothing to
// key a marker to, nothing may be recorded and nothing may read back as viewed.
// Marking unread content as read is the one failure this feature must not have.
func TestViewedRejectsEmptyDigest(t *testing.T) {
	v := &Viewed{Files: map[string]ViewedMark{}}
	v.Set("a.py", "", true)
	if len(v.Files) != 0 {
		t.Errorf("an empty digest was recorded: %+v", v.Files)
	}
	v.Set("a.py", "d1", true)
	if v.Has("a.py", "") {
		t.Error("an empty digest matched a real marker")
	}
	// A path with no marker at all must not match either.
	if v.Has("nope.py", "d1") {
		t.Error("an unmarked path reported as viewed")
	}
}

// TestViewedSeparatesCheckouts guards against two clones of the same project
// sharing markers — reviewing in one would silently check files off in the other.
func TestViewedSeparatesCheckouts(t *testing.T) {
	dir := t.TempDir()
	a, b := "/src/one/project", "/src/two/project"
	if ViewedPath(dir, a) == ViewedPath(dir, b) {
		t.Fatal("two checkouts of the same project name share a file")
	}
	v := LoadViewed(dir, a)
	v.Set("a.py", "d1", true)
	StoreViewed(dir, a, v)
	if LoadViewed(dir, b).Has("a.py", "d1") {
		t.Error("a marker leaked between checkouts")
	}

	// The human-readable half should still be there, and the whole name safe.
	name := filepath.Base(ViewedPath(dir, a))
	if !strings.HasPrefix(name, "project-") {
		t.Errorf("filename %q does not lead with the repo name", name)
	}
}

// TestViewedPathDisabled covers the two ways there is nowhere to write: caching
// turned off, and a diff that came from outside any repo.
func TestViewedPathDisabled(t *testing.T) {
	if p := ViewedPath("", "/src/project"); p != "" {
		t.Errorf("got %q with caching disabled, want no path", p)
	}
	if p := ViewedPath(t.TempDir(), ""); p != "" {
		t.Errorf("got %q with no repo root, want no path", p)
	}
	// Both halves must be no-ops rather than panics, since the review screen calls
	// them on every toggle regardless.
	dir := t.TempDir()
	v := LoadViewed("", "/src/project")
	v.Set("a.py", "d1", true)
	StoreViewed("", "/src/project", v)
	StoreViewed(dir, "", v)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d entries with nowhere to write", len(entries))
	}
}

// TestViewedSanitizesRepoName keeps a hostile or merely odd directory name from
// escaping the viewed directory.
func TestViewedSanitizesRepoName(t *testing.T) {
	dir := t.TempDir()
	for _, repo := range []string{"/src/..", "/src/a b/c:d", "/src/" + strings.Repeat("x", 200)} {
		p := ViewedPath(dir, repo)
		if p == "" {
			t.Errorf("%q: no path at all", repo)
			continue
		}
		want := filepath.Join(dir, "porkchop", "viewed")
		if got := filepath.Dir(p); got != want {
			t.Errorf("%q escaped the viewed dir: %s", repo, got)
		}
		if base := filepath.Base(p); strings.ContainsAny(base, "/: ") || strings.HasPrefix(base, ".") {
			t.Errorf("%q produced an unsafe filename %q", repo, base)
		}
	}
}

// TestViewedCorruptFileIsEmpty holds the "cache is never a hard dependency" line:
// a truncated or hand-edited file must degrade to no markers, not to a failed
// review session.
func TestViewedCorruptFileIsEmpty(t *testing.T) {
	dir, repo := t.TempDir(), "/src/project"
	path := ViewedPath(dir, repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"files": {"a.py": tru`), 0o600); err != nil {
		t.Fatal(err)
	}
	v := LoadViewed(dir, repo)
	if len(v.Files) != 0 {
		t.Errorf("corrupt file yielded %d markers", len(v.Files))
	}
	// And it must be recoverable by writing over it.
	v.Set("a.py", "d1", true)
	StoreViewed(dir, repo, v)
	if !LoadViewed(dir, repo).Has("a.py", "d1") {
		t.Error("could not write over a corrupt file")
	}
}

func TestViewedPrunes(t *testing.T) {
	now := time.Now().UTC()
	v := &Viewed{Files: map[string]ViewedMark{
		"fresh.py": {Digest: "d1", At: now.Add(-time.Hour)},
		"stale.py": {Digest: "d2", At: now.Add(-viewedRetention - time.Hour)},
	}}
	v.prune(now)
	if !v.Has("fresh.py", "d1") {
		t.Error("pruned a fresh marker")
	}
	if v.Has("stale.py", "d2") {
		t.Error("kept a marker past the retention window")
	}
}

func TestViewedCapsEntries(t *testing.T) {
	now := time.Now().UTC()
	v := &Viewed{Files: map[string]ViewedMark{}}
	// Older paths sort earlier in time, so the survivors should be the newest.
	for i := 0; i < viewedMax+50; i++ {
		v.Files["pkg/file"+strconv.Itoa(i)+".go"] =
			ViewedMark{Digest: "d", At: now.Add(-time.Duration(viewedMax+50-i) * time.Minute)}
	}
	before := len(v.Files)
	v.prune(now)
	if len(v.Files) != viewedMax {
		t.Fatalf("pruned %d entries down to %d, want %d", before, len(v.Files), viewedMax)
	}
	// The evicted ones must be the oldest, not an arbitrary map-order slice.
	oldest := now.Add(-time.Duration(viewedMax+50) * time.Minute)
	for p, m := range v.Files {
		if m.At.Before(oldest.Add(50 * time.Minute)) {
			t.Fatalf("kept %s (%s) while evicting newer entries", p, m.At)
		}
	}
}
