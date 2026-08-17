package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonbosch/porkchop/meat"
)

// plainP is the empty (no-color) palette; coloredP fakes git-resolved colors
// without shelling out, so colorization logic is tested deterministically.
var coloredP = diffPalette{meta: "\x1b[1m", frag: "\x1b[36m", old: "\x1b[31m", new: "\x1b[32m"}

func TestFormatBodyPlain(t *testing.T) {
	res := &meat.Result{Summary: "did a thing", SmartDiff: "@@ -1 +1 @@\n-old\n+new\n"}
	got := formatBody(res, "", diffPalette{})
	want := "# did a thing\n\n@@ -1 +1 @@\n-old\n+new\n"
	if got != want {
		t.Errorf("formatBody(plain) =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("plain output must contain no ANSI escapes")
	}
}

func TestFormatBodyEmptyDiff(t *testing.T) {
	for _, p := range []diffPalette{{}, coloredP} {
		got := formatBody(&meat.Result{Summary: "s"}, "", p)
		if !strings.Contains(got, "no meaningful change") {
			t.Errorf("palette %+v: empty diff should say so, got %q", p, got)
		}
	}
}

// TestColorizeDiffLineSlots checks each line kind maps to the CORRECT palette
// slot (not merely "some color"), so a wrong-slot regression is caught.
func TestColorizeDiffLineSlots(t *testing.T) {
	cases := []struct {
		line string
		want string // expected prefix escape, or "" for unchanged
	}{
		{" context", ""},
		{"+added", coloredP.new},
		{"-removed", coloredP.old},
		{"@@ -1 +1 @@", coloredP.frag},
		{"diff --git a/x b/x", coloredP.meta},
		{"index abc..def 100644", coloredP.meta},
		{"--- a/x", coloredP.meta}, // file header is meta, not "old"
		{"+++ b/x", coloredP.meta}, // file header is meta, not "new"
		{"new file mode 100644", coloredP.meta},
		{"deleted file mode 100644", coloredP.meta},
		{"old mode 100644", coloredP.meta},
		{"new mode 100755", coloredP.meta},
		{"rename from a", coloredP.meta},
		{"rename to b", coloredP.meta},
		{"copy from a", coloredP.meta},
		{"copy to b", coloredP.meta},
		{"similarity index 90%", coloredP.meta},
		{"dissimilarity index 40%", coloredP.meta},
	}
	for _, c := range cases {
		got := colorizeDiffLine(c.line, coloredP)
		if !strings.Contains(got, c.line) {
			t.Errorf("colorizeDiffLine(%q) dropped text: %q", c.line, got)
		}
		if c.want == "" {
			if got != c.line {
				t.Errorf("colorizeDiffLine(%q) should be unchanged, got %q", c.line, got)
			}
			continue
		}
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("colorizeDiffLine(%q) used wrong slot: %q, want prefix %q", c.line, got, c.want)
		}
		if !strings.HasSuffix(got, ansiReset) {
			t.Errorf("colored line %q must end with a reset", got)
		}
	}
}

// TestPaletteDisabledIsAllPlain ensures palette(false) colors nothing.
func TestPaletteDisabledIsAllPlain(t *testing.T) {
	if palette(false) != (diffPalette{}) {
		t.Error("palette(false) must be the empty palette")
	}
	for _, line := range []string{"+a", "-b", "@@ x @@", "diff --git a b"} {
		if got := colorizeDiffLine(line, diffPalette{}); got != line {
			t.Errorf("empty palette colored %q -> %q", line, got)
		}
	}
}

// TestIsTerminalNonFile: a non-*os.File writer is never a terminal.
func TestIsTerminalNonFile(t *testing.T) {
	if isTerminal(&strings.Builder{}) {
		t.Error("a non-*os.File writer must not be treated as a terminal")
	}
}

// TestIsTerminalRegularFileAndDevNull guards the regression the char-device
// check had: a regular file and /dev/null (a char device!) must both be
// non-terminals, so redirected output stays plain.
func TestIsTerminalRegularFileAndDevNull(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a regular file must not be a terminal")
	}

	dn, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer dn.Close()
	if isTerminal(dn) {
		t.Errorf("%s (a char device) must not be treated as a terminal", os.DevNull)
	}
}

// TestRenderResultToFileIsPlain is a near-end-to-end check of the public entry
// point: rendering to a regular file (non-tty) must be plain text with no ANSI
// and no pager involvement.
func TestRenderResultToFileIsPlain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	res := &meat.Result{Summary: "s", SmartDiff: "@@ x @@\n+a\n-b\n"}
	renderResult(f, res, "kept 2/9 changed lines")
	f.Close()

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "\x1b[") {
		t.Errorf("redirected output must be plain, got escapes:\n%q", got)
	}
	if !strings.Contains(string(got), "+a") || !strings.Contains(string(got), "# s") {
		t.Errorf("redirected output missing content:\n%s", got)
	}
	if !strings.Contains(string(got), "# kept 2/9 changed lines") {
		t.Errorf("redirected output missing elision manifest:\n%s", got)
	}
}

// TestGitWantsColorHonorsConfig checks we defer to git's own color policy:
// color.diff=never disables color even when stdout is a tty, and =always
// enables it even when not. Uses GIT_CONFIG_* env so the real user/repo config
// is untouched.
func TestGitWantsColorHonorsConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "color.diff")

	t.Setenv("GIT_CONFIG_VALUE_0", "never")
	if gitWantsColor(true) {
		t.Error("color.diff=never should disable color even on a tty")
	}

	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	if !gitWantsColor(false) {
		t.Error("color.diff=always should enable color even when not a tty")
	}
}

// TestRenderJSON pins the -json wire form: one JSON object, snake_case keys,
// including the machine-computed elision manifest, no ANSI.
func TestRenderJSON(t *testing.T) {
	var buf bytes.Buffer
	res := &meat.Result{
		SmartDiff:    "@@ x @@\n+a\n",
		Summary:      "did <a> thing",
		InputTokens:  10,
		OutputTokens: 2,
	}
	renderJSON(&buf, res, "kept 1/5 changed lines")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("-json output is not valid JSON: %v\n%s", err, buf.String())
	}
	for k, want := range map[string]any{
		"smart_diff":    "@@ x @@\n+a\n",
		"summary":       "did <a> thing", // SetEscapeHTML(false): '<' not \u003c
		"input_tokens":  float64(10),
		"output_tokens": float64(2),
		"elision":       "kept 1/5 changed lines",
	} {
		if got[k] != want {
			t.Errorf("json[%q] = %v, want %v", k, got[k], want)
		}
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Error("json output must contain no ANSI escapes")
	}
	if strings.Contains(buf.String(), `\u003c`) {
		t.Error("json output should not HTML-escape (SetEscapeHTML(false))")
	}
}
