package diffview

import (
	"fmt"
	"strings"
	"testing"
)

// TestSplitCoversEveryRowOnce is the structural guarantee the split view rests
// on: the layout is a permutation-free repartition of the row model. Every row
// appears in exactly one cell of exactly one line, and in its original order. A
// row shown twice would double-count a change; a row shown zero times would hide
// one, which for this tool is the unacceptable direction.
func TestSplitCoversEveryRowOnce(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			rows := Parse(pair[1])
			lay := Split(rows)

			seen := make([]int, len(rows))
			// Order is checked within each column of the paired lines, and among
			// the full-width lines, but not over the flattened sequence. Two things
			// legitimately break a single global order: a change block puts dels[1]
			// on a later line than adds[0], and a fold row spliced back into a
			// block can carry a lower index than an addition already shown. What
			// must hold is that neither column ever reads backward on its own.
			lastOld, lastNew, lastFull := -1, -1, -1
			for _, l := range lay.Lines {
				for _, r := range l.Rows() {
					if r < 0 || r >= len(rows) {
						t.Fatalf("line references row %d, out of range [0,%d)", r, len(rows))
					}
					seen[r]++
				}
				if l.Kind == SplitFull {
					if l.Row <= lastFull {
						t.Fatalf("full-width rows out of order: %d after %d", l.Row, lastFull)
					}
					lastFull = l.Row
					continue
				}
				if l.Left.Row >= 0 {
					if l.Left.Row <= lastOld {
						t.Fatalf("old column went backward: row %d after %d", l.Left.Row, lastOld)
					}
					lastOld = l.Left.Row
				}
				if l.Right.Row >= 0 {
					if l.Right.Row <= lastNew {
						t.Fatalf("new column went backward: row %d after %d", l.Right.Row, lastNew)
					}
					lastNew = l.Right.Row
				}
			}
			for i, n := range seen {
				if n != 1 {
					t.Fatalf("row %d (%q) appears %d times, want exactly 1", i, rows[i].Text, n)
				}
			}
		})
	}
}

// TestSplitPutsRowsOnTheRightSide checks the column discipline: the old column
// never shows an addition, the new column never shows a removal, context sits in
// both at once, and everything structural spans the full width. This is what
// makes the column a row is in meaningful on its own.
func TestSplitPutsRowsOnTheRightSide(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			rows := Parse(pair[1])
			for _, l := range Split(rows).Lines {
				if l.Kind == SplitFull {
					switch k := rows[l.Row].Kind; k {
					case RowMeta, RowHunk, RowFold:
					default:
						t.Fatalf("row %d of kind %v spans both columns, want meta/hunk/fold", l.Row, k)
					}
					continue
				}
				if l.Left.Row < 0 && l.Right.Row < 0 {
					t.Fatal("paired line has filler on both sides")
				}
				if l.Left.Row >= 0 {
					if k := rows[l.Left.Row].Kind; k != RowDel && k != RowContext {
						t.Fatalf("row %d of kind %v is in the old column", l.Left.Row, k)
					}
				}
				if l.Right.Row >= 0 {
					if k := rows[l.Right.Row].Kind; k != RowAdd && k != RowContext {
						t.Fatalf("row %d of kind %v is in the new column", l.Right.Row, k)
					}
				}
				// Context is the same line on both sides, never a pairing of two
				// different context rows.
				if l.Left.Row >= 0 && rows[l.Left.Row].Kind == RowContext && l.Right.Row != l.Left.Row {
					t.Fatalf("context row %d is paired with %d instead of itself", l.Left.Row, l.Right.Row)
				}
			}
		})
	}
}

// TestSpansAreWellFormed checks every span is usable as a slice index: ascending,
// non-overlapping, inside the text, and never covering the leading marker byte
// (which always differs between a '-' and a '+' line and means nothing).
func TestSpansAreWellFormed(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			rows := Parse(pair[1])
			lay := Split(rows)
			for i, spans := range lay.Spans {
				checkSpans(t, rows[i].Text, spans, fmt.Sprintf("row %d", i))
			}
			// The cells must carry the same spans the row index does, since ui
			// reads them from whichever is convenient for the view it is drawing.
			for _, l := range lay.Lines {
				for _, c := range []Cell{l.Left, l.Right} {
					if c.Row < 0 {
						if len(c.Spans) > 0 {
							t.Fatal("filler cell carries spans")
						}
						continue
					}
					if !sameSpans(c.Spans, lay.Spans[c.Row]) {
						t.Fatalf("row %d: cell spans %v disagree with Spans[%d] %v",
							c.Row, c.Spans, c.Row, lay.Spans[c.Row])
					}
				}
			}
		})
	}
}

