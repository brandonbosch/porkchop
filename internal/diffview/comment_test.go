package diffview

import (
	"strings"
	"testing"
)

// rawDiffFor wraps hunk body lines in the minimum git headers Parse needs, so a
// case can be written as the lines a reviewer would actually see.
func rawDiffFor(path string, body ...string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\n")
	b.WriteString("--- a/" + path + "\n")
	b.WriteString("+++ b/" + path + "\n")
	b.WriteString("@@ -1,9 +1,9 @@\n")
	for _, l := range body {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// countsFor runs the classification over a whole synthetic diff, the way an
// elision covering all of it would.
func countsFor(t *testing.T, path string, body ...string) (comment, blank, changed int) {
	t.Helper()
	rows := Parse(rawDiffFor(path, body...))
	files := rawFileNames(rows)
	for _, r := range rows {
		if !isChangedLine(r) {
			continue
		}
		changed++
		if isBlankChange(r) {
			blank++
		}
	}
	return commentChanges(rows, files), blank, changed
}

// TestCommentChangesByLanguage pins the lexical rules per language family. Each
// case is a whole hidden run, and the number wanted is how many of its changed
// lines the marker may call comment.
func TestCommentChangesByLanguage(t *testing.T) {
	tests := []struct {
		name string
		path string
		body []string
		want int
	}{
		{
			name: "python line comments",
			path: "a/b.py",
			body: []string{"+# one", "+    # two", "+x = 1"},
			want: 2,
		},
		{
			name: "python docstring opened and closed inside the run",
			path: "a/b.py",
			body: []string{`+    """Summary line.`, "+", "+    More prose.", `+    """`},
			// The empty line is blank, not comment: the two counts stay disjoint.
			want: 3,
		},
		{
			name: "python docstring left open by the run is still prose",
			path: "a/b.py",
			body: []string{`+    """Summary line.`, "+    More prose."},
			want: 2,
		},
		{
			name: "python triple-quoted data is not claimed",
			path: "a/b.py",
			body: []string{`+    SQL = """`, "+    SELECT 1", `+    """`},
			want: 0,
		},
		{
			name: "go line and block comments",
			path: "main.go",
			body: []string{"+// doc", "+/* block", "+ still block", "+*/", "+func f() {}"},
			want: 4,
		},
		{
			name: "go block closing with code after it is code",
			path: "main.go",
			body: []string{"+/* note */ x := 1"},
			want: 0,
		},
		{
			name: "lua block opener is not swallowed by its line prefix",
			path: "s.lua",
			body: []string{"+--[[ note", "+ still note", "+]]", "+local x = 1"},
			want: 3,
		},
		{
			name: "markup comments",
			path: "docs/x.md",
			body: []string{"+<!-- note", "+ more", "+-->", "+text"},
			want: 3,
		},
		{
			name: "sql line comments",
			path: "q.sql",
			body: []string{"+-- note", "+SELECT 1"},
			want: 1,
		},
		{
			name: "extensionless makefile",
			path: "Makefile",
			body: []string{"+# note", "+all:"},
			want: 1,
		},
		{
			name: "unknown extension claims nothing",
			path: "notes.xyz",
			body: []string{"+# looks like a comment", "+// so does this"},
			want: 0,
		},
		{
			name: "context lines are never counted",
			path: "a/b.py",
			body: []string{" # unchanged comment", "+x = 1"},
			want: 0,
		},
		{
			name: "both sides are scanned independently",
			path: "main.go",
			body: []string{"-// old", "+// new", "+x := 1"},
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, changed := countsFor(t, tt.path, tt.body...)
			if got != tt.want {
				t.Errorf("commentChanges = %d, want %d", got, tt.want)
			}
			if got > changed {
				t.Errorf("commentChanges = %d exceeds %d changed lines", got, changed)
			}
		})
	}
}

// TestCommentDeclinesARunThatBeginsInsideABlock is the safety property the whole
// feature rests on. A hidden run can start anywhere, including in the middle of a
// docstring, and there the scanner sees a closing """ with no opener. Python's
// delimiters are symmetric, so reading it as an opener would invert the rest of
// the run and label real code as prose — the one error a reviewer cannot catch,
// because the marker is telling them not to look. It must decline instead.
func TestCommentDeclinesARunThatBeginsInsideABlock(t *testing.T) {
	got, _, changed := countsFor(t, "a/b.py",
		`+    """`, // the tail of a docstring whose opener the reading diff kept
		"+    def hidden(self):",
		"+        return 1",
	)
	if got != 0 {
		t.Errorf("commentChanges = %d, want 0 — a bare \"\"\" was read as an opener and %d changed lines were labelled prose", got, changed)
	}
}

// TestCommentIsDisjointFromBlankAndBoundedByChanged is the arithmetic the marker
// wording adds under: it prints Comment and Blank side by side and lets the
// reviewer read them as the whole of Changed, which is a lie if they overlap.
func TestCommentIsDisjointFromBlankAndBoundedByChanged(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])
			commentOnly := 0
			for _, e := range a.Elisions {
				if e.Comment+e.Blank > e.Changed {
					t.Errorf("elision at %d: Comment=%d + Blank=%d exceeds Changed=%d",
						e.RawStart, e.Comment, e.Blank, e.Changed)
				}
				if e.Changed > 0 && e.Comment > 0 && e.Comment+e.Blank == e.Changed {
					commentOnly++
					// An independent smoke test of the same claim, by a rule the
					// classifier does not use: the goldens are Python, and no line of
					// prose opens a suite. If an inverted block ever labelled real
					// code as comment, a "def"/"if"/"for" would surface here — and
					// nowhere else, because the marker is telling the reviewer not
					// to look.
					for _, l := range a.Hidden(e) {
						if len(l) == 0 || l[0] != '+' && l[0] != '-' {
							continue
						}
						body := strings.TrimSpace(l[1:])
						if strings.HasSuffix(body, ":") && startsWithPythonKeyword(body) {
							t.Errorf("elision at %d is worded comment-only but hides %q",
								e.RawStart, l)
						}
					}
				}
			}
			if commentOnly == 0 {
				t.Error("no comment-only elisions in this golden, so the marker wording is untested here")
			}
		})
	}
}

// TestCommentNeverClaimsAContextOnlyElision guards the boundary with the other
// wording rules: an elision that hides no changed line at all is context, and
// must not acquire a comment count that would reword it.
func TestCommentNeverClaimsAContextOnlyElision(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			for _, e := range Align(pair[0], pair[1]).Elisions {
				if e.Changed == 0 && e.Comment != 0 {
					t.Errorf("context-only elision at %d carries Comment=%d", e.RawStart, e.Comment)
				}
			}
		})
	}
}

// startsWithPythonKeyword reports whether a line opens a Python suite. It exists
// only for the golden smoke test above and deliberately shares no code with the
// classifier it is checking.
func startsWithPythonKeyword(body string) bool {
	for _, kw := range []string{"def ", "class ", "if ", "elif ", "else", "for ", "while ", "try", "except", "finally", "with "} {
		if strings.HasPrefix(body, kw) {
			return true
		}
	}
	return false
}
