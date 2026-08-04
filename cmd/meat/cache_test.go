package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonbosch/porkchop/meat"
)

func TestCacheKeyDependsOnDiffModelAndRubric(t *testing.T) {
	base := cacheKey("diff A", "model-1", "rubric-1")

	if base != cacheKey("diff A", "model-1", "rubric-1") {
		t.Fatal("cacheKey is not deterministic")
	}
	if base == cacheKey("diff B", "model-1", "rubric-1") {
		t.Error("changing the diff did not change the key")
	}
	if base == cacheKey("diff A", "model-2", "rubric-1") {
		t.Error("changing the model did not change the key")
	}
	// Rubric versioning: tuning the rubric (a meat upgrade) must invalidate
	// stale cached abridgements.
	if base == cacheKey("diff A", "model-1", "rubric-2") {
		t.Error("changing the rubric did not change the key")
	}
	// Guard against a naive concatenation collision: ("a","b") vs ("a\x00b","").
	if cacheKey("a", "b", "r") == cacheKey("a\x00b", "", "r") {
		t.Error("model/diff boundary is ambiguous (concatenation collision)")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey("some diff", "m", "r")
	want := &meat.Result{
		SmartDiff:    "+ kept line\n",
		Summary:      "a summary",
		InputTokens:  123,
		OutputTokens: 45,
	}

	if _, ok := cacheLoad(dir, key); ok {
		t.Fatal("unexpected hit on empty cache")
	}
	cacheStore(dir, key, want)
	got, ok := cacheLoad(dir, key)
	if !ok {
		t.Fatal("expected hit after store")
	}
	if *got != *want {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
}

func TestCacheDisabledWithEmptyDir(t *testing.T) {
	key := cacheKey("d", "m", "r")
	// Storing to "" is a no-op and loading from "" always misses.
	cacheStore("", key, &meat.Result{Summary: "x"})
	if _, ok := cacheLoad("", key); ok {
		t.Error("empty dir should disable the cache")
	}
}

func TestCacheMissOnCorruptEntry(t *testing.T) {
	dir := t.TempDir()
	key := cacheKey("d", "m", "r")
	if err := os.WriteFile(filepath.Join(dir, key+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheLoad(dir, key); ok {
		t.Error("corrupt entry should be treated as a miss, not a hit")
	}
}

func TestCacheDirEnvOverride(t *testing.T) {
	t.Setenv("MEAT_CACHE", "/custom/path")
	if got := cacheDir(); got != "/custom/path" {
		t.Errorf("cacheDir() = %q, want /custom/path", got)
	}
	t.Setenv("MEAT_CACHE", "")
	if got := cacheDir(); got != "" {
		t.Errorf("cacheDir() with empty MEAT_CACHE = %q, want \"\" (disabled)", got)
	}
}

// newOpts builds a runOpts wired to a temp cache and buffers, recording how many
// times compute was invoked so tests can assert the model path was/wasn't hit.
func newOpts(t *testing.T, diff, model string, compute func() (*meat.Result, error)) (*runOpts, *int, *bytes.Buffer) {
	t.Helper()
	calls := 0
	var out bytes.Buffer
	o := &runOpts{
		diff:     diff,
		model:    model,
		rubric:   "test-rubric",
		cacheDir: t.TempDir(),
		compute: func(context.Context) (*meat.Result, error) {
			calls++
			return compute()
		},
		render: func(res *meat.Result) { out.WriteString(formatBody(res, "", palette(false))) },
		stderr: &out,
	}
	return o, &calls, &out
}

// TestRunCacheHitSkipsCompute is the core promise: an identical re-run is served
// from cache and NEVER calls compute (which, in production, is the only thing
// that needs LLM credentials/network). This is what makes a re-run instant and
// usable offline.
func TestRunCacheHitSkipsCompute(t *testing.T) {
	const diff, model = "diff --git a/x b/x\n+new\n", "m"
	fresh := &meat.Result{SmartDiff: "+new (kept)\n", Summary: "computed", InputTokens: 7, OutputTokens: 3}
	o, calls, out := newOpts(t, diff, model, func() (*meat.Result, error) { return fresh, nil })

	// First run: miss -> compute once -> store.
	if err := run(context.Background(), *o); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("first run: compute called %d times, want 1", *calls)
	}
	// A fresh compute reports token usage and elapsed wall-clock time.
	if !strings.Contains(out.String(), "tokens in=7 out=3 in ") {
		t.Errorf("fresh run should report tokens and elapsed time:\n%s", out.String())
	}

	// Second run with compute that would FAIL if called: proves the hit path
	// never touches it.
	boom := func() (*meat.Result, error) { return nil, errors.New("compute must not be called on a cache hit") }
	o2 := *o
	o2.compute = func(context.Context) (*meat.Result, error) { *calls++; return boom() }
	out.Reset()
	if err := run(context.Background(), o2); err != nil {
		t.Fatalf("second run errored (compute was called): %v", err)
	}
	if *calls != 1 {
		t.Fatalf("second run called compute; total calls = %d, want 1", *calls)
	}
	if !strings.Contains(out.String(), "computed") {
		t.Errorf("cached summary not printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "meat: cached") {
		t.Errorf("expected cache-hit notice:\n%s", out.String())
	}
}

// TestRunNoCacheBypassesReadButWritesThrough verifies -no-cache recomputes even
// when a (stale) entry exists, and refreshes the entry afterward.
func TestRunNoCacheBypassesReadButWritesThrough(t *testing.T) {
	const diff, model = "d", "m"
	fresh := &meat.Result{Summary: "fresh"}
	o, calls, out := newOpts(t, diff, model, func() (*meat.Result, error) { return fresh, nil })

	// Pre-seed a stale entry.
	cacheStore(o.cacheDir, cacheKey(diff, model, o.rubric), &meat.Result{Summary: "stale"})

	o.noCache = true
	if err := run(context.Background(), *o); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("-no-cache should recompute: compute calls = %d, want 1", *calls)
	}
	if strings.Contains(out.String(), "stale") || !strings.Contains(out.String(), "fresh") {
		t.Errorf("-no-cache served stale result:\n%s", out.String())
	}
	// The write-through should have refreshed the entry.
	if got, ok := cacheLoad(o.cacheDir, cacheKey(diff, model, o.rubric)); !ok || got.Summary != "fresh" {
		t.Errorf("cache not refreshed after -no-cache run: %+v ok=%v", got, ok)
	}
}

// TestRunComputeErrorPropagates ensures a compute failure on a miss surfaces and
// does not write a bogus cache entry.
func TestRunComputeErrorPropagates(t *testing.T) {
	o, _, _ := newOpts(t, "d", "m", func() (*meat.Result, error) { return nil, errors.New("llm down") })
	if err := run(context.Background(), *o); err == nil {
		t.Fatal("expected error from failing compute")
	}
	if _, ok := cacheLoad(o.cacheDir, cacheKey("d", "m", o.rubric)); ok {
		t.Error("failed compute should not populate the cache")
	}
}
