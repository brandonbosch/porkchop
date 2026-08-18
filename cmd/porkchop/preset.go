package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brandonbosch/porkchop/internal/model"
)

// A preset is a named, stored set of backend settings — the thing that turns
//
//	porkchop -provider openai-compat -model Ornith-1.0-35B-4bit-mlx \
//	         -base-url http://localhost:8888/v1
//
// into `porkchop -preset omlx`. It exists because switching backends is a thing
// reviewers do routinely (a local model at home, Bedrock at work) and retyping
// four flags is how people end up with a shell alias that silently outlives the
// setting it was written for.
//
// It is called a *preset* and not a "profile" on purpose. This tool already has
// two profiles in play — the ~/.aws named profile $AWS_PROFILE selects, and the
// Bedrock inference profile that *is* the model id — and telling them apart was
// worth its own commit. A third meaning would spend that work.
//
// Deliberately not implemented: repo-local config. porkchop assumes everything
// it sends a model is CUI, so a .porkchop.json a repository could carry would
// let the thing under review choose where its own diff gets sent. That is the
// silent-egress failure internal/model is built to prevent, arriving one layer
// up. Presets are user-level only, and there is no search of the working
// directory or its parents.
type preset struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Region   string `json:"region"`
	BaseURL  string `json:"base_url"`
	// APIKeyEnv names an environment variable to read the key from. The key
	// itself is deliberately not storable here: this file is long-lived,
	// world-readable by default, and routinely pasted into a bug report.
	APIKeyEnv string `json:"api_key_env"`
}

// presetFile is the on-disk shape of the config file.
type presetFile struct {
	// DefaultPreset is used when no -preset flag and no $PORKCHOP_PRESET is
	// given. Empty means "no preset", which leaves the built-in Bedrock default
	// in place — a config file that exists must not, by existing, change where
	// an unadorned `porkchop` sends a diff.
	DefaultPreset string            `json:"default_preset"`
	Presets       map[string]preset `json:"presets"`
}

// configPath is where presets live: $PORKCHOP_CONFIG if set, else the XDG
// location. $PORKCHOP_CONFIG is what makes this testable without touching a
// developer's real config, and is the escape hatch for anyone whose home
// directory is not where their configuration lives.
func configPath() string {
	if p := os.Getenv("PORKCHOP_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "porkchop", "config.json")
}

// loadPresetFile reads the config file. A missing file is not an error — the
// overwhelmingly common case is not having one — but an unreadable or malformed
// one is. Failing soft on a corrupt config would silently drop the reviewer back
// to a different backend than the one they configured, which is precisely the
// class of surprise this package refuses everywhere else.
func loadPresetFile(path string) (*presetFile, error) {
	if path == "" {
		return &presetFile{}, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &presetFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("porkchop: cannot read %s: %w", path, err)
	}
	// A config file another account can edit is a config file another account
	// can point at their own endpoint. Cheap to check, and the check is the
	// difference between "CUI went somewhere else" being possible and not.
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return nil, fmt.Errorf("porkchop: %s is writable by other users (mode %04o): it chooses where diffs are sent, so tighten it with `chmod 600 %s`", path, mode, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("porkchop: cannot read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Strict, because the failure mode of a lenient decode here is a typo like
	// "baseurl" being ignored and the run going to the default backend while
	// looking configured.
	dec.DisallowUnknownFields()
	var pf presetFile
	if err := dec.Decode(&pf); err != nil {
		return nil, fmt.Errorf("porkchop: cannot parse %s: %w", path, err)
	}
	return &pf, nil
}

// resolvePreset picks the preset for this run and returns it as a model.Config
// to be used as the weakest layer of resolution. name is the -preset flag;
// empty falls back to $PORKCHOP_PRESET and then the file's default_preset.
//
// The returned Config is a *fallback*, never an override: it is handed to
// model.ResolveWithDefaults, where an explicit flag and then the environment
// both outrank it, and where every provider's own validation still applies.
func resolvePreset(name string) (model.Config, error) {
	path := configPath()
	pf, err := loadPresetFile(path)
	if err != nil {
		return model.Config{}, err
	}

	// -preset, then $PORKCHOP_PRESET, then the file's own default. All three
	// are equally deliberate — someone typed each of them — so there is no
	// further precedence to apply once a name is in hand.
	name = cmp.Or(name, os.Getenv("PORKCHOP_PRESET"), pf.DefaultPreset)
	if name == "" {
		return model.Config{}, nil
	}

	p, ok := pf.Presets[name]
	if !ok {
		// Naming a preset that is not there must never fall through to a
		// different backend: the reviewer asked for a specific place to send
		// this diff. Only a stale default_preset is silently ignorable, and it
		// is not — it is the same mistake, just written down earlier.
		return model.Config{}, fmt.Errorf("porkchop: no preset %q in %s%s\nwrite a starter config with: porkchop -write-config", name, path, availablePresets(pf))
	}
	cfg := model.Config{
		Provider: p.Provider,
		Model:    p.Model,
		Region:   p.Region,
		BaseURL:  p.BaseURL,
	}
	if p.APIKeyEnv != "" {
		cfg.APIKey = os.Getenv(p.APIKeyEnv)
		if cfg.APIKey == "" {
			return model.Config{}, fmt.Errorf("porkchop: preset %q reads its key from $%s, which is empty or unset", name, p.APIKeyEnv)
		}
	}
	return cfg, nil
}

// availablePresets lists what the file does define, so a typo is one line from
// being fixed rather than one file-open away.
func availablePresets(pf *presetFile) string {
	if len(pf.Presets) == 0 {
		return " (it defines none)"
	}
	names := make([]string, 0, len(pf.Presets))
	for n := range pf.Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return "\ndefined there: " + strings.Join(names, ", ")
}

// starterConfig is what -write-config writes. It is a working file rather than
// a commented template, because JSON has no comments and a file that must be
// edited before it parses is a worse introduction than one that runs.
//
// default_preset is empty on purpose. Writing a config file must not change
// where an unadorned `porkchop` sends a diff; opting in is a separate, typed
// decision.
const starterConfig = `{
  "default_preset": "",
  "presets": {
    "bedrock": {
      "provider": "bedrock",
      "model": "us-gov.anthropic.claude-sonnet-4-5-20250929-v1:0",
      "region": "us-gov-west-1"
    },
    "omlx": {
      "provider": "openai-compat",
      "model": "Ornith-1.0-35B-4bit-mlx",
      "base_url": "http://localhost:8888/v1",
      "api_key_env": "OMLX_API_KEY"
    },
    "ollama": {
      "provider": "openai-compat",
      "model": "qwen3-coder:30b",
      "base_url": "http://localhost:11434/v1"
    },
    "anthropic": {
      "provider": "anthropic",
      "api_key_env": "ANTHROPIC_API_KEY"
    }
  }
}
`

// writeStarterConfig creates the config file, refusing to clobber an existing
// one. Returns the path it wrote.
func writeStarterConfig() (string, error) {
	path := configPath()
	if path == "" {
		return "", errors.New("porkchop: cannot locate a config directory (no $HOME and no $PORKCHOP_CONFIG)")
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("porkchop: %s already exists; edit it rather than overwriting a working configuration", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("porkchop: cannot stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("porkchop: cannot create %s: %w", filepath.Dir(path), err)
	}
	// 0600, matching the permission check in loadPresetFile: this file decides
	// where CUI is sent.
	if err := os.WriteFile(path, []byte(starterConfig), 0o600); err != nil {
		return "", fmt.Errorf("porkchop: cannot write %s: %w", path, err)
	}
	return path, nil
}
