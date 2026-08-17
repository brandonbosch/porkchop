package diffview

import (
	"strings"
	"testing"
)

// TestNumsExactOnAKnownDiff pins the numbering against a diff small enough to
// count by hand, including the case that motivates taking numbers from the raw
// side at all: the reading diff drops two lines from the middle of the hunk, so
// counting forward from its own @@ header would put every row after the gap two
// lines too early.
func TestNumsExactOnAKnownDiff(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/f.py b/f.py",
		"--- a/f.py",
		"+++ b/f.py",
		"@@ -10,7 +10,7 @@ def f():",
		" keep one",
		"-drop me",
		"-drop me too",
		"-old line",
		"+new line",
		" keep two",
		" keep three",
	}, "\n")
	// The reading diff meat might produce from it: the two "drop me" removals are
	// gone, and the @@ header is left untouched and therefore stale.
	reading := strings.Join([]string{
		"diff --git a/f.py b/f.py",
		"--- a/f.py",
		"+++ b/f.py",
		"@@ -10,7 +10,7 @@ def f():",
		" keep one",
		"-old line",
		"+new line",
		" keep two",
		" keep three",
	}, "\n")

	a := Align(raw, reading)
	if len(a.Nums) != len(a.Rows) {
		t.Fatalf("Nums has %d entries for %d rows", len(a.Nums), len(a.Rows))
	}

	want := map[string]LineNo{
		" keep one":   {Old: 10, New: 10},
		"-old line":   {Old: 13},
		"+new line":   {New: 11},
		" keep two":   {Old: 14, New: 12},
		" keep three": {Old: 15, New: 13},
	}
	for i, r := range a.Rows {
		w, ok := want[r.Text]
		if !ok {
			// Structural rows carry no numbers.
			if a.Nums[i] != (LineNo{}) {
				t.Errorf("row %d %q got %+v, want no number", i, r.Text, a.Nums[i])
			}
			continue
		}
		if a.Nums[i] != w {
			t.Errorf("row %d %q got %+v, want %+v", i, r.Text, a.Nums[i], w)
		}
	}
}

// TestNumsMatchRowPolarity checks the numbers agree with what each row is: an
// addition exists only in the new file, a removal only in the old, and context in
// both. A row numbered on the wrong side would put it in the wrong column's
// gutter, which is the kind of quiet wrongness this tool cannot afford.
func TestNumsMatchRowPolarity(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])
			numbered := 0
			for i, r := range a.Rows {
				n := a.Nums[i]
				if n.Old == 0 && n.New == 0 {
					continue // unmatched or structural: rendered blank
				}
				numbered++
				switch r.Kind {
				case RowAdd:
					if n.Old != 0 || n.New == 0 {
						t.Fatalf("row %d %q is an addition but numbered %+v", i, r.Text, n)
					}
				case RowDel:
					if n.New != 0 || n.Old == 0 {
						t.Fatalf("row %d %q is a removal but numbered %+v", i, r.Text, n)
					}
				case RowContext:
					if n.Old == 0 || n.New == 0 {
						t.Fatalf("row %d %q is context but numbered %+v", i, r.Text, n)
					}
				default:
					t.Fatalf("row %d of kind %v carries a number %+v", i, r.Kind, n)
				}
			}
			if numbered == 0 {
				t.Fatal("no rows got line numbers")
			}
			t.Logf("numbered %d of %d rows", numbered, len(a.Rows))
		})
	}
}

// TestNumsAdvanceWithinEachFile checks each side's numbering only ever moves
// forward inside a file, and never starts before the hunk header says the hunk
// does. Elisions make the numbers skip, which is the point; they must not repeat
// or reverse.
func TestNumsAdvanceWithinEachFile(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align(pair[0], pair[1])
			lastOld, lastNew := 0, 0
			hunkOld, hunkNew := 0, 0
			for i, r := range a.Rows {
				switch {
				case r.Kind == RowMeta && strings.HasPrefix(r.Text, "diff --git "):
					lastOld, lastNew, hunkOld, hunkNew = 0, 0, 0, 0
					continue
				case r.Kind == RowHunk:
					hunkOld, hunkNew = hunkStarts(r.Text)
					continue
				}
				n := a.Nums[i]
				if n.Old != 0 {
					if n.Old <= lastOld {
						t.Fatalf("row %d %q: old line %d did not advance past %d", i, r.Text, n.Old, lastOld)
					}
					if n.Old < hunkOld {
						t.Fatalf("row %d %q: old line %d precedes its hunk start %d", i, r.Text, n.Old, hunkOld)
					}
					lastOld = n.Old
				}
				if n.New != 0 {
					if n.New <= lastNew {
						t.Fatalf("row %d %q: new line %d did not advance past %d", i, r.Text, n.New, lastNew)
					}
					if n.New < hunkNew {
						t.Fatalf("row %d %q: new line %d precedes its hunk start %d", i, r.Text, n.New, hunkNew)
					}
					lastNew = n.New
				}
			}
		})
	}
}

// TestNumsAbsentWithoutAnOriginal checks the degraded path: with no raw diff there
// is nothing to take exact numbers from, and porkchop reports none rather than
// counting forward from headers it knows to be stale.
func TestNumsAbsentWithoutAnOriginal(t *testing.T) {
	for name, pair := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			a := Align("", pair[1])
			if len(a.Nums) != len(a.Rows) {
				t.Fatalf("Nums has %d entries for %d rows", len(a.Nums), len(a.Rows))
			}
			for i, n := range a.Nums {
				if n != (LineNo{}) {
					t.Fatalf("row %d got %+v with no original supplied", i, n)
				}
			}
		})
	}
}

func TestHunkStarts(t *testing.T) {
	tests := []struct {
		line     string
		old, new int
	}{
		{"@@ -1,4 +1,6 @@", 1, 1},
		{"@@ -10,7 +12,9 @@ def f():", 10, 12},
		{"@@ -1 +1 @@", 1, 1},
		{"@@ -0,0 +1,5 @@", 0, 1},
		{"@@ -12,3 +14 @@ trailing", 12, 14},
		// Malformed headers yield zeros, which render blank rather than wrong.
		{"@@ nonsense @@", 0, 0},
		{"@@", 0, 0},
		{"not a hunk header", 0, 0},
		{"@@ -x,y +z,w @@", 0, 0},
	}
	for _, tc := range tests {
		old, new := hunkStarts(tc.line)
		if old != tc.old || new != tc.new {
			t.Errorf("hunkStarts(%q) = (%d,%d), want (%d,%d)", tc.line, old, new, tc.old, tc.new)
		}
	}
}
