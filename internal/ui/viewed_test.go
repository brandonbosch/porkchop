package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// fakeViewed is a ViewedStore backed by a map, standing in for the repo's marker
// file. saves counts flushes so a test can assert a toggle is durable rather than
// only on screen.
type fakeViewed struct {
	marks map[string]string // path -> digest
	saves int
}

func newFakeViewed() *fakeViewed { return &fakeViewed{marks: map[string]string{}} }

func (f *fakeViewed) Has(path, digest string) bool {
	if digest == "" {
		return false
	}
	d, ok := f.marks[path]
	return ok && d == digest
}

func (f *fakeViewed) Set(path, digest string, on bool) {
	if !on {
		delete(f.marks, path)
		return
	}
	if digest == "" {
		return
	}
	f.marks[path] = digest
}

func (f *fakeViewed) Save() { f.saves++ }

// press drives a rune key through the model, the way a reviewer would.
func press(m Model, r rune) Model {
	up, _ := m.Update(tea.KeyPressMsg{Code: r})
	return up.(Model)
}

func pressCode(m Model, code rune) Model {
	up, _ := m.Update(tea.KeyPressMsg{Code: code})
	return up.(Model)
}

// viewedModel builds a laid-out model over a golden with a marker store attached.
func viewedModel(t *testing.T, in Input, vs ViewedStore, width int) Model {
	t.Helper()
	in.Viewed = vs
	up, _ := New(in).Update(tea.WindowSizeMsg{Width: width, Height: 40})
	return up.(Model)
}

// TestViewedTogglePersists is the core of the workflow: `v` records a decision that
// outlives the process. The second model is built fresh from the same store, which
// is what "quit and come back" means.
func TestViewedTogglePersists(t *testing.T) {
	for name, in := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			vs := newFakeViewed()
			m := viewedModel(t, in, vs, 200)
			if len(m.files) == 0 {
				t.Skip("golden names no files")
			}
			if m.currentFileViewed() {
				t.Fatal("a file is checked off before the reviewer has done anything")
			}

			m = press(m, 'v')
			if !m.currentFileViewed() {
				t.Fatal("v did not check the current file off")
			}
			if vs.saves == 0 {
				t.Error("v did not flush the store")
			}
			if got := len(vs.marks); got != 1 {
				t.Fatalf("store holds %d markers after one v, want 1", got)
			}

			// A fresh screen over the same store must come back checked off.
			again := viewedModel(t, in, vs, 200)
			if !again.viewed[0] {
				t.Error("the marker did not survive rebuilding the screen")
			}

			// And v again clears it, durably.
			m = press(m, 'v')
			if m.currentFileViewed() {
				t.Error("v did not un-check the file")
			}
			if len(vs.marks) != 0 {
				t.Errorf("store still holds %d markers after unchecking", len(vs.marks))
			}
		})
	}
}

// TestViewedSurvivesTheChangeGrowing is the agent-session case end to end, at the
// level a reviewer experiences it: check a file off, let the rest of the diff move,
// and the file stays checked. This is the behaviour the per-file digest exists for,
// and it is what a cache-key-keyed marker would get wrong.
func TestViewedSurvivesTheChangeGrowing(t *testing.T) {
	in := goldenInputs(t)["django-526b1b414d8e.golden.diff"]
	if in.RawDiff == "" {
		t.Skip("django golden unavailable")
	}
	vs := newFakeViewed()
	m := viewedModel(t, in, vs, 200)
	if len(m.files) < 2 {
		t.Skipf("golden has %d files, need 2", len(m.files))
	}

	// The reviewer checks off the first file.
	m = press(m, 'v')
	first := m.files[0]
	if !vs.Has(first, m.digests[0]) {
		t.Fatalf("%s was not recorded", first)
	}

	// The agent commits again, touching only the last file: its section of the raw
	// diff grows, every other file's is untouched.
	last := m.files[len(m.files)-1]
	grown := in
	grown.RawDiff = appendToFileSection(t, in.RawDiff, last)

	after := viewedModel(t, grown, vs, 200)
	if !after.viewed[0] {
		t.Errorf("%s un-checked itself when a different file changed", first)
	}
	if after.viewed[len(after.viewed)-1] {
		t.Errorf("%s stayed checked though its own content changed", last)
	}
}

