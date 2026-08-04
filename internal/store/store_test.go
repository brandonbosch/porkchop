package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonbosch/porkchop/meat"
)

func TestKeyDeterministicAndInputSensitive(t *testing.T) {
	base := Key("diff-a", "model-x", "rubric-1")
	if base != Key("diff-a", "model-x", "rubric-1") {
		t.Fatal("Key is not deterministic for identical inputs")
	}
	// Every input that shapes the answer must change the key, or a stale result
	// would be served after a diff edit, model switch, or rubric upgrade.
	for name, k := range map[string]string{
		"diff":   Key("diff-b", "model-x", "rubric-1"),
		"model":  Key("diff-a", "model-y", "rubric-1"),
		"rubric": Key("diff-a", "model-x", "rubric-2"),
	} {
		if k == base {
			t.Errorf("changing %s did not change the cache key", name)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := Key("some diff", "model-x", "rubric-1")
	want := &meat.Result{SmartDiff: "+kept line\n", Summary: "a change", InputTokens: 12, OutputTokens: 34}

	Store(dir, key, want)
	got, ok := Load(dir, key)
	if !ok {
		t.Fatal("Load reported a miss right after Store")
	}
	if *got != *want {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
}

func TestLoadMiss(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Load(dir, "nonexistent"); ok {
		t.Error("Load reported a hit for a key that was never stored")
	}
	// A disabled cache (empty dir) is always a miss, never an error.
	if _, ok := Load("", "any"); ok {
		t.Error("Load on a disabled cache reported a hit")
	}
}

func TestLoadCorruptIsMiss(t *testing.T) {
	dir := t.TempDir()
	key := "corrupt"
	if err := os.WriteFile(filepath.Join(dir, key+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Corruption must degrade to a recompute, never crash the caller.
	if _, ok := Load(dir, key); ok {
		t.Error("Load treated corrupt JSON as a hit")
	}
}
