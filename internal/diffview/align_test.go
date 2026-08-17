package diffview

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/brandonbosch/porkchop/meat"
)

// goldens returns the meat/testdata pairs: raw diff and the reading diff meat
// produced from it. These are the only real-world fixtures available offline,
// and every structural property below is asserted against all of them.
func goldens(t *testing.T) map[string][2]string {
	t.Helper()
	dir := filepath.Join("..", "..", "meat", "testdata", "python")
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.diff"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no goldens found in %s: %v", dir, err)
	}
	out := make(map[string][2]string, len(matches))
	for _, g := range matches {
		reading, err := os.ReadFile(g)
		if err != nil {
			t.Fatalf("read %s: %v", g, err)
		}
		raw, err := os.ReadFile(strings.TrimSuffix(g, ".golden.diff") + ".diff")
		if err != nil {
			t.Fatalf("read raw sibling of %s: %v", g, err)
		}
		out[filepath.Base(g)] = [2]string{string(raw), string(reading)}
	}
	return out
}

// TestAlignPartitionsRaw is the core structural guarantee: the elisions are
// ordered, non-overlapping, in bounds, and non-empty. Together with the matched
// lines they therefore partition the raw diff, which is what makes "expand this
// elision" show each hidden line exactly once and never lose one.
func TestAlignPartitionsRaw(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])
			if len(a.Raw) == 0 {
				t.Fatal("no raw lines parsed")
			}
			prevEnd := 0
			for i, e := range a.Elisions {
				switch {
				case e.RawStart < prevEnd:
					t.Fatalf("elision %d starts at %d, overlapping previous end %d", i, e.RawStart, prevEnd)
				case e.RawStart >= e.RawEnd:
					t.Fatalf("elision %d is empty: [%d,%d)", i, e.RawStart, e.RawEnd)
				case e.RawEnd > len(a.Raw):
					t.Fatalf("elision %d ends at %d, past raw length %d", i, e.RawEnd, len(a.Raw))
				}
				prevEnd = e.RawEnd
			}
		})
	}
}

// TestAlignBeforeRowOrdering checks the marker anchors advance monotonically, so
// ui can draw markers in a single forward pass over the rows.
func TestAlignBeforeRowOrdering(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])
			prev := -1
			for i, e := range a.Elisions {
				if e.BeforeRow <= prev {
					t.Fatalf("elision %d anchors at row %d, not after previous %d", i, e.BeforeRow, prev)
				}
				if e.BeforeRow > len(a.Rows) {
					t.Fatalf("elision %d anchors past the last row: %d > %d", i, e.BeforeRow, len(a.Rows))
				}
				prev = e.BeforeRow
			}
		})
	}
}

// TestAlignEveryChangedReadingRowMatches is the non-tautological check that the
// walk actually located things: every added or removed row in the reading diff
// must have found a home in the raw diff, so the number of changed raw lines
// left uncovered by elisions equals the number of changed reading rows. A
// regression in the matcher shows up here as a mismatch.
func TestAlignEveryChangedReadingRowMatches(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])

			covered := make([]bool, len(a.Raw))
			for _, e := range a.Elisions {
				for i := e.RawStart; i < e.RawEnd; i++ {
					if covered[i] {
						t.Fatalf("raw line %d covered by two elisions", i)
					}
					covered[i] = true
				}
			}

			rawRows := Parse(pair[0])
			matchedChanged := 0
			for i, r := range rawRows {
				if !covered[i] && isChangedLine(r) {
					matchedChanged++
				}
			}
			readingChanged := 0
			for _, r := range a.Rows {
				if r.Kind == RowAdd || r.Kind == RowDel {
					readingChanged++
				}
			}
			if matchedChanged != readingChanged {
				t.Errorf("matched %d changed raw lines but the reading diff shows %d changed rows",
					matchedChanged, readingChanged)
			}
		})
	}
}

