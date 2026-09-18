package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnvOnlyUpstream(t *testing.T) {
	t.Setenv("TOLL_UPSTREAM_URL", "https://api.example.com/v1")
	t.Setenv("TOLL_UPSTREAM_API_KEY", "sk-test")

	cfg, err := Load(t.Context(), Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("upstreams = %d, want 1", len(cfg.Upstreams))
	}
	u := cfg.Upstreams[0]
	if u.URL.String() != "https://api.example.com/v1" || u.APIKey != "sk-test" || u.Name != "default" {
		t.Errorf("unexpected upstream: %+v", u)
	}
	if u.Refresh != 5*time.Minute {
		t.Errorf("refresh = %v, want 5m", u.Refresh)
	}
}

// TestNoUpstreamStartsClean verifies an empty configuration is valid: the
// gateway must start without any provider (they can be added later via the
// admin UI).
func TestNoUpstreamStartsClean(t *testing.T) {
	cfg, err := Load(t.Context(), Flags{})
	if err != nil {
		t.Fatalf("Load with no upstreams: %v", err)
	}
	if len(cfg.Upstreams) != 0 {
		t.Fatalf("upstreams = %d, want 0", len(cfg.Upstreams))
	}
}

const fullYAML = `
listen: ":9090"
log_level: debug
data_dir: /tmp/toll-test-data
profiles:
  - name: glm-only
    provider_filter:
      mode: include
      values: [hyper]
    model_filter:
      mode: exclude
      values: [hyper/secret]
upstreams:
  - name: hyper
    url: https://hyper.charm.land/v1
    api_key_env: HYPER_API_KEY
    refresh_interval: 1m
    alias_rules:
      - match: "^(.+)$"
        as: "hyper/$1"
        name: "Hyper $1"
    models:
      - id: some-upstream-model
        alias: custom/alias
        name: Custom Name
        disabled: true
        metadata:
          pricing:
            input: 0.5
    overlays:
      - match: "^custom/"
        metadata:
          capabilities:
            vision: true
`

func TestFileConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toll.yaml")
	if err := os.WriteFile(path, []byte(fullYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	t.Setenv("HYPER_API_KEY", "sk-hyper")
	t.Setenv("TOLL_LISTEN", ":7070") // env must beat the file's :9090

	cfg, err := Load(t.Context(), Flags{})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Listen != ":7070" {
		t.Errorf("listen = %q, want :7070 (env wins over file)", cfg.Listen)
	}
	if cfg.DataDir != "/tmp/toll-test-data" {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}

	if len(cfg.Upstreams) != 1 {
		t.Fatalf("upstreams = %d, want 1", len(cfg.Upstreams))
	}
	u := cfg.Upstreams[0]
	if u.Name != "hyper" || u.APIKey != "sk-hyper" || u.Refresh != time.Minute {
		t.Errorf("unexpected upstream: name=%q key=%q refresh=%v", u.Name, u.APIKey, u.Refresh)
	}
	if len(u.AliasRules) != 1 || u.AliasRules[0].Regexp() == nil {
		t.Errorf("alias rules not compiled")
	} else if got := u.AliasRules[0].Regexp().ReplaceAllString("glm-5.3-flash", u.AliasRules[0].As); got != "hyper/glm-5.3-flash" {
		t.Errorf("alias rule produced %q", got)
	}
	if len(u.Overlays) != 1 || u.Overlays[0].Regexp() == nil {
		t.Errorf("overlays not compiled")
	}
	if len(u.Models) != 1 || u.Models[0].Alias != "custom/alias" {
		t.Errorf("explicit model entry not parsed: %+v", u.Models)
	}
	if u.Models[0].Disabled == nil || !*u.Models[0].Disabled {
		t.Errorf("model entry disabled not parsed: %+v", u.Models[0])
	}

	if len(cfg.Profiles) != 1 {
		t.Fatalf("profiles = %d, want 1: %+v", len(cfg.Profiles), cfg.Profiles)
	}
	p := cfg.Profiles[0]
	if p.Name != "glm-only" ||
		p.ProviderFilter.Mode != "include" || len(p.ProviderFilter.Values) != 1 ||
		p.ProviderFilter.Values[0] != "hyper" ||
		p.ModelFilter.Mode != "exclude" || len(p.ModelFilter.Values) != 1 ||
		p.ModelFilter.Values[0] != "hyper/secret" {
		t.Errorf("profile not parsed: %+v", p)
	}
}

func TestProfileValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"reserved default", "profiles:\n  - name: All\n    provider_filter: {mode: none}\n"},
		{"empty name", "profiles:\n  - name: '  '\n    provider_filter: {mode: none}\n"},
		{"duplicate", "profiles:\n  - name: a\n  - name: a\n"},
		{"bad mode", "profiles:\n  - name: a\n    provider_filter: {mode: sometimes, values: [x]}\n"},
		{"empty include", "profiles:\n  - name: a\n    model_filter: {mode: include, values: []}\n"},
		{"derived with filters", "profiles:\n  - name: child\n    provider_filter: {mode: include, values: [a]}\n    parents: [base]\n  - name: base\n"},
		{"unknown parent", "profiles:\n  - name: child\n    parents: [missing]\n"},
		{"self parent", "profiles:\n  - name: a\n    parents: [a]\n"},
		{"duplicate parent", "profiles:\n  - name: child\n    parents: [base, base]\n  - name: base\n"},
		{"cycle", "profiles:\n  - name: a\n    parents: [b]\n  - name: b\n    parents: [a]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "toll.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TOLL_CONFIG", path)
			if _, err := Load(t.Context(), Flags{}); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestProfileParentsParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toll.yaml")
	yaml := "profiles:\n" +
		"  - name: child\n" +
		"    parents: [base, All]\n" +
		"  - name: base\n" +
		"    provider_filter: {mode: include, values: [a]}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	cfg, err := Load(t.Context(), Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("profiles = %+v", cfg.Profiles)
	}
	child := cfg.Profiles[0]
	if child.Name != "child" || len(child.Parents) != 2 ||
		child.Parents[0] != "base" || child.Parents[1] != DefaultProfileName {
		t.Errorf("child = %+v", child)
	}
}

func TestMissingSecretEnvFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toll.yaml")
	if err := os.WriteFile(path, []byte(fullYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	os.Unsetenv("HYPER_API_KEY")

	_, err := Load(t.Context(), Flags{})
	if err == nil {
		t.Fatal("expected error for missing HYPER_API_KEY")
	}
}

func TestBadRegexFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toll.yaml")
	bad := `
upstreams:
  - name: x
    url: https://x.example/v1
    api_key_env: X_KEY
    alias_rules:
      - match: "([unclosed"
        as: "y/$1"
`
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	t.Setenv("X_KEY", "k")

	if _, err := Load(t.Context(), Flags{}); err == nil {
		t.Fatal("expected error for invalid alias regex")
	}
}

func TestStorePromptsFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toll.yaml")
	if err := os.WriteFile(path, []byte("store_prompts: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	os.Unsetenv("TOLL_STORE_PROMPTS")

	cfg, err := Load(t.Context(), Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorePrompts == nil || *cfg.StorePrompts {
		t.Fatalf("file store_prompts = %v, want false", cfg.StorePrompts)
	}

	// The env var wins over the file.
	t.Setenv("TOLL_STORE_PROMPTS", "true")
	cfg, err = Load(t.Context(), Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorePrompts == nil || !*cfg.StorePrompts {
		t.Errorf("env store_prompts = %v, want true", cfg.StorePrompts)
	}
}

func TestFlagsBeatEverything(t *testing.T) {
	t.Setenv("TOLL_UPSTREAM_URL", "https://api.example.com/v1")
	t.Setenv("TOLL_UPSTREAM_API_KEY", "sk-test")

	cfg, err := Load(t.Context(), Flags{Listen: ":1234"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":1234" {
		t.Errorf("listen = %q, want :1234", cfg.Listen)
	}
}
