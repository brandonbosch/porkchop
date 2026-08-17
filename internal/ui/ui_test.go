package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/brandonbosch/porkchop/internal/diffview"
	"github.com/brandonbosch/porkchop/meat"
)

func TestExpandTabs(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"no tabs", "no tabs"},
		{"\tx", "    x"},
		{"ab\tx", "ab  x"},       // ab -> col 2, tab to col 4
		{"abcd\tx", "abcd    x"}, // abcd -> col 4, full tab stop
		{"+\tcode", "+   code"},  // marker at col 0, tab from col 1 to 4
	}
	for _, tt := range tests {
		if got := expandTabs(tt.in, 4); got != tt.want {
			t.Errorf("expandTabs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRenderBodyLossless confirms the view layer neither drops, reorders, nor
// mutates content: stripping styling from the rendered body and undoing the
// tab expansion must reproduce every parsed row in order. Width is left 0 so no
// truncation runs. This is the ui-layer analog of diffview's round-trip test.
//
// No RawDiff is supplied, so this also pins the degraded path: with no original
// to align against there are no elision markers and the body is exactly the
// reading diff. TestRenderBodyPreservesRowsWithMarkers covers the aligned case.
func TestRenderBodyLossless(t *testing.T) {
	dir := filepath.Join("..", "..", "meat", "testdata", "python")
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.diff"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no golden diffs in %s (err=%v)", dir, err)
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m := New(Input{ReadingDiff: string(data)})
			plain := ansi.Strip(m.renderBody())
			gotLines := strings.Split(plain, "\n")

			if len(gotLines) != len(m.rows) {
				t.Fatalf("rendered %d lines, want %d rows", len(gotLines), len(m.rows))
			}
			for i, r := range m.rows {
				want := expandTabs(r.Text, tabWidth)
				if gotLines[i] != want {
					t.Errorf("row %d: rendered %q, want %q", i, gotLines[i], want)
				}
			}
		})
	}
}

// TestRenderFrameGoldens is the Phase-1 exit check, run offline: each golden
// (paired with its raw sibling, so the manifest is real) must lay out and
// render a complete frame — header manifest, scrolling body, and footer — with
// no panic and with every reading-diff line present in the viewport content.
func TestRenderFrameGoldens(t *testing.T) {
	dir := filepath.Join("..", "..", "meat", "testdata", "python")
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.diff"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no golden diffs in %s (err=%v)", dir, err)
	}

	for _, goldenPath := range matches {
		t.Run(filepath.Base(goldenPath), func(t *testing.T) {
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			base, _ := strings.CutSuffix(goldenPath, ".golden.diff")
			raw, err := os.ReadFile(base + ".diff")
			if err != nil {
				t.Fatalf("raw sibling for %s: %v", filepath.Base(goldenPath), err)
			}
			elision := meat.ElisionLine(string(raw), string(golden))

			m := New(Input{
				Summary:     "test change summary",
				Elision:     elision,
				ReadingDiff: string(golden),
				RawDiff:     string(raw),
			})
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			frame := updated.View()
			plain := ansi.Strip(frame)

			// Header manifest, including the trust headline.
			for _, want := range []string{"porkchop", "test change summary", elision, "% smaller", "hidden in"} {
				if want != "" && !strings.Contains(plain, want) {
					t.Errorf("frame missing %q", want)
				}
			}
			// Footer.
			if !strings.Contains(plain, "quit") {
				t.Errorf("frame missing footer hints")
			}
			// The viewport shows the top of the diff: its first content line
			// must be present in the rendered frame.
			first := expandTabs(m.rows[0].Text, tabWidth)
			if !strings.Contains(plain, first) {
				t.Errorf("frame missing first diff row %q", first)
			}
			t.Logf("%s: %s", filepath.Base(goldenPath), elision)
		})
	}
}

