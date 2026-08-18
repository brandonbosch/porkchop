package ui

import "fmt"

// ViewedStore persists per-file "viewed" markers between review sessions. It is
// injected rather than reached for, so the review screen keeps knowing nothing
// about caches or the filesystem: a test supplies a map, cmd/porkchop supplies the
// repo's marker file, and a diff arriving from outside a repo supplies nil.
//
// digest is the file's content identity from diffview.Alignment.Digests, which is
// what makes a marker survive the rest of the change moving underneath it. It is
// empty when porkchop was given no original diff to derive one from; a store must
// then decline to record anything, since the alternative is marking unread content
// as read.
type ViewedStore interface {
	// Has reports whether path was marked at exactly this digest.
	Has(path, digest string) bool
	// Set marks or unmarks path at this digest.
	Set(path, digest string, on bool)
	// Save flushes to wherever the markers live. Called on every toggle: the file
	// is tiny, and a reviewer who quits with ctrl+c should not lose their place in
	// a fifteen-file change.
	Save()
}

// initViewed seeds the per-file marker state from the store, pairing each file
// with its content digest. Both slices are parallel to m.files, so a marker, a
// digest, and a name are always the same index.
func (m *Model) initViewed() {
	m.viewed = make([]bool, len(m.files))
	m.digests = make([]string, len(m.files))
	for i, path := range m.files {
		// A nil Digests map (no original supplied) yields "", which is the signal
		// that markers cannot be persisted for this session.
		m.digests[i] = m.align.Digests[path]
		if m.vs != nil && m.vs.Has(path, m.digests[i]) {
			m.viewed[i] = true
		}
	}
}

// currentFileViewed reports whether the file the reviewer is currently inside has
// been checked off.
func (m Model) currentFileViewed() bool {
	if len(m.viewed) == 0 {
		return false
	}
	return m.viewed[clampIndex(m.currentFileIndex(), len(m.viewed))]
}

// viewedCount is how many of the change's files have been checked off.
func (m Model) viewedCount() int {
	n := 0
	for _, v := range m.viewed {
		if v {
			n++
		}
	}
	return n
}

// viewedTile is the header's progress line, or "" before the reviewer has checked
// anything off. It stays hidden until there is progress to report so it does not
// crowd the manifest on a narrow terminal, and it names the finish line explicitly
// when the last file goes green — the one moment a reviewer wants told, not
// inferred from two numbers being equal.
func (m Model) viewedTile() string {
	n := len(m.files)
	if n == 0 {
		return ""
	}
	switch v := m.viewedCount(); {
	case v == 0:
		return ""
	case v == n:
		return fmt.Sprintf("✓ all %d %s viewed", n, plural(n, "file", "files"))
	default:
		return fmt.Sprintf("✓ %d/%d viewed", v, n)
	}
}

// toggleViewed checks the current file off, or un-checks it.
//
// With no original diff there is no digest to key a marker to, so the toggle still
// shows on screen for this session but nothing is written — the store declines an
// empty digest rather than recording a marker it could never validate.
func (m *Model) toggleViewed() {
	if len(m.files) == 0 || m.audit {
		return
	}
	i := clampIndex(m.currentFileIndex(), len(m.viewed))
	m.viewed[i] = !m.viewed[i]
	if m.vs != nil {
		m.vs.Set(m.files[i], m.digests[i], m.viewed[i])
		m.vs.Save()
	}
}

// stepUnviewed jumps to the next file the reviewer has not checked off.
//
// Unlike every other stepping key this one wraps, and deliberately: the unviewed
// set shrinks as the reviewer works, so clamping at the end would strand the files
// they skipped above the cursor with no key that reaches them. Wrapping makes `tab`
// mean "what is left", which is the question, and it terminates on its own — once
// nothing is unviewed the key does nothing at all.
func (m *Model) stepUnviewed() {
	if len(m.files) == 0 || m.audit {
		return
	}
	from := clampIndex(m.currentFileIndex(), len(m.files))
	for off := 1; off <= len(m.files); off++ {
		i := (from + off) % len(m.files)
		if !m.viewed[i] {
			m.jumpToRow(m.fileRows[i])
			return
		}
	}
}
