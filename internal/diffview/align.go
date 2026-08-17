package diffview

import (
	"regexp"
	"sort"
	"strings"
)

// Align maps a reading diff back onto the raw diff it was abridged from, so a
// reviewer can see — and expand — exactly what meat left out.
//
// This is approach (a) from PLAN.md: meat discards the model's edit plan after
// applying it, so rather than change the core we recompute the mapping. The walk
// is a port of meat's own retainedDiffStats (meat/elision.go), which exists for
// the same reason: a reading diff's @@ counts are deliberately stale once lines
// are removed, so the only trustworthy way to locate kept content in the
// original is to align the two texts directly.
//
// The algorithm is a greedy forward match. Every non-fold reading row is located
// at or after the last match's position, an exact (kind, text) index handling the
// common case in one lookup and a compiled projection regex handling rows meat
// partially elided ("foo(...)"). The interesting output is the complement: each
// run of raw lines that no reading row claimed is an Elision — a hole in the
// reading diff with a known, exact preimage in the original.
//
// Why the complement rather than meat's "..." fold rows: meat marks only a small
// fraction of what it removes. Measured across meat/testdata, the goldens carry
// 1-2 fold rows but 14-27 distinct gaps that hide changed lines (62-90 changed
// lines each). Keying fold expansion to fold rows alone would therefore expose
// roughly a twentieth of the hidden content, which is not a basis for trust. So
// Elisions are derived from the alignment, and a fold row, when meat did emit
// one, is recorded as an attribute of the gap it marks (FoldRow) rather than as
// the thing that defines it.
//
// Align is total: it never errors and never panics, on any input. A reading row
// that cannot be located is skipped, which widens a neighbouring gap rather than
// corrupting the mapping — an abridgement that diverges from its original
// degrades into "more is hidden", the safe direction for a trust feature.
func Align(raw, readingDiff string) Alignment {
	rows := Parse(readingDiff)
	rawRows := Parse(raw)

	a := Alignment{Rows: rows, Raw: make([]string, len(rawRows))}
	for i, r := range rawRows {
		a.Raw[i] = r.Text
	}
	if len(rawRows) == 0 {
		return a
	}

	// Index the raw side by (kind, text). Most reading rows are verbatim copies
	// of a raw row, and indexing keeps a divergent abridgement from turning the
	// walk into a full forward scan per row.
	exact := make(map[rowKey][]int, len(rawRows))
	for i, r := range rawRows {
		k := rowKey{kind: r.Kind, text: r.Text}
		exact[k] = append(exact[k], i)
	}

	matches := make([]rowMatch, 0, len(rows))
	rawPos := 0
	for ri, r := range rows {
		// Fold rows stand for content that is by definition absent from the
		// reading diff, so they match nothing and must not advance rawPos: the
		// gap they mark is discovered as the hole they leave behind.
		if r.Kind == RowFold {
			continue
		}

		at := nextAtOrAfter(exact[rowKey{kind: r.Kind, text: r.Text}], rawPos)

		// A partially elided row ("self.foo(...)") has no exact preimage. Its
		// projection may also match an earlier raw row than a later identical
		// literal would, so search only up to the exact match when one exists —
		// preserving meat's greedy semantics.
		if isHunkSource(r.Kind) && len(r.Text) > 1 && containsPlaceholder(r.Text[1:]) {
			if re, ok := compileProjection(r.Text[1:]); ok {
				end := len(rawRows)
				if at >= 0 {
					end = at
				}
				for i := rawPos; i < end; i++ {
					if rawRows[i].Kind != r.Kind || rawRows[i].Text == "" || rawRows[i].Text[0] != r.Text[0] {
						continue
					}
					if re.MatchString(rawRows[i].Text[1:]) {
						at = i
						break
					}
				}
			}
		}

		if at < 0 {
			continue
		}
		matches = append(matches, rowMatch{row: ri, raw: at})
		rawPos = at + 1
	}

	// The gaps between consecutive matches are the elisions. prevRow/prevRaw
	// start at -1 so a diff whose opening lines were dropped yields a leading
	// elision, and the tail is closed after the loop.
	files := rawFileNames(rawRows)
	prevRow, prevRaw := -1, -1
	for _, m := range matches {
		if m.raw-prevRaw > 1 {
			a.Elisions = append(a.Elisions, a.newElision(rawRows, files, prevRaw+1, m.raw, prevRow, m.row))
		}
		prevRow, prevRaw = m.row, m.raw
	}
	if len(rawRows)-prevRaw > 1 {
		a.Elisions = append(a.Elisions, a.newElision(rawRows, files, prevRaw+1, len(rawRows), prevRow, len(rows)))
	}
	return a
}

