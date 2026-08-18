package diffview

import (
	"regexp"
	"sort"
	"strconv"
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

	a := Alignment{Rows: rows, Raw: make([]string, len(rawRows)), Nums: make([]LineNo, len(rows))}
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

	// Line numbers come from the raw side, where they are exact, and are carried
	// across to the rows that matched. Deriving them from the reading diff's own
	// @@ headers instead would drift by the size of every elision, which is
	// precisely the content whose absence the reader is being asked to trust.
	rawNums := rawLineNumbers(rawRows)
	for _, m := range matches {
		a.Nums[m.row] = rawNums[m.raw]
	}

	// The gaps between consecutive matches are the elisions. prevRow/prevRaw
	// start at -1 so a diff whose opening lines were dropped yields a leading
	// elision, and the tail is closed after the loop.
	files := rawFileNames(rawRows)
	a.Digests = fileDigests(rawRows, files)
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

// FileOf returns the path a metadata row names, for the two row kinds that name
// one: "diff --git a/p b/p" and "+++ b/p". It reports false for every other line,
// including a deletion's "+++ /dev/null", which names no file.
//
// It exists so a caller labelling reading-diff rows by file resolves paths the
// same way the raw-side alignment does, rather than reimplementing the two
// header shapes and drifting from it.
func FileOf(line string) (string, bool) {
	if p, ok := pathFromNewFileMarker(line); ok {
		return p, true
	}
	return pathFromGitHeader(line)
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
	// Nums is parallel to Rows and gives each row's true position in the old and
	// new versions of its file. It is zero — meaning "no number to show" — for
	// structural rows, for fold rows, for either side of a row that exists on
	// only one side, and for every row when no original was supplied.
	Nums []LineNo
	// Digests gives each file in the original diff a content identity: a hash of
	// that file's own section of it. It is what a persisted "viewed" marker is
	// keyed to, so a marker survives the rest of the change moving underneath it.
	// Nil when no original was supplied, in which case markers cannot be
	// persisted at all — see fileDigests.
	Digests map[string]string
}

// LineNo is a row's position in the pre-image and post-image of the change.
//
// The numbers are read off the raw diff rather than the reading diff, because
// the reading diff cannot support them: meat elides lines without renumbering
// the @@ headers it leaves behind, so counting forward from a header inside a
// reading diff drifts by the size of every gap it has passed. A wrong line
// number in a tool whose purpose is justified trust is worse than no line
// number, so a row porkchop cannot place exactly is left at zero and rendered
// blank.
type LineNo struct {
	// Old is the 1-based line in the old version, or 0 if the row has none —
	// which is the case for an added line.
	Old int
	// New is the 1-based line in the new version, or 0 for a removed line.
	New int
}

// rawLineNumbers assigns every line of a raw diff its old and new line numbers,
// counting forward from each hunk header. This is only sound on a raw diff,
// where nothing has been removed; see LineNo.
func rawLineNumbers(rawRows []Row) []LineNo {
	nums := make([]LineNo, len(rawRows))
	oldNo, newNo := 0, 0
	for i, r := range rawRows {
		switch r.Kind {
		case RowHunk:
			oldNo, newNo = hunkStarts(r.Text)
		case RowContext:
			nums[i] = LineNo{Old: oldNo, New: newNo}
			oldNo++
			newNo++
		case RowDel:
			nums[i] = LineNo{Old: oldNo}
			oldNo++
		case RowAdd:
			nums[i] = LineNo{New: newNo}
			newNo++
		case RowFold:
			// On the raw side a "..." line is ordinary source — a Python Ellipsis
			// or stub body — not a marker, so it occupies a real line on whichever
			// sides its polarity implies. This mirrors isChangedLine.
			switch {
			case r.Text == "":
			case r.Text[0] == '+':
				nums[i] = LineNo{New: newNo}
				newNo++
			case r.Text[0] == '-':
				nums[i] = LineNo{Old: oldNo}
				oldNo++
			default:
				nums[i] = LineNo{Old: oldNo, New: newNo}
				oldNo++
				newNo++
			}
		}
	}
	return nums
}

// hunkStarts reads the two starting line numbers out of an "@@ -a,b +c,d @@"
// header. Only the starts are taken: meat documents that the counts go stale,
// and nothing here needs them. A header that does not parse yields zeros, which
// render blank rather than wrong.
func hunkStarts(line string) (old, new int) {
	rest, ok := strings.CutPrefix(line, "@@ ")
	if !ok {
		return 0, 0
	}
	old, rest, ok = cutLineNumber(rest, '-')
	if !ok {
		return 0, 0
	}
	new, _, ok = cutLineNumber(rest, '+')
	if !ok {
		return old, 0
	}
	return old, new
}

// cutLineNumber reads a "<sign><number>" field, tolerating the ",count" suffix
// being present or absent — "@@ -1 +1 @@" is as valid as "@@ -1,4 +1,6 @@".
func cutLineNumber(s string, sign byte) (n int, rest string, ok bool) {
	i := strings.IndexByte(s, sign)
	if i < 0 {
		return 0, s, false
	}
	s = s[i+1:]
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, s, false
	}
	return n, s[end:], true
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
	// Blank counts how many of those Changed lines have no content at all — an
	// added or removed empty line. When Blank equals Changed the elision hides
	// nothing a reviewer can read, which is worth saying in the marker: about one
	// marker in five across meat/testdata is this case, and a marker that costs a
	// row of screen to reveal whitespace teaches a reviewer to distrust the expand
	// affordance. It changes wording only; Changed, and every count derived from
	// it, is untouched.
	Blank int
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
		if !isChangedLine(rawRows[i]) {
			continue
		}
		e.Changed++
		if isBlankChange(rawRows[i]) {
			e.Blank++
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

// isBlankChange reports whether a changed raw row adds or removes a line with no
// content — nothing but its +/- marker, or that marker and whitespace.
//
// Whitespace-only counts as blank because the judgement being made is "is there
// anything here for a reviewer to read", and for a whole hidden line there is not.
// This affects how a marker is worded and nothing else; the line is still counted
// as changed everywhere it matters.
func isBlankChange(r Row) bool {
	return len(r.Text) > 0 && strings.TrimSpace(r.Text[1:]) == ""
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
