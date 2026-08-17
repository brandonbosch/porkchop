package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInstalledBinaryName builds the CLI the same way `go install` does and
// asserts the produced executable is named `meat` (not the module path). This
// is the whole point of locating the package under cmd/meat: Go derives the
// binary name from the package's directory, so the directory must be `meat`.
func TestInstalledBinaryName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build in short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not found: %v", err)
	}

	gobin := t.TempDir()
	cmd := exec.Command(goBin, "install", "github.com/brandonbosch/porkchop/cmd/meat")
	cmd.Env = append(os.Environ(), "GOBIN="+gobin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go install failed: %v\n%s", err, out)
	}

	want := "meat" + goExeSuffix()
	if _, err := os.Stat(filepath.Join(gobin, want)); err != nil {
		entries, _ := os.ReadDir(gobin)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected installed binary named %q, GOBIN contains %v", want, names)
	}
}

// goExeSuffix returns the platform executable suffix (".exe" on Windows).
func goExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
