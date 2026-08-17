package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/brandonbosch/porkchop/internal/diffview"
)

// numberedDiff is a raw diff and the reading diff meat might abridge it to, built
// so the old and new numbering diverge by a known amount: the change above the
// context lines removes two lines and adds one, so every later new-side number
// runs one behind its old-side counterpart.
const (
	numberedRaw = "diff --git a/f.py b/f.py\n" +
		"--- a/f.py\n" +
		"+++ b/f.py\n" +
		"@@ -10,7 +10,6 @@ def f():\n" +
		" keep one\n" +
		"-drop me\n" +
		"-old line\n" +
		"+new line\n" +
		" keep two\n" +
		" keep three\n"
	numberedReading = "diff --git a/f.py b/f.py\n" +
		"--- a/f.py\n" +
		"+++ b/f.py\n" +
		"@@ -10,7 +10,6 @@ def f():\n" +
		" keep one\n" +
		"-old line\n" +
		"+new line\n" +
		" keep two\n" +
		" keep three\n"
)

// splitModel builds a model in the two-column view at a given width.
func splitModel(t *testing.T, in Input, width int) Model {
	t.Helper()
	m := New(in)
	up, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	got := up.(Model)
	if !got.split {
		t.Fatalf("model at width %d is not in the split view", width)
	}
	return got
}

// TestSplitChosenByWidth checks porkchop opens in whichever view the terminal can
// actually carry, and that `u` overrides the choice for good — a resize must not
// undo what the reviewer asked for.
func TestSplitChosenByWidth(t *testing.T) {
	in := Input{ReadingDiff: numberedReading, RawDiff: numberedRaw}

	narrow, _ := New(in).Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	if narrow.(Model).split {
		t.Error("80 columns is too narrow for the split view but it was chosen anyway")
	}
	wide, _ := New(in).Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	if !wide.(Model).split {
		t.Error("200 columns should open in the split view")
	}

	// u pins the choice: switching to unified at 200 columns must survive a resize
	// that would otherwise re-pick split.
	unified, _ := wide.Update(tea.KeyPressMsg{Code: 'u'})
	if unified.(Model).split {
		t.Fatal("u did not leave the split view")
	}
	resized, _ := unified.Update(tea.WindowSizeMsg{Width: 220, Height: 40})
	if resized.(Model).split {
		t.Error("a resize undid the reviewer's explicit choice of the unified view")
	}
}

// TestSplitColumnsAlign is the geometry guarantee: every paired line is exactly as
// wide as the layout says, so the rule between the columns forms an unbroken
// vertical line down the screen. A single line that pads differently bends it, and
// the eye reads a bent column boundary as a rendering fault.
func TestSplitColumnsAlign(t *testing.T) {
	for name, in := range goldenInputs(t) {
		for _, width := range []int{120, 150, 200} {
			t.Run(name, func(t *testing.T) {
				m := splitModel(t, in, width)
				want := 2*(m.gutterCells()+m.colWidth()) + columnGap
				pairs := 0
				for i, bl := range m.body {
					if bl.kind != bodyPair {
						continue
					}
					pairs++
					line := ansi.Strip(m.renderBodyLine(i, bl))
					if got := lipgloss.Width(line); got != want {
						t.Fatalf("width %d: pair line %d is %d cells, want %d\n%q",
							width, i, got, want, line)
					}
					if col := cellIndexOf(line, columnSep); col != m.gutterCells()+m.colWidth()+1 {
						t.Fatalf("width %d: pair line %d has its separator at cell %d, want %d\n%q",
							width, i, col, m.gutterCells()+m.colWidth()+1, line)
					}
				}
				if pairs == 0 {
					t.Fatal("no paired lines rendered")
				}
			})
		}
	}
}

// cellIndexOf is the display column the first occurrence of sub starts at, or -1.
func cellIndexOf(s, sub string) int {
	before, _, found := strings.Cut(s, sub)
	if !found {
		return -1
	}
	return lipgloss.Width(before)
}

// assertRowVisible checks a row is somewhere in the viewport's window.
func assertRowVisible(t *testing.T, m *Model, row int, what string) {
	t.Helper()
	line := m.rowLine[row]
	if line < 0 {
		t.Fatalf("%s (row %d) is not on any body line", what, row)
	}
	top, height := m.vp.YOffset(), m.vp.Height()
	if line < top || line >= top+height {
		t.Errorf("%s is on line %d, outside the visible window [%d,%d)", what, line, top, top+height)
	}
}

