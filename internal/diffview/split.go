package diffview

import (
	"unicode"
	"unicode/utf8"
)

// Split lays a reading diff's rows out as two columns — the old version on the
// left, the new on the right — and computes, for every removed line paired with
// an added one, which tokens actually differ.
//
// The layout rules are the honest ones rather than the clever ones. Context rows
// carry identical text on both sides, so they occupy one line with the same row
// in both cells. A change block (a run of '-' rows followed by a run of '+' rows,
// which is how a unified diff spells a replacement) is paired positionally:
// dels[i] opposite adds[i], with filler opposite the tail of whichever run is
// longer. Everything that is not a source line — file and hunk headers, and
// meat's "..." fold rows — spans both columns, because such a row describes the
// change rather than living on one side of it.
//
// Positional pairing is deliberate. A similarity-matching pass could pair
// dels[0] with adds[2] when a block reorders lines, but it would also invent
// correspondences that are not in the diff, and this is a tool for reviewing
// generated code — a reviewer needs the display to be a faithful function of the
// input, not a guess about intent. The intra-line pass compensates without
// lying: when a positional pair turns out to be two unrelated lines, the
// similarity gate below drops the token highlighting rather than painting noise,
// and the reviewer still sees the plain red/green classification that is true.
//
// A fold row inside a change block is the one case where display order and row
// order come apart: the fold is re-emitted at full width where it interrupted the
// block, so the block's two runs still pair against each other. That keeps the
// pairing true at the cost of a full-width row whose index can be lower than an
// addition already shown above it — see SplitLine.Rows.
//
// Spans are returned twice, once on the cells (for the split view) and once
// indexed by row (for the unified view), so both views highlight the same tokens
// from one computation.
func Split(rows []Row) Layout {
	lay := Layout{
		Lines: make([]SplitLine, 0, len(rows)),
		Spans: make([][]Span, len(rows)),
	}

	var b block
	for i, r := range rows {
		switch r.Kind {
		case RowDel:
			// A removal arriving after an addition begins a second replacement,
			// which is what a unified diff means by "-a +b -c +d".
			if len(b.adds) > 0 {
				lay.flush(rows, &b)
			}
			b.dels = append(b.dels, i)
		case RowAdd:
			b.adds = append(b.adds, i)
		case RowFold:
			// A fold inside a change block must not break it. meat emits the fold
			// in place of same-polarity lines it collapsed, so the runs either
			// side of it are still one replacement — splitting there would pair a
			// removal against an addition that has nothing to do with it, and the
			// reviewer would read a correspondence that is not in the diff. The
			// fold is instead remembered and re-emitted, full width, at the point
			// in the block it interrupted.
			if len(b.dels) == 0 && len(b.adds) == 0 {
				lay.appendFull(i)
				continue
			}
			b.folds = append(b.folds, i)
			b.foldPair = append(b.foldPair, max(len(b.dels), len(b.adds)))
		case RowContext:
			lay.flush(rows, &b)
			lay.Lines = append(lay.Lines, SplitLine{
				Kind: SplitPair, Row: -1,
				Left: Cell{Row: i}, Right: Cell{Row: i},
			})
		default:
			lay.flush(rows, &b)
			lay.appendFull(i)
		}
	}
	lay.flush(rows, &b)
	return lay
}

// block accumulates one change block as Split walks the rows: the run of
// removals, the run of additions, and any fold rows that fell among them.
type block struct {
	dels, adds []int
	folds      []int
	// foldPair is parallel to folds and holds the pair line each fold belongs
	// before, fixed when the fold was seen as the number of pair lines the block
	// had produced by then. That is max of the two run lengths, because the block
	// emits one line per row of its longer run.
	foldPair []int
}

func (b *block) empty() bool {
	return len(b.dels) == 0 && len(b.adds) == 0 && len(b.folds) == 0
}

func (b *block) reset() {
	b.dels, b.adds = b.dels[:0], b.adds[:0]
	b.folds, b.foldPair = b.folds[:0], b.foldPair[:0]
}

