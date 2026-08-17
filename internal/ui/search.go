package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/brandonbosch/porkchop/internal/diffview"
)

// hit is one search match: which body line it is on, which of that line's content
// areas, and where inside that area's display text it sits.
type hit struct {
	line int
	side int
	span diffview.Span
}

// hitLayer is the search state for a single content area, as the painter needs
// it: every match to highlight, plus the one the reviewer is currently on, if it
// happens to be in this area.
type hitLayer struct {
	spans []diffview.Span
	cur   diffview.Span
}

// editQuery applies one keypress to the query being typed and reports whether it
// consumed the key.
//
// The prompt is hand-rolled rather than built on bubbles/textinput, which would
// otherwise be the obvious choice. textinput imports a clipboard library that
// shells out to pbcopy/xclip for its yank support; porkchop is meant to run
// against CUI in a controlled environment, and an unannounced subprocess launched
// from a search box is a bad trade for editing niceties nobody needs in a
// single-line query. What a query does need — append, backspace, delete word,
// clear — is the whole of it.
func (m *Model) editQuery(msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "backspace":
		if m.query != "" {
			_, size := utf8.DecodeLastRuneInString(m.query)
			m.query = m.query[:len(m.query)-size]
		}
	case "ctrl+w":
		m.query = strings.TrimRight(m.query, " ")
		if i := strings.LastIndexByte(m.query, ' '); i >= 0 {
			m.query = m.query[:i+1]
		} else {
			m.query = ""
		}
	case "ctrl+u":
		m.query = ""
	default:
		// Only literal text extends the query; anything with a modifier or a
		// non-printing key is left for the caller to interpret.
		if r := msg.Code; msg.Mod == 0 && unicode.IsPrint(r) {
			m.query += string(r)
			break
		}
		return false
	}
	return true
}

// renderPrompt is the query line the footer shows while a search is being typed,
// with a block cursor drawn into the string so the frame stays a plain value.
func (m Model) renderPrompt() string {
	return m.st.prompt.Render("/"+m.query) + m.st.matchCur.Render(" ")
}

// hitsAt returns the matches inside one content area of one body line.
func (m Model) hitsAt(line, side int) hitLayer {
	if len(m.hits) == 0 {
		return hitLayer{}
	}
	l := hitLayer{spans: m.hitIndex[hitKey(line, side)]}
	if m.hitCur >= 0 && m.hitCur < len(m.hits) {
		if h := m.hits[m.hitCur]; h.line == line && h.side == side {
			l.cur = h.span
		}
	}
	return l
}

func hitKey(line, side int) int { return line*2 + side }

// findHits locates every occurrence of the query in what is currently on screen,
// in reading order.
//
// Matching runs on each content area's *display* text — the same string the
// painter will style, obtained from the same displayCell — so a hit's offsets are
// valid for the line as drawn, tab expansion and column clipping included. A
// consequence worth knowing: text clipped off the right edge of a narrow column
// is not searched, because it is not on screen to be found.
func (m *Model) findHits() {
	m.hits, m.hitIndex, m.hitCur = nil, nil, -1
	if m.query == "" {
		return
	}
	// Smart case: a lower-case query matches either case, and an upper-case letter
	// in the query is taken as a deliberate request to be exact.
	fold := !hasUpper(m.query)

	m.hitIndex = make(map[int][]diffview.Span)
	for i, bl := range m.body {
		for _, side := range []int{sideOld, sideNew} {
			text, _ := m.displayCell(bl, side)
			if text == "" {
				continue
			}
			spans := indexAll(text, m.query, fold)
			if len(spans) == 0 {
				continue
			}
			m.hitIndex[hitKey(i, side)] = spans
			for _, s := range spans {
				m.hits = append(m.hits, hit{line: i, side: side, span: s})
			}
		}
	}
	if len(m.hits) > 0 {
		m.hitCur = m.nearestHit()
	}
}

// nearestHit is the first match at or below the top of the viewport, so opening a
// search lands on something the reviewer can already see rather than jumping to
// the top of the diff.
func (m Model) nearestHit() int {
	for i, h := range m.hits {
		if h.line >= m.vp.YOffset() {
			return i
		}
	}
	return 0
}

// stepHit moves to the next or previous match. It wraps, which is what a search
// is expected to do — unlike elision stepping, where clamping keeps a held key
// from cycling through the reviewer's whole audit.
func (m *Model) stepHit(delta int) {
	if len(m.hits) == 0 {
		return
	}
	m.hitCur = (m.hitCur + delta + len(m.hits)) % len(m.hits)
	m.setContent()
	// Columns are passed as zero deliberately: porkchop clips every line to the
	// terminal width rather than scrolling horizontally, so there is never a
	// column off-screen to bring into view and only the vertical half applies.
	m.vp.EnsureVisible(m.hits[m.hitCur].line, 0, 0)
}

// updateSearch handles a keypress while the query prompt is open. Enter commits
// the query and esc abandons it; anything the editor does not consume is dropped
// rather than falling through to the review keymap.
func (m Model) updateSearch(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.closeSearch(true)
		return m, nil
	case "esc":
		m.closeSearch(false)
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	}
	if m.editQuery(msg) {
		m.findHits()
		m.setContent()
		// Jumping to the first hit as it is typed is what makes an incremental
		// search worth having: the reviewer sees the match, not just the count.
		if m.hitCur >= 0 {
			m.vp.EnsureVisible(m.hits[m.hitCur].line, 0, 0)
		}
	}
	return m, nil
}

// openSearch puts the footer into query entry. Highlighting updates on every
// keystroke, so the reviewer sees what the query catches before committing to it.
func (m *Model) openSearch() {
	m.searching = true
	m.query = ""
	m.findHits()
	m.setContent()
}

// closeSearch leaves query entry, keeping the highlights when the query was
// committed with enter and dropping them when it was abandoned with esc.
func (m *Model) closeSearch(keep bool) {
	m.searching = false
	if keep && m.query != "" {
		return
	}
	m.clearSearch()
}

func (m *Model) clearSearch() {
	m.query = ""
	m.findHits()
	m.setContent()
}

// searchActive reports whether a committed query is highlighting anything, which
// is what decides whether n/N steps matches or elisions.
func (m Model) searchActive() bool {
	return !m.searching && m.query != ""
}

func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// indexAll returns the non-overlapping occurrences of needle in hay as byte
// ranges.
//
// Case folding is applied per rune during comparison rather than by lower-casing
// both strings first, because lower-casing can change a string's byte length —
// and every offset returned here is used to slice the original.
func indexAll(hay, needle string, fold bool) []diffview.Span {
	if needle == "" {
		return nil
	}
	var out []diffview.Span
	for i := 0; i < len(hay); {
		if n := matchAt(hay[i:], needle, fold); n > 0 {
			out = append(out, diffview.Span{Start: i, End: i + n})
			i += n
			continue
		}
		_, size := utf8.DecodeRuneInString(hay[i:])
		i += size
	}
	return out
}

// matchAt reports the byte length of needle where it occurs at the start of s, or
// 0 if it does not. The length can differ from len(needle) under folding, since
// two runes that fold together need not encode to the same number of bytes.
func matchAt(s, needle string, fold bool) int {
	si, ni := 0, 0
	for ni < len(needle) {
		if si >= len(s) {
			return 0
		}
		sr, ss := utf8.DecodeRuneInString(s[si:])
		nr, ns := utf8.DecodeRuneInString(needle[ni:])
		if sr != nr && (!fold || unicode.ToLower(sr) != unicode.ToLower(nr)) {
			return 0
		}
		si += ss
		ni += ns
	}
	return si
}