func checkSpans(t *testing.T, text string, spans []Span, what string) {
	t.Helper()
	prev := 0
	for i, s := range spans {
		switch {
		case s.Start < 1:
			t.Fatalf("%s: span %d starts at %d, which is inside the marker byte", what, i, s.Start)
		case s.Start >= s.End:
			t.Fatalf("%s: span %d is empty: [%d,%d)", what, i, s.Start, s.End)
		case s.Start < prev:
			t.Fatalf("%s: span %d starts at %d, overlapping previous end %d", what, i, s.Start, prev)
		case s.End > len(text):
			t.Fatalf("%s: span %d ends at %d, past text length %d", what, i, s.End, len(text))
		}
		prev = s.End
	}
}

func sameSpans(a, b []Span) bool {
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

// TestSpansIsolateExactlyTheDifference is the correctness property of the
// intra-line pass, and the reason it can be trusted to brighten the right
// tokens: the spans mark the tokens outside a longest common subsequence, so
// deleting the spanned ranges from the old line and from the new line must leave
// two byte-identical strings. If highlighting ever drifted off the real
// difference — an off-by-one, a mismerged run, a bad backtrack — the two
// remainders would diverge and this fails.
func TestSpansIsolateExactlyTheDifference(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			rows := Parse(pair[1])
			lay := Split(rows)
			checked := 0
			for _, l := range lay.Lines {
				if l.Kind != SplitPair || l.Left.Row < 0 || l.Right.Row < 0 {
					continue
				}
				if len(l.Left.Spans) == 0 && len(l.Right.Spans) == 0 {
					continue // the similarity gate declined to highlight
				}
				del := cutSpans(rows[l.Left.Row].Text, l.Left.Spans)
				add := cutSpans(rows[l.Right.Row].Text, l.Right.Spans)
				// Both remainders still carry their marker byte, which differs by
				// design, so compare past it.
				if del[1:] != add[1:] {
					t.Fatalf("rows %d/%d: unspanned remainders differ\n old %q\n new %q\n from %q\n      %q",
						l.Left.Row, l.Right.Row, del[1:], add[1:],
						rows[l.Left.Row].Text, rows[l.Right.Row].Text)
				}
				checked++
			}
			if checked == 0 {
				t.Fatal("no highlighted pairs found; the invariant went untested")
			}
			t.Logf("verified %d highlighted pairs", checked)
		})
	}
}

// cutSpans returns text with every spanned range removed.
func cutSpans(text string, spans []Span) string {
	var b strings.Builder
	at := 0
	for _, s := range spans {
		b.WriteString(text[at:s.Start])
		at = s.End
	}
	b.WriteString(text[at:])
	return b.String()
}

func TestIntralineCases(t *testing.T) {
	tests := []struct {
		name     string
		del, add string
		// want is the del and add content with «» around each span, which reads
		// as what the reviewer would see highlighted.
		wantDel, wantAdd string
	}{{
		name: "one identifier changes",
		del:  "-    return foo(x)",
		add:  "+    return bar(x)",
		// A tight span on the differing token only.
		wantDel: "    return «foo»(x)",
		wantAdd: "    return «bar»(x)",
	}, {
		name:    "argument appended",
		del:     "-call(a, b)",
		add:     "+call(a, b, c)",
		wantDel: "call(a, b)",
		wantAdd: "call(a, b«, c»)",
	}, {
		name:    "argument removed",
		del:     "-call(a, b, c)",
		add:     "+call(a, b)",
		wantDel: "call(a, b«, c»)",
		wantAdd: "call(a, b)",
	}, {
		name:    "reindented only",
		del:     "-  x = 1",
		add:     "+      x = 1",
		wantDel: "«  »x = 1",
		wantAdd: "«      »x = 1",
	}, {
		name:    "punctuation edit localizes",
		del:     "-if (a == b) {",
		add:     "+if (a != b) {",
		wantDel: "if (a «==» b) {",
		wantAdd: "if (a «!=» b) {",
	}, {
		// The similarity gate: two lines sharing almost nothing get no spans at
		// all, because marking nearly every token would say less than the plain
		// red/green already does.
		name:    "unrelated lines are not highlighted",
		del:     "-import os",
		add:     "+from collections.abc import Sequence",
		wantDel: "import os",
		wantAdd: "from collections.abc import Sequence",
	}, {
		name:    "empty replaced by content is not highlighted",
		del:     "-",
		add:     "+value = compute()",
		wantDel: "",
		wantAdd: "value = compute()",
	}, {
		name:    "unicode identifier boundaries hold",
		del:     "-café = 1",
		add:     "+café = 2",
		wantDel: "café = «1»",
		wantAdd: "café = «2»",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delSpans, addSpans := intraline(tc.del, tc.add)
			checkSpans(t, tc.del, delSpans, "del")
			checkSpans(t, tc.add, addSpans, "add")
			if got := mark(tc.del, delSpans); got != tc.wantDel {
				t.Errorf("old side:\n got %q\nwant %q", got, tc.wantDel)
			}
			if got := mark(tc.add, addSpans); got != tc.wantAdd {
				t.Errorf("new side:\n got %q\nwant %q", got, tc.wantAdd)
			}
		})
	}
}

