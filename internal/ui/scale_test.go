package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/brandonbosch/porkchop/internal/diffview"
	"github.com/brandonbosch/porkchop/meat"
)

// The largest real fixture available offline is six files, but the size porkchop is
// built for is a whole agent session — PLAN.md's Phase 2 exit names fifteen. The
// synthetic change below is the only way to exercise that scale without a model, and
// it is deliberately synthetic: it makes no claim about what meat's output looks
// like, only about how many files, hunks and elisions the navigation must survive.
const (
	scaleFiles = 20
	scaleHunks = 3
)

// syntheticChange builds a raw diff of n files with h hunks each, and a reading diff
// that drops the last hunk of every third file — so most files are fully present,
// some carry an elision, and the alignment has real gaps to map.
func syntheticChange(n, h int) (raw, reading string) {
	var rawB, readB strings.Builder
	for f := range n {
		path := fmt.Sprintf("pkg/module%02d/service%02d.go", f, f)
		header := fmt.Sprintf("diff --git a/%s b/%s\nindex %07d..%07d 100644\n--- a/%s\n+++ b/%s\n",
			path, path, f+1000000, f+2000000, path, path)
		rawB.WriteString(header)
		readB.WriteString(header)

		elide := f%3 == 0
		for k := range h {
			start := 1 + k*40
			hunk := fmt.Sprintf("@@ -%d,6 +%d,6 @@ func Handle%02d%02d(ctx context.Context) error {\n", start, start, f, k)
			hunk += fmt.Sprintf(" \tlogger := log.With(\"module\", %q)\n", path)
			hunk += fmt.Sprintf("-\ttimeout := %d * time.Second\n", k+1)
			hunk += fmt.Sprintf("+\ttimeout := %d * time.Millisecond\n", (k+1)*250)
			hunk += " \tdefer cancel()\n"
			rawB.WriteString(hunk)
			// The reading diff keeps every hunk but the last of an elided file.
			if !elide || k < h-1 {
				readB.WriteString(hunk)
			}
		}
	}
	return rawB.String(), readB.String()
}

func scaleModel(t *testing.T, width int) Model {
	t.Helper()
	raw, reading := syntheticChange(scaleFiles, scaleHunks)
	in := Input{
		Summary:     "a whole agent session",
		Elision:     meat.ElisionLine(raw, reading),
		ReadingDiff: reading,
		RawDiff:     raw,
		Viewed:      newFakeViewed(),
	}
	up, _ := New(in).Update(tea.WindowSizeMsg{Width: width, Height: 40})
	m := up.(Model)
	if len(m.files) != scaleFiles {
		t.Fatalf("fixture yielded %d files, want %d", len(m.files), scaleFiles)
	}
	return m
}

// TestScaleFixtureIsWellFormed checks the fixture before anything is asserted on top
// of it: a synthetic change that did not actually produce elisions would make every
// test below pass for the wrong reason.
func TestScaleFixtureIsWellFormed(t *testing.T) {
	m := scaleModel(t, 200)
	if len(m.marks) == 0 {
		t.Fatal("fixture produced no elision markers")
	}
	if got := m.align.ChangedHidden(); got == 0 {
		t.Fatal("fixture hides no changed lines")
	}
	if len(m.hunkRows) < scaleFiles {
		t.Errorf("fixture has %d hunk headers, want at least one per file", len(m.hunkRows))
	}
	// Distinct identities, or "viewed" would collapse files onto each other.
	seen := map[string]string{}
	for i, d := range m.digests {
		if d == "" {
			t.Fatalf("file %d (%s) has no digest", i, m.files[i])
		}
		if prev, ok := seen[d]; ok {
			t.Errorf("%s and %s share a digest", prev, m.files[i])
		}
		seen[d] = m.files[i]
	}
}

