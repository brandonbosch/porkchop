package diffview

import (
	"strings"
	"testing"
)

// TestDigestsCoverEveryRawFile checks the identity map is complete and keyed the
// way callers will look things up: one entry per file the original diff touches,
// under the same path the row model reports.
func TestDigestsCoverEveryRawFile(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			raw, reading := pair[0], pair[1]
			a := Align(raw, reading)

			// The paths the alignment labels raw lines with are the authority; the
			// digest map must have exactly those keys.
			want := map[string]bool{}
			for _, f := range rawFileNames(Parse(raw)) {
				if f != "" {
					want[f] = true
				}
			}
			if len(want) == 0 {
				t.Fatal("fixture names no files")
			}
			if len(a.Digests) != len(want) {
				t.Fatalf("digests cover %d files, raw diff has %d", len(a.Digests), len(want))
			}
			for path := range want {
				d, ok := a.Digests[path]
				if !ok {
					t.Errorf("no digest for %q", path)
					continue
				}
				if len(d) != 64 {
					t.Errorf("digest for %q is %d chars, want 64 hex", path, len(d))
				}
			}
		})
	}
}

// TestDigestsAreStable is the property the whole feature rests on: the same file
// section hashes the same every time, so a marker written in one session is
// recognized in the next.
func TestDigestsAreStable(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			first := Align(pair[0], pair[1]).Digests
			second := FileDigests(pair[0])
			if len(first) != len(second) {
				t.Fatalf("Alignment.Digests has %d entries, FileDigests %d", len(first), len(second))
			}
			for path, d := range first {
				if second[path] != d {
					t.Errorf("%s: %q vs %q", path, d, second[path])
				}
			}
		})
	}
}

// TestDigestsIsolateTheChangedFile is the agent-session case in miniature: edit
// one file's section of the diff and only that file's identity may move. If a
// neighbouring file's digest shifted too, a reviewer's checked-off files would
// un-check themselves every time the agent touched anything.
func TestDigestsIsolateTheChangedFile(t *testing.T) {
	const raw = `diff --git a/a.py b/a.py
index 1111111..2222222 100644
--- a/a.py
+++ b/a.py
@@ -1,3 +1,3 @@
 import os
-x = 1
+x = 2
diff --git a/b.py b/b.py
index 3333333..4444444 100644
--- a/b.py
+++ b/b.py
@@ -1,3 +1,3 @@
 import sys
-y = 1
+y = 2
`
	before := FileDigests(raw)
	if len(before) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(before), before)
	}

	// A later commit revisits b.py only.
	after := FileDigests(strings.Replace(raw, "+y = 2", "+y = 3", 1))
	if after["a.py"] != before["a.py"] {
		t.Error("a.py's digest moved though its section did not")
	}
	if after["b.py"] == before["b.py"] {
		t.Error("b.py's digest did not move though its section did")
	}
}

// TestDigestsBindThePath guards the rename case: two files whose sections are
// byte-identical apart from their headers must not share one marker.
func TestDigestsBindThePath(t *testing.T) {
	section := func(path string) string {
		return "diff --git a/" + path + " b/" + path + "\n" +
			"--- a/" + path + "\n+++ b/" + path + "\n" +
			"@@ -1,2 +1,2 @@\n-x = 1\n+x = 2\n"
	}
	d := FileDigests(section("one.py") + section("two.py"))
	if len(d) != 2 {
		t.Fatalf("want 2 files, got %d", len(d))
	}
	if d["one.py"] == d["two.py"] {
		t.Error("identical sections under different paths share a digest")
	}
}

// TestDigestsIgnoreCommitPreamble matters because porkchop reads a single commit
// via `git show`, which prefixes the diff with commit metadata including the sha.
// If that preamble were folded into the first file's digest, every file in a
// rebased or amended commit would un-view — and reviewing HEAD and reviewing
// HEAD~1..HEAD would disagree about the same file.
func TestDigestsIgnoreCommitPreamble(t *testing.T) {
	const body = `diff --git a/a.py b/a.py
--- a/a.py
+++ b/a.py
@@ -1,2 +1,2 @@
-x = 1
+x = 2
`
	const preamble = `commit 9f8e7d6c5b4a39281706fedcba9876543210abcd
Author:     A U Thor <author@example.com>
AuthorDate: Mon Aug 17 12:00:00 2026 +0000

    tweak x

`
	bare := FileDigests(body)
	shown := FileDigests(preamble + body)
	if bare["a.py"] != shown["a.py"] {
		t.Errorf("commit preamble changed a.py's digest: %q vs %q", bare["a.py"], shown["a.py"])
	}
}

// TestDigestsAbsentWithoutAnOriginal pins the degradation: with no original diff
// there is nothing trustworthy to key a marker to, so there are no digests and
// the caller must fall back to markers that live only for the session.
func TestDigestsAbsentWithoutAnOriginal(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			if d := Align("", pair[1]).Digests; d != nil {
				t.Errorf("got %d digests with no original, want none", len(d))
			}
		})
	}
	if d := FileDigests(""); d != nil {
		t.Errorf("got %d digests for an empty diff, want none", len(d))
	}
}