// flush emits one split line per row of the block's longer run, pairing the two
// runs positionally and filling the shorter run's tail with blanks, with the
// block's fold rows spliced back in at full width where they occurred.
func (l *Layout) flush(rows []Row, b *block) {
	if b.empty() {
		return
	}
	n := max(len(b.dels), len(b.adds))
	for i := 0; i <= n; i++ {
		for j, at := range b.foldPair {
			if at == i {
				l.appendFull(b.folds[j])
			}
		}
		if i == n {
			break
		}

		line := SplitLine{Kind: SplitPair, Row: -1, Left: Cell{Row: -1}, Right: Cell{Row: -1}}
		if i < len(b.dels) {
			line.Left.Row = b.dels[i]
		}
		if i < len(b.adds) {
			line.Right.Row = b.adds[i]
		}
		// Only a real pair has a counterpart to differ from; filler does not.
		if line.Left.Row >= 0 && line.Right.Row >= 0 {
			del, add := intraline(rows[line.Left.Row].Text, rows[line.Right.Row].Text)
			line.Left.Spans, line.Right.Spans = del, add
			l.Spans[line.Left.Row], l.Spans[line.Right.Row] = del, add
		}
		l.Lines = append(l.Lines, line)
	}
	b.reset()
}

func (l *Layout) appendFull(row int) {
	l.Lines = append(l.Lines, SplitLine{
		Kind: SplitFull, Row: row,
		Left: Cell{Row: -1}, Right: Cell{Row: -1},
	})
}

// Layout is the two-column presentation of a row model.
type Layout struct {
	// Lines is the split view in display order. Together the lines reference
	// every input row exactly once.
	Lines []SplitLine
	// Spans is parallel to the rows Split was given: Spans[i] holds row i's
	// intra-line spans, so the unified view highlights the same tokens without
	// recomputing them. It is nil for rows that have none.
	Spans [][]Span
}

// SplitLineKind distinguishes a line that spans both columns from a paired one.
type SplitLineKind uint8

const (
	// SplitFull spans both columns: file and hunk headers, and meat's "..." fold
	// rows. Row names the row; Left and Right are unused and set to -1.
	SplitFull SplitLineKind = iota
	// SplitPair is a source line with an old-side and a new-side cell. Either
	// cell may be filler, but never both.
	SplitPair
)

// SplitLine is one display line of the two-column view.
type SplitLine struct {
	Kind SplitLineKind
	// Row is the row this line spans for SplitFull, and -1 for SplitPair.
	Row   int
	Left  Cell
	Right Cell
}

// Rows returns the row indices this line occupies, which is one for SplitFull,
// and one or two for SplitPair.
//
// Note that the highest row a line covers does not increase monotonically from
// one line to the next: a change block of three removals against one addition
// lays out as (del0,add0), (del1,-), (del2,-), whose maxima run 3, 1, 2. A caller
// placing content between lines must therefore index rows to lines — see
// Layout.LineOfRow — rather than carrying a high-water mark forward.
func (l SplitLine) Rows() []int {
	if l.Kind == SplitFull {
		return []int{l.Row}
	}
	var out []int
	if l.Left.Row >= 0 {
		out = append(out, l.Left.Row)
	}
	if l.Right.Row >= 0 && l.Right.Row != l.Left.Row {
		out = append(out, l.Right.Row)
	}
	return out
}

// LineOfRow maps each row to the index of the split line that shows it, which is
// how a caller splices something anchored to a row — an elision marker — into the
// two-column layout. Rows are covered exactly once, so the map is total.
//
// A marker anchored to a row in the middle of a change block resolves to the
// line that block put the row on, which means the marker lands inside the block
// rather than between its columns. That is the intended reading: the two columns
// of a pair are the same moment in the change and there is no "between" for a
// full-width row to occupy, so the marker attaches to the block it interrupts.
func (l Layout) LineOfRow() []int {
	of := make([]int, len(l.Spans))
	for i := range of {
		of[i] = -1
	}
	for li, line := range l.Lines {
		for _, r := range line.Rows() {
			of[r] = li
		}
	}
	return of
}

// Cell is one side of a split line.
type Cell struct {
	// Row is the index into the row model of the line shown in this column, or
	// -1 for filler: the blank that a shorter run of a change block leaves
	// opposite a longer one.
	Row int
	// Spans are the byte ranges of the row's text that differ from the text of
	// the cell opposite, ascending and non-overlapping. It is empty for filler
	// and context, and empty for a pair the similarity gate judged unrelated.
	Spans []Span
}