// TestGutterLeavesOneLeftEdge is a regression test for a double prefix. A row draws
// its own line-number gutter, so indenting it by the gutter width as well pushes
// its content twice as far right as the headers around it, which have no numbers to
// draw — leaving two left edges down the screen. The check is exact: a row's
// rendered line must be its gutter followed by its content and nothing else.
func TestGutterLeavesOneLeftEdge(t *testing.T) {
	for name, in := range goldenInputs(t) {
		for _, width := range []int{96, 200} {
			t.Run(name, func(t *testing.T) {
				up, _ := New(in).Update(tea.WindowSizeMsg{Width: width, Height: 40})
				m := up.(Model)
				g := m.gutterCells()
				if g == 0 {
					t.Fatal("expected line numbers on an aligned golden")
				}
				rows := 0
				for i, bl := range m.body {
					if bl.kind != bodyRow {
						continue
					}
					rows++
					line := ansi.Strip(m.renderBodyLine(i, bl))
					want, _ := m.displayCell(bl, sideOld)
					// The gutter is digits and spaces only, so cells and bytes agree
					// over it and slicing by g is exact.
					if len(line) < g {
						t.Fatalf("width %d: line %d is shorter than the gutter: %q", width, i, line)
					}
					if got := line[g:]; got != want {
						t.Fatalf("width %d: row line %d content starts in the wrong place\n got %q\nwant %q",
							width, i, got, want)
					}
				}
				if rows == 0 {
					t.Fatal("no row lines rendered")
				}
			})
		}
	}
}

// TestSplitColumnsShowTheirOwnLineNumbers is a regression test with teeth. The old
// column must number lines in the old file and the new column in the new one; a
// gutter that shows the old number on both sides looks entirely plausible on a
// diff where the two happen to coincide, and is wrong everywhere else.
func TestSplitColumnsShowTheirOwnLineNumbers(t *testing.T) {
	m := splitModel(t, Input{ReadingDiff: numberedReading, RawDiff: numberedRaw}, 160)

	// " keep two" is context at old line 13 and new line 12: the hunk starts at 10
	// on both sides, and the change above removes two lines while adding one, so
	// the new file runs one line ahead of itself from here on.
	var got string
	for i, bl := range m.body {
		if bl.kind == bodyPair && bl.left.Row >= 0 && strings.Contains(m.rows[bl.left.Row].Text, "keep two") {
			got = ansi.Strip(m.renderBodyLine(i, bl))
			break
		}
	}
	if got == "" {
		t.Fatal("could not find the context row in the split body")
	}

	old, new, _ := strings.Cut(got, columnSep)
	if want := "13"; !strings.Contains(old, want) {
		t.Errorf("old column %q does not carry old line %s", strings.TrimSpace(old), want)
	}
	if want := "12"; !strings.Contains(new, want) {
		t.Errorf("new column %q does not carry new line %s", strings.TrimSpace(new), want)
	}
	// The failure this guards against: a gutter that reads n.Old regardless of the
	// column it is drawing, which puts the old number on both sides.
	if strings.Contains(new, "13") {
		t.Errorf("new column %q is showing the old line number", strings.TrimSpace(new))
	}
}

// TestIntralineEmphasisIsRenderedOnTheChangedTokensOnly checks the emphasis
// reaches the screen and lands only where it should. The rendered line is walked
// as styled segments, and the ones carrying the emphasis background must
// reconstruct exactly the tokens diffview marked.
func TestIntralineEmphasisIsRenderedOnTheChangedTokensOnly(t *testing.T) {
	reading := "@@ -1,2 +1,2 @@\n" +
		" ctx\n" +
		"-    return foo(x)\n" +
		"+    return bar(x)\n"
	m := New(Input{ReadingDiff: reading})
	m.width = 200
	m.split = true
	m.splitPinned = true
	m.rebuild()

	// Dark is the seeded palette, so these are the backgrounds in play.
	const (
		delBg = "48;2;110;43;43"
		addBg = "48;2;43;92;52"
	)
	found := 0
	for i, bl := range m.body {
		// Only a replacement has tokens to contrast. A context line is the same row
		// in both columns and has nothing to differ from.
		if bl.kind != bodyPair || bl.left.Row < 0 || bl.right.Row < 0 || bl.left.Row == bl.right.Row {
			continue
		}
		rendered := m.renderBodyLine(i, bl)
		for bg, want := range map[string]string{delBg: "foo", addBg: "bar"} {
			if got := emphasized(rendered, bg); got != want {
				t.Errorf("background %s emphasizes %q, want %q", bg, got, want)
			}
		}
		found++
	}
	if found != 1 {
		t.Fatalf("expected one highlighted pair, found %d", found)
	}
}

// emphasized concatenates the text of every styled run whose SGR parameters
// contain bg. Runs are delimited by the reset Lip Gloss closes each with, and the
// parameters are read between the "\x1b[" and its terminating "m" rather than by
// scanning for the last "m" in the segment, which would find one in the text.
func emphasized(rendered, bg string) string {
	var out strings.Builder
	for _, seg := range strings.Split(rendered, "\x1b[m") {
		i := strings.Index(seg, "\x1b[")
		if i < 0 {
			continue
		}
		j := strings.IndexByte(seg[i:], 'm')
		if j < 0 {
			continue
		}
		if params := seg[i+2 : i+j]; strings.Contains(params, bg) {
			out.WriteString(seg[i+j+1:])
		}
	}
	return out.String()
}