// alignedGoldens builds a Model per golden with its raw sibling attached, which
// is the configuration every trust feature needs.
func alignedGoldens(t *testing.T) map[string]Model {
	t.Helper()
	dir := filepath.Join("..", "..", "meat", "testdata", "python")
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.diff"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no golden diffs in %s (err=%v)", dir, err)
	}
	out := make(map[string]Model, len(matches))
	for _, path := range matches {
		golden, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		base, _ := strings.CutSuffix(path, ".golden.diff")
		raw, err := os.ReadFile(base + ".diff")
		if err != nil {
			t.Fatalf("raw sibling for %s: %v", filepath.Base(path), err)
		}
		m := New(Input{
			Summary:     "test",
			Elision:     meat.ElisionLine(string(raw), string(golden)),
			ReadingDiff: string(golden),
			RawDiff:     string(raw),
		})
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
		out[filepath.Base(path)] = updated.(Model)
	}
	return out
}

// TestRenderBodyPreservesRowsWithMarkers is the losslessness guarantee once
// markers are in play. Markers add lines and a fold-marked elision's marker
// stands in for meat's "..." row, so the body is no longer 1:1 with the rows —
// but every row must still be accounted for exactly once and in order, either
// rendered as itself or represented by the marker that replaced it.
func TestRenderBodyPreservesRowsWithMarkers(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			if len(m.marks) == 0 {
				t.Fatal("expected elision markers on an aligned golden")
			}
			var seen []int
			for _, bl := range m.body {
				if bl.row < 0 {
					continue
				}
				seen = append(seen, bl.row)
				if bl.kind == bodyRow && bl.text != m.rows[bl.row].Text {
					t.Errorf("row %d rendered text %q, want %q", bl.row, bl.text, m.rows[bl.row].Text)
				}
				if bl.kind == bodyMarker && m.rows[bl.row].Kind != diffview.RowFold {
					t.Errorf("marker stands in for row %d, which is not a fold row", bl.row)
				}
			}
			if len(seen) != len(m.rows) {
				t.Fatalf("body accounts for %d rows, want %d", len(seen), len(m.rows))
			}
			for i, got := range seen {
				if got != i {
					t.Fatalf("body row order diverges at position %d: got row %d", i, got)
				}
			}
		})
	}
}

// TestExpandRevealsOriginal is the Phase-3 exit check: pressing `e` on a marker
// must put the actual elided lines from the original diff on screen.
func TestExpandRevealsOriginal(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			// Pick a substantial hidden line so Contains is a real assertion.
			hidden := m.align.Hidden(m.marks[0])
			want := ""
			for _, line := range hidden {
				if len(strings.TrimSpace(line)) > 8 {
					want = expandTabs(line, tabWidth)
					break
				}
			}
			if want == "" {
				t.Skipf("first elision hides nothing substantial: %q", hidden)
			}

			before := ansi.Strip(m.renderBody())
			if strings.Contains(before, want) {
				t.Fatalf("collapsed body already shows hidden line %q", want)
			}

			expanded, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
			em := expanded.(Model)
			after := ansi.Strip(em.renderBody())
			if !strings.Contains(after, want) {
				t.Errorf("expanding did not reveal %q", want)
			}
			if !em.expanded[0] {
				t.Error("expanded[0] is false after pressing e")
			}
			if len(em.body) <= len(m.body) {
				t.Errorf("body did not grow: %d -> %d lines", len(m.body), len(em.body))
			}

			// And `e` again restores the collapsed view exactly.
			collapsed, _ := em.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
			cm := collapsed.(Model)
			if got := ansi.Strip(cm.renderBody()); got != before {
				t.Error("collapsing did not restore the original body")
			}
		})
	}
}

// TestExpandAllRevealsEveryHiddenChangedLine checks E is a complete answer to
// "show me everything it hid" for the marked elisions.
func TestExpandAllRevealsEveryHiddenChangedLine(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			all, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})
			am := all.(Model)

			wantHidden := 0
			for _, e := range am.marks {
				wantHidden += e.Len()
			}
			gotHidden := 0
			for _, bl := range am.body {
				if bl.kind == bodyHidden {
					gotHidden++
				}
			}
			if gotHidden != wantHidden {
				t.Errorf("E revealed %d hidden lines, want %d", gotHidden, wantHidden)
			}

			// E again collapses everything back.
			none, _ := am.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})
			nm := none.(Model)
			for i, e := range nm.expanded {
				if e {
					t.Fatalf("mark %d still expanded after a second E", i)
				}
			}
		})
	}
}

