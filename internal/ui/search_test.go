package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// searchDiff has the needle "session" at a known count: three lowercase and one
// capitalised, so smart case has something to distinguish.
const searchDiff = "diff --git a/s.py b/s.py\n" +
	"--- a/s.py\n" +
	"+++ b/s.py\n" +
	"@@ -1,5 +1,5 @@\n" +
	" def open_session(self):\n" +
	"-    self.session = None\n" +
	"+    self.session = Session()\n" +
	"    return self.session\n"

// typeQuery opens the search prompt and types a query into it, returning the model
// with the query still open.
func typeQuery(m tea.Model, q string) tea.Model {
	m, _ = m.Update(tea.KeyPressMsg{Code: '/'})
	for _, r := range q {
		m, _ = m.Update(tea.KeyPressMsg{Code: r})
	}
	return m
}

func searchReady(t *testing.T, diff string, width int) tea.Model {
	t.Helper()
	m, _ := New(Input{ReadingDiff: diff}).Update(tea.WindowSizeMsg{Width: width, Height: 30})
	return m
}

// TestSearchFindsAndHighlights checks a query reaches the screen: matches are
// located, counted, and painted.
func TestSearchFindsAndHighlights(t *testing.T) {
	m := typeQuery(searchReady(t, searchDiff, 80), "session")
	sm := m.(Model)

	if !sm.searching {
		t.Fatal("the prompt is not open")
	}
	if sm.query != "session" {
		t.Fatalf("query is %q, want %q", sm.query, "session")
	}
	// Unified at 80 columns, so one content area per line. Five occurrences:
	// "open_session", the two "self.session"s, the closing "self.session", and
	// "Session()" — which a lower-case query catches by folding.
	if len(sm.hits) != 5 {
		t.Errorf("found %d matches, want 5: %v", len(sm.hits), sm.hits)
	}
	// The footer reports the count while typing.
	if got := ansi.Strip(sm.renderFooter()); !strings.Contains(got, "5 matches") {
		t.Errorf("footer %q does not report the match count", got)
	}
	// And the match background reaches the body. Dark is the seeded palette.
	const matchBg = "48;2;90;74;26"
	if got := emphasized(sm.renderBody(), matchBg); !strings.Contains(got, "session") {
		t.Errorf("no match highlighting in the body; highlighted %q", got)
	}
}

// TestSearchSmartCase checks a lower-case query is case-insensitive and an
// upper-case letter makes it exact, which is the behaviour that lets a reviewer
// type fast by default and be precise on purpose.
func TestSearchSmartCase(t *testing.T) {
	lower := typeQuery(searchReady(t, searchDiff, 80), "session").(Model)
	upper := typeQuery(searchReady(t, searchDiff, 80), "Session").(Model)

	if len(lower.hits) <= len(upper.hits) {
		t.Errorf("lower-case query found %d matches and %q found %d; the lower-case one should match more",
			len(lower.hits), "Session", len(upper.hits))
	}
	if len(upper.hits) != 1 {
		t.Errorf("%q found %d matches, want the 1 capitalised occurrence", "Session", len(upper.hits))
	}
}

// TestSearchStepWraps checks n/N walk the matches and wrap, which is what a search
// is expected to do — as against elision stepping, which clamps.
func TestSearchStepWraps(t *testing.T) {
	m, _ := typeQuery(searchReady(t, searchDiff, 80), "session").Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sm := m.(Model)
	if sm.searching {
		t.Fatal("enter did not close the prompt")
	}
	if !sm.searchActive() {
		t.Fatal("the committed query is not active")
	}

	start := sm.hitCur
	cur := tea.Model(sm)
	for range len(sm.hits) {
		cur, _ = cur.Update(tea.KeyPressMsg{Code: 'n'})
	}
	if got := cur.(Model).hitCur; got != start {
		t.Errorf("walking every match returned to %d, want the starting %d", got, start)
	}
	back, _ := cur.Update(tea.KeyPressMsg{Code: 'N'})
	if got, want := back.(Model).hitCur, (start-1+len(sm.hits))%len(sm.hits); got != want {
		t.Errorf("N moved to %d, want %d", got, want)
	}
}