// rawFileNames labels every raw line with the file it belongs to, by carrying
// the most recent file header forward. The "+++ b/path" marker is preferred over
// "diff --git a/path b/path" because it names exactly one path and so needs no
// guessing about where the first path ends and the second begins.
func rawFileNames(rawRows []Row) []string {
	files := make([]string, len(rawRows))
	cur := ""
	for i, r := range rawRows {
		if r.Kind == RowMeta {
			if p, ok := pathFromGitHeader(r.Text); ok {
				cur = p
			}
			if p, ok := pathFromNewFileMarker(r.Text); ok {
				cur = p
			}
		}
		files[i] = cur
	}
	return files
}

func pathFromGitHeader(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "diff --git ")
	if !ok {
		return "", false
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+len(" b/"):], true
	}
	return strings.TrimSpace(rest), true
}

func pathFromNewFileMarker(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "+++ ")
	if !ok {
		return "", false
	}
	rest, _, _ = strings.Cut(rest, "\t")
	// A deletion's "+++ /dev/null" names no file; keep the "diff --git" path.
	if rest == "/dev/null" || rest == "" {
		return "", false
	}
	return strings.TrimPrefix(rest, "b/"), true
}

// Alignment is a reading diff paired with its original, plus the mapping that
// says which original lines the reading diff dropped.
type Alignment struct {
	// Rows is the reading diff's row model, identical to Parse(readingDiff).
	Rows []Row
	// Raw is the original diff split into lines, indexed by every Elision.
	Raw []string
	// Elisions are the runs of Raw the reading diff omits, in reading order and
	// non-overlapping. Together with the matched rows they partition Raw.
	Elisions []Elision
}

// Elision is one contiguous run of original diff lines that the reading diff
// omits. RawStart:RawEnd is a half-open range into Alignment.Raw.
type Elision struct {
	RawStart int
	RawEnd   int
	// BeforeRow is the index in Rows of the row that follows this elision, i.e.
	// where a marker for it belongs. It is len(Rows) for a trailing elision.
	BeforeRow int
	// FoldRow is the index in Rows of the "..." fold row meat emitted for this
	// elision, or -1 when meat dropped the content silently — which is the
	// common case. ui expands in place at FoldRow when set, and synthesizes a
	// marker before BeforeRow when not.
	FoldRow int
	// Changed counts the hidden lines that are real additions or removals, as
	// opposed to context meat merely trimmed. It is what makes an elision worth
	// a reviewer's attention, and what the marker reports.
	Changed int
	// File is the path of the file this elision falls in, taken from the raw
	// diff's own headers, so the audit view can group the discard pile by file.
	// It is empty when the elision precedes any file header.
	File string
}

// Len is the number of original lines the elision hides, context included.
func (e Elision) Len() int { return e.RawEnd - e.RawStart }

// Hidden returns the original diff lines this elision stands for.
func (a Alignment) Hidden(e Elision) []string { return a.Raw[e.RawStart:e.RawEnd] }

// ElisionBefore returns the elision positioned immediately before Rows[row],
// which is how ui finds the marker to draw while walking rows in order.
func (a Alignment) ElisionBefore(row int) (Elision, bool) {
	for _, e := range a.Elisions {
		if e.BeforeRow == row {
			return e, true
		}
	}
	return Elision{}, false
}