// appendToFileSection adds a line to one file's hunk in a raw diff, simulating a
// later commit touching that file and nothing else.
func appendToFileSection(t *testing.T, raw, path string) string {
	t.Helper()
	lines := strings.Split(raw, "\n")
	in, last := false, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "diff --git ") {
			in = strings.HasSuffix(l, " b/"+path)
			continue
		}
		if in && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") || strings.HasPrefix(l, " ")) {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("no hunk lines found for %q", path)
	}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, "+# a later commit touched this file")
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n")
}

// TestViewedWithNoOriginalIsSessionOnly pins the honest degradation. Rendering a
// reading diff with no original gives porkchop no trustworthy identity for a file,
// so the toggle still works on screen — a reviewer can track themselves within the
// session — but nothing is written that a later session would read back and treat
// as "already reviewed".
func TestViewedWithNoOriginalIsSessionOnly(t *testing.T) {
	in := goldenInputs(t)["flask-c17f37939073.golden.diff"]
	in.RawDiff = ""
	vs := newFakeViewed()
	m := viewedModel(t, in, vs, 200)
	if len(m.files) == 0 {
		t.Skip("golden names no files")
	}
	for _, d := range m.digests {
		if d != "" {
			t.Fatalf("got a digest %q with no original diff", d)
		}
	}
	m = press(m, 'v')
	if !m.currentFileViewed() {
		t.Error("v did not track the file within the session")
	}
	if len(vs.marks) != 0 {
		t.Errorf("recorded %d markers with no digest to key them to", len(vs.marks))
	}
}

// TestViewedNilStore covers a diff piped in from outside any repo: there is nowhere
// to persist, and the screen must still work.
func TestViewedNilStore(t *testing.T) {
	in := goldenInputs(t)["flask-c17f37939073.golden.diff"]
	up, _ := New(in).Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m := up.(Model)
	m = press(m, 'v')
	if len(m.files) > 0 && !m.currentFileViewed() {
		t.Error("v did not work with no store attached")
	}
	m = press(m, 'v')
	if m.currentFileViewed() {
		t.Error("v did not toggle back with no store attached")
	}
}

// TestStepUnviewedWraps checks tab finds what is left. It wraps on purpose: the
// unviewed set shrinks as the reviewer works, so a clamping key would strand files
// they skipped above the cursor.
func TestStepUnviewedWraps(t *testing.T) {
	in := goldenInputs(t)["django-526b1b414d8e.golden.diff"]
	m := viewedModel(t, in, newFakeViewed(), 200)
	if len(m.files) < 3 {
		t.Skipf("golden has %d files, need 3", len(m.files))
	}

	// Check off everything but the first file, then step from the end: tab must
	// come back around to it rather than settling.
	for i := 1; i < len(m.viewed); i++ {
		m.viewed[i] = true
	}
	m.jumpToRow(m.fileRows[len(m.fileRows)-1])
	if got := m.currentFileIndex(); got == 0 {
		t.Fatalf("setup failed: still at file 0")
	}
	m = pressCode(m, tea.KeyTab)
	if got := m.currentFileIndex(); got != 0 {
		t.Errorf("tab from the last file landed on %d, want 0 (the only unviewed file)", got)
	}

	// With nothing unviewed, tab must do nothing at all rather than cycle.
	for i := range m.viewed {
		m.viewed[i] = true
	}
	before := m.vp.YOffset()
	m = pressCode(m, tea.KeyTab)
	if got := m.vp.YOffset(); got != before {
		t.Errorf("tab moved from %d to %d with every file viewed", before, got)
	}
}