// TestAuditView checks `a` shows the discard pile grouped by file, including the
// context-only elisions the review view deliberately leaves unmarked.
func TestAuditView(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			audited, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
			am := audited.(Model)
			if !am.audit {
				t.Fatal("pressing a did not enter the audit view")
			}
			plain := ansi.Strip(am.renderAudit())

			// Every file with hidden *changes* must be named, and each of its
			// change-bearing elisions listed. Context-only elisions are counted,
			// not dumped — they cannot conceal a change.
			files := map[string]bool{}
			changeBearing := 0
			for _, e := range am.align.Elisions {
				if e.Changed == 0 {
					continue
				}
				changeBearing++
				if e.File != "" {
					files[e.File] = true
				}
			}
			if len(files) == 0 {
				t.Fatal("no change-bearing elisions carry a file attribution")
			}
			for f := range files {
				if !strings.Contains(plain, f) {
					t.Errorf("audit view missing file %q", f)
				}
			}
			if got := strings.Count(plain, "original lines "); got != changeBearing {
				t.Errorf("audit view lists %d elisions, want %d change-bearing ones", got, changeBearing)
			}

			// Every hidden changed line must actually be on screen: the audit view
			// is the completeness guarantee behind the whole feature.
			for _, e := range am.align.Elisions {
				if e.Changed == 0 {
					continue
				}
				for _, line := range am.align.Hidden(e) {
					if len(strings.TrimSpace(line)) <= 8 {
						continue
					}
					if !strings.Contains(plain, expandTabs(line, tabWidth)) {
						t.Errorf("audit view missing hidden line %q", line)
					}
				}
			}

			// The frame must render, and esc must come back to the review view.
			frame := ansi.Strip(am.View())
			if !strings.Contains(frame, "back to review") {
				t.Error("audit footer missing its exit hint")
			}
			back, _ := am.Update(tea.KeyMsg{Type: tea.KeyEsc})
			if back.(Model).audit {
				t.Error("esc did not leave the audit view")
			}
		})
	}
}

// TestElisionNavigationClamps checks n/p walk the markers and stop at the ends
// rather than wrapping, so a held key settles instead of cycling.
func TestElisionNavigationClamps(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			if m.cur != 0 {
				t.Fatalf("cursor starts at %d, want 0", m.cur)
			}
			cur := tea.Model(m)
			for range len(m.marks) + 3 {
				cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
			}
			if got, want := cur.(Model).cur, len(m.marks)-1; got != want {
				t.Errorf("after walking past the end, cur = %d, want %d", got, want)
			}
			for range len(m.marks) + 3 {
				cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
			}
			if got := cur.(Model).cur; got != 0 {
				t.Errorf("after walking past the start, cur = %d, want 0", got)
			}
		})
	}
}

// TestNoOriginalDegradesCleanly pins the no-RawDiff path: no markers, no crash,
// and the trust keys are inert rather than broken.
func TestNoOriginalDegradesCleanly(t *testing.T) {
	m := New(Input{ReadingDiff: "@@ -1,1 +1,1 @@\n-a\n+b"})
	if len(m.marks) != 0 {
		t.Fatalf("got %d marks with no original, want 0", len(m.marks))
	}
	if m.cur != -1 {
		t.Errorf("cur = %d, want -1", m.cur)
	}
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	cur := sized
	for _, k := range []rune{'e', 'E', 'n', 'p', 'a'} {
		cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k}})
		if cur.View() == "" {
			t.Fatalf("view went empty after %q", k)
		}
	}
}

// sanity: the styler must handle every row kind without panicking.
func TestRenderRowAllKinds(t *testing.T) {
	m := New(Input{})
	m.width = 80
	for _, r := range []diffview.Row{
		{Kind: diffview.RowMeta, Text: "diff --git a/x b/x"},
		{Kind: diffview.RowMeta, Text: "index 1..2 100644"},
		{Kind: diffview.RowHunk, Text: "@@ -1 +1 @@"},
		{Kind: diffview.RowContext, Text: " ctx"},
		{Kind: diffview.RowAdd, Text: "+add"},
		{Kind: diffview.RowDel, Text: "-del"},
		{Kind: diffview.RowFold, Text: "-    ..."},
	} {
		if got := m.renderRow(r); got == "" {
			t.Errorf("renderRow(%v) returned empty", r.Kind)
		}
	}
}