// mark renders a line's content with guillemets around each span, so a test's
// expectation reads like the highlighted line itself.
func mark(text string, spans []Span) string {
	var b strings.Builder
	at := 1 // past the marker byte
	for _, s := range spans {
		b.WriteString(text[at:s.Start])
		b.WriteString("«")
		b.WriteString(text[s.Start:s.End])
		b.WriteString("»")
		at = s.End
	}
	b.WriteString(text[at:])
	return b.String()
}

// TestSplitPairsRunsPositionally pins the pairing rule from PLAN.md: a run of
// removals lines up row-by-row against the run of additions that follows it, and
// the longer run's tail gets filler opposite it.
func TestSplitPairsRunsPositionally(t *testing.T) {
	rows := Parse(strings.Join([]string{
		"@@ -1,4 +1,5 @@",
		" ctx",
		"-old one",
		"-old two",
		"+new one",
		"+new two",
		"+new three",
		" tail",
	}, "\n"))
	lay := Split(rows)

	var got []string
	for _, l := range lay.Lines {
		if l.Kind == SplitFull {
			got = append(got, fmt.Sprintf("full(%s)", rows[l.Row].Text))
			continue
		}
		got = append(got, fmt.Sprintf("pair(%s|%s)", cellText(rows, l.Left), cellText(rows, l.Right)))
	}
	want := []string{
		"full(@@ -1,4 +1,5 @@)",
		"pair( ctx| ctx)",
		"pair(-old one|+new one)",
		"pair(-old two|+new two)",
		"pair(_|+new three)",
		"pair( tail| tail)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("layout:\n got %v\nwant %v", got, want)
	}
}

// TestSplitStartsNewBlockOnRemovalAfterAddition checks that "-a +b -c +d" reads
// as two replacements rather than one four-row block, which is what a unified
// diff means by it.
func TestSplitStartsNewBlockOnRemovalAfterAddition(t *testing.T) {
	rows := Parse(strings.Join([]string{
		"@@ -1,2 +1,2 @@",
		"-alpha",
		"+ALPHA",
		"-beta",
		"+BETA",
	}, "\n"))

	var got []string
	for _, l := range Split(rows).Lines {
		if l.Kind == SplitPair {
			got = append(got, fmt.Sprintf("%s|%s", cellText(rows, l.Left), cellText(rows, l.Right)))
		}
	}
	want := []string{"-alpha|+ALPHA", "-beta|+BETA"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func cellText(rows []Row, c Cell) string {
	if c.Row < 0 {
		return "_"
	}
	return rows[c.Row].Text
}

// TestSplitIsTotal checks Split survives the degenerate inputs a real run can
// hand it, since a renderer that panics on an odd diff is worse than one that
// renders it plainly.
func TestSplitIsTotal(t *testing.T) {
	for _, in := range []string{
		"", "\n", "@@", "@@ -1 +1 @@", "+", "-", " ",
		"-\n+", "+only an addition", "-only a removal",
		"diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b",
		strings.Repeat("+x\n", 100) + strings.Repeat("-y\n", 100),
	} {
		rows := Parse(in)
		lay := Split(rows)
		seen := make([]int, len(rows))
		for _, l := range lay.Lines {
			for _, r := range l.Rows() {
				seen[r]++
			}
		}
		for i, n := range seen {
			if n != 1 {
				t.Fatalf("input %q: row %d covered %d times", in, i, n)
			}
		}
	}
}

// TestSplitFallsBackOnHugeLines exercises the maxTokenProduct cap: a pair that is
// similar overall — so the gate lets it through — but whose differing middle is
// too large to align token by token. The fallback must still be a coarse answer
// rather than a wrong one, so the shared prefix and suffix stay outside the span.
func TestSplitFallsBackOnHugeLines(t *testing.T) {
	// A long identical head and tail keep the two lines similar; the middle
	// differs in ~400 tokens per side, whose product is well past the cap.
	shared := strings.Repeat("shared_token ", 400)
	var del, add strings.Builder
	del.WriteString("-" + shared)
	add.WriteString("+" + shared)
	for i := range 200 {
		fmt.Fprintf(&del, "a%d,", i)
		fmt.Fprintf(&add, "b%d,", i)
	}
	del.WriteString(shared)
	add.WriteString(shared)

	delSpans, addSpans := intraline(del.String(), add.String())
	checkSpans(t, del.String(), delSpans, "long del")
	checkSpans(t, add.String(), addSpans, "long add")
	if len(delSpans) != 1 || len(addSpans) != 1 {
		t.Fatalf("want one coarse span per side, got %d and %d", len(delSpans), len(addSpans))
	}
	// The shared head and tail must fall outside the span. The exact boundary is
	// the aligner's to choose — the comma abutting the differing region belongs to
	// either side's common run — so this asserts containment, not equality.
	if head := "-" + shared; delSpans[0].Start < len(head) {
		t.Errorf("span starts at %d, inside the shared head of %d bytes", delSpans[0].Start, len(head))
	}
	if tail := len(del.String()) - len(shared); delSpans[0].End > tail {
		t.Errorf("span ends at %d, inside the shared tail starting at %d", delSpans[0].End, tail)
	}
}
