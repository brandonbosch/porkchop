// Package ui is porkchop's terminal review screen: a Bubble Tea model that
// presents meat's reading diff the way a reviewer wants to read it.
//
// It owns every visual decision (all Lip Gloss styling lives here); the semantic
// row model and the raw-diff alignment it renders come from internal/diffview,
// which is pure and terminal-free. Keeping the split this way means the taste
// lives in one place and the hard logic stays testable.
//
// Everything on screen is a pure function of the two-string seam (the raw diff
// and meat's reading diff) that cmd/porkchop hands in via Input — no git, LLM,
// cache, or terminal state leaks into what is drawn.
//
// The screen has two views. The review view is the reading diff in one column,
// classified green/red/amber, with an elision marker wherever meat dropped
// changed lines; `e` expands a marker in place to show the original content it
// hides. The audit view (`a`) drops the reading diff entirely and shows only the
// discard pile, grouped by file. Both exist because an abridgement you cannot
// cheaply check is not something a reviewer can responsibly trust — surfacing
// the original on demand is the point of the tool, not a convenience.
//
// Note that markers are mostly synthesized rather than read off meat's "..."
// fold rows: meat marks only a small minority of what it removes (see
// diffview.Align), so keying expansion to fold rows alone would expose a
// fraction of the hidden content. A marker is drawn for every elision that hides
// changed lines; elisions that merely trimmed context stay invisible here and
// are reported in aggregate in the header.
package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/brandonbosch/porkchop/internal/diffview"
)

// tabWidth is the display width porkchop expands source tabs to. A fixed stop
// keeps intra-line alignment predictable when measuring and truncating.
const tabWidth = 4

// minSplitWidth is the terminal width at which porkchop opens in the two-column
// view instead of the unified one, and minColWidth the narrowest content column
// worth putting a line of code in. Below either, the split is not a better way to
// read the same diff — it is the same diff with less of each line visible — so the
// unified view is chosen instead and `u` still overrides the choice by hand.
const (
	minSplitWidth = 120
	minColWidth   = 24
)

// Input is everything the review screen needs, all derived upstream from the
// raw diff and meat's Result.
type Input struct {
	// Summary is meat's one-line, high-level description of the change.
	Summary string
	// Elision is meat.ElisionLine's authoritative manifest, e.g.
	// "kept 12/240 changed lines in 3/7 files".
	Elision string
	// ReadingDiff is meat.Result.SmartDiff — the abridged diff to render.
	ReadingDiff string
	// RawDiff is the original pre-abridgement diff. It backs fold expansion and
	// the audit view, and supplies the size-reduction stat. When empty (a
	// reading diff rendered with no original available) the screen degrades
	// cleanly to the unified view with no markers.
	RawDiff string
	// Viewed persists the per-file "viewed" markers across sessions. Nil is
	// allowed and means the markers live only as long as this screen does.
	Viewed ViewedStore
}

// Run launches the review screen and blocks until the reviewer quits.
//
// Alt-screen and mouse reporting are not set here: in Bubble Tea v2 they are
// properties of the view, declared in View below.
func Run(in Input) error {
	p := tea.NewProgram(New(in))
	_, err := p.Run()
	return err
}

// Model is the Bubble Tea model for the review screen.
type Model struct {
	summary      string
	elision      string
	align        diffview.Alignment
	rows         []diffview.Row
	lay          diffview.Layout
	readingBytes int
	rawBytes     int

	// split is whether the two-column view is showing. It is chosen from the
	// terminal width on the first layout and then left alone once the reviewer has
	// pressed `u`, so a resize never undoes a deliberate choice.
	split       bool
	splitPinned bool

	// numDigits is the width the line-number gutter needs, and 0 when porkchop has
	// no exact numbers to show — see diffview.LineNo.
	numDigits int

	// files names each file in the diff and fileRows locates its header row, for
	// the breadcrumb and for ]/[ stepping. rowFile indexes rows into files.
	files    []string
	fileRows []int
	hunkRows []int
	rowFile  []int

	// Per-file "viewed" markers, parallel to files: viewed is this session's
	// state, digests the content identity each marker is keyed to, and vs the
	// place they persist (nil for session-only).
	viewed  []bool
	digests []string
	vs      ViewedStore

	// Search. query is the committed or in-progress needle, hits every match on
	// screen in reading order, and hitIndex the same matches keyed by content area
	// so the painter can find a line's own without scanning.
	searching bool
	query     string
	hits      []hit
	hitIndex  map[int][]diffview.Span
	hitCur    int

	// marks are the elisions that get a marker, in reading order; expanded is
	// parallel to it. cur is the marker `e` acts on, or -1 when there are none.
	marks    []diffview.Elision
	expanded []bool
	cur      int

	// contextOnly counts elisions that hid no changed lines. They get no marker
	// (they are trimmed context, not hidden change) but are worth reporting.
	contextOnly int

	// restated marks the git header rows a file banner stands in for, and banners
	// holds those banners by the "diff --git" row that opens each file.
	restated []bool
	banners  map[int]*fileBanner

	// body is the rendered review view as semantic lines; markerLine maps each
	// mark to its line index in body, and rowLine each row to the line it is shown
	// on, both so a jump can be turned into a scroll offset.
	body       []bodyLine
	markerLine []int
	rowLine    []int

	audit      bool
	mainOffset int

	vp     viewport.Model
	ready  bool
	width  int
	height int

	// dark is the terminal background porkchop is painting for. It starts true
	// and is corrected on the tea.BackgroundColorMsg that Init requests, so the
	// palette is deterministic for offline rendering and still adapts on a real
	// terminal. st is the palette resolved against it.
	dark bool
	st   styles
}

// bodyKind distinguishes the three kinds of line in the review view: rows that
// came from the reading diff, elision markers, and original content revealed by
// expanding a marker.
type bodyKind uint8