// TestSearchOwnsTheKeyboard is the safety property of a modal prompt: while it is
// open, keys that mean something in the review view must not fire. Typing "q" into
// the search box has to search for "q".
func TestSearchOwnsTheKeyboard(t *testing.T) {
	m := typeQuery(searchReady(t, searchDiff, 80), "q").(Model)
	if m.query != "q" {
		t.Errorf("query is %q; the prompt did not consume the keypress", m.query)
	}
	for _, key := range "aeEun" {
		next, _ := tea.Model(m).Update(tea.KeyPressMsg{Code: key})
		m = next.(Model)
	}
	if m.query != "qaeEun" {
		t.Errorf("query is %q, want %q — a review keybinding leaked through", m.query, "qaeEun")
	}
	if m.audit {
		t.Error("typing 'a' into the search box opened the audit view")
	}
}

// TestSearchEscapes checks the two ways out: esc in the prompt abandons the query,
// and esc after committing clears the highlights before it will quit.
func TestSearchEscapes(t *testing.T) {
	typed := typeQuery(searchReady(t, searchDiff, 80), "session")

	abandoned, _ := typed.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	am := abandoned.(Model)
	if am.searching || am.query != "" || len(am.hits) != 0 {
		t.Errorf("esc left searching=%v query=%q hits=%d", am.searching, am.query, len(am.hits))
	}

	committed, _ := typed.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	cleared, _ := committed.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	clm := cleared.(Model)
	if clm.searchActive() || len(clm.hits) != 0 {
		t.Errorf("esc after committing left query=%q hits=%d", clm.query, len(clm.hits))
	}
}

// TestSearchEditing covers the prompt's editing keys, which exist so a mistyped
// query can be fixed rather than restarted.
func TestSearchEditing(t *testing.T) {
	m := typeQuery(searchReady(t, searchDiff, 80), "self.sess")

	back, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := back.(Model).query; got != "self.ses" {
		t.Errorf("backspace left %q, want %q", got, "self.ses")
	}
	word, _ := m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if got := word.(Model).query; got != "" {
		t.Errorf("ctrl+w on a single word left %q, want empty", got)
	}
	clear, _ := m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if got := clear.(Model).query; got != "" {
		t.Errorf("ctrl+u left %q, want empty", got)
	}
}

// TestSearchDoesNotShadowElisionStepping checks n/p keep meaning "elision" when no
// search is live, so Phase 3's binding is intact whenever the reviewer is not
// searching.
func TestSearchDoesNotShadowElisionStepping(t *testing.T) {
	for name, in := range goldenInputs(t) {
		t.Run(name, func(t *testing.T) {
			m := splitModel(t, in, 200)
			if len(m.marks) < 2 {
				t.Skip("needs at least two elisions")
			}
			stepped, _ := tea.Model(m).Update(tea.KeyPressMsg{Code: 'n'})
			if got := stepped.(Model).cur; got != 1 {
				t.Fatalf("n moved the elision cursor to %d, want 1", got)
			}

			// With a search committed, n moves matches and leaves the elision cursor
			// where it was.
			searched, _ := typeQuery(stepped, "def").Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			sm := searched.(Model)
			if len(sm.hits) == 0 {
				t.Skip("query matched nothing in this golden")
			}
			after, _ := searched.Update(tea.KeyPressMsg{Code: 'n'})
			am := after.(Model)
			if am.cur != 1 {
				t.Errorf("n moved the elision cursor to %d while searching; want it left at 1", am.cur)
			}
			if am.hitCur == sm.hitCur && len(sm.hits) > 1 {
				t.Error("n did not move the match cursor while a search was active")
			}
		})
	}
}

// TestSearchSurvivesViewToggle checks a committed query is re-found against the
// rebuilt body when the view changes, rather than left pointing at lines that have
// moved.
func TestSearchSurvivesViewToggle(t *testing.T) {
	m, _ := typeQuery(searchReady(t, searchDiff, 200), "session").Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	before := m.(Model)
	if len(before.hits) == 0 {
		t.Fatal("no matches before the toggle")
	}

	toggled, _ := m.Update(tea.KeyPressMsg{Code: 'u'})
	tm := toggled.(Model)
	if len(tm.hits) == 0 {
		t.Fatal("the query lost all its matches across the toggle")
	}
	for _, h := range tm.hits {
		if h.line < 0 || h.line >= len(tm.body) {
			t.Fatalf("match on line %d, outside the rebuilt body of %d lines", h.line, len(tm.body))
		}
		text, _ := tm.displayCell(tm.body[h.line], h.side)
		if h.span.End > len(text) {
			t.Fatalf("match span %v runs past its line %q", h.span, text)
		}
		if got := strings.ToLower(text[h.span.Start:h.span.End]); got != "session" {
			t.Errorf("match points at %q, want %q", got, "session")
		}
	}
}
