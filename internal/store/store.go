// Package store is porkchop's result cache.
//
// It reuses meat's content-addressed ~/.meat store untouched: the same key
// (SHA of rubric-protocol + model + diff) means a commit processed by plain
// `meat` is a free cache hit for porkchop and vice versa. Like gitx, this is a
// self-contained copy of cmd/meat's cache plumbing, kept separate so the
// upstream tree stays byte-pristine for clean merges.
//
// Porkchop's own sidecar metadata (the compiled edit plan for fold expansion
// and the audit view) will be added here in a later phase as a sibling
// `<key>.porkchop.json`, leaving meat's `<key>.json` untouched.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/brandonbosch/porkchop/meat"
)

// Dir is the directory holding cached results. Overridable via MEAT_CACHE (set
// it to empty to disable caching) — the same env var meat honors, so both tools
// share one cache location.
func Dir() string {
	if v, ok := os.LookupEnv("MEAT_CACHE"); ok {
		return v // may be "" to disable
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".meat")
}

// Key is the content hash that names a cached result. It covers every input
// that shapes the answer: the diff text, the model id, and the complete
// rubric/compiler protocol (via its hash) — so editing the diff, switching
// models, or upgrading meat's rubric or a hard compiler invariant misses the
// cache and recomputes, while an identical re-run hits.
func Key(diff, model, rubric string) string {
	h := sha256.New()
	// NUL separators keep field boundaries unambiguous: a NUL can't appear in a
	// model id or rubric hash and is vanishingly unlikely to matter for diffs.
	h.Write([]byte(rubric))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(diff))
	return hex.EncodeToString(h.Sum(nil))
}

// Load returns a cached result for the key, and whether one was found. A
// missing or unreadable/corrupt entry is treated as a miss (caching is a
// best-effort optimization, never a hard dependency).
func Load(dir, key string) (*meat.Result, bool) {
	if dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return nil, false
	}
	var res meat.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, false
	}
	return &res, true
}

// Store writes a result for the key. Failures are silently ignored: a machine
// that can't write the cache should still produce its answer.
func Store(dir, key string, res *meat.Result) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(res)
	if err != nil {
		return
	}
	// Write atomically so a concurrent reader never sees a half-written file.
	tmp, err := os.CreateTemp(dir, key+".*.tmp")
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
	if err := os.Rename(tmpName, filepath.Join(dir, key+".json")); err != nil {
		os.Remove(tmpName)
	}
}
