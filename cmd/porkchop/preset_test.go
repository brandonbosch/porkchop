package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonbosch/porkchop/internal/model"
)

// cfgZero is "no preset selected": every field empty, so model.Resolve applies
// the built-in default exactly as it would with no config file at all.
var cfgZero = model.Config{}

// writeConfigFile puts a config file somewhere private and points porkchop at
// it, so no test can be influenced by — or damage — the developer's real one.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PORKCHOP_CONFIG", path)
	return path
}

// clearPresetEnv keeps a developer's own shell out of these tests; several of
// these variables are routinely exported on a machine that runs porkchop.
func clearPresetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORKCHOP_PRESET", "PORKCHOP_PROVIDER", "PORKCHOP_MODEL", "PORKCHOP_BASE_URL",
		"PORKCHOP_BEDROCK_REGION", "PORKCHOP_API_KEY", "MEAT_MODEL",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "AWS_BEARER_TOKEN_BEDROCK",
		"AWS_REGION", "AWS_DEFAULT_REGION",
	} {
		t.Setenv(k, "")
	}
}

const localConfig = `{
  "presets": {
    "omlx": {
      "provider": "openai-compat",
      "model": "Ornith-1.0-35B-4bit-mlx",
      "base_url": "http://localhost:8888/v1",
      "api_key_env": "TEST_OMLX_KEY"
    }
  }
}`

// TestPreset_SuppliesTheWholeBackend is the feature in one test: a preset turns
// four flags into one word.
func TestPreset_SuppliesTheWholeBackend(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, localConfig)
	t.Setenv("TEST_OMLX_KEY", "sk-local")

	cfg, err := resolvePreset("omlx")
	if err != nil {
		t.Fatalf("resolvePreset: %v", err)
	}
	if cfg.Provider != "openai-compat" || cfg.Model != "Ornith-1.0-35B-4bit-mlx" {
		t.Errorf("provider/model = %q/%q", cfg.Provider, cfg.Model)
	}
	if cfg.BaseURL != "http://localhost:8888/v1" {
		t.Errorf("base URL = %q", cfg.BaseURL)
	}
	// The key comes from the named variable, never from the file itself.
	if cfg.APIKey != "sk-local" {
		t.Errorf("api key = %q, want it read from $TEST_OMLX_KEY", cfg.APIKey)
	}
}

// TestPreset_MissingNameIsAnError: falling through to the default backend after
// being asked for a specific one would send a diff somewhere the reviewer did
// not choose. That is the failure this whole area exists to prevent.
func TestPreset_MissingNameIsAnError(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, localConfig)

	_, err := resolvePreset("typo")
	if err == nil {
		t.Fatal("want an error for a preset that is not defined")
	}
	// The error has to be actionable: name what does exist.
	if !strings.Contains(err.Error(), "omlx") {
		t.Errorf("error = %v, want it to list the defined presets", err)
	}
}

// TestPreset_UnknownFieldIsAnError: a lenient decode would let "baseurl" be
// ignored and the run proceed on the default backend while looking configured.
func TestPreset_UnknownFieldIsAnError(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, `{"presets":{"x":{"provider":"openai-compat","baseurl":"http://localhost:8888/v1"}}}`)

	if _, err := resolvePreset("x"); err == nil {
		t.Fatal("want an error for a misspelled field")
	}
}

// TestPreset_WorldWritableIsRefused: a config another account can edit is a
// config another account can aim at their own endpoint.
func TestPreset_WorldWritableIsRefused(t *testing.T) {
	clearPresetEnv(t)
	path := writeConfigFile(t, localConfig)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := resolvePreset("omlx")
	if err == nil {
		t.Fatal("want an error for a world-writable config")
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("error = %v, want it to name the permission problem", err)
	}
}

// TestPreset_NoFileIsNotAnError: not having a config is the common case, and it
// must leave the built-in default untouched.
func TestPreset_NoFileIsNotAnError(t *testing.T) {
	clearPresetEnv(t)
	t.Setenv("PORKCHOP_CONFIG", filepath.Join(t.TempDir(), "absent.json"))

	cfg, err := resolvePreset("")
	if err != nil {
		t.Fatalf("resolvePreset: %v", err)
	}
	if cfg != cfgZero {
		t.Errorf("cfg = %+v, want the zero value", cfg)
	}
}

// TestPreset_EmptyKeyEnvIsAnError: silently sending no credential to a server
// that requires one produces a 401 three minutes into a run, with nothing
// pointing at the cause.
func TestPreset_EmptyKeyEnvIsAnError(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, localConfig)
	t.Setenv("TEST_OMLX_KEY", "")

	_, err := resolvePreset("omlx")
	if err == nil {
		t.Fatal("want an error when the named key variable is unset")
	}
	if !strings.Contains(err.Error(), "TEST_OMLX_KEY") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}

// TestPreset_DefaultPresetApplies covers the file selecting its own default,
// and TestPreset_WritingAConfigChangesNothing below covers the deliberate gap:
// a *starter* file must not.
func TestPreset_DefaultPresetApplies(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, `{
  "default_preset": "local",
  "presets": {"local": {"provider": "openai-compat", "model": "m", "base_url": "http://localhost:1/v1"}}
}`)

	cfg, err := resolvePreset("")
	if err != nil {
		t.Fatalf("resolvePreset: %v", err)
	}
	if cfg.Provider != "openai-compat" {
		t.Errorf("provider = %q, want the file's default_preset to apply", cfg.Provider)
	}
}

// TestPreset_WritingAConfigChangesNothing: the starter file ships with useful
// presets but no default, so running -write-config cannot move where an
// unadorned `porkchop` sends a diff.
func TestPreset_WritingAConfigChangesNothing(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, starterConfig)

	cfg, err := resolvePreset("")
	if err != nil {
		t.Fatalf("resolvePreset: %v", err)
	}
	if cfg != cfgZero {
		t.Errorf("cfg = %+v, want the starter config to select nothing", cfg)
	}
}

// TestWriteStarterConfig_RefusesToClobber protects a working configuration from
// a mistyped flag.
func TestWriteStarterConfig_RefusesToClobber(t *testing.T) {
	clearPresetEnv(t)
	writeConfigFile(t, localConfig)

	if _, err := writeStarterConfig(); err == nil {
		t.Fatal("want an error rather than overwriting an existing config")
	}
}

// TestWriteStarterConfig_RoundTrips: the file it writes must be one the loader
// accepts, including under the strict decode.
func TestWriteStarterConfig_RoundTrips(t *testing.T) {
	clearPresetEnv(t)
	path := filepath.Join(t.TempDir(), "porkchop", "config.json")
	t.Setenv("PORKCHOP_CONFIG", path)

	written, err := writeStarterConfig()
	if err != nil {
		t.Fatalf("writeStarterConfig: %v", err)
	}
	info, err := os.Stat(written)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// It names endpoints; it gets the same permissions the loader demands.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mode = %04o, want no group or other access", perm)
	}
	pf, err := loadPresetFile(written)
	if err != nil {
		t.Fatalf("loadPresetFile: %v", err)
	}
	for _, want := range []string{"bedrock", "omlx", "ollama", "anthropic"} {
		if _, ok := pf.Presets[want]; !ok {
			t.Errorf("starter config is missing the %q preset", want)
		}
	}
}
