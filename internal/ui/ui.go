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
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/brandonbosch/porkchop/internal/diffview"
)

// tabWidth is the display width porkchop expands source tabs to. A fixed stop
// keeps intra-line alignment predictable when measuring and truncating.
const tabWidth = 4

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
	readingBytes int
	rawBytes     int

	// marks are the elisions that get a marker, in reading order; expanded is
	// parallel to it. cur is the marker `e` acts on, or -1 when there are none.
	marks    []diffview.Elision
	expanded []bool
	cur      int

	// contextOnly counts elisions that hid no changed lines. They get no marker
	// (they are trimmed context, not hidden change) but are worth reporting.
	contextOnly int

	// body is the rendered review view as semantic lines; markerLine maps each
	// mark to its line index in body, for scrolling to it.
	body       []bodyLine
	markerLine []int

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
)

// bodyLine is one line of the review view before styling. row and mark are
// indices back into Model.rows / Model.marks, or -1; keeping them lets tests
// assert the view neither drops nor reorders content.
type bodyLine struct {
	kind bodyKind
	row  int
	mark int
	text string
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
		dark:         true,
		st:           newStyles(true),
	}
	m.rows = m.align.Rows

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
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.audit {
				m.setAudit(false)
				return m, nil
			}
			return m, tea.Quit
		case "a":
			m.setAudit(!m.audit)
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
			m.vp.GotoBottom()
			return m, nil
		case "n":
			m.stepMark(1)
			return m, nil
		case "p", "N":
			m.stepMark(-1)
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
	m.setContent()
}

func (m *Model) setContent() {
	if m.audit {
		m.vp.SetContent(m.renderAudit())
		return
	}
	m.vp.SetContent(m.renderBody())
}

// rebuild recomputes the review view's line model. Called whenever expansion
// state changes, since expanding a marker splices original lines into the body.
func (m *Model) rebuild() {
	m.body = m.buildBody()
	m.markerLine = make([]int, len(m.marks))
	for i, bl := range m.body {
		if bl.kind == bodyMarker && bl.mark >= 0 {
			m.markerLine[bl.mark] = i
		}
	}
}

// buildBody walks the reading diff's rows in order, splicing in a marker at each
// elision and, where expanded, the original lines that elision hides.
//
// An elision meat marked with a "..." row is rendered *as* that row's
// replacement — the marker says "12 changed lines hidden", which strictly
// dominates what "..." conveys — so each row still appears exactly once, either
// as itself or as the marker standing in for it.
func (m *Model) buildBody() []bodyLine {
	out := make([]bodyLine, 0, len(m.rows)+len(m.marks))

	atFold := make(map[int]int, len(m.marks))
	atBefore := make(map[int]int, len(m.marks))
	for i, e := range m.marks {
		if e.FoldRow >= 0 {
			atFold[e.FoldRow] = i
		} else {
			atBefore[e.BeforeRow] = i
		}
	}

	emit := func(mark, row int) {
		out = append(out, bodyLine{kind: bodyMarker, row: row, mark: mark, text: m.markerText(mark)})
		if !m.expanded[mark] {
			return
		}
		for _, line := range m.align.Hidden(m.marks[mark]) {
			out = append(out, bodyLine{kind: bodyHidden, row: -1, mark: mark, text: line})
		}
	}

	for i, r := range m.rows {
		if mark, ok := atBefore[i]; ok {
			emit(mark, -1)
		}
		if mark, ok := atFold[i]; ok {
			emit(mark, i)
			continue
		}
		out = append(out, bodyLine{kind: bodyRow, row: i, mark: -1, text: r.Text})
	}
	// An elision after the last row anchors at len(rows).
	if mark, ok := atBefore[len(m.rows)]; ok {
		emit(mark, -1)
	}
	return out
}

// markerText describes what a marker hides and how to act on it. The wording
// leads with the count of *changed* lines because that is the number a reviewer
// is deciding whether to trust.
func (m Model) markerText(i int) string {
	e := m.marks[i]
	glyph, hint := "▸", "e expand"
	if m.expanded[i] {
		glyph, hint = "▾", "e collapse"
	}

	var what string
	switch {
	case e.Changed > 0:
		what = fmt.Sprintf("%d changed lines", e.Changed)
		if e.Len() > e.Changed {
			what += fmt.Sprintf(" (+%d context)", e.Len()-e.Changed)
		}
	default:
		what = fmt.Sprintf("%d context lines", e.Len())
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
		b.WriteString(m.renderBodyLine(bl))
	}
	return b.String()
}

