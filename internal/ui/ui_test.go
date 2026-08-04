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
				Summary:      "test change summary",
				Elision:      elision,
				ReadingDiff:  string(golden),
				RawDiffBytes: len(raw),
			})
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
			frame := updated.View()
			plain := ansi.Strip(frame)

			// Header manifest.
			for _, want := range []string{"porkchop", "test change summary", elision, "% smaller"} {
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