// Span is a byte range within a Row's Text that differs from the row it is
// paired with. Offsets are into Text exactly as diffview emits it, leading
// marker byte included — so Start is never 0 on a hunk source line, and a caller
// that reslices the text must shift the offsets to match.
type Span struct {
	Start int
	End   int
}

// minSimilarityPct is how much of a paired line must survive unchanged before
// intra-line highlighting is worth drawing. Below it the two lines are treated
// as unrelated and get no spans at all.
//
// The threshold exists because token highlighting is only informative when it is
// selective: a pair that shares almost nothing produces spans covering nearly
// every token, which tells the reviewer less than the plain red/green already
// did while costing more attention to read. Whitespace does not count toward
// the total, or two lines sharing nothing but their indentation would qualify.
const minSimilarityPct = 30

// maxTokenProduct caps the LCS table. Above it the middles are reported as one
// span each rather than diffed, which keeps a minified or generated line — the
// only realistic way to exceed 256 differing tokens per side — from costing
// quadratic time. The fallback is honest: it says the whole middle changed,
// which is nearly true at that scale.
const maxTokenProduct = 1 << 16

// intraline computes which byte ranges of a removed line and its paired added
// line differ, at token granularity, so ui can brighten exactly the tokens that
// changed and leave the rest in the row's own color.
//
// The two texts arrive with their diff marker bytes attached, and those bytes
// always differ ('-' versus '+') without meaning anything, so the comparison
// runs on the content after the marker and the returned offsets are shifted back
// past it. Common leading and trailing tokens are trimmed before the LCS, which
// makes the common case — a small edit inside a long line — cheap, and also
// improves the result by anchoring the diff at the edges the way a reader does.
func intraline(del, add string) (delSpans, addSpans []Span) {
	if len(del) < 2 || len(add) < 2 {
		return nil, nil
	}
	delText, addText := del[1:], add[1:]

	a, b := tokenize(delText), tokenize(addText)

	lo := 0
	for lo < len(a) && lo < len(b) && a[lo].text == b[lo].text {
		lo++
	}
	hiA, hiB := len(a), len(b)
	for hiA > lo && hiB > lo && a[hiA-1].text == b[hiB-1].text {
		hiA--
		hiB--
	}

	aChanged := make([]bool, len(a))
	bChanged := make([]bool, len(b))
	switch midA, midB := hiA-lo, hiB-lo; {
	case midA == 0 && midB == 0:
		// Identical content under different markers. meat should not emit this,
		// but nothing here needs to assume that.
		return nil, nil
	case midA == 0 || midB == 0:
		// One side's middle is empty, so the other's is a pure insertion or
		// deletion and there is nothing to align inside it.
		markRange(aChanged, lo, hiA)
		markRange(bChanged, lo, hiB)
	case midA*midB > maxTokenProduct:
		markRange(aChanged, lo, hiA)
		markRange(bChanged, lo, hiB)
	default:
		ac, bc := lcsChanged(a[lo:hiA], b[lo:hiB])
		copy(aChanged[lo:hiA], ac)
		copy(bChanged[lo:hiB], bc)
	}

	if !similarEnough(a, b, aChanged) {
		return nil, nil
	}
	// The marker byte is one byte wide on every line diffview classifies as
	// source, so shifting by one is exact rather than an approximation.
	return spansFrom(a, aChanged, 1), spansFrom(b, bChanged, 1)
}

// similarEnough applies minSimilarityPct. The unchanged weight is measured on
// one side only: LCS-matched tokens are equal by definition, so the surviving
// byte count is the same on both.
//
// Whitespace is excluded from both sides of the ratio, not just the numerator.
// Discounting it in the numerator alone would make a line's own indentation
// count against its similarity, which suppressed highlighting on exactly the
// case that needs it most — a line whose only change is how far it is indented.
func similarEnough(a, b []token, aChanged []bool) bool {
	same := 0
	for i, t := range a {
		if !aChanged[i] && !t.space {
			same += len(t.text)
		}
	}
	return same*100 >= minSimilarityPct*max(nonSpaceLen(a), nonSpaceLen(b))
}

