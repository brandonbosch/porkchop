package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// viewedRetention is how long an untouched marker survives, and viewedMax how
// many a single repo may keep. Both exist only to keep the file from growing
// without bound in a long-lived repo; a reviewer who comes back to a change after
// three months should be re-reading it anyway.
const (
	viewedRetention = 90 * 24 * time.Hour
	viewedMax       = 4096
)

// Viewed is the set of files a reviewer has checked off, for one repo.
//
// It is deliberately *not* stored beside a cached result. meat's cache is keyed by
// the hash of the whole diff, so a per-key sidecar would drop every marker each
// time any file in the change moved — exactly backwards for the case markers exist
// for, which is reviewing a range while an agent keeps adding commits to it. Marks
// are therefore per repo and keyed by the file's own content digest
// (diffview.Alignment.Digests), so they survive the rest of the change moving and
// un-check themselves only when their own file does.
//
// One entry per path, not per digest: re-viewing a file after it changes replaces
// the old mark rather than accumulating a history of versions read, and a stale
// digest simply fails to match.
type Viewed struct {
	// Repo is the repo root the file belongs to, recorded for humans reading the
	// file; the filename already encodes it.
	Repo string `json:"repo,omitempty"`
	// Files maps a path in the diff to the version of it that was read.
	Files map[string]ViewedMark `json:"files,omitempty"`
}

// ViewedMark is one file's marker: which version was read, and when.
type ViewedMark struct {
	// Digest is the diffview digest of the file's section of the original diff at
	// the moment it was marked.
	Digest string `json:"digest"`
	// At is when it was marked, used for pruning.
	At time.Time `json:"at"`
}

// Has reports whether path has been marked at this exact digest. A mark recorded
// against a different digest is not a match: the file has changed since it was
// read, which is precisely when the reviewer needs to look again.
//
// An empty digest never matches. Without an original diff porkchop has no
// trustworthy identity for a file (see diffview.FileDigests), and guessing would
// mark unread content as read.
func (v *Viewed) Has(path, digest string) bool {
	if v == nil || digest == "" {
		return false
	}
	m, ok := v.Files[path]
	return ok && m.Digest == digest
}

// Set marks or unmarks path at the given digest. Marking with an empty digest is
// a no-op, for the reason given on Has.
func (v *Viewed) Set(path, digest string, on bool) {
	if v == nil {
		return
	}
	if !on {
		delete(v.Files, path)
		return
	}
	if digest == "" {
		return
	}
	if v.Files == nil {
		v.Files = make(map[string]ViewedMark)
	}
	v.Files[path] = ViewedMark{Digest: digest, At: time.Now().UTC()}
}

// prune drops markers that have aged out and, if the repo is still over the cap,
// the oldest of what remains.
func (v *Viewed) prune(now time.Time) {
	if v == nil {
		return
	}
	for path, m := range v.Files {
		if now.Sub(m.At) > viewedRetention {
			delete(v.Files, path)
		}
	}
	if len(v.Files) <= viewedMax {
		return
	}
	paths := make([]string, 0, len(v.Files))
	for p := range v.Files {
		paths = append(paths, p)
	}
	// Oldest first, path breaking ties so the eviction is deterministic.
	sort.Slice(paths, func(i, j int) bool {
		a, b := v.Files[paths[i]], v.Files[paths[j]]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return paths[i] < paths[j]
	})
	for _, p := range paths[:len(v.Files)-viewedMax] {
		delete(v.Files, p)
	}
}

// ViewedPath is the file holding a repo's markers, or "" when there is nowhere to
// keep them — no cache dir (MEAT_CACHE="" disables caching entirely) or no repo
// root, as when a diff arrives on stdin from outside a repo. Callers treat "" as
// "markers are session-only".
//
// Porkchop's own state lives under a porkchop/ subdirectory rather than beside
// meat's flat <key>.json entries, so sharing the cache with plain meat stays a
// read-only arrangement in both directions.
func ViewedPath(dir, repoRoot string) string {
	if dir == "" || repoRoot == "" {
		return ""
	}
	// Name the file after the repo for legibility, and hash the full path for
	// identity — two checkouts of the same project must not share markers.
	sum := sha256.Sum256([]byte(repoRoot))
	name := sanitize(filepath.Base(repoRoot)) + "-" + hex.EncodeToString(sum[:])[:16] + ".json"
	return filepath.Join(dir, "porkchop", "viewed", name)
}

// sanitize reduces a repo directory name to something safe in a filename, so the
// human-readable half of the name cannot escape the directory or upset a shell.
func sanitize(name string) string {
	keep := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}
	out := strings.Map(keep, name)
	out = strings.Trim(out, ".-")
	if out == "" {
		return "repo"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// LoadViewed reads a repo's markers. Like Load, a missing, unreadable, or corrupt
// file is an empty result rather than an error: losing markers costs a reviewer a
// second look, and is never worth failing a review session over.
func LoadViewed(dir, repoRoot string) *Viewed {
	v := &Viewed{Repo: repoRoot, Files: map[string]ViewedMark{}}
	path := ViewedPath(dir, repoRoot)
	if path == "" {
		return v
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return v
	}
	var on Viewed
	if err := json.Unmarshal(data, &on); err != nil {
		return v
	}
	if on.Files != nil {
		v.Files = on.Files
	}
	v.prune(time.Now().UTC())
	return v
}

// StoreViewed writes a repo's markers, pruning first. Failures are ignored, for
// the reason given on LoadViewed.
func StoreViewed(dir, repoRoot string, v *Viewed) {
	path := ViewedPath(dir, repoRoot)
	if path == "" || v == nil {
		return
	}
	v.prune(time.Now().UTC())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	// Same atomic replace as Store: a reader must never see half a file, and two
	// porkchop sessions in one repo must not interleave into a corrupt one.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
	}
}