// TestViewedShowsInHeaderAndRule checks the two places the state is reported: a
// check beside the file name, and progress in the manifest. Without them the
// feature is invisible and a reviewer cannot tell what they have left.
func TestViewedShowsInHeaderAndRule(t *testing.T) {
	in := goldenInputs(t)["django-526b1b414d8e.golden.diff"]
	m := viewedModel(t, in, newFakeViewed(), 200)
	if len(m.files) < 2 {
		t.Skipf("golden has %d files", len(m.files))
	}

	if got := ansi.Strip(m.renderHeader()); strings.Contains(got, "viewed") {
		t.Error("the progress tile shows before anything has been viewed")
	}
	if got := ansi.Strip(m.renderRule()); strings.Contains(got, "✓") {
		t.Error("the rule shows a check before anything has been viewed")
	}

	m = press(m, 'v')
	rule := ansi.Strip(m.renderRule())
	if !strings.Contains(rule, "✓") {
		t.Errorf("no check in the rule after v: %q", rule)
	}
	if !strings.Contains(rule, m.files[0]) {
		t.Errorf("the check displaced the file name: %q", rule)
	}
	header := ansi.Strip(m.renderHeader())
	if want := fmt.Sprintf("1/%d viewed", len(m.files)); !strings.Contains(header, want) {
		t.Errorf("header does not report %q:\n%s", want, header)
	}

	// The finish line is named rather than left to be inferred from two equal
	// numbers, because "am I done" is the question the tile exists to answer.
	for i := range m.viewed {
		m.viewed[i] = true
	}
	if got := ansi.Strip(m.renderHeader()); !strings.Contains(got, fmt.Sprintf("all %d files viewed", len(m.files))) {
		t.Errorf("header does not call out completion:\n%s", got)
	}
}

// TestHeaderAndFooterFitTheWidth is the guard the tiering needs: a footer wider
// than the terminal loses its tail, and the tail is where `q quit` lives. The
// header's tile row must hold one line too, since the viewport's height is measured
// from it.
func TestHeaderAndFooterFitTheWidth(t *testing.T) {
	in := goldenInputs(t)["django-526b1b414d8e.golden.diff"]
	for _, width := range []int{60, 80, 100, 120, 140, 200} {
		for _, allViewed := range []bool{false, true} {
			m := viewedModel(t, in, newFakeViewed(), width)
			if allViewed {
				for i := range m.viewed {
					m.viewed[i] = true
				}
			}
			// Every mode the footer has a tier for.
			modes := map[string]Model{"review": m, "audit": m, "search": m}
			am := m
			am.audit = true
			modes["audit"] = am
			sm := m
			sm.query = "def"
			sm.findHits()
			modes["search"] = sm

			for mode, mm := range modes {
				what := fmt.Sprintf("w=%d viewed=%v mode=%s", width, allViewed, mode)
				footer := mm.renderFooter()
				if got := lipgloss.Width(footer); got > width {
					t.Errorf("%s: footer is %d cells wide: %q", what, got, ansi.Strip(footer))
				}
				// The last hint must survive, or the reviewer cannot find the exit.
				if plain := ansi.Strip(footer); !strings.Contains(plain, "q quit") {
					t.Errorf("%s: footer lost its tail: %q", what, plain)
				}
				header := mm.renderHeader()
				if got := lipgloss.Height(header); got != 3 {
					t.Errorf("%s: header is %d lines, want 3 (title, tiles, rule)", what, got)
				}
				for _, line := range strings.Split(header, "\n") {
					if got := lipgloss.Width(line); got > width {
						t.Errorf("%s: header line is %d cells: %q", what, got, ansi.Strip(line))
					}
				}
			}
		}
	}
}