// TestFileSteppingAtScale walks ] across twenty files. The assertion is that stepping
// visits every file in order and never wraps: a skipped file in a twenty-file change
// is a file the reviewer never reads, and a wrap is a reviewer who cannot tell they
// have reached the end.
func TestFileSteppingAtScale(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "unified"
		width := 90
		if split {
			name, width = "split", 200
		}
		t.Run(name, func(t *testing.T) {
			m := scaleModel(t, width)
			cur := tea.Model(m)

			seen := []int{0}
			for range scaleFiles + 3 {
				cur, _ = cur.Update(tea.KeyPressMsg{Code: ']'})
				at := cur.(Model).currentFileIndex()
				if last := seen[len(seen)-1]; at < last {
					t.Fatalf("] went backwards, from file %d to %d", last, at)
				} else if at > last+1 {
					t.Fatalf("] skipped from file %d to %d", last, at)
				}
				seen = append(seen, at)
			}

			// Stepping saturates at the end rather than cycling; the last file's
			// header is on screen even though the viewport cannot put it at the top.
			end := cur.(Model)
			assertRowVisible(t, &end, m.fileRows[len(m.fileRows)-1], "last file header")
			before := end.vp.YOffset()
			settledModel, _ := end.Update(tea.KeyPressMsg{Code: ']'})
			settled := settledModel.(Model)
			if got := settled.vp.YOffset(); got != before {
				t.Errorf("] at the end moved from %d to %d instead of settling", before, got)
			}

			// And back to the top, one file at a time.
			for range scaleFiles + 3 {
				cur, _ = cur.Update(tea.KeyPressMsg{Code: '['})
			}
			top := cur.(Model)
			if got := top.vp.YOffset(); got != 0 {
				t.Errorf("[ from the end left the offset at %d, want 0", got)
			}
		})
	}
}

// TestHunkSteppingAtScale is the same for }, which is the key that matters once a
// single file is longer than the screen.
func TestHunkSteppingAtScale(t *testing.T) {
	m := scaleModel(t, 200)
	cur := tea.Model(m)
	visited := 0
	for range len(m.hunkRows) + 3 {
		cur, _ = cur.Update(tea.KeyPressMsg{Code: '}'})
		row := cur.(Model).topRow()
		if row < len(m.rows) && m.rows[row].Kind == diffview.RowHunk {
			visited++
		}
	}
	if visited == 0 {
		t.Fatal("} never landed on a hunk header")
	}
	end := cur.(Model)
	assertRowVisible(t, &end, m.hunkRows[len(m.hunkRows)-1], "last hunk header")
}

// TestViewedAtScale is the workflow this phase exists for: twenty files, checked off
// one at a time, with tab always finding what is left and the header counting down.
func TestViewedAtScale(t *testing.T) {
	m := scaleModel(t, 200)

	// Work the change front to back the way a reviewer would: check off, tab to the
	// next unviewed, repeat. Every file must be reachable this way.
	for range scaleFiles {
		m = press(m, 'v')
		m = pressCode(m, tea.KeyTab)
	}
	if got := m.viewedCount(); got != scaleFiles {
		t.Fatalf("checked off %d of %d files by alternating v and tab", got, scaleFiles)
	}
	if got := ansi.Strip(m.renderHeader()); !strings.Contains(got, fmt.Sprintf("all %d files viewed", scaleFiles)) {
		t.Errorf("header does not report completion:\n%s", got)
	}

	// Un-check one in the middle: tab must find it from anywhere, which is the whole
	// reason it wraps.
	const target = 7
	m.viewed[target] = false
	m.jumpToRow(m.fileRows[scaleFiles-1])
	m = pressCode(m, tea.KeyTab)
	if got := m.currentFileIndex(); got != target {
		t.Errorf("tab from the last file landed on %d, want the one unviewed file %d", got, target)
	}
}