func (m Model) renderBodyLine(bl bodyLine) string {
	switch bl.kind {
	case bodyMarker:
		// The current marker carries a caret; others get matching blanks so the
		// marker text stays in one column as the cursor moves.
		prefix := "  "
		style := m.st.marker
		if bl.mark == m.cur {
			prefix = "❯ "
			style = m.st.markerCur
		}
		return m.clamp(style).Render(prefix + bl.text)

	case bodyHidden:
		// Expanded original content is set apart by a gutter and dimmed: this is
		// what the model judged noise, shown for checking, not for reading.
		text := expandTabs(bl.text, tabWidth)
		return m.clamp(m.st.hiddenGutter).Render("│") + m.clamp(m.hiddenStyle(bl.text)).Render(text)

	default:
		return m.renderRow(m.rows[bl.row])
	}
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

func (m Model) renderRow(r diffview.Row) string {
	text := expandTabs(r.Text, tabWidth)
	var style lipgloss.Style
	switch r.Kind {
	case diffview.RowAdd:
		style = m.st.add
	case diffview.RowDel:
		style = m.st.del
	case diffview.RowFold:
		style = m.st.fold
	case diffview.RowHunk:
		style = m.st.hunk
	case diffview.RowMeta:
		// The per-file "diff --git" header is a navigational anchor; make it
		// bolder than the index/---/+++ noise around it.
		if strings.HasPrefix(text, "diff --git ") {
			style = m.st.fileHeader
		} else {
			style = m.st.meta
		}
	default:
		style = m.st.context
	}
	return m.clamp(style).Render(text)
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
			b.WriteString(m.clamp(m.st.marker).Render(label))
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
	if m.rawBytes > 0 {
		saved := m.rawBytes - m.readingBytes
		pct := saved * 100 / m.rawBytes
		tiles = append(tiles, m.st.tile.Render(fmt.Sprintf("%d%% smaller  %s → %s",
			pct, humanBytes(m.rawBytes), humanBytes(m.readingBytes))))
	} else {
		tiles = append(tiles, m.st.tile.Render(fmt.Sprintf("%d rows", len(m.rows))))
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, tiles...)
	return lipgloss.JoinVertical(lipgloss.Left, title, bar, m.st.rule.Render(strings.Repeat("─", max(m.width, 1))))
}

func (m Model) renderFooter() string {
	var help string
	switch {
	case m.audit:
		help = "j/k scroll · a/esc back to review · q quit"
	case len(m.marks) == 0:
		help = "j/k scroll · g/G top/bottom · a audit · q quit"
	default:
		help = fmt.Sprintf("j/k scroll · n/p elision (%d/%d) · e expand · E all · a audit · q quit",
			m.cur+1, len(m.marks))
	}

	pct := 0
	if m.ready {
		pct = int(m.vp.ScrollPercent() * 100)
	}
	scroll := fmt.Sprintf("%3d%%", pct)
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

// stepMark moves the cursor to the next or previous elision marker and scrolls
// it into view. It clamps rather than wrapping, so holding `n` settles at the
// last elision instead of cycling silently.
func (m *Model) stepMark(delta int) {
	if len(m.marks) == 0 || m.audit {
		return
	}
	next := m.cur + delta
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
	if m.cur < 0 || m.cur >= len(m.marks) || m.audit {
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

// scrollToMark brings a marker into view, seating it a third of the way down so
// the expanded content below it is visible without a second keystroke.
func (m *Model) scrollToMark(i int) {
	if !m.ready || i < 0 || i >= len(m.markerLine) {
		return
	}
	m.vp.SetYOffset(max(m.markerLine[i]-m.vp.Height()/3, 0))
}

// expandTabs replaces tabs with spaces to the next multiple of tabWidth,
// tracking column position so alignment survives the expansion.
func expandTabs(s string, width int) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := width - col%width
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
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