// TestAlignAgreesWithCoreElisionLine cross-validates the mapping against meat's
// own accounting. meat.ElisionLine reports "kept K/T changed lines", computed by
// retainedDiffStats — the same alignment, counted differently: meat scores each
// polarity-carrying fold row as a kept changed line, where porkchop attributes
// the lines behind that fold to the elision instead. So the identity is
//
//	hidden = (T - K) + foldRowsWithPolarity
//
// Holding this exactly is strong evidence the port aligns identically to the
// core it was derived from, on real fixtures. (meat is imported here only by the
// test; the diffview package itself stays pure stdlib.)
func TestAlignAgreesWithCoreElisionLine(t *testing.T) {
	kept := regexp.MustCompile(`kept (\d+)/(\d+) changed lines`)
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			line := meat.ElisionLine(pair[0], pair[1])
			m := kept.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("could not read counts from core elision line %q", line)
			}
			var keptN, totalN int
			fmt.Sscanf(m[1], "%d", &keptN)
			fmt.Sscanf(m[2], "%d", &totalN)

			a := Align(pair[0], pair[1])
			folds := 0
			for _, r := range a.Rows {
				if r.Kind == RowFold && r.Text != "" && (r.Text[0] == '+' || r.Text[0] == '-') {
					folds++
				}
			}
			want := (totalN - keptN) + folds
			if got := a.ChangedHidden(); got != want {
				t.Errorf("hidden changed lines = %d, want %d (core says %q, %d polarity fold rows)",
					got, want, line, folds)
			}
		})
	}
}

// TestAlignFoldAttribution pins the finding that motivated the design: meat
// marks only a small minority of its elisions with a "..." row, so most gaps
// carry FoldRow == -1 and must be surfaced by a synthesized marker. It also
// checks no fold row is claimed by two elisions.
func TestAlignFoldAttribution(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])

			seen := map[int]bool{}
			marked, changeBearing := 0, 0
			for _, e := range a.Elisions {
				if e.FoldRow >= 0 {
					if e.FoldRow >= len(a.Rows) || a.Rows[e.FoldRow].Kind != RowFold {
						t.Fatalf("FoldRow %d is not a fold row", e.FoldRow)
					}
					if seen[e.FoldRow] {
						t.Fatalf("fold row %d attributed to two elisions", e.FoldRow)
					}
					seen[e.FoldRow] = true
					marked++
				}
				if e.Changed > 0 {
					changeBearing++
				}
			}
			if changeBearing == 0 {
				t.Fatal("expected at least one elision hiding changed lines")
			}
			if marked >= changeBearing {
				t.Errorf("expected fold-marked elisions (%d) to be a minority of "+
					"change-bearing ones (%d); the synthesized-marker design assumes this",
					marked, changeBearing)
			}
			t.Logf("%d elisions, %d hide changed lines, %d marked with a fold row, %d changed lines hidden",
				len(a.Elisions), changeBearing, marked, a.ChangedHidden())
		})
	}
}

// TestAlignHiddenContentIsExact spot-checks the payoff: expanding an elision
// yields the original lines verbatim, in order, at the right place.
func TestAlignHiddenContentIsExact(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/f.go b/f.go",
		"--- a/f.go",
		"+++ b/f.go",
		"@@ -1,4 +1,4 @@",
		" keep",
		"-gone one",
		"-gone two",
		"+added",
		" tail",
	}, "\n")
	reading := strings.Join([]string{
		"diff --git a/f.go b/f.go",
		"--- a/f.go",
		"+++ b/f.go",
		"@@ -1,4 +1,4 @@",
		" keep",
		"+added",
		" tail",
	}, "\n")

	a := Align(raw, reading)
	if len(a.Elisions) != 1 {
		t.Fatalf("got %d elisions, want 1: %+v", len(a.Elisions), a.Elisions)
	}
	e := a.Elisions[0]
	if got, want := a.Hidden(e), []string{"-gone one", "-gone two"}; !equalLines(got, want) {
		t.Errorf("hidden = %q, want %q", got, want)
	}
	if e.Changed != 2 {
		t.Errorf("Changed = %d, want 2", e.Changed)
	}
	if e.FoldRow != -1 {
		t.Errorf("FoldRow = %d, want -1 (meat emitted no marker)", e.FoldRow)
	}
	// The marker belongs immediately before the "+added" row.
	if want := 5; e.BeforeRow != want {
		t.Errorf("BeforeRow = %d, want %d (%q)", e.BeforeRow, want, a.Rows[want].Text)
	}
}