func nonSpaceLen(toks []token) int {
	n := 0
	for _, t := range toks {
		if !t.space {
			n += len(t.text)
		}
	}
	return n
}

func markRange(changed []bool, lo, hi int) {
	for i := lo; i < hi; i++ {
		changed[i] = true
	}
}

// lcsChanged marks the tokens on each side that are not part of a longest common
// subsequence of the two. A full table is built because the walk back out of it
// is what produces the marks; at maxTokenProduct that is 256 KB, and the cap is
// there so it cannot grow past that.
func lcsChanged(a, b []token) (aChanged, bChanged []bool) {
	n, m := len(a), len(b)
	// tab[i*(m+1)+j] is the LCS length of a[i:] against b[j:].
	tab := make([]int, (n+1)*(m+1))
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].text == b[j].text {
				tab[i*(m+1)+j] = tab[(i+1)*(m+1)+j+1] + 1
				continue
			}
			tab[i*(m+1)+j] = max(tab[(i+1)*(m+1)+j], tab[i*(m+1)+j+1])
		}
	}

	aChanged, bChanged = make([]bool, n), make([]bool, m)
	i, j := 0, 0
	for i < n && j < m {
		if a[i].text == b[j].text {
			i++
			j++
			continue
		}
		// Tie broken toward consuming the old side first, so a replacement reads
		// as "this became that" rather than the reverse.
		if tab[(i+1)*(m+1)+j] >= tab[i*(m+1)+j+1] {
			aChanged[i] = true
			i++
			continue
		}
		bChanged[j] = true
		j++
	}
	markRange(aChanged, i, n)
	markRange(bChanged, j, m)
	return aChanged, bChanged
}

// spansFrom merges runs of adjacent changed tokens into one span each, so a
// changed identifier followed by a changed operator reads as a single
// highlighted region instead of two abutting ones.
func spansFrom(toks []token, changed []bool, offset int) []Span {
	var out []Span
	for i := 0; i < len(toks); {
		if !changed[i] {
			i++
			continue
		}
		j := i
		for j < len(toks) && changed[j] {
			j++
		}
		out = append(out, Span{Start: toks[i].start + offset, End: toks[j-1].end + offset})
		i = j
	}
	return out
}

// token is one lexical unit of a source line, carrying its byte range so a span
// can be mapped back onto the line.
type token struct {
	start, end int
	text       string
	// space marks a whitespace run, which the similarity gate discounts.
	space bool
}

// tokenize splits a line the way a reader sees it, into three classes of run:
// identifiers and numbers, whitespace, and operator punctuation.
//
// Each grouping earns its place. Holding an identifier together is what makes a
// renamed variable one highlight instead of a scatter of letters. Holding a
// whitespace run together makes a reindentation read as a single change. And
// holding an operator run together is what makes "==" become "!=" highlight as a
// whole operator: with punctuation tokenized one character at a time, the common
// subsequence legitimately matches the trailing "=" of both, and the reviewer is
// shown a highlight on "=" alone — accurate about tokens, and misleading about
// what changed.
//
// It is deliberately language-agnostic. A real lexer per language would group
// string literals and comments better, but it would need to know which language
// it is looking at, and being wrong about that is worse than being uniformly
// simple.
func tokenize(s string) []token {
	toks := make([]token, 0, len(s)/4+1)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		start := i
		space := isSpaceRune(r)
		i += size
		switch {
		case isWordRune(r):
			i += runLen(s[i:], isWordRune)
		case space:
			i += runLen(s[i:], isSpaceRune)
		default:
			i += runLen(s[i:], isOperatorRune)
		}
		toks = append(toks, token{start: start, end: i, text: s[start:i], space: space})
	}
	return toks
}

// runLen is the byte length of the leading run of s whose runes satisfy pred.
func runLen(s string, pred func(rune) bool) int {
	n := 0
	for n < len(s) {
		r, size := utf8.DecodeRuneInString(s[n:])
		if !pred(r) {
			break
		}
		n += size
	}
	return n
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t'
}

// isOperatorRune is the complement of the other two classes, so the three
// partition every rune and tokenize can never stall on one.
func isOperatorRune(r rune) bool {
	return !isWordRune(r) && !isSpaceRune(r)
}
