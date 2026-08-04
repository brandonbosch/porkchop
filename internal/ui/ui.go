// Package ui is porkchop's terminal review screen: a Bubble Tea model that
// presents meat's reading diff the way a reviewer wants to read it.
//
// This is the Phase-1 unified view — one column, classified green/red/amber,
// with a header manifest and a scrolling body. It owns every visual decision
// (all Lip Gloss styling lives here); the semantic row model it renders comes
// from internal/diffview, which is pure and terminal-free. Keeping the split
// this way means the taste lives in one place and the parsing stays testable.
//
// Everything on screen is a pure function of the two-string seam (the raw diff
// and meat's reading diff) that cmd/porkchop computes and hands in via Input —
// no git, LLM, cache, or terminal state leaks into what is drawn.
package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonbosch/porkchop/internal/diffview"
)

// tabWidth is the display width porkchop expands source tabs to. A fixed stop
// keeps intra-line alignment predictable when measuring and truncating.
const tabWidth = 4

// Input is everything the review screen needs, all derived upstream from the
// raw diff and meat's Result. ui parses ReadingDiff itself (it is the diffview
// consumer) and needs only byte sizes for the reduction stat.
type Input struct {
	// Summary is meat's one-line, high-level description of the change.
	Summary string
	// Elision is meat.ElisionLine's authoritative manifest, e.g.
	// "kept 12/240 changed lines in 3/7 files".
	Elision string
	// ReadingDiff is meat.Result.SmartDiff — the abridged diff to render.
	ReadingDiff string
	// RawDiffBytes is the size of the pre-abridgement diff, for "N% smaller".
	RawDiffBytes int
}

// Run launches the review screen and blocks until the reviewer quits.
func Run(in Input) error {
	p := tea.NewProgram(New(in), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// Model is the Bubble Tea model for the unified review screen.
type Model struct {
	summary      string
	elision      string
	rows         []diffview.Row
	readingBytes int
	rawBytes     int

	vp     viewport.Model
	ready  bool
	width  int
	height int
	st     styles
}

// New builds a Model from Input, parsing the reading diff into rows up front.
func New(in Input) Model {
	return Model{
		summary:      strings.TrimSpace(in.Summary),
		elision:      strings.TrimSpace(in.Elision),
		rows:         diffview.Parse(in.ReadingDiff),
		readingBytes: len(in.ReadingDiff),
		rawBytes:     in.RawDiffBytes,
		st:           newStyles(),
	}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "j":
			m.vp.LineDown(1)
			return m, nil
		case "k":
			m.vp.LineUp(1)
			return m, nil
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			return m, nil
		}
	}

	// Everything else — arrows, page keys, half-page (ctrl+u/ctrl+d), and the
	// mouse wheel — is handled by the viewport's own keymap.
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m Model) View() string {
	if !m.ready {
		return "\n  initializing…"
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.renderHeader(), m.vp.View(), m.renderFooter())
}

// layout (re)sizes the viewport around the header and one-line footer and
// refreshes its content. Called on every resize. The header height is measured
// from the same renderHeader View() uses, so they never disagree.
func (m *Model) layout() {
	headerHeight := lipgloss.Height(m.renderHeader())
	const footerHeight = 1
	body := m.height - headerHeight - footerHeight
	if body < 1 {
		body = 1
	}
	if !m.ready {
		m.vp = viewport.New(m.width, body)
		m.vp.MouseWheelEnabled = true
		m.ready = true
	} else {
		m.vp.Width = m.width
		m.vp.Height = body
	}
	m.vp.SetContent(m.renderBody())
}

// renderBody styles every row and joins them into the viewport content. Each
// line is tab-expanded and hard-truncated to the terminal width so a long
// source line clips cleanly instead of wrapping and breaking the row grid.
func (m Model) renderBody() string {
	var b strings.Builder
	for i, r := range m.rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.renderRow(r))
	}
	return b.String()
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
	if m.width > 0 {
		style = style.MaxWidth(m.width)
	}
	return style.Render(text)
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
	tiles = append(tiles, m.st.tile.Render(fmt.Sprintf("%d rows", len(m.rows))))
	if m.rawBytes > 0 {
		saved := m.rawBytes - m.readingBytes
		pct := saved * 100 / m.rawBytes
		tiles = append(tiles, m.st.tile.Render(fmt.Sprintf("%d%% smaller  %s → %s",
			pct, humanBytes(m.rawBytes), humanBytes(m.readingBytes))))
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, tiles...)
	return lipgloss.JoinVertical(lipgloss.Left, title, bar, m.st.rule.Render(strings.Repeat("─", max(m.width, 1))))
}

func (m Model) renderFooter() string {
	pct := 0
	if m.ready {
		pct = int(m.vp.ScrollPercent() * 100)
	}
	help := "j/k scroll · g/G top/bottom · ctrl+d/u half-page · q quit"
	scroll := fmt.Sprintf("%3d%%", pct)
	gap := m.width - lipgloss.Width(help) - lipgloss.Width(scroll)
	if gap < 1 {
		gap = 1
	}
	line := help + strings.Repeat(" ", gap) + scroll
	return m.st.footer.MaxWidth(max(m.width, 1)).Render(line)
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