// TestSplitCarriesElisionMarkersAndExpansion is the Phase-2-meets-Phase-3 check
// required by PLAN.md: the trust features must work identically in the two-column
// view, with markers spanning the full width and expansion revealing the same
// original content.
func TestSplitCarriesElisionMarkersAndExpansion(t *testing.T) {
	for name, in := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			m := splitModel(t, in, 200)
			if len(m.marks) == 0 {
				t.Fatal("no markers in the split view")
			}

			// Every marker is drawn, and drawn full width rather than inside a column.
			plain := ansi.Strip(m.renderBody())
			for i := range m.marks {
				if !strings.Contains(plain, m.markerText(i)) {
					t.Errorf("split view is missing marker %d: %q", i, m.markerText(i))
				}
			}
			for i, bl := range m.body {
				if bl.kind != bodyMarker {
					continue
				}
				if strings.Contains(ansi.Strip(m.renderBodyLine(i, bl)), columnSep) {
					t.Errorf("marker %d is drawn inside the columns rather than across them", bl.mark)
				}
			}

			// And expansion still reveals the original.
			want := ""
			for _, line := range m.align.Hidden(m.marks[0]) {
				if len(strings.TrimSpace(line)) > 8 {
					want = expandTabs(line, tabWidth)
					break
				}
			}
			if want == "" {
				t.Skip("first elision hides nothing substantial")
			}
			expanded, _ := m.Update(tea.KeyPressMsg{Code: 'e'})
			if got := ansi.Strip(expanded.(Model).renderBody()); !strings.Contains(got, want) {
				t.Errorf("expanding in the split view did not reveal %q", want)
			}
		})
	}
}

// TestToggleViewKeepsPlace checks `u` does not lose the reviewer's position. The
// two views have different line counts, so the scroll offset cannot be carried
// across — the row at the top has to be.
func TestToggleViewKeepsPlace(t *testing.T) {
	for name, in := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			m := splitModel(t, in, 200)
			m.vp.SetYOffset(len(m.body) / 2)
			anchor := m.topRow()

			toggled, _ := m.Update(tea.KeyPressMsg{Code: 'u'})
			tm := toggled.(Model)
			if tm.split {
				t.Fatal("u did not switch views")
			}
			if got := tm.topRow(); got != anchor {
				t.Errorf("top row moved from %d to %d across the toggle", anchor, got)
			}
		})
	}
}

// TestFileAndHunkStepping checks ]/[ and }/{ land on headers and clamp at the ends
// rather than wrapping, which is what makes holding a key settle.
func TestFileAndHunkStepping(t *testing.T) {
	for name, in := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			m := splitModel(t, in, 200)
			if len(m.files) < 2 {
				t.Skipf("golden has %d files", len(m.files))
			}

			// The assertion is visibility and settling, not an exact offset. A file
			// header within one screen of the end of the diff can never be scrolled
			// to the top — the viewport correctly refuses to scroll past the bottom —
			// so what stepping must guarantee is that the header is on screen and
			// that pressing the key again does not wrap around to the top.
			cur := tea.Model(m)
			for range len(m.files) + 3 {
				cur, _ = cur.Update(tea.KeyPressMsg{Code: ']'})
			}
			last := cur.(Model)
			assertRowVisible(t, &last, m.fileRows[len(m.fileRows)-1], "last file header")
			settled, _ := last.Update(tea.KeyPressMsg{Code: ']'})
			sm := settled.(Model)
			if got, want := sm.vp.YOffset(), last.vp.YOffset(); got != want {
				t.Errorf("] at the last file moved from %d to %d instead of settling", want, got)
			}

			for range len(m.files) + 3 {
				cur, _ = cur.Update(tea.KeyPressMsg{Code: '['})
			}
			first := cur.(Model)
			assertRowVisible(t, &first, m.fileRows[0], "first file header")
			if got := first.vp.YOffset(); got != 0 {
				t.Errorf("[ at the first file left the offset at %d, want 0", got)
			}

			// Hunk stepping lands on a hunk header.
			hunked, _ := cur.Update(tea.KeyPressMsg{Code: '}'})
			hm := hunked.(Model)
			if row := hm.topRow(); row >= len(hm.rows) || hm.rows[row].Kind != diffview.RowHunk {
				t.Errorf("} landed on row %d, which is not a hunk header", row)
			}
		})
	}
}

// TestBreadcrumbNamesTheCurrentFile checks the header rule reports where the
// reviewer is, which is the whole of how a 15-file change stays navigable without
// a file tree.
func TestBreadcrumbNamesTheCurrentFile(t *testing.T) {
	for name, in := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			m := splitModel(t, in, 200)
			if len(m.files) < 2 {
				t.Skipf("golden has %d files", len(m.files))
			}
			// The second file, not the last: a header near the end of the diff cannot
			// be scrolled to the top, so using it would test the viewport's clamping
			// rather than the breadcrumb.
			m.jumpToRow(m.fileRows[1])
			rule := ansi.Strip(m.renderRule())
			if want := m.files[1]; !strings.Contains(rule, want) {
				t.Errorf("rule %q does not name the current file %q", strings.TrimSpace(rule), want)
			}
			if !strings.Contains(rule, "─") {
				t.Errorf("rule %q lost its divider", rule)
			}
		})
	}
}
