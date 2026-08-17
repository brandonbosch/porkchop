package ui

import (
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/brandonbosch/porkchop/internal/diffview"
)

// The split view's column separator, and the two furniture widths derived from
// it. columnGap is the cells " │ " occupies; gutterGap is the single space
// between a line number and the content it numbers.
const (
	columnSep = "│"
	columnGap = 3
	gutterGap = 1
)

// sideOld and sideNew index the two content areas of a body line. A full-width
// line has only sideOld.
const (
	sideOld = 0
	sideNew = 1
)

// gutterCells is the width the line-number gutter occupies in the current view,
// including its trailing space, or 0 when there are no numbers to show.
//
// The unified view carries both numbers, old then new, because a single column of
// content is being read as two versions at once and either number may be the one
// the reviewer wants. Each split column carries only its own.
func (m Model) gutterCells() int {
	return m.gutterCellsFor(m.split)
}

// gutterCellsFor takes the view explicitly, because the width available to a
// split column has to be computable while deciding whether to use the split view
// at all — at which point m.split is still the answer being worked out.
func (m Model) gutterCellsFor(split bool) int {
	if m.numDigits == 0 {
		return 0
	}
	if split {
		return m.numDigits + gutterGap
	}
	return 2*m.numDigits + 2*gutterGap
}

// colWidth is the content width of one split column. It never returns less than
// one, so that a reviewer who forces the split view onto a terminal too narrow for
// it gets a cramped screen rather than a broken one.
func (m Model) colWidth() int {
	return max((m.width-columnGap-2*m.gutterCellsFor(true))/2, 1)
}

// splitViable reports whether the terminal is wide enough for two columns to be
// worth reading. Below this porkchop shows the unified view instead, which is why
// a narrow session is a degraded experience rather than a broken one.
func (m Model) splitViable() bool {
	return m.width >= minSplitWidth && m.colWidth() >= minColWidth
}

// displayCell prepares one content area of a body line for the screen: tabs
// expanded, clipped to the width available to it, with the intra-line spans moved
// to match.
//
// Both the renderer and the search pass go through this one function. If they
// prepared text separately the two could disagree by a byte and search
// highlighting would land off its match, so agreeing by construction is the
// point of routing them together.
func (m Model) displayCell(bl bodyLine, side int) (string, []diffview.Span) {
	if bl.kind == bodyPair {
		c := bl.left
		if side == sideNew {
			c = bl.right
		}
		if c.Row < 0 {
			return "", nil
		}
		return prepare(m.rows[c.Row].Text, m.colWidth(), c.Spans)
	}
	if side != sideOld {
		return "", nil
	}

	avail := m.width - m.prefixCells(bl)
	switch bl.kind {
	case bodyMarker, bodyHidden:
		return prepare(bl.text, avail, nil)
	default:
		return prepare(bl.text, avail, m.spansOf(bl.row))
	}
}

// prefixCells is how many cells sit to the left of a full-width line's content.
//
// Every full-width line reserves the gutter's width whether or not it has numbers
// to put there: a row draws its own gutter, blank when it has none, and a marker
// or a revealed line is indented by the same amount instead. Reserving it
// uniformly is what keeps one left edge down the screen — the alternative, letting
// unnumbered lines start further left, produces a ragged margin that reads as a
// rendering fault rather than as a distinction.
func (m Model) prefixCells(bl bodyLine) int {
	if bl.kind == bodyHidden {
		// One cell more for the "│" rail that sets revealed content apart.
		return m.gutterCells() + 1
	}
	return m.gutterCells()
}

// spansOf is row's intra-line spans, or nil when it has none.
func (m Model) spansOf(row int) []diffview.Span {
	if row < 0 || row >= len(m.lay.Spans) {
		return nil
	}
	return m.lay.Spans[row]
}

// renderBodyLine styles one line of the review view.
func (m Model) renderBodyLine(i int, bl bodyLine) string {
	if bl.kind == bodyPair {
		return m.renderPair(i, bl)
	}

	text, emph := m.displayCell(bl, sideOld)

	switch bl.kind {
	case bodyMarker:
		// The current marker carries a caret; others get matching blanks so the
		// marker text stays in one column as the cursor moves.
		prefix, style := "  ", m.st.marker
		if bl.mark == m.cur {
			prefix, style = "❯ ", m.st.markerCur
		}
		return strings.Repeat(" ", m.gutterCells()) +
			m.paint(text, style, style, nil, m.hitsAt(i, sideOld), prefix)

	case bodyHidden:
		// Expanded original content is set apart by a rail and dimmed: this is what
		// the model judged noise, shown for checking, not for reading.
		base := m.hiddenStyle(bl.text)
		return strings.Repeat(" ", m.gutterCells()) + m.st.hiddenGutter.Render(columnSep) +
			m.paint(text, base, base, nil, m.hitsAt(i, sideOld), "")

	default:
		r := m.rows[bl.row]
		base := m.rowStyle(r)
		return m.numGutter(bl.row, sideOld) +
			m.paint(text, base, m.emphStyle(r.Kind), emph, m.hitsAt(i, sideOld), "")
	}
}