func TestAlignFoldMarkedElision(t *testing.T) {
	raw := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" keep",
		"-gone one",
		"-gone two",
		" tail",
	}, "\n")
	reading := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" keep",
		"- ...",
		" tail",
	}, "\n")

	a := Align(raw, reading)
	if len(a.Elisions) != 1 {
		t.Fatalf("got %d elisions, want 1", len(a.Elisions))
	}
	e := a.Elisions[0]
	if e.FoldRow != 2 {
		t.Errorf("FoldRow = %d, want 2 (meat's '- ...' row)", e.FoldRow)
	}
	if got, ok := a.ElisionAtFold(2); !ok || got.RawStart != e.RawStart {
		t.Errorf("ElisionAtFold(2) = %+v, %v; want the recorded elision", got, ok)
	}
	if e.Changed != 2 {
		t.Errorf("Changed = %d, want 2", e.Changed)
	}
}

func TestAlignEdgeCases(t *testing.T) {
	tests := []struct {
		name           string
		raw, reading   string
		wantElisions   int
		wantHidden     int
		wantFirstRange [2]int
	}{{
		name:    "identical diffs hide nothing",
		raw:     "@@ -1,1 +1,1 @@\n-a\n+b",
		reading: "@@ -1,1 +1,1 @@\n-a\n+b",
	}, {
		// The reading diff's row kinds are part of the match key, inherited from
		// meat. A context line outside any hunk is metadata, not context, so it
		// cannot align to a context line inside one — and a reading diff that
		// lost its hunk header aligns nothing and hides everything. Degrading
		// toward "more is hidden" is the safe direction for a trust feature.
		name:           "reading diff missing its hunk header aligns nothing",
		raw:            "@@ -1,2 +1,2 @@\n-a\n+b\n unchanged",
		reading:        " unchanged",
		wantElisions:   1,
		wantHidden:     2,
		wantFirstRange: [2]int{0, 4},
	}, {
		name:           "leading elision before the first matched row",
		raw:            "diff --git a/x b/x\n@@ -1,2 +1,2 @@\n-a\n+b\n unchanged",
		reading:        "@@ -1,2 +1,2 @@\n unchanged",
		wantElisions:   2,
		wantHidden:     2,
		wantFirstRange: [2]int{0, 1},
	}, {
		name:           "trailing elision",
		raw:            "@@ -1,2 +1,2 @@\n unchanged\n-a\n+b",
		reading:        "@@ -1,2 +1,2 @@\n unchanged",
		wantElisions:   1,
		wantHidden:     2,
		wantFirstRange: [2]int{2, 4},
	}, {
		name:           "context-only elision hides no changes",
		raw:            "@@ -1,3 +1,3 @@\n ctx one\n ctx two\n-a",
		reading:        "@@ -1,3 +1,3 @@\n-a",
		wantElisions:   1,
		wantHidden:     0,
		wantFirstRange: [2]int{1, 3},
	}, {
		name:    "empty reading diff over empty raw",
		raw:     "",
		reading: "",
	}, {
		name:           "everything elided",
		raw:            "@@ -1,1 +1,1 @@\n-a\n+b",
		reading:        "",
		wantElisions:   1,
		wantHidden:     2,
		wantFirstRange: [2]int{0, 3},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Align(tt.raw, tt.reading)
			if len(a.Elisions) != tt.wantElisions {
				t.Fatalf("got %d elisions, want %d: %+v", len(a.Elisions), tt.wantElisions, a.Elisions)
			}
			if got := a.ChangedHidden(); got != tt.wantHidden {
				t.Errorf("ChangedHidden = %d, want %d", got, tt.wantHidden)
			}
			if tt.wantElisions > 0 {
				e := a.Elisions[0]
				if [2]int{e.RawStart, e.RawEnd} != tt.wantFirstRange {
					t.Errorf("first elision range = [%d,%d), want %v", e.RawStart, e.RawEnd, tt.wantFirstRange)
				}
			}
		})
	}
}