// TestMarkerNamesWhatItHides covers the wording rules: a marker whose hidden
// changed lines are all empty says "blank", one whose hidden changed lines are
// nothing but prose says "comment", and only a marker hiding real code says
// "changed". A reviewer reads the marker to decide whether to spend an expand,
// so each of the three has to be distinguishable without paying for it.
//
// The marker is still drawn and still counted in every case — suppressing one
// would put the header's "N hidden in M spots" out of step with what is on screen.
func TestMarkerNamesWhatItHides(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			blankOnly, commentOnly, code := 0, 0, 0
			for i, e := range m.marks {
				text := m.markerText(i)
				switch {
				case e.Changed > 0 && e.Blank == e.Changed:
					blankOnly++
					if !strings.Contains(text, "blank") {
						t.Errorf("mark %d hides %d blank lines but reads %q", i, e.Blank, text)
					}
					if strings.Contains(text, "changed") {
						t.Errorf("mark %d reads %q, which invites an expand that reveals nothing", i, text)
					}
					if want := fmt.Sprintf("%d blank", e.Blank); !strings.Contains(text, want) {
						t.Errorf("mark %d does not carry %q: %q", i, want, text)
					}
				case e.Changed > 0 && e.Comment > 0 && e.Comment+e.Blank == e.Changed:
					commentOnly++
					if want := fmt.Sprintf("%d comment", e.Comment); !strings.Contains(text, want) {
						t.Errorf("mark %d hides %d comment lines but reads %q", i, e.Comment, text)
					}
					if strings.Contains(text, "changed") {
						t.Errorf("mark %d hides no code but reads %q", i, text)
					}
					// The blank remainder is named rather than folded into the
					// comment count, so the two still add up to Changed on screen.
					if e.Blank > 0 {
						if want := fmt.Sprintf("+%d blank", e.Blank); !strings.Contains(text, want) {
							t.Errorf("mark %d does not carry %q: %q", i, want, text)
						}
					}
				case e.Changed > 0:
					code++
					// A marker hiding any real code must keep saying "changed", even
					// when some of what it hides is blank or commentary.
					if !strings.Contains(text, "changed") {
						t.Errorf("mark %d hides %d changed lines (%d blank, %d comment) but reads %q",
							i, e.Changed, e.Blank, e.Comment, text)
					}
				}
			}
			if blankOnly == 0 || commentOnly == 0 || code == 0 {
				t.Skipf("golden has %d blank-only, %d comment-only and %d substantive markers",
					blankOnly, commentOnly, code)
			}

			// The header's accounting is wording-independent: every marker still
			// counts, blank or not.
			if got := ansi.Strip(m.renderHeader()); !strings.Contains(got, fmt.Sprintf("in %d spots", len(m.marks))) {
				t.Errorf("header no longer reconciles with the marker count:\n%s", got)
			}
		})
	}
}

// TestMarkerColorMatchesItsWording is the coupling that makes the tiering safe to
// rely on. A reviewer who learns "amber means code" stops reading the words, so a
// marker painted amber while reading "comment" would be worse than no color at
// all. Both come from kindOf; this checks they still agree on real fixtures, and
// that the three tiers are actually distinguishable rather than three names for
// one style.
func TestMarkerColorMatchesItsWording(t *testing.T) {
	for name, m := range alignedGoldens(t) {
		t.Run(name, func(t *testing.T) {
			seen := map[markerKind]bool{}
			for i, e := range m.marks {
				kind := kindOf(e)
				seen[kind] = true
				text := m.markerText(i)
				want := map[markerKind]string{
					markerCode:    "changed",
					markerProse:   "comment",
					markerEmpty:   "blank",
					markerContext: "context",
				}[kind]
				if !strings.Contains(text, want) {
					t.Errorf("mark %d is painted as kind %d but reads %q", i, kind, text)
				}
				// The rendered line must carry the tier's own style, not the amber
				// default, or the words and the color part company on screen.
				styled := m.st.forMarker(kind, false).Render(text)
				if plain := ansi.Strip(styled); plain != text {
					t.Errorf("mark %d: styling altered the text %q -> %q", i, text, plain)
				}
				if kind != markerCode && styled == m.st.marker.Render(text) {
					t.Errorf("mark %d reads %q but is painted with the code tier", i, text)
				}
			}
			if len(seen) < 3 {
				t.Skipf("golden exercises only %d marker kinds", len(seen))
			}
			// Distinct tiers must render distinctly, in both palettes.
			for _, dark := range []bool{true, false} {
				st := newStyles(dark)
				const probe = "▸ 1 line hidden"
				code, prose, quiet := st.forMarker(markerCode, false).Render(probe),
					st.forMarker(markerProse, false).Render(probe),
					st.forMarker(markerEmpty, false).Render(probe)
				if code == prose || prose == quiet || code == quiet {
					t.Errorf("dark=%v: marker tiers are not visually distinct:\n  code  %q\n  prose %q\n  quiet %q",
						dark, code, prose, quiet)
				}
			}
		})
	}
}