// renderPair draws one line of the two-column view: each side's line number, its
// content, and the rule between them. A side with no line — the filler a shorter
// run of a change block leaves behind — renders as blank, and the absence of a
// line number beside it is what says there is nothing there rather than an empty
// line there.
func (m Model) renderPair(i int, bl bodyLine) string {
	cw := m.colWidth()
	var b strings.Builder
	for _, side := range []int{sideOld, sideNew} {
		c := bl.left
		if side == sideNew {
			c = bl.right
			b.WriteString(" " + m.st.sep.Render(columnSep) + " ")
		}
		if c.Row < 0 {
			b.WriteString(strings.Repeat(" ", m.gutterCells()+cw))
			continue
		}
		text, emph := m.displayCell(bl, side)
		r := m.rows[c.Row]
		base := m.rowStyle(r)
		b.WriteString(m.numGutter(c.Row, side))
		b.WriteString(padTo(m.paint(text, base, m.emphStyle(r.Kind), emph, m.hitsAt(i, side), ""), cw))
	}
	return b.String()
}

// numGutter renders a row's line numbers: both sides in the unified view, and
// only this column's in the split view. It is empty when porkchop has no exact
// numbers, which is the case when it was given no original to align against.
func (m Model) numGutter(row, side int) string {
	if m.numDigits == 0 {
		return ""
	}
	n := diffview.LineNo{}
	if row >= 0 && row < len(m.align.Nums) {
		n = m.align.Nums[row]
	}
	if m.split {
		// Each column shows its own file's numbering. This matters most for
		// context, which is the same row on both sides but sits at a different
		// line in each once the change above it has added or removed any.
		which := n.Old
		if side == sideNew {
			which = n.New
		}
		return m.st.gutter.Render(gutterNum(which, m.numDigits) + " ")
	}
	return m.st.gutter.Render(gutterNum(n.Old, m.numDigits) + " " + gutterNum(n.New, m.numDigits) + " ")
}

