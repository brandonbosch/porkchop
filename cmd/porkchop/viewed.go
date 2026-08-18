package main

import (
	"github.com/brandonbosch/porkchop/internal/gitx"
	"github.com/brandonbosch/porkchop/internal/store"
	"github.com/brandonbosch/porkchop/internal/ui"
)

// viewedStore adapts the on-disk marker file to the interface the review screen
// wants, binding it to one repo and flushing on every change.
//
// The adapter lives here rather than in either package so that internal/ui keeps
// knowing nothing about the filesystem and internal/store keeps knowing nothing
// about the screen — cmd/porkchop is the only place allowed to know both.
type viewedStore struct {
	dir, repo string
	v         *store.Viewed
}

// openViewed loads this repo's markers, or returns nil when there is nowhere to
// keep them: caching disabled, or a diff read from stdin outside any repo. A nil
// store is a supported state — the review screen tracks markers for the session and
// simply forgets them.
func openViewed() ui.ViewedStore {
	dir, repo := store.Dir(), gitx.Root()
	if store.ViewedPath(dir, repo) == "" {
		return nil
	}
	return &viewedStore{dir: dir, repo: repo, v: store.LoadViewed(dir, repo)}
}

func (s *viewedStore) Has(path, digest string) bool { return s.v.Has(path, digest) }

func (s *viewedStore) Set(path, digest string, on bool) { s.v.Set(path, digest, on) }

func (s *viewedStore) Save() { store.StoreViewed(s.dir, s.repo, s.v) }