// TestBreadcrumbAtScale checks the header rule still answers "where am I" when the
// answer is one of twenty — the case the breadcrumb replaced a file tree for.
func TestBreadcrumbAtScale(t *testing.T) {
	for _, width := range []int{80, 100, 120, 200} {
		m := scaleModel(t, width)
		cur := tea.Model(m)
		for range 4 {
			cur, _ = cur.Update(tea.KeyPressMsg{Code: ']'})
		}
		at := cur.(Model)
		rule := ansi.Strip(at.renderRule())
		if want := fmt.Sprintf("(%d/%d)", at.currentFileIndex()+1, scaleFiles); !strings.Contains(rule, want) {
			t.Errorf("w=%d: rule %q does not carry %s", width, rule, want)
		}
		if got := lipgloss.Width(at.renderRule()); got > width {
			t.Errorf("w=%d: rule is %d cells wide", width, got)
		}
		// The frame must stay exactly as tall as layout measured it, whatever the
		// path length; a wrapped header would push the body off the bottom.
		if got := lipgloss.Height(at.renderHeader()); got != 3 {
			t.Errorf("w=%d: header is %d lines, want 3", width, got)
		}
	}
}

// TestExpansionAtScale checks E over a twenty-file change: every hidden changed line
// must be revealed, and collapsing must put the screen back exactly as it was. This
// is Phase 3's guarantee, re-checked at a size no real fixture reaches.
func TestExpansionAtScale(t *testing.T) {
	m := scaleModel(t, 200)
	before := m.renderBody()

	m = press(m, 'E')
	expanded := ansi.Strip(m.renderBody())
	hidden := 0
	for _, e := range m.marks {
		for _, line := range m.align.Hidden(e) {
			if len(line) == 0 || (line[0] != '+' && line[0] != '-') {
				continue
			}
			hidden++
			// Compare on the tab-free remainder: the body expands tabs against the
			// full line including its +/- marker, so the leading run of spaces
			// differs from expanding the content alone.
			if want := strings.TrimSpace(line[1:]); !strings.Contains(expanded, want) {
				t.Fatalf("expanded body is missing a hidden line: %q", line)
			}
		}
	}
	if hidden == 0 {
		t.Fatal("no hidden changed lines to expand")
	}

	m = press(m, 'E')
	if got := m.renderBody(); got != before {
		t.Error("collapsing every elision did not restore the original body")
	}
}

// TestLastFileIsReachable pins the reachability property the blank tail exists for.
// Before it, the final screenful of a long change could not be scrolled to the top,
// so the breadcrumb could not name those files and `v` could not check them off —
// invisible on a six-file fixture that fits the screen, fatal on a real session.
func TestLastFileIsReachable(t *testing.T) {
	m := scaleModel(t, 200)
	last := scaleFiles - 1

	cur := tea.Model(m)
	for range scaleFiles + 3 {
		cur, _ = cur.Update(tea.KeyPressMsg{Code: ']'})
	}
	end := cur.(Model)
	if got := end.currentFileIndex(); got != last {
		t.Fatalf("stepping to the end reports file %d, want %d", got, last)
	}
	if got := ansi.Strip(end.renderRule()); !strings.Contains(got, fmt.Sprintf("(%d/%d)", last+1, scaleFiles)) {
		t.Errorf("the breadcrumb cannot name the last file: %q", got)
	}

	// And it can be checked off, which is the decision the reviewer came to make.
	end = press(end, 'v')
	if !end.viewed[last] {
		t.Errorf("v at the end checked off file %d instead of %d", indexOfViewed(end), last)
	}

	// G lands on the content, not on the blank tail, and reads as the end. The
	// breadcrumb is not asserted here: G fills the screen with the tail of the diff,
	// so the top of the screen is legitimately inside an earlier file, and the
	// breadcrumb names the file at the top. Reaching a file to act on it is what ]
	// and tab are for, and they scroll it to the top.
	bottom := press(m, 'G')
	if got := bottom.scrollPercent(); got != 100 {
		t.Errorf("G reports %d%%, want 100 — the blank tail is being counted as content", got)
	}
	// The last body line must be on screen after G, or G did not reach the end.
	if line := len(bottom.body) - 1; line < bottom.vp.YOffset() || line >= bottom.vp.YOffset()+bottom.vp.Height() {
		t.Errorf("G left the last body line (%d) off screen, offset %d height %d",
			line, bottom.vp.YOffset(), bottom.vp.Height())
	}
	assertRowVisible(t, &bottom, m.fileRows[last], "last file header after G")
}

func indexOfViewed(m Model) int {
	for i, v := range m.viewed {
		if v {
			return i
		}
	}
	return -1
}