const (
	bodyRow bodyKind = iota
	bodyMarker
	bodyHidden
	// bodyPair is one line of the two-column view, carrying a cell per side. It
	// replaces bodyRow for source lines when the split view is showing.
	bodyPair
	// bodyBanner is the in-flow separator that opens a file, standing in for the
	// git header rows it restates. bodySpacer is the blank line above it.
	bodyBanner
	bodySpacer
)

// bodyLine is one line of the review view before styling. row and mark are
// indices back into Model.rows / Model.marks, or -1; keeping them lets tests
// assert the view neither drops nor reorders content.
type bodyLine struct {
	kind bodyKind
	row  int
	mark int
	text string
	// left and right are the two columns of a bodyPair, either of which may be
	// filler (Row < 0). They are unset for every other kind.
	left  diffview.Cell
	right diffview.Cell
	// spanRows are the additional rows this line stands in for, beyond row. Only
	// a banner has them: it replaces a run of header rows with one separator, and
	// listing them here is what keeps "every row is accounted for exactly once"
	// true of a view that draws fewer lines than it has rows.
	spanRows []int
}

// rows returns the row indices this line accounts for, which is what lets a test
// assert the body neither drops nor duplicates any of them.
func (bl bodyLine) rows() []int {
	if bl.kind != bodyPair {
		if bl.row >= 0 {
			return append([]int{bl.row}, bl.spanRows...)
		}
		return nil
	}
	var out []int
	if bl.left.Row >= 0 {
		out = append(out, bl.left.Row)
	}
	if bl.right.Row >= 0 && bl.right.Row != bl.left.Row {
		out = append(out, bl.right.Row)
	}
	return out
}

// firstRow is the lowest row this line shows, or -1 for a line that shows none.
// It is what "where am I" means for the breadcrumb and for ]/[ stepping.
func (bl bodyLine) firstRow() int {
	rows := bl.rows()
	if len(rows) == 0 {
		return -1
	}
	return min(rows[0], rows[len(rows)-1])
}

// New builds a Model from Input, aligning the reading diff against its original
// up front so markers and expansion are ready before the first frame.
func New(in Input) Model {
	m := Model{
		summary:      strings.TrimSpace(in.Summary),
		elision:      strings.TrimSpace(in.Elision),
		align:        diffview.Align(in.RawDiff, in.ReadingDiff),
		readingBytes: len(in.ReadingDiff),
		rawBytes:     len(in.RawDiff),
		cur:          -1,
		hitCur:       -1,
		dark:         true,
		st:           newStyles(true),
		vs:           in.Viewed,
	}
	m.rows = m.align.Rows
	m.lay = diffview.Split(m.rows)
	m.numDigits = numberWidth(m.align.Nums)
	m.indexFiles()
	m.initViewed()

	// A marker is worth drawing when the elision hides real change, or when meat
	// itself flagged it — in which case hiding the flag would be a regression
	// against plain meat.
	for _, e := range m.align.Elisions {
		if e.Changed > 0 || e.FoldRow >= 0 {
			m.marks = append(m.marks, e)
			continue
		}
		m.contextOnly++
	}
	m.expanded = make([]bool, len(m.marks))
	if len(m.marks) > 0 {
		m.cur = 0
	}
	m.rebuild()
	return m
}

