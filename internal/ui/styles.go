package ui

import "github.com/charmbracelet/lipgloss"

// styles is porkchop's whole visual vocabulary in one place. Colors are
// AdaptiveColor so the view stays legible on both light and dark terminals;
// Lip Gloss picks the right side from the detected background.
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

func newStyles() styles {
	var (
		green   = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#3fb950"}
		red     = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#f85149"}
		amber   = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#d29922"}
		magenta = lipgloss.AdaptiveColor{Light: "#8250df", Dark: "#bc8cff"}
		cyan    = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58a6ff"}
		dim     = lipgloss.AdaptiveColor{Light: "#6e7781", Dark: "#8b949e"}
		faint   = lipgloss.AdaptiveColor{Light: "#8c959f", Dark: "#6e7681"}

		// Muted polarity for revealed content: legible, but clearly a lower tier
		// than the reading diff's own add/del.
		mutedGreen = lipgloss.AdaptiveColor{Light: "#5a9367", Dark: "#2d6a35"}
		mutedRed   = lipgloss.AdaptiveColor{Light: "#c08a8a", Dark: "#7d3b38"}
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
