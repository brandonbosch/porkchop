package model

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// clearProviderEnv makes a test independent of the developer's own shell, which
// on a real machine has credentials for several of these backends at once.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORKCHOP_PROVIDER", "PORKCHOP_MODEL", "PORKCHOP_BEDROCK_REGION", "PORKCHOP_BASE_URL", "PORKCHOP_API_KEY",
		"MEAT_MODEL", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY",
		// Not cosmetic: a Bedrock API key left in the environment would send
		// the no-fallback tests down the bearer branch, where they would pass
		// without testing anything.
		"AWS_BEARER_TOKEN_BEDROCK",
	} {
		t.Setenv(k, "")
	}
}

func TestResolve_DefaultsToBedrock(t *testing.T) {
	clearProviderEnv(t)
	cfg, err := Resolve(Config{Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The default must be the compliant backend. A default that could reach a
	// public API is the failure this package exists to prevent.
	if cfg.Provider != ProviderBedrock {
		t.Errorf("provider = %q, want %q", cfg.Provider, ProviderBedrock)
	}
}

// TestResolve_BedrockNeedsAnExplicitProfile: fantasy v0.41.1 passes the model id
// through verbatim, so there is no partial id porkchop could complete. Guessing
// a default profile would fail at the far end of a turn with a worse message.
func TestResolve_BedrockNeedsAnExplicitProfile(t *testing.T) {
	clearProviderEnv(t)
	_, err := Resolve(Config{Provider: ProviderBedrock})
	if err == nil {
		t.Fatal("want an error when no model id is given")
	}
	if !strings.Contains(err.Error(), "inference profile") {
		t.Errorf("error = %v, want it to name the inference profile id", err)
	}
}

func TestResolve_ModelPrecedence(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("MEAT_MODEL", "from-meat")
	t.Setenv("PORKCHOP_MODEL", "from-porkchop")

	// PORKCHOP_MODEL wins, so a machine that also runs plain meat can point
	// porkchop at a Bedrock profile without disturbing MEAT_MODEL.
	cfg, err := Resolve(Config{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "from-porkchop" {
		t.Errorf("model = %q, want the PORKCHOP_MODEL value", cfg.Model)
	}

	// An explicit value still beats both.
	cfg, err = Resolve(Config{Model: "explicit"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "explicit" {
		t.Errorf("model = %q, want the explicit value", cfg.Model)
	}

	t.Setenv("PORKCHOP_MODEL", "")
	cfg, err = Resolve(Config{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "from-meat" {
		t.Errorf("model = %q, want the MEAT_MODEL value", cfg.Model)
	}
}

func TestResolve_PublicProvidersHaveDefaults(t *testing.T) {
	clearProviderEnv(t)
	for _, tc := range []struct{ provider, want string }{
		{ProviderAnthropic, "claude-opus-4-8"},
		{ProviderOpenAI, "gpt-5.6-sol"},
	} {
		cfg, err := Resolve(Config{Provider: tc.provider})
		if err != nil {
			t.Fatalf("%s: %v", tc.provider, err)
		}
		if cfg.Model != tc.want {
			t.Errorf("%s model = %q, want %q", tc.provider, cfg.Model, tc.want)
		}
	}
}

func TestResolve_CompatNeedsAnEndpoint(t *testing.T) {
	clearProviderEnv(t)
	if _, err := Resolve(Config{Provider: ProviderCompat, Model: "qwen3"}); err == nil {
		t.Fatal("want an error when no base URL is given")
	}
	cfg, err := Resolve(Config{Provider: ProviderCompat, Model: "qwen3", BaseURL: "http://localhost:11434/v1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("base URL = %q", cfg.BaseURL)
	}
}

func TestResolve_UnknownProvider(t *testing.T) {
	clearProviderEnv(t)
	if _, err := Resolve(Config{Provider: "gemini"}); err == nil {
		t.Fatal("want an error for an unknown provider")
	}
}

// isolateAWS points every AWS discovery mechanism at an empty temp directory,
// so the test sees no credentials however the developer's machine is set up.
func isolateAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE":                        filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE":            filepath.Join(dir, "credentials"),
		"AWS_PROFILE":                            "",
		"AWS_ACCESS_KEY_ID":                      "",
		"AWS_SECRET_ACCESS_KEY":                  "",
		"AWS_SESSION_TOKEN":                      "",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "",
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "",
		"AWS_EC2_METADATA_DISABLED":              "true",
		"AWS_REGION":                             "us-east-1",
	} {
		t.Setenv(k, v)
	}
}

// TestOpen_BedrockWithBrokenCredentialsDoesNotFallBack is the most important
// test in this package.
//
// In fantasy v0.41.1 the Bedrock credential path swallows a configuration error
// and appends no Bedrock option, leaving a plain Anthropic client pointed at
// api.anthropic.com — which then picks up whatever ANTHROPIC_API_KEY the
// machine has set for home use. On a CUI diff that is silent egress that looks
// like success. ANTHROPIC_API_KEY is deliberately set here: the test asserts
// porkchop errors rather than quietly using it.
func TestOpen_BedrockWithBrokenCredentialsDoesNotFallBack(t *testing.T) {
	clearProviderEnv(t)
	isolateAWS(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-this-must-never-be-used")

	cfg := Config{Provider: ProviderBedrock, Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}

	m, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("want an error with no AWS credentials; a usable model here means porkchop fell back to the public API")
	}
	if m != nil {
		t.Error("model is non-nil alongside the error")
	}
	if !strings.Contains(err.Error(), "bedrock") {
		t.Errorf("error = %v, want it to name bedrock", err)
	}
	if strings.Contains(err.Error(), "sk-ant-") {
		t.Errorf("error leaks the API key: %v", err)
	}
}

// TestOpen_BedrockWithAnUnknownProfileFails covers the other broken-credential
// shape: a structural configuration error, which is where LoadDefaultConfig
// itself returns an error rather than deferring to Retrieve.
func TestOpen_BedrockWithAnUnknownProfileFails(t *testing.T) {
	clearProviderEnv(t)
	isolateAWS(t)
	t.Setenv("AWS_PROFILE", "porkchop-no-such-profile")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-this-must-never-be-used")

	if _, err := Open(context.Background(), Config{
		Provider: ProviderBedrock,
		Model:    "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	}); err == nil {
		t.Fatal("want an error for an unknown AWS profile")
	}
}

// TestOpen_BedrockNeedsARegion: without one there is no endpoint to pin to.
func TestOpen_BedrockNeedsARegion(t *testing.T) {
	clearProviderEnv(t)
	isolateAWS(t)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	_, err := Open(context.Background(), Config{
		Provider: ProviderBedrock,
		Model:    "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	})
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("error = %v, want it to ask for a region", err)
	}
}

func TestOpen_PublicProvidersNeedTheirKeys(t *testing.T) {
	clearProviderEnv(t)
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI} {
		if _, err := Open(context.Background(), Config{Provider: provider}); err == nil {
			t.Errorf("%s: want an error with no API key set", provider)
		}
	}
}

func TestOpen_CompatBuildsAModel(t *testing.T) {
	clearProviderEnv(t)
	m, err := Open(context.Background(), Config{
		Provider: ProviderCompat,
		Model:    "qwen3",
		BaseURL:  "http://localhost:11434/v1",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m == nil {
		t.Fatal("model is nil")
	}
}

// TestResolve_BedrockAPIKeyNeedsARegion guards a trap in fantasy that is quiet
// and expensive: bedrockBasicAuthConfig defaults a missing region to commercial
// "us-east-1". A GovCloud key with no region set would therefore aim a CUI
// request at the wrong partition while looking perfectly configured.
func TestResolve_BedrockAPIKeyNeedsARegion(t *testing.T) {
	clearProviderEnv(t)
	isolateAWS(t)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-api-key")

	_, err := Resolve(Config{Provider: ProviderBedrock, Model: "m"})
	if err == nil {
		t.Fatal("want an error: a Bedrock API key carries no region")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error = %v, want it to ask for a region", err)
	}
}

// TestOpen_BedrockAPIKeyBypassesTheCredentialChain: a bearer token replaces the
// AWS credential chain outright, so this must succeed in an environment with no
// SigV4 credentials whatsoever — the environment the no-fallback tests use to
// prove failure.
func TestOpen_BedrockAPIKeyBypassesTheCredentialChain(t *testing.T) {
	clearProviderEnv(t)
	isolateAWS(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-api-key")

	m, err := Open(context.Background(), Config{
		Provider: ProviderBedrock,
		Model:    "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Region:   "us-gov-west-1",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m == nil {
		t.Fatal("model is nil")
	}
}

func TestPinFor(t *testing.T) {
	// GovCloud falls out of the same hostname shape the SDK builds.
	if got, err := pinFor(Config{}, "us-gov-west-1"); err != nil || got != "bedrock-runtime.us-gov-west-1.amazonaws.com" {
		t.Errorf("pinFor = %q, %v", got, err)
	}
	// A FIPS or VPC endpoint the SDK's hardcoded hostname cannot express.
	if got, err := pinFor(Config{BaseURL: "https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com"}, "us-gov-west-1"); err != nil || got != "bedrock-runtime-fips.us-gov-west-1.amazonaws.com" {
		t.Errorf("pinFor with a base URL = %q, %v", got, err)
	}
	// Plaintext would carry CUI in the clear; refuse before anything is built.
	if _, err := pinFor(Config{BaseURL: "http://bedrock-runtime.us-gov-west-1.amazonaws.com"}, "us-gov-west-1"); err == nil {
		t.Error("want an error for a plaintext base URL")
	}
	if _, err := pinFor(Config{BaseURL: "not a url"}, "us-gov-west-1"); err == nil {
		t.Error("want an error for a base URL with no host")
	}
}