// Init asks the terminal for its background color; the palette New assumed is
// corrected when the answer arrives.
func (m Model) Init() tea.Cmd { return tea.RequestBackgroundColor }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.BackgroundColorMsg:
		if dark := msg.IsDark(); dark != m.dark {
			m.dark, m.st = dark, newStyles(dark)
			if m.ready {
				m.setContent()
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		// While the query prompt is open it owns the keyboard: a reviewer typing
		// "quit" into the search box must not quit.
		if m.searching {
			return m.updateSearch(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			// esc unwinds one layer at a time — first the search, then the audit
			// view — and only quits when there is nothing left to back out of.
			switch {
			case m.searchActive():
				m.clearSearch()
			case m.audit:
				m.setAudit(false)
			default:
				return m, tea.Quit
			}
			return m, nil
		case "a":
			m.setAudit(!m.audit)
			return m, nil
		case "u":
			m.toggleSplit()
			return m, nil
		case "/":
			m.openSearch()
			return m, nil
		case "j":
			m.vp.ScrollDown(1)
			return m, nil
		case "k":
			m.vp.ScrollUp(1)
			return m, nil
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.gotoBottom()
			return m, nil
		case "]":
			m.stepAnchor(m.fileRows, 1)
			return m, nil
		case "[":
			m.stepAnchor(m.fileRows, -1)
			return m, nil
		case "}":
			m.stepAnchor(m.hunkRows, 1)
			return m, nil
		case "{":
			m.stepAnchor(m.hunkRows, -1)
			return m, nil
		case "n":
			// n/N step search matches while a search is live and elisions
			// otherwise, which is what a pager-literate reviewer expects. The
			// footer always names which of the two is bound.
			if m.searchActive() {
				m.stepHit(1)
				return m, nil
			}
			m.stepMark(1)
			return m, nil
		case "N":
			if m.searchActive() {
				m.stepHit(-1)
				return m, nil
			}
			m.stepMark(-1)
			return m, nil
		case "p":
			m.stepMark(-1)
			return m, nil
		case "v":
			m.toggleViewed()
			return m, nil
		case "tab":
			m.stepUnviewed()
			return m, nil
		case "e":
			m.toggleCurrent()
			return m, nil
		case "E":
			m.toggleAll()
			return m, nil
		}
	}

	// Everything else — arrows, page keys, half-page (ctrl+u/ctrl+d), and the
	// mouse wheel — is handled by the viewport's own keymap.
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// View renders the frame and declares the terminal modes it needs. Alt-screen
// and mouse reporting are view properties in Bubble Tea v2 rather than program
// options, so they are stated here on every frame.
func (m Model) View() tea.View {
	content := "\n  initializing…"
	if m.ready {
		content = lipgloss.JoinVertical(lipgloss.Left, m.renderHeader(), m.vp.View(), m.renderFooter())
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// layout (re)sizes the viewport around the header and one-line footer and
// refreshes its content. The header height is measured from the same
// renderHeader View() uses, so they never disagree.
func (m *Model) layout() {
	// The view is chosen from the width until the reviewer chooses for themselves,
	// after which the choice stands through any resize.
	if !m.splitPinned {
		m.split = m.splitViable()
	}
	headerHeight := lipgloss.Height(m.renderHeader())
	const footerHeight = 1
	body := max(m.height-headerHeight-footerHeight, 1)
	if !m.ready {
		m.vp = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(body))
		m.vp.MouseWheelEnabled = true
		m.ready = true
	} else {
		m.vp.SetWidth(m.width)
		m.vp.SetHeight(body)
	}
	// Both the line model and the search offsets are functions of the width, so a
	// resize has to rebuild them rather than just re-clip what is there.
	m.rebuild()
	m.setContent()
}

func (m *Model) setContent() {
	if m.audit {
		m.vp.SetContent(m.renderAudit())
		return
	}
	// The body is followed by blank lines so that every line of it — including the
	// last file's header — can be scrolled to the top of the screen.
	//
	// Without them the final screenful of a long change is unreachable as a scroll
	// position: the viewport correctly refuses to scroll past its content, so the
	// last few files never reach the top, the breadcrumb can never name them, and
	// `v` can never check them off. That costs nothing on a six-file fixture that
	// fits the screen and makes the last files of a twenty-file change unreviewable.
	// Editors scroll past the end for the same reason.
	m.vp.SetContent(m.renderBody() + strings.Repeat("\n", m.tailPad()))
}

// tailPad is how many blank lines follow the body: enough for its last line to sit
// at the top of the screen, and no more.
func (m Model) tailPad() int {
	if m.audit {
		return 0
	}
	return max(m.vp.Height()-1, 0)
}

// rebuild recomputes the review view's line model. Called whenever the expansion
// state or the view changes, since either alters which lines the body holds.
func (m *Model) rebuild() {
	m.body = m.buildBody()
	m.markerLine = make([]int, len(m.marks))
	m.rowLine = make([]int, len(m.rows))
	for i := range m.rowLine {
		m.rowLine[i] = -1
	}
	for i, bl := range m.body {
		if bl.kind == bodyMarker && bl.mark >= 0 {
			m.markerLine[bl.mark] = i
		}
		for _, r := range bl.rows() {
			m.rowLine[r] = i
		}
	}
	// Matches are found against the body as just built, so they must be refreshed
	// with it or their offsets would refer to lines that have moved.
	m.findHits()
}

func (m *Model) buildBody() []bodyLine {
	if m.split {
		return m.buildSplitBody()
	}
	return m.buildUnifiedBody()
}

// buildUnifiedBody walks the reading diff's rows in order, splicing in a marker at
// each elision and, where expanded, the original lines that elision hides.
//
// An elision meat marked with a "..." row is rendered *as* that row's
// replacement — the marker says "12 changed lines hidden", which strictly
// dominates what "..." conveys — so each row still appears exactly once, either
// as itself or as the marker standing in for it.
func (m *Model) buildUnifiedBody() []bodyLine {
	out := make([]bodyLine, 0, len(m.rows)+len(m.marks))

	atFold := make(map[int]int, len(m.marks))
	atBefore := make(map[int][]int, len(m.marks))
	for i, e := range m.marks {
		if e.FoldRow >= 0 {
			atFold[e.FoldRow] = i
			continue
		}
		atBefore[e.BeforeRow] = append(atBefore[e.BeforeRow], i)
	}

	for i, r := range m.rows {
		for _, mark := range atBefore[i] {
			out = m.emitMark(out, mark, -1)
		}
		if mark, ok := atFold[i]; ok {
			out = m.emitMark(out, mark, i)
			continue
		}
		if bl, ok := m.bannerLine(i); ok {
			if len(out) > 0 {
				out = append(out, bodyLine{kind: bodySpacer, row: -1, mark: -1})
			}
			out = append(out, bl)
			continue
		}
		if m.restated[i] {
			continue
		}
		out = append(out, bodyLine{kind: bodyRow, row: i, mark: -1, text: r.Text})
	}
	// An elision after the last row anchors at len(rows).
	for _, mark := range atBefore[len(m.rows)] {
		out = m.emitMark(out, mark, -1)
	}
	return out
}

// buildSplitBody is the same splice against the two-column layout.
//
// Markers are anchored to rows while the layout is a list of lines, so each
// anchor is resolved through the row-to-line map. The consequence is that a
// marker whose anchor falls inside a change block attaches to that block rather
// than appearing between its two columns, which is the only available reading: the
// columns of a pair are the same moment of the change, and there is no "between"
// for a full-width row to occupy.
func (m *Model) buildSplitBody() []bodyLine {
	out := make([]bodyLine, 0, len(m.lay.Lines)+len(m.marks))

	lineOf := m.lay.LineOfRow()
	atFold := make(map[int]int, len(m.marks))
	atBefore := make(map[int][]int, len(m.marks))
	var trailing []int
	for i, e := range m.marks {
		switch {
		case e.FoldRow >= 0 && e.FoldRow < len(lineOf) && lineOf[e.FoldRow] >= 0:
			atFold[lineOf[e.FoldRow]] = i
		case e.BeforeRow >= 0 && e.BeforeRow < len(lineOf) && lineOf[e.BeforeRow] >= 0:
			at := lineOf[e.BeforeRow]
			atBefore[at] = append(atBefore[at], i)
		default:
			// An elision past the last row, or one whose anchor no layout line
			// claims, goes at the end rather than being dropped.
			trailing = append(trailing, i)
		}
	}

	for li, l := range m.lay.Lines {
		for _, mark := range atBefore[li] {
			out = m.emitMark(out, mark, -1)
		}
		if mark, ok := atFold[li]; ok {
			out = m.emitMark(out, mark, l.Row)
			continue
		}
		if l.Kind == diffview.SplitFull {
			if bl, ok := m.bannerLine(l.Row); ok {
				if len(out) > 0 {
					out = append(out, bodyLine{kind: bodySpacer, row: -1, mark: -1})
				}
				out = append(out, bl)
				continue
			}
			if m.restated[l.Row] {
				continue
			}
			out = append(out, bodyLine{kind: bodyRow, row: l.Row, mark: -1, text: m.rows[l.Row].Text})
			continue
		}
		out = append(out, bodyLine{kind: bodyPair, row: -1, mark: -1, left: l.Left, right: l.Right})
	}
	for _, mark := range trailing {
		out = m.emitMark(out, mark, -1)
	}
	return out
}

// bannerLine is the body line that opens the file starting at row, if one does.
// It carries every header row it replaces, so the body still accounts for each of
// them exactly once — the banner *is* those rows, drawn as one line.
func (m Model) bannerLine(row int) (bodyLine, bool) {
	b, ok := m.banners[row]
	if !ok {
		return bodyLine{}, false
	}
	return bodyLine{kind: bodyBanner, row: row, mark: -1, text: b.label, spanRows: b.rows}, true
}

// emitMark appends a marker and, when it is expanded, the original lines it hides.
func (m *Model) emitMark(out []bodyLine, mark, row int) []bodyLine {
	out = append(out, bodyLine{kind: bodyMarker, row: row, mark: mark, text: m.markerText(mark)})
	if !m.expanded[mark] {
		return out
	}
	for _, line := range m.align.Hidden(m.marks[mark]) {
		out = append(out, bodyLine{kind: bodyHidden, row: -1, mark: mark, text: line})
	}
	return out
}

// markerKind is what an elision hides. It drives the marker's wording and its
// color, from this one classification on purpose: a marker that reads "comment"
// but is painted like code — or the reverse — is worse than either signal alone,
// because a reviewer who learns the colors stops reading the words.
type markerKind uint8

const (
	markerCode    markerKind = iota // hides real code — the amber warning
	markerProse                     // hides nothing but commentary
	markerEmpty                     // hides nothing but blank lines
	markerContext                   // hides no changed line at all
)

func kindOf(e diffview.Elision) markerKind {
	switch {
	case e.Changed == 0:
		return markerContext
	case e.Blank == e.Changed:
		return markerEmpty
	case e.Comment > 0 && e.Comment+e.Blank == e.Changed:
		return markerProse
	default:
		return markerCode
	}
}

// markerText describes what a marker hides and how to act on it. The wording
// leads with the count of *changed* lines because that is the number a reviewer
// is deciding whether to trust — except where the hidden lines are all blank or
// all commentary, when it leads with that instead, since the decision the marker
// exists to inform is "is this worth an expand".
func (m Model) markerText(i int) string {
	e := m.marks[i]
	glyph, hint := "▸", "e expand"
	if m.expanded[i] {
		glyph, hint = "▾", "e collapse"
	}

	// The wording names the most specific thing the marker can honestly claim,
	// because what the reviewer is deciding is whether to spend an expand. Both
	// special cases below are still drawn and still counted, so the header's
	// "N hidden in M spots" continues to reconcile with what is on screen —
	// suppressing them would not.
	kind := kindOf(e)
	var what string
	var qual []string
	switch kind {
	case markerEmpty:
		// Everything this marker hides is an empty line. Saying so costs the same
		// row and tells the reviewer there is nothing behind it, rather than
		// inviting an expand that reveals whitespace.
		what = fmt.Sprintf("%d blank %s", e.Blank, plural(e.Blank, "line", "lines"))
	case markerProse:
		// Nothing here but prose: a docstring, a block of "#" lines, a license
		// header. Worth an expand sometimes — a comment can be the whole point of
		// a change — but it is the reviewer's call to make from the marker rather
		// than after paying for it, which is the same bargain the blank case
		// offers. Comment and Blank are disjoint, so the two counts sum to Changed.
		what = fmt.Sprintf("%d comment %s", e.Comment, plural(e.Comment, "line", "lines"))
		if e.Blank > 0 {
			qual = append(qual, fmt.Sprintf("+%d blank", e.Blank))
		}
	case markerContext:
		what = fmt.Sprintf("%d context %s", e.Len(), plural(e.Len(), "line", "lines"))
	default:
		what = fmt.Sprintf("%d changed %s", e.Changed, plural(e.Changed, "line", "lines"))
	}
	if kind != markerContext {
		if n := e.Len() - e.Changed; n > 0 {
			qual = append(qual, fmt.Sprintf("+%d context", n))
		}
	}
	if len(qual) > 0 {
		what += " (" + strings.Join(qual, ", ") + ")"
	}
	if !m.expanded[i] {
		what += " hidden"
	}
	return fmt.Sprintf("%s %s  ·  %s", glyph, what, hint)
}

func (m Model) renderBody() string {
	var b strings.Builder
	for i, bl := range m.body {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.renderBodyLine(i, bl))
	}
	return b.String()
}

// hiddenStyle keeps polarity legible inside expanded content without letting it
// compete with the reading diff's own add/del colors.
func (m Model) hiddenStyle(line string) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "+"):
		return m.st.hiddenAdd
	case strings.HasPrefix(line, "-"):
		return m.st.hiddenDel
	default:
		return m.st.hidden
	}
}

