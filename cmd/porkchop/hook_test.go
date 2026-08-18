package main

import (
	"strings"
	"testing"
)

func TestStripBlock(t *testing.T) {
	block := hookBlock("/usr/local/bin/porkchop")

	t.Run("absent", func(t *testing.T) {
		const other = "#!/bin/sh\necho hi\n"
		rest, found := stripBlock(other)
		if found {
			t.Error("reported a block in a hook that has none")
		}
		if rest != other {
			t.Errorf("modified a hook with no block:\n%q", rest)
		}
	})

	t.Run("only", func(t *testing.T) {
		rest, found := stripBlock("#!/bin/sh\n\n" + block + "\n")
		if !found {
			t.Fatal("did not find the block")
		}
		if !isEmptyScript(rest) {
			t.Errorf("left content behind:\n%q", rest)
		}
	})

	t.Run("keeps surrounding content", func(t *testing.T) {
		before := "#!/bin/sh\necho before\n"
		after := "echo after\n"
		rest, found := stripBlock(before + "\n" + block + "\n" + after)
		if !found {
			t.Fatal("did not find the block")
		}
		if strings.Contains(rest, "porkchop") {
			t.Errorf("porkchop content survived:\n%q", rest)
		}
		for _, want := range []string{"echo before", "echo after"} {
			if !strings.Contains(rest, want) {
				t.Errorf("removed someone else's %q:\n%q", want, rest)
			}
		}
	})

	// A hand-edited hook that lost its end marker must still be cleanable, or the
	// only way out is editing the file by hand — which is what went wrong already.
	t.Run("unterminated", func(t *testing.T) {
		truncated := strings.ReplaceAll(block, hookEnd, "")
		rest, found := stripBlock("#!/bin/sh\necho before\n\n" + truncated)
		if !found {
			t.Fatal("did not find the block")
		}
		if strings.Contains(rest, "porkchop_warm_cache") {
			t.Errorf("porkchop content survived:\n%q", rest)
		}
		if !strings.Contains(rest, "echo before") {
			t.Errorf("removed content ahead of the block:\n%q", rest)
		}
	})
}

// TestComposeHookIsIdempotent is what makes re-running install safe: two installs
// must leave exactly one block, and the shebang must not accumulate either.
func TestComposeHookIsIdempotent(t *testing.T) {
	block := hookBlock("/usr/local/bin/porkchop")
	first := composeHook("", block)
	body, found := stripBlock(first)
	if !found {
		t.Fatal("composed hook has no block")
	}
	second := composeHook(body, block)
	if second != first {
		t.Errorf("install is not idempotent:\nfirst:\n%q\nsecond:\n%q", first, second)
	}
	if got := strings.Count(second, hookBegin); got != 1 {
		t.Errorf("hook has %d begin markers, want 1", got)
	}
	if got := strings.Count(second, "#!"); got != 1 {
		t.Errorf("hook has %d shebangs, want 1", got)
	}
	if !strings.HasPrefix(second, "#!/bin/sh") {
		t.Errorf("hook does not open with a shebang:\n%q", second)
	}
}

// TestComposeHookKeepsTheExistingShebang matters because a team's hook may name a
// specific shell; replacing it with /bin/sh could change how their own lines behave.
func TestComposeHookKeepsTheExistingShebang(t *testing.T) {
	got := composeHook("#!/bin/bash", hookBlock("/x/porkchop"))
	if !strings.HasPrefix(got, "#!/bin/bash\n") {
		t.Errorf("lost the original shebang:\n%q", got)
	}
	if strings.Contains(got, "#!/bin/sh") {
		t.Errorf("added a second shebang:\n%q", got)
	}
}

func TestLooksLikeShell(t *testing.T) {
	shells := []string{
		"#!/bin/sh\n", "#!/bin/bash\n", "#!/usr/bin/env bash\n",
		"#!/bin/sh -e\n", "#!/usr/bin/env zsh\n", "#!/bin/dash\n",
	}
	for _, s := range shells {
		if !looksLikeShell(s) {
			t.Errorf("%q not recognized as a shell script", strings.TrimSpace(s))
		}
	}
	notShells := []string{
		"#!/usr/bin/python3\n", "#!/usr/bin/env python3\n", "#!/usr/bin/perl\n",
		"echo no shebang\n", "", "#!/usr/bin/env ruby\n",
	}
	for _, s := range notShells {
		if looksLikeShell(s) {
			t.Errorf("%q wrongly recognized as a shell script", strings.TrimSpace(s))
		}
	}
}

func TestIsEmptyScript(t *testing.T) {
	empty := []string{"", "\n\n", "#!/bin/sh", "#!/bin/sh\n\n  \n"}
	for _, s := range empty {
		if !isEmptyScript(s) {
			t.Errorf("%q should count as empty", s)
		}
	}
	notEmpty := []string{"echo hi", "#!/bin/sh\necho hi", "# a comment someone wrote"}
	for _, s := range notEmpty {
		if isEmptyScript(s) {
			t.Errorf("%q should not count as empty", s)
		}
	}
}

// TestShellQuote guards the one injection route into the generated hook: the path
// of the binary, which porkchop does not choose and which may contain anything a
// filesystem allows.
func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"/usr/local/bin/porkchop":    `'/usr/local/bin/porkchop'`,
		"/home/o'brien/bin/porkchop": `'/home/o'\''brien/bin/porkchop'`,
		"/tmp/a b/porkchop":          `'/tmp/a b/porkchop'`,
		"/tmp/$(rm -rf ~)/pc":        `'/tmp/$(rm -rf ~)/pc'`,
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestHookBlockShape pins the three properties the block is claimed to have, so a
// later edit that quietly drops one is caught here rather than by a reviewer whose
// commits have started hanging.
func TestHookBlockShape(t *testing.T) {
	block := hookBlock("/usr/local/bin/porkchop")
	checks := map[string]string{
		"runs process on HEAD":     "process HEAD",
		"detaches the model call":  "&",
		"closes stdin":             "</dev/null",
		"redirects both streams":   "2>&1",
		"cannot fail the commit":   "|| true",
		"falls back to PATH":       "command -v porkchop",
		"prefers the baked binary": "'/usr/local/bin/porkchop'",
		"bounds the log":           "porkchop-process.log",
	}
	for what, want := range checks {
		if !strings.Contains(block, want) {
			t.Errorf("block no longer %s (missing %q)", what, want)
		}
	}
	if !strings.HasPrefix(block, hookBegin) || !strings.HasSuffix(block, hookEnd) {
		t.Error("block is not delimited by its markers")
	}
}
