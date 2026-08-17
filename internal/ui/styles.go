package ui

import "github.com/charmbracelet/lipgloss"

// styles is porkchop's whole visual vocabulary in one place. Colors are
// AdaptiveColor so the view stays legible on both light and dark terminals;
// Lip Gloss picks the right side from the detected background.
type styles struct {
	add        lipgloss.Style // added lines (green)
	del        lipgloss.Style // removed lines (red)
	fold       lipgloss.Style // "..." elision markers (amber, faint — "noise")
	context    lipgloss.Style // unchanged context (dim)
	meta       lipgloss.Style // index/---/+++ and other file metadata (dim)
	fileHeader lipgloss.Style // the "diff --git" per-file anchor (bold cyan)
	hunk       lipgloss.Style // "@@" hunk headers (magenta)

	summary  lipgloss.Style // header title line
	tile     lipgloss.Style // a neutral stat tile
	tileKept lipgloss.Style // the retained-lines stat tile (emphasized)
	rule     lipgloss.Style // the thin divider under the header
	footer   lipgloss.Style // the keybinding/scroll footer
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

		summary:  lipgloss.NewStyle().Bold(true),
		tile:     tile.Foreground(dim),
		tileKept: tile.Bold(true),
		rule:     lipgloss.NewStyle().Foreground(faint),
		footer:   lipgloss.NewStyle().Foreground(dim),
	}
}