// gutterNum right-aligns a line number in w cells, or leaves the space blank when
// the row has no line on that side.
func gutterNum(n, w int) string {
	if n <= 0 {
		return strings.Repeat(" ", w)
	}
	s := strconv.Itoa(n)
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

func (m Model) rowStyle(r diffview.Row) lipgloss.Style {
	switch r.Kind {
	case diffview.RowAdd:
		return m.st.add
	case diffview.RowDel:
		return m.st.del
	case diffview.RowFold:
		return m.st.fold
	case diffview.RowHunk:
		return m.st.hunk
	case diffview.RowMeta:
		// The per-file "diff --git" header is a navigational anchor; make it
		// bolder than the index/---/+++ noise around it.
		if strings.HasPrefix(r.Text, "diff --git ") {
			return m.st.fileHeader
		}
		return m.st.meta
	default:
		return m.st.context
	}
}

// emphStyle is the background a changed token gets, which side of the change it
// is on deciding the hue.
func (m Model) emphStyle(k diffview.RowKind) lipgloss.Style {
	switch k {
	case diffview.RowAdd:
		return m.st.emphAdd
	case diffview.RowDel:
		return m.st.emphDel
	default:
		return m.st.context
	}
}

// paint styles text with base, overlaying emph on the intra-line changed tokens
// and the search style on the matches, and a third style where the two coincide
// so a hit on a changed token stays distinguishable from both. prefix is rendered
// in base ahead of the text and is never searched or emphasized.
//
// Painting the layers into the content is what makes intra-line highlighting and
// search coexist. The viewport's own SetHighlights cannot be used for the search
// half: it resolves match offsets by walking graphemes of the ANSI-stripped
// content while indexing bytes into the unstripped string, so it only agrees with
// itself when the content carries no escape sequences — and sub-line styling is
// exactly what intra-line highlighting puts there.
func (m Model) paint(text string, base, emph lipgloss.Style, emphSpans []diffview.Span, hits hitLayer, prefix string) string {
	matches, cur := hits.spans, hits.cur
	if prefix != "" {
		text = prefix + text
		emphSpans = shiftSpans(emphSpans, len(prefix))
		matches = shiftSpans(matches, len(prefix))
		if cur.End > cur.Start {
			cur = diffview.Span{Start: cur.Start + len(prefix), End: cur.End + len(prefix)}
		}
	}
	if len(emphSpans) == 0 && len(matches) == 0 {
		return base.Render(text)
	}

	cuts := make([]int, 0, 2*(len(emphSpans)+len(matches))+2)
	cuts = append(cuts, 0, len(text))
	for _, s := range emphSpans {
		cuts = append(cuts, s.Start, s.End)
	}
	for _, s := range matches {
		cuts = append(cuts, s.Start, s.End)
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	var b strings.Builder
	for i := 0; i+1 < len(cuts); i++ {
		lo, hi := cuts[i], cuts[i+1]
		if lo < 0 || hi > len(text) || lo >= hi {
			continue
		}
		style := base
		onCur := cur.End > cur.Start && lo >= cur.Start && hi <= cur.End
		switch inEmph, inMatch := covers(emphSpans, lo), covers(matches, lo); {
		case inMatch && onCur:
			// The focused match wins outright over the token emphasis under it:
			// n/N stepping has to be unmistakable, and it is transient where the
			// emphasis is not.
			style = m.st.matchCur
		case inEmph && inMatch:
			style = m.st.matchEmph
		case inMatch:
			style = m.st.match
		case inEmph:
			style = emph
		}
		b.WriteString(style.Render(text[lo:hi]))
	}
	return b.String()
}

func covers(spans []diffview.Span, pos int) bool {
	for _, s := range spans {
		if s.Start > pos {
			return false
		}
		if pos < s.End {
			return true
		}
	}
	return false
}

func shiftSpans(spans []diffview.Span, by int) []diffview.Span {
	if len(spans) == 0 || by == 0 {
		return spans
	}
	out := make([]diffview.Span, len(spans))
	for i, s := range spans {
		out[i] = diffview.Span{Start: s.Start + by, End: s.End + by}
	}
	return out
}

// prepare expands a line's tabs and clips it to width cells, keeping the spans
// pointing at the same characters throughout. A width of 0 or less means no
// clipping, which is what the tests that check losslessness rely on.
func prepare(text string, width int, spans []diffview.Span) (string, []diffview.Span) {
	text, spans = expandTabsSpans(text, tabWidth, spans)
	if width > 0 && ansi.StringWidth(text) > width {
		text = ansi.Truncate(text, width, "")
		spans = clipSpans(spans, len(text))
	}
	return text, spans
}

// padTo pads a rendered string out to w cells so the column beside it starts in
// the right place.
func padTo(s string, w int) string {
	if gap := w - ansi.StringWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// clipSpans drops spans that start past limit and trims the one that straddles
// it, so truncating a line cannot leave a span pointing outside it.
func clipSpans(spans []diffview.Span, limit int) []diffview.Span {
	out := make([]diffview.Span, 0, len(spans))
	for _, s := range spans {
		if s.Start >= limit {
			break
		}
		out = append(out, diffview.Span{Start: s.Start, End: min(s.End, limit)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// expandTabs replaces tabs with spaces to the next multiple of width.
func expandTabs(s string, width int) string {
	out, _ := expandTabsSpans(s, width, nil)
	return out
}

// expandTabsSpans expands tabs and moves every span offset to where its
// characters ended up, so intra-line highlighting still lands on the tokens it
// was computed for in a tab-indented file.
func expandTabsSpans(s string, width int, spans []diffview.Span) (string, []diffview.Span) {
	if !strings.ContainsRune(s, '\t') {
		return s, spans
	}

	// moved[i] is where byte i of s ends up. Entries are written at rune
	// boundaries and then filled forward, so an offset that somehow landed inside
	// a multi-byte rune maps to that rune's start rather than out of bounds.
	moved := make([]int, len(s)+1)
	for i := range moved {
		moved[i] = -1
	}

	var b strings.Builder
	col := 0
	for i, r := range s {
		moved[i] = b.Len()
		if r == '\t' {
			n := width - col%width
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	moved[len(s)] = b.Len()
	last := 0
	for i, v := range moved {
		if v < 0 {
			moved[i] = last
			continue
		}
		last = v
	}

	if len(spans) == 0 {
		return b.String(), nil
	}
	out := make([]diffview.Span, 0, len(spans))
	for _, sp := range spans {
		start, end := moved[clampIndex(sp.Start, len(s))], moved[clampIndex(sp.End, len(s))]
		if end > start {
			out = append(out, diffview.Span{Start: start, End: end})
		}
	}
	return b.String(), out
}

func clampIndex(i, n int) int {
	return min(max(i, 0), n)
}