// ElisionAtFold returns the elision meat marked with the fold row at index row.
func (a Alignment) ElisionAtFold(row int) (Elision, bool) {
	for _, e := range a.Elisions {
		if e.FoldRow == row {
			return e, true
		}
	}
	return Elision{}, false
}

// ChangedHidden is the total number of added or removed original lines that the
// reading diff does not show — the headline trust number.
func (a Alignment) ChangedHidden() int {
	n := 0
	for _, e := range a.Elisions {
		n += e.Changed
	}
	return n
}

type rowKey struct {
	kind RowKind
	text string
}

type rowMatch struct{ row, raw int }

// newElision records a gap, counting what it hides and attributing it to a fold
// row when meat emitted one anywhere between the rows that bracket the gap.
func (a *Alignment) newElision(rawRows []Row, files []string, lo, hi, prevRow, beforeRow int) Elision {
	e := Elision{RawStart: lo, RawEnd: hi, BeforeRow: beforeRow, FoldRow: -1}
	if lo < len(files) {
		e.File = files[lo]
	}
	for i := lo; i < hi; i++ {
		if isChangedLine(rawRows[i]) {
			e.Changed++
		}
	}
	for r := prevRow + 1; r < beforeRow && r < len(a.Rows); r++ {
		if a.Rows[r].Kind == RowFold {
			e.FoldRow = r
			break
		}
	}
	return e
}

// isChangedLine reports whether a raw row is a real addition or removal.
//
// The RowFold case is a genuine ambiguity inherited from meat: a line whose
// content is exactly "..." is ordinary source in several languages (a Python
// Ellipsis or stub body, for one), so on the raw side it is not a fold at all.
// meat's own counters treat such a line as a changed line when it carries +/-
// polarity, and matching that keeps porkchop's numbers equal to ElisionLine's.
func isChangedLine(r Row) bool {
	switch r.Kind {
	case RowAdd, RowDel:
		return true
	case RowFold:
		return r.Text != "" && (r.Text[0] == '+' || r.Text[0] == '-')
	}
	return false
}

// isHunkSource reports whether a kind is a source line inside a hunk body, the
// only kind of row a projection match applies to.
func isHunkSource(k RowKind) bool {
	return k == RowAdd || k == RowDel || k == RowContext
}

func nextAtOrAfter(positions []int, min int) int {
	i := sort.SearchInts(positions, min)
	if i == len(positions) {
		return -1
	}
	return positions[i]
}

func containsPlaceholder(text string) bool {
	return strings.Contains(text, "...") || strings.ContainsRune(text, '…')
}

// compileProjection builds a matcher for a partially elided line: a port of
// meat's compileElisionProjection in its non-strict mode. Each visible ellipsis
// ("…" or a run of three or more dots) becomes ".+" — at least one character, so
// an ellipsis can never stand for nothing and quietly validate a changed line
// such as "!allowed" against "allowed". Everything else is literal. A line with
// no ellipsis has no projection and reports false.
func compileProjection(content string) (*regexp.Regexp, bool) {
	runes := []rune(content)
	var pattern, literal strings.Builder
	pattern.WriteByte('^')
	wildcards := 0
	lastWildcard := false
	flush := func() {
		if literal.Len() == 0 {
			return
		}
		pattern.WriteString(regexp.QuoteMeta(literal.String()))
		literal.Reset()
	}

	for i := 0; i < len(runes); {
		wildcard := false
		switch runes[i] {
		case '…':
			wildcard = true
			i++
		case '.':
			j := i
			for j < len(runes) && runes[j] == '.' {
				j++
			}
			if j-i >= 3 {
				wildcard = true
				i = j
			}
		}
		if wildcard {
			// Collapse adjacent ellipses; ".+.+" would only be a slower ".+".
			if lastWildcard {
				continue
			}
			flush()
			pattern.WriteString(".+")
			wildcards++
			lastWildcard = true
			continue
		}
		literal.WriteRune(runes[i])
		lastWildcard = false
		i++
	}
	flush()
	pattern.WriteByte('$')

	if wildcards == 0 {
		return nil, false
	}
	re, err := regexp.Compile(pattern.String())
	return re, err == nil
}
