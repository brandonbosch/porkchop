package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// bodyOf renders a reading diff with no original, which is the path that shows
// the header block exactly as the reviewer sees it.
func bodyOf(t *testing.T, diff string) (Model, []string) {
	t.Helper()
	m := New(Input{ReadingDiff: diff})
	m.width = 80
	m.rebuild()
	return m, strings.Split(ansi.Strip(m.renderBody()), "\n")
}

func hasLineWith(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// TestBannerFoldsOnlyWhatItRestates is the rule the collapse rests on. A git
// header carries no line of the change, so folding it is not eliding — but only
// as long as nothing that varies goes with it. Each case here is a header git
// emits for a reason, and the reason has to survive.
func TestBannerFoldsOnlyWhatItRestates(t *testing.T) {
	tests := []struct {
		name  string
		diff  string
		gone  []string
		kept  []string
		label string
	}{
		{
			name: "ordinary edit",
			diff: `diff --git a/pkg/x.go b/pkg/x.go
index f6cce962..040b2841 100644
--- a/pkg/x.go
+++ b/pkg/x.go
@@ -1,2 +1,2 @@
-old
+new
`,
			gone:  []string{"index f6cce962", "--- a/pkg/x.go", "+++ b/pkg/x.go"},
			kept:  []string{"@@ -1,2 +1,2 @@", "-old", "+new"},
			label: "pkg/x.go",
		},
		{
			name: "new file keeps the marks that say it is new",
			diff: `diff --git a/pkg/new.go b/pkg/new.go
new file mode 100644
index 00000000..040b2841
--- /dev/null
+++ b/pkg/new.go
@@ -0,0 +1 @@
+hello
`,
			gone:  []string{"index 00000000", "+++ b/pkg/new.go"},
			kept:  []string{"new file mode 100644", "--- /dev/null"},
			label: "pkg/new.go",
		},
		{
			name: "deleted file keeps its /dev/null",
			diff: `diff --git a/pkg/gone.go b/pkg/gone.go
deleted file mode 100644
index 040b2841..00000000
--- a/pkg/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-bye
`,
			gone:  []string{"index 040b2841", "--- a/pkg/gone.go"},
			kept:  []string{"deleted file mode 100644", "+++ /dev/null"},
			label: "pkg/gone.go",
		},
		{
			name: "rename shows both paths",
			diff: `diff --git a/pkg/old.go b/pkg/new.go
similarity index 94%
rename from pkg/old.go
rename to pkg/new.go
index f6cce962..040b2841 100644
--- a/pkg/old.go
+++ b/pkg/new.go
@@ -1 +1 @@
-a
+b
`,
			gone:  []string{"index f6cce962", "--- a/pkg/old.go", "+++ b/pkg/new.go"},
			kept:  []string{"similarity index 94%", "rename from pkg/old.go", "rename to pkg/new.go"},
			label: "pkg/old.go → pkg/new.go",
		},
		{
			name: "mode change survives",
			diff: `diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
index f6cce962..040b2841
--- a/run.sh
+++ b/run.sh
@@ -1 +1 @@
-a
+b
`,
			gone:  []string{"index f6cce962"},
			kept:  []string{"old mode 100644", "new mode 100755"},
			label: "run.sh",
		},
		{
			name: "binary marker survives",
			diff: `diff --git a/logo.png b/logo.png
index f6cce962..040b2841 100644
Binary files a/logo.png and b/logo.png differ
`,
			gone:  []string{"index f6cce962"},
			kept:  []string{"Binary files a/logo.png and b/logo.png differ"},
			label: "logo.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, lines := bodyOf(t, tt.diff)
			if !hasLineWith(lines, tt.label) {
				t.Errorf("no banner reading %q in:\n%s", tt.label, strings.Join(lines, "\n"))
			}
			for _, want := range tt.kept {
				if !hasLineWith(lines, want) {
					t.Errorf("%q was folded away but nothing restates it:\n%s", want, strings.Join(lines, "\n"))
				}
			}
			for _, gone := range tt.gone {
				if hasLineWith(lines, gone) {
					t.Errorf("%q is still on screen; the banner already says it", gone)
				}
			}
			// Whatever the body draws, every row is still accounted for exactly
			// once — the banner stands in for the rows it swallowed.
			seen := make([]int, len(m.rows))
			for _, bl := range m.body {
				for _, r := range bl.rows() {
					seen[r]++
				}
			}
			for i, n := range seen {
				if n != 1 {
					t.Errorf("row %d (%q) accounted for %d times, want 1", i, m.rows[i].Text, n)
				}
			}
		})
	}
}

// TestBannerSeparatesFilesInBothViews is the thing that prompted the change: a
// file header mashed between the previous file's last line and this one's first
// is a boundary the eye slides over. Every file after the first opens with a
// blank line, in the unified view and the split view alike.
func TestBannerSeparatesFilesInBothViews(t *testing.T) {
	for name, base := range alignedGoldens(t) {
		for _, split := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				m := base
				m.split, m.splitPinned = split, true
				m.rebuild()

				banners := 0
				for i, bl := range m.body {
					if bl.kind != bodyBanner {
						continue
					}
					banners++
					if i == 0 {
						continue // the first file needs no gap above it
					}
					if m.body[i-1].kind != bodySpacer {
						t.Errorf("banner %q at line %d has no blank line above it", bl.text, i)
					}
				}
				if banners != len(m.files) {
					t.Errorf("drew %d banners for %d files", banners, len(m.files))
				}
			})
		}
	}
}