// TestAlignPartiallyElidedRow covers the projection path. The goldens contain no
// partially elided rows at all, so this synthetic case is its only coverage —
// without it the branch would ship untested.
func TestAlignPartiallyElidedRow(t *testing.T) {
	raw := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" head",
		"-    self.session = make_session(app, request, cookies)",
		"+    self._session = make_session(app, request, cookies)",
		" tail",
	}, "\n")
	reading := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" head",
		"-    self.session = make_session(...)",
		"+    self._session = make_session(...)",
		" tail",
	}, "\n")

	a := Align(raw, reading)
	if got := a.ChangedHidden(); got != 0 {
		t.Errorf("ChangedHidden = %d, want 0: both changed rows should have "+
			"projected onto their originals, leaving nothing hidden. Elisions: %+v",
			got, a.Elisions)
	}
	if len(a.Elisions) != 0 {
		t.Errorf("got %d elisions, want 0: %+v", len(a.Elisions), a.Elisions)
	}
}

func TestCompileProjection(t *testing.T) {
	tests := []struct {
		name, content string
		ok            bool
		match, reject []string
	}{{
		name:    "call arguments elided",
		content: "foo(...)",
		ok:      true,
		match:   []string{"foo(a, b)", "foo(x)"},
		reject:  []string{"foo()", "bar(a)"},
	}, {
		name:    "unicode ellipsis",
		content: "a…z",
		ok:      true,
		match:   []string{"abcz", "a z"},
		reject:  []string{"az", "abc"},
	}, {
		name:    "no ellipsis has no projection",
		content: "plain text",
		ok:      false,
	}, {
		name:    "two dots are literal, not a wildcard",
		content: "range(a..b)",
		ok:      false,
	}, {
		name:    "regex metacharacters stay literal",
		content: "re.match(^x$, ...)",
		ok:      true,
		match:   []string{"re.match(^x$, s)"},
		reject:  []string{"re.match(x, s)"},
	}, {
		name:    "adjacent ellipses collapse",
		content: "a......z",
		ok:      true,
		match:   []string{"abz"},
		reject:  []string{"az"},
	}, {
		name:    "ellipsis must stand for at least one character",
		content: "!...allowed",
		ok:      true,
		match:   []string{"!not_allowed"},
		reject:  []string{"!allowed"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re, ok := compileProjection(tt.content)
			if ok != tt.ok {
				t.Fatalf("compileProjection(%q) ok = %v, want %v", tt.content, ok, tt.ok)
			}
			if !ok {
				return
			}
			for _, s := range tt.match {
				if !re.MatchString(s) {
					t.Errorf("%q should match %q (pattern %s)", tt.content, s, re)
				}
			}
			for _, s := range tt.reject {
				if re.MatchString(s) {
					t.Errorf("%q should not match %q (pattern %s)", tt.content, s, re)
				}
			}
		})
	}
}

// TestAlignIsTotal asserts Align never panics, whatever it is handed. It backs a
// trust feature, so a malformed or adversarial diff must degrade rather than
// crash the reviewer's session.
func TestAlignIsTotal(t *testing.T) {
	inputs := []string{
		"", "\n", "\n\n\n", "@@", "@@ -1,1 +1,1 @@", "---", "--- a\n+++ b",
		"+", "-", " ", "...", "+ ...", "- ...", "  ...  ",
		"diff --git a/x b/x", "\\ No newline at end of file",
		"@@ -1 +1 @@\n-\xff\xfe\n+\x00", strings.Repeat("+x\n", 500),
		"@@ -1 +1 @@\n+…\n+..\n+....",
	}
	for _, raw := range inputs {
		for _, reading := range inputs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Align(%q, %q) panicked: %v", raw, reading, r)
					}
				}()
				a := Align(raw, reading)
				for _, e := range a.Elisions {
					if e.RawStart < 0 || e.RawEnd > len(a.Raw) || e.RawStart >= e.RawEnd {
						t.Fatalf("Align(%q, %q) produced invalid elision %+v (raw len %d)",
							raw, reading, e, len(a.Raw))
					}
					_ = a.Hidden(e)
				}
			}()
		}
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
