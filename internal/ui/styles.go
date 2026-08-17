package ui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// styles is porkchop's whole visual vocabulary in one place. Every color is
// picked from a light/dark pair so the view stays legible on both kinds of
// terminal; which side wins is decided once, by the caller, and passed in.
type styles struct {
	add        lipgloss.Style // added lines (green)
	del        lipgloss.Style // removed lines (red)
	fold       lipgloss.Style // meat's own "..." rows, where left in place (amber)
	context    lipgloss.Style // unchanged context (dim)
	meta       lipgloss.Style // index/---/+++ and other file metadata (dim)
	fileHeader lipgloss.Style // the "diff --git" per-file anchor (bold cyan)
	hunk       lipgloss.Style // "@@" hunk headers (magenta)

	// Trust affordances. A marker announces hidden content and has to read as an
	// action, not as diff content — amber, and brighter still under the cursor so
	// there is never doubt about which one `e` will expand.
	marker    lipgloss.Style // an elision marker (amber)
	markerCur lipgloss.Style // the marker the cursor is on (bold amber)

	// Expanded original content is deliberately quieter than the reading diff:
	// it is there to be checked, not read. Polarity survives, saturation does not.
	hidden       lipgloss.Style // revealed context (faint)
	hiddenAdd    lipgloss.Style // revealed addition (faint green)
	hiddenDel    lipgloss.Style // revealed removal (faint red)
	hiddenGutter lipgloss.Style // the "│" rail marking revealed content

	summary    lipgloss.Style // header title line
	tile       lipgloss.Style // a neutral stat tile
	tileKept   lipgloss.Style // the retained-lines stat tile (emphasized)
	tileHidden lipgloss.Style // the hidden-changed-lines tile (amber, the warning)
	rule       lipgloss.Style // the thin divider under the header
	footer     lipgloss.Style // the keybinding/scroll footer
}

// newStyles resolves the palette against a known background. Taking `dark` as an
// argument rather than detecting it here keeps this package free of terminal
// I/O: Model starts on the dark palette (so offline golden rendering is
// deterministic) and swaps once the real background arrives as a
// tea.BackgroundColorMsg.
func newStyles(dark bool) styles {
	pick := lipgloss.LightDark(dark)
	hex := func(light, dark string) color.Color {
		return pick(lipgloss.Color(light), lipgloss.Color(dark))
	}

	var (
		green   = hex("#1a7f37", "#3fb950")
		red     = hex("#cf222e", "#f85149")
		amber   = hex("#9a6700", "#d29922")
		magenta = hex("#8250df", "#bc8cff")
		cyan    = hex("#0969da", "#58a6ff")
		dim     = hex("#6e7781", "#8b949e")
		faint   = hex("#8c959f", "#6e7681")

		// Muted polarity for revealed content: legible, but clearly a lower tier
		// than the reading diff's own add/del.
		mutedGreen = hex("#5a9367", "#2d6a35")
		mutedRed   = hex("#c08a8a", "#7d3b38")
	)

	tile := lipgloss.NewStyle().Padding(0, 1).MarginRight(1)

	return styles{
		add:        lipgloss.NewStyle().Foreground(green),
		del:        lipgloss.NewStyle().Foreground(red),
		fold:       lipgloss.NewStyle().Foreground(amber).Faint(true).Italic(true),
		context:    lipgloss.NewStyle().Foreground(dim),
		meta:       lipgloss.NewStyle().Foreground(faint),
		fileHeader: lipgloss.NewStyle().Foreground(cyan).Bold(true),
		hunk:       lipgloss.NewStyle().Foreground(magenta).Bold(true),

		marker:    lipgloss.NewStyle().Foreground(amber).Italic(true),
		markerCur: lipgloss.NewStyle().Foreground(amber).Bold(true),

		hidden:       lipgloss.NewStyle().Foreground(faint),
		hiddenAdd:    lipgloss.NewStyle().Foreground(mutedGreen),
		hiddenDel:    lipgloss.NewStyle().Foreground(mutedRed),
		hiddenGutter: lipgloss.NewStyle().Foreground(amber).Faint(true),

		summary:    lipgloss.NewStyle().Bold(true),
		tile:       tile.Foreground(dim),
		tileKept:   tile.Bold(true),
		tileHidden: tile.Foreground(amber).Bold(true),
		rule:       lipgloss.NewStyle().Foreground(faint),
		footer:     lipgloss.NewStyle().Foreground(dim),
	}
}