// clamp hard-truncates to the terminal width so a long source line clips
// cleanly instead of wrapping and breaking the row grid.
func (m Model) clamp(s lipgloss.Style) lipgloss.Style {
	if m.width > 0 {
		return s.MaxWidth(m.width)
	}
	return s
}

// renderAudit is the discard pile: what meat removed, grouped by file, with the
// reading diff out of the way. It exists to answer "what did it hide, and was it
// right?" in one pass.
//
// It lists only the elisions that hide changed lines. That is not a shortcut: it
// follows from meat being subtractive, so the reading diff's lines are a
// subsequence of the original's and no line can be reclassified. A context line
// is by definition identical in the old and new versions, so a gap containing no
// added or removed lines cannot conceal any part of the change — it is exactly
// the comprehension padding meat is supposed to drop. Those gaps are reported as
// counts rather than dumped, because burying the change-bearing elisions under
// them is what would actually cost the reviewer their audit.
func (m Model) renderAudit() string {
	if len(m.align.Elisions) == 0 {
		return m.st.meta.Render("  nothing was elided — the reading diff is the whole diff")
	}

	type group struct {
		changeBearing []diffview.Elision
		contextOnly   int
		contextLines  int
		changed       int
	}
	byFile := map[string]*group{}
	var order []string
	for _, e := range m.align.Elisions {
		name := e.File
		if name == "" {
			name = "(before any file header)"
		}
		g, seen := byFile[name]
		if !seen {
			g = &group{}
			byFile[name] = g
			order = append(order, name)
		}
		if e.Changed == 0 {
			g.contextOnly++
			g.contextLines += e.Len()
			continue
		}
		g.changeBearing = append(g.changeBearing, e)
		g.changed += e.Changed
	}
	sort.Strings(order)

	filesWithChange := 0
	for _, g := range byFile {
		if len(g.changeBearing) > 0 {
			filesWithChange++
		}
	}

	var b strings.Builder
	b.WriteString(m.clamp(m.st.summary).Render(fmt.Sprintf(
		"%d changed %s hidden across %d %s in %d %s",
		m.align.ChangedHidden(), plural(m.align.ChangedHidden(), "line", "lines"),
		len(m.marks), plural(len(m.marks), "spot", "spots"),
		filesWithChange, plural(filesWithChange, "file", "files"))))
	b.WriteByte('\n')
	if m.contextOnly > 0 {
		b.WriteString(m.clamp(m.st.meta).Render(fmt.Sprintf(
			"%d context-only %s omitted — unchanged lines cannot hide a change",
			m.contextOnly, plural(m.contextOnly, "elision", "elisions"))))
		b.WriteByte('\n')
	}

	wrote := 0
	for _, name := range order {
		g := byFile[name]
		if len(g.changeBearing) == 0 {
			continue
		}
		b.WriteString("\n")
		b.WriteString(m.clamp(m.st.fileHeader).Render(name))
		b.WriteByte('\n')
		note := fmt.Sprintf("  %d changed %s hidden in %d %s",
			g.changed, plural(g.changed, "line", "lines"),
			len(g.changeBearing), plural(len(g.changeBearing), "spot", "spots"))
		if g.contextOnly > 0 {
			note += fmt.Sprintf("  ·  %d context %s also trimmed",
				g.contextLines, plural(g.contextLines, "line", "lines"))
		}
		b.WriteString(m.clamp(m.st.meta).Render(note))
		b.WriteByte('\n')

		for _, e := range g.changeBearing {
			label := fmt.Sprintf("  ▾ %d changed, %d total  ·  original lines %d-%d",
				e.Changed, e.Len(), e.RawStart+1, e.RawEnd)
			// Tiered the same way as in the review view. The audit still lists
			// every hidden line under every heading — the color only says which
			// headings are worth reading first.
			b.WriteString(m.clamp(m.st.forMarker(kindOf(e), false)).Render(label))
			b.WriteByte('\n')
			for _, line := range m.align.Hidden(e) {
				b.WriteString(m.clamp(m.st.hiddenGutter).Render("    │"))
				b.WriteString(m.clamp(m.hiddenStyle(line)).Render(expandTabs(line, tabWidth)))
				b.WriteByte('\n')
			}
		}
		wrote++
	}
	if wrote == 0 {
		b.WriteString("\n")
		b.WriteString(m.clamp(m.st.meta).Render(
			"  meat elided only unchanged context — no part of the change is hidden"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderHeader is the manifest: meat's summary over a row of stat tiles.
func (m Model) renderHeader() string {
	summary := m.summary
	if summary == "" {
		summary = "(no summary)"
	}
	title := m.st.summary.MaxWidth(max(m.width, 1)).Render("porkchop  " + summary)

	var tiles []string
	if m.elision != "" {
		tiles = append(tiles, m.st.tileKept.Render(m.elision))
	}
	// The trust headline: how much changed content is not on screen.
	if hidden := m.align.ChangedHidden(); hidden > 0 {
		tiles = append(tiles, m.st.tileHidden.Render(fmt.Sprintf("%d hidden in %d spots", hidden, len(m.marks))))
	}
	// Review progress, once there is any. It sits with the trust stats rather than
	// in the footer because it is state, not a keybinding.
	if tile := m.viewedTile(); tile != "" {
		tiles = append(tiles, m.st.tileViewed.Render(tile))
	}
	if m.rawBytes > 0 {
		saved := m.rawBytes - m.readingBytes
		pct := saved * 100 / m.rawBytes
		tiles = append(tiles, m.st.tile.Render(fmt.Sprintf("%d%% smaller  %s → %s",
			pct, humanBytes(m.rawBytes), humanBytes(m.readingBytes))))
	} else {
		tiles = append(tiles, m.st.tile.Render(fmt.Sprintf("%d rows", len(m.rows))))
	}
	// Clipped, not wrapped: the tile row is one line by construction, and the
	// header height is measured from this same render — a wrapped bar would silently
	// disagree with the viewport's idea of where the body starts.
	bar := lipgloss.NewStyle().MaxWidth(max(m.width, 1)).Render(lipgloss.JoinHorizontal(lipgloss.Top, tiles...))
	return lipgloss.JoinVertical(lipgloss.Left, title, bar, m.renderRule())
}

// renderRule is the divider under the header, carrying the name of the file the
// reviewer is currently inside. The breadcrumb goes in the rule because it costs
// no vertical space there, and on a 15-file change knowing which file you are
// looking at is not optional — but neither is a line of diff.
func (m Model) renderRule() string {
	w := max(m.width, 1)
	label := m.currentFile()
	// The check rides with the file name because "have I read this one" is a
	// property of the file the reviewer is looking at, not of the change.
	check := ""
	if m.currentFileViewed() {
		check = "✓ "
	}
	const lead = 2
	shown := check + label
	// Fall back to a plain rule when the name would leave no rule to speak of.
	if label == "" || w < lead+lipgloss.Width(shown)+6 {
		return m.st.rule.Render(strings.Repeat("─", w))
	}
	tail := w - lead - lipgloss.Width(shown) - 2
	return m.st.rule.Render(strings.Repeat("─", lead)) + " " +
		m.st.viewed.Render(check) + m.st.breadcrumb.Render(label) + " " +
		m.st.rule.Render(strings.Repeat("─", max(tail, 0)))
}

// currentFileIndex is which file the top of the viewport is inside.
func (m Model) currentFileIndex() int {
	if row := m.topRow(); row >= 0 && row < len(m.rowFile) && m.rowFile[row] >= 0 {
		return m.rowFile[row]
	}
	return 0
}

// currentFile names the file at the top of the viewport and its position in the
// change.
func (m Model) currentFile() string {
	if len(m.files) == 0 {
		return ""
	}
	idx := m.currentFileIndex()
	name := m.files[idx]
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("%s  (%d/%d)", name, idx+1, len(m.files))
}

// otherView is what `u` would switch to, so the footer can say what the key does
// rather than what the state already is.
func (m Model) otherView() string {
	if m.split {
		return "unified"
	}
	return "split"
}

// hintTiers is how many footer hint lists there are, tried widest-first.
const hintTiers = 4

// renderFooter is the keybinding line for the current mode, at the longest hint
// list that fits the terminal.
//
// The lists a reviewer needs at 80 columns and at 200 are different lists, not a
// prefix and a suffix of one list, so the footer picks a list rather than fitting
// hints one at a time. Which list is chosen is *measured* against the actual
// rendered width rather than compared to a column threshold: the hints interpolate
// counts ("n/p elision (240/1200)"), so any hardcoded threshold is wrong for some
// change — and a footer wider than the terminal is silently clipped, losing its
// tail, which is where `q quit` lives.
func (m Model) renderFooter() string {
	if m.searching {
		hint := "type to search · esc cancel"
		if m.query != "" {
			hint = fmt.Sprintf("%d %s · enter keep · esc cancel",
				len(m.hits), plural(len(m.hits), "match", "matches"))
		}
		line := m.renderPrompt() + "  " + m.st.footer.Render(hint)
		return lipgloss.NewStyle().MaxWidth(max(m.width, 1)).Render(line)
	}

	scroll := m.scrollLabel()
	for tier := hintTiers - 1; tier > 0; tier-- {
		help := strings.Join(m.hints(tier), " · ")
		// One cell minimum between the hints and the percentage, or they touch.
		if lipgloss.Width(help)+1+lipgloss.Width(scroll) <= m.width {
			return m.footerLine(help, scroll)
		}
	}
	// The narrowest list is used whether or not it fits; at that point there is
	// nothing left to drop.
	return m.footerLine(strings.Join(m.hints(0), " · "), scroll)
}

// hints is the footer's hint list for the current mode at a given width tier, in
// reading order. Tier 0 is what a reviewer cannot work without; each tier above it
// adds what the next stretch of terminal can afford, ordered so the keys that do
// the most work survive the longest:
//
//	1  file and hunk stepping — how you get around a fifteen-file change
//	2  view switching and the audit view — mode changes, not motion
//	3  tab and g/G — shortcuts for something already reachable another way
//
// `v` is in every tier: it is the only key that records a decision rather than
// moving the screen, so a reviewer who never sees it never learns the tool is
// tracking their progress at all.
func (m Model) hints(tier int) []string {
	// The audit view has no files to step and nothing to check off. It is one list,
	// and the only question it raises is how to get back out.
	if m.audit {
		return []string{"j/k scroll", "a/esc back to review", "q quit"}
	}
	var h []string
	switch {
	case m.searchActive():
		h = append(h, fmt.Sprintf("n/N match (%d/%d)", m.hitCur+1, len(m.hits)), "esc clear")
		if tier >= 1 {
			h = append(h, "]/[ file")
		}
	case len(m.marks) == 0:
		h = append(h, "j/k scroll")
		if tier >= 3 {
			h = append(h, "g/G top/bottom")
		}
		if tier >= 1 {
			h = append(h, "]/[ file", "}/{ hunk")
		}
	default:
		h = append(h, fmt.Sprintf("n/p elision (%d/%d)", m.cur+1, len(m.marks)), "e expand")
		if tier >= 1 {
			h = append(h, "E all", "]/[ file", "}/{ hunk")
		}
	}
	if len(m.files) > 0 {
		h = append(h, "v viewed")
		if tier >= 3 {
			h = append(h, "tab unviewed")
		}
	}
	// A live search owns n/N and esc; offering `/ search` again says nothing.
	if !m.searchActive() {
		h = append(h, "/ search")
	}
	if tier >= 2 {
		h = append(h, fmt.Sprintf("u %s", m.otherView()), "a audit")
	}
	return append(h, "q quit")
}

// scrollLabel is the right-hand percentage, fixed width so the hints do not shift
// as the reviewer scrolls.
//
// It measures progress through the content, not through the viewport: the blank tail
// setContent appends is scrollable but is not diff, and counting it would report the
// end of a change as somewhere around 60%.
func (m Model) scrollLabel() string {
	return fmt.Sprintf("%3d%%", m.scrollPercent())
}

func (m Model) scrollPercent() int {
	if !m.ready {
		return 0
	}
	if m.audit {
		return int(m.vp.ScrollPercent() * 100)
	}
	height := m.vp.Height()
	if len(m.body) <= height {
		return 100
	}
	bottom := m.vp.YOffset() + height
	return min(100, max(bottom*100/len(m.body), 0))
}

// footerLine right-aligns the scroll percentage against the hints.
func (m Model) footerLine(help, scroll string) string {
	gap := max(m.width-lipgloss.Width(help)-lipgloss.Width(scroll), 1)
	return m.st.footer.MaxWidth(max(m.width, 1)).Render(help + strings.Repeat(" ", gap) + scroll)
}

// setAudit switches views, preserving the review view's scroll position so
// checking the discard pile and coming back does not lose the reviewer's place.
func (m *Model) setAudit(on bool) {
	if on == m.audit {
		return
	}
	if on {
		m.mainOffset = m.vp.YOffset()
		m.audit = true
		m.setContent()
		m.vp.GotoTop()
		return
	}
	m.audit = false
	m.setContent()
	m.vp.SetYOffset(m.mainOffset)
}

// numberWidth is how many digits the line-number gutter needs, and 0 when there
// are no numbers to show at all.
func numberWidth(nums []diffview.LineNo) int {
	high := 0
	for _, n := range nums {
		high = max(high, n.Old, n.New)
	}
	if high == 0 {
		return 0
	}
	return len(strconv.Itoa(high))
}

// indexFiles labels every row with the file it belongs to and records where each
// file's header and each hunk header sits, which is what ]/[ and }/{ step between
// and what the breadcrumb reads.
func (m *Model) indexFiles() {
	m.rowFile = make([]int, len(m.rows))
	m.restated = make([]bool, len(m.rows))
	m.banners = map[int]*fileBanner{}
	cur, banner := -1, (*fileBanner)(nil)
	for i, r := range m.rows {
		switch {
		case r.Kind == diffview.RowHunk:
			m.hunkRows = append(m.hunkRows, i)
			banner = nil
		case r.Kind == diffview.RowMeta && strings.HasPrefix(r.Text, "diff --git "):
			cur = len(m.files)
			name, _ := diffview.FileOf(r.Text)
			m.files = append(m.files, name)
			m.fileRows = append(m.fileRows, i)
			banner = &fileBanner{label: name}
			m.banners[i] = banner
			if old, new, ok := diffview.GitHeaderPaths(r.Text); ok {
				banner.old, banner.new = old, new
				if trimSide(old) != trimSide(new) {
					// A rename is the one case where the two paths are not the same
					// fact twice, so the banner has to carry both.
					banner.label = trimSide(old) + " → " + trimSide(new)
					banner.renamed = true
				}
			}
		case r.Kind == diffview.RowMeta && strings.HasPrefix(r.Text, "+++ ") && cur >= 0:
			// "+++ b/path" names exactly one path, so it is preferred over the
			// two-path "diff --git" line whenever it turns up.
			if name, ok := diffview.FileOf(r.Text); ok {
				m.files[cur] = name
				if banner != nil && !banner.renamed {
					banner.label = name
				}
			}
		case r.Kind != diffview.RowMeta:
			banner = nil
		}
		// Header rows the banner already says are folded into it. Everything else
		// git chose to emit — a mode change, a new or deleted file, a similarity
		// index, a binary marker — is content the banner does not carry, and stays.
		if banner != nil && banner.restates(r) {
			m.restated[i] = true
			banner.rows = append(banner.rows, i)
		}
		m.rowFile[i] = cur
	}
}

// fileBanner is the one-line separator that opens a file in the body, and the
// record of which header rows it stands in for.
//
// Collapsing the block is not the same kind of act as meat's eliding: a git
// header carries no line of the change, and the banner restates every path in it.
// So the rule is exactly that — a row is folded into the banner only when the
// banner already says what it says. `index <sha>..<sha>` goes because the blob
// hashes are not usable from inside a reader and the mode it trails is repeated
// by the "new file mode"/"old mode" lines whenever it is news; "---" and "+++" go
// only when they name the paths the banner is already showing, which leaves
// /dev/null (an add or a delete) on screen where it belongs.
type fileBanner struct {
	label    string
	old, new string
	renamed  bool
	rows     []int
}

func (b *fileBanner) restates(r diffview.Row) bool {
	if r.Kind != diffview.RowMeta {
		return false
	}
	switch {
	case strings.HasPrefix(r.Text, "index "):
		return true
	case strings.HasPrefix(r.Text, "--- "):
		return metaPath(r.Text, "--- ") == b.old
	case strings.HasPrefix(r.Text, "+++ "):
		return metaPath(r.Text, "+++ ") == b.new
	}
	return false
}

// metaPath is the path a "---"/"+++" line names, with git's optional trailing
// timestamp field removed.
func metaPath(line, prefix string) string {
	rest := strings.TrimPrefix(line, prefix)
	rest, _, _ = strings.Cut(rest, "\t")
	return rest
}

// trimSide drops the "a/" or "b/" git prefixes a header path carries.
func trimSide(p string) string {
	if rest, ok := strings.CutPrefix(p, "a/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(p, "b/"); ok {
		return rest
	}
	return p
}

// topRow is the first row at or below the top of the viewport — what "where the
// reviewer is" means for the breadcrumb and for anchor stepping.
func (m Model) topRow() int {
	for i := m.vp.YOffset(); i < len(m.body); i++ {
		if r := m.body[i].firstRow(); r >= 0 {
			return r
		}
	}
	// Scrolled into the blank tail setContent appends. The reviewer is still looking
	// at the end of the last file, so report its last row; falling off the end here
	// would snap the breadcrumb back to the first file.
	for i := len(m.body) - 1; i >= 0; i-- {
		if r := m.body[i].firstRow(); r >= 0 {
			return r
		}
	}
	return len(m.rows)
}

// gotoBottom scrolls to the end of the content rather than the end of the padded
// viewport content, so G fills the screen with the tail of the diff instead of with
// the blank tail behind it.
func (m *Model) gotoBottom() {
	if m.audit {
		m.vp.GotoBottom()
		return
	}
	m.vp.SetYOffset(max(len(m.body)-m.vp.Height(), 0))
}

// stepAnchor jumps to the next or previous row in a sorted list of anchors — file
// headers or hunk headers. It clamps at the ends rather than wrapping, matching
// elision stepping so that holding a key settles instead of cycling.
func (m *Model) stepAnchor(anchors []int, delta int) {
	if len(anchors) == 0 || m.audit {
		return
	}
	from := m.topRow()
	target := anchors[0]
	if delta > 0 {
		target = anchors[len(anchors)-1]
		for _, a := range anchors {
			if a > from {
				target = a
				break
			}
		}
	} else {
		for i := len(anchors) - 1; i >= 0; i-- {
			if anchors[i] < from {
				target = anchors[i]
				break
			}
		}
	}
	m.jumpToRow(target)
}

// jumpToRow scrolls so a row sits at the top of the viewport. Headers are what
// this is used for, and a header belongs at the top of what it heads.
func (m *Model) jumpToRow(row int) {
	if !m.ready || row < 0 || row >= len(m.rowLine) {
		return
	}
	if line := m.rowLine[row]; line >= 0 {
		m.vp.SetYOffset(line)
	}
}

// toggleSplit switches between the unified and two-column views, keeping whatever
// row was at the top of the screen at the top of the screen so the reviewer does
// not lose their place. The choice is pinned once made by hand, so a later resize
// will not silently undo it.
func (m *Model) toggleSplit() {
	if m.audit {
		return
	}
	anchor := m.topRow()
	m.split = !m.split
	m.splitPinned = true
	m.rebuild()
	m.setContent()
	m.jumpToRow(anchor)
}

// stepMark moves the cursor to the next or previous elision marker and scrolls
// it into view. It clamps rather than wrapping, so holding `n` settles at the
// last elision instead of cycling silently.
func (m *Model) stepMark(delta int) {
	if len(m.marks) == 0 || m.audit {
		return
	}
	next, ok := m.markFromView(delta)
	if !ok {
		next = m.cur + delta
	}
	switch {
	case next < 0:
		next = 0
	case next >= len(m.marks):
		next = len(m.marks) - 1
	}
	m.cur = next
	// The caret moved, so the marker styling changed on two lines.
	m.rebuild()
	m.setContent()
	m.scrollToMark(m.cur)
}

func (m *Model) toggleCurrent() {
	if len(m.marks) == 0 || m.audit {
		return
	}
	// e acts on the marker the reviewer is looking at. Without this it acted on
	// wherever n last was, expanding content off screen — a worse version of
	// the same defect, because nothing visible moves to explain it.
	if seated, ok := m.markFromView(1); ok {
		m.cur = seated
	}
	if m.cur < 0 || m.cur >= len(m.marks) {
		return
	}
	m.expanded[m.cur] = !m.expanded[m.cur]
	m.rebuild()
	m.setContent()
	m.scrollToMark(m.cur)
}

// toggleAll expands everything, or collapses everything if anything is already
// expanded — so E is always a single keystroke back to a known state.
func (m *Model) toggleAll() {
	if len(m.marks) == 0 || m.audit {
		return
	}
	anyExpanded := false
	for _, e := range m.expanded {
		if e {
			anyExpanded = true
			break
		}
	}
	for i := range m.expanded {
		m.expanded[i] = !anyExpanded
	}
	m.rebuild()
	m.setContent()
	if m.cur >= 0 {
		m.scrollToMark(m.cur)
	}
}

// markFromView re-seats the elision cursor from the viewport, and reports
// whether it had to.
//
// n/p keep a cursor because they need one: scrollToMark seats a marker a third
// of the way down the screen, so the marker just landed on stays visible, and a
// rule of "the first marker at or below the top" would keep finding it and n
// would stick. But that cursor was *only* ever moved by n/p, so every other way
// of getting somewhere — tab, ]/[, }/{, plain scrolling — left it behind, and
// the next n yanked the reviewer back to wherever they last pressed it.
//
// So: walk from the cursor while the reviewer is still looking at it, and
// otherwise seat it where they actually are. Forward means the first marker at
// or below the top of the screen, backward the last one above it — the same
// "from where I am" rule stepAnchor already uses for files and hunks, which is
// what made n the odd key out.
func (m Model) markFromView(delta int) (int, bool) {
	if !m.ready || len(m.markerLine) == 0 {
		return 0, false
	}
	top := m.vp.YOffset()
	if cur := m.cur; cur >= 0 && cur < len(m.markerLine) {
		if line := m.markerLine[cur]; line >= top && line < top+m.vp.Height() {
			return 0, false // still on screen: keep walking
		}
	}
	if delta > 0 {
		for i, line := range m.markerLine {
			if line >= top {
				return i, true
			}
		}
		return len(m.markerLine) - 1, true
	}
	for i := len(m.markerLine) - 1; i >= 0; i-- {
		if m.markerLine[i] < top {
			return i, true
		}
	}
	return 0, true
}

// scrollToMark brings a marker into view, seating it a third of the way down so
// the expanded content below it is visible without a second keystroke.
func (m *Model) scrollToMark(i int) {
	if !m.ready || i < 0 || i >= len(m.markerLine) {
		return
	}
	m.vp.SetYOffset(max(m.markerLine[i]-m.vp.Height()/3, 0))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
