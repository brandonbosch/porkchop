package diffview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClassification(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []RowKind
	}{
		{
			name: "file header then simple hunk",
			in: "diff --git a/x.py b/x.py\n" +
				"index abc..def 100644\n" +
				"--- a/x.py\n" +
				"+++ b/x.py\n" +
				"@@ -1,3 +1,3 @@\n" +
				" ctx\n" +
				"-old\n" +
				"+new\n",
			want: []RowKind{RowMeta, RowMeta, RowMeta, RowMeta, RowHunk, RowContext, RowDel, RowAdd},
		},
		{
			name: "fold rows of each polarity are folds, not add/del/context",
			in: "@@ -1,4 +1,4 @@\n" +
				" a\n" +
				"-    ...\n" +
				"+    ...\n" +
				"     ...\n",
			want: []RowKind{RowHunk, RowContext, RowFold, RowFold, RowFold},
		},
		{
			name: "source line that looks like a file marker stays source inside a hunk",
			// "-- counter" is a removed line whose content is "- counter"; it is
			// only a "---" file marker when paired with a "+++" line.
			in: "@@ -1,2 +1,2 @@\n" +
				"-- counter\n" +
				"++ counter\n",
			want: []RowKind{RowHunk, RowDel, RowAdd},
		},
		{
			name: "--- / +++ pair inside a hunk ends the hunk (new file, no diff --git)",
			in: "@@ -1,1 +1,1 @@\n" +
				" a\n" +
				"--- b/y.py\n" +
				"+++ b/y.py\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-z\n",
			want: []RowKind{RowHunk, RowContext, RowMeta, RowMeta, RowHunk, RowDel},
		},
		{
			name: "no-newline marker and second hunk",
			in: "@@ -1,1 +1,1 @@\n" +
				"-a\n" +
				"\\ No newline at end of file\n" +
				"@@ -5,1 +5,1 @@\n" +
				"+b\n",
			want: []RowKind{RowHunk, RowDel, RowMeta, RowHunk, RowAdd},
		},
		{
			name: "empty input yields no rows",
			in:   "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := Parse(tt.in)
			if len(rows) != len(tt.want) {
				t.Fatalf("got %d rows, want %d\nrows: %+v", len(rows), len(tt.want), rows)
			}
			for i, r := range rows {
				if r.Kind != tt.want[i] {
					t.Errorf("row %d (%q): got kind %v, want %v", i, r.Text, r.Kind, tt.want[i])
				}
			}
		})
	}
}

// goldenDir locates meat/testdata/python from the diffview package directory.
func goldenDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "meat", "testdata", "python")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("golden dir not found (%s): %v", dir, err)
	}
	return dir
}

// TestParseGoldensLossless is the strongest signal: every reading-diff golden
// must parse into rows whose text, rejoined, reproduces the input exactly. This
// proves the classifier assigns exactly one kind to every line and never drops,
// duplicates, or mutates content.
func TestParseGoldensLossless(t *testing.T) {
	dir := goldenDir(t)
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.diff"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no golden diffs found in %s (err=%v)", dir, err)
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)
			rows := Parse(content)

			var b strings.Builder
			for i, r := range rows {
				if i > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(r.Text)
			}
			// Parse drops a single trailing newline (like meat's splitter), so
			// compare against the input with at most one trailing newline removed.
			want := strings.TrimSuffix(content, "\n")
			if got := b.String(); got != want {
				t.Errorf("round-trip mismatch for %s", filepath.Base(path))
			}

			// Sanity: a real code change must produce at least one hunk and at
			// least one added or removed row.
			var hunks, changes int
			for _, r := range rows {
				switch r.Kind {
				case RowHunk:
					hunks++
				case RowAdd, RowDel:
					changes++
				}
			}
			if hunks == 0 {
				t.Errorf("%s: expected at least one hunk row", filepath.Base(path))
			}
			if changes == 0 {
				t.Errorf("%s: expected at least one add/del row", filepath.Base(path))
			}
			t.Logf("%s: %d rows (%d hunks, %d changed)", filepath.Base(path), len(rows), hunks, changes)
		})
	}
}
