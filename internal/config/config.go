// Package config loads gateway configuration following 12-factor:
// the environment is the single source of truth for secrets and deployment
// knobs; a YAML file (if present) provides structured deployment config such
// as upstreams, alias rules and model overlays. Precedence:
//
//	flags > environment (TOLL_*) > config file > defaults
//
// Secrets are never read from the file — upstream entries reference
// environment variables by name via `api_key_env`.
package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"gopkg.in/yaml.v3"
)

// Flags are CLI overrides, applied on top of environment/file config.
type Flags struct {
	Listen string
	Config string
}

// AliasRule rewrites upstream model IDs (and optionally display names) into
// gateway-facing ones. `Match` is an anchored regex against the upstream
// model ID; `As` and `Name` are templates where $1, $2… refer to capture
// groups. Example:
//
//	match: "^(.+)$"
//	as:   "hyper/$1"
//	name: "Hyper $1"
type AliasRule struct {
	Match string `yaml:"match"`
	As    string `yaml:"as"`
	Name  string `yaml:"name"`

	compiled *regexp.Regexp
}

// Regexp returns the compiled match pattern.
func (r *AliasRule) Regexp() *regexp.Regexp { return r.compiled }

// ModelEntry is an explicit, config-defined model: pinned upstream ID with
// optional alias, display name and metadata overlay. Overlays are merged
// into discovered metadata; keys here win.
type ModelEntry struct {
	ID       string         `yaml:"id"`
	Alias    string         `yaml:"alias"`
	Name     string         `yaml:"name"`
	Metadata map[string]any `yaml:"metadata"`

	// Disabled marks the model as unavailable: it is hidden from /v1/models
	// and cannot be routed. Nil leaves the registry's current disabled state
	// untouched, so toggles made in the admin UI survive discovery refreshes;
	// true/false pins the state and always wins on the next sync.
	Disabled *bool `yaml:"disabled"`
}

// Overlay is a regex-scoped metadata patch applied to discovered models.
type Overlay struct {
	Match    string         `yaml:"match"`
	Metadata map[string]any `yaml:"metadata"`

	compiled *regexp.Regexp
}

// Regexp returns the compiled match pattern.
func (o *Overlay) Regexp() *regexp.Regexp { return o.compiled }

// DefaultProfileName is the seeded, read-only profile every virtual key falls
// back to. It is reserved: a config may not redefine it.
const DefaultProfileName = "All"

// ProfileFilter is one dimension of a profile's rule set: a mode plus the
// values it includes or excludes. Mode "" or "none" imposes no constraint.
type ProfileFilter struct {
	Mode   string   `yaml:"mode"`
	Values []string `yaml:"values"`
}

// Profile is a named, reusable provider/model filter. Config profiles seed the
// store's profiles table at startup; afterwards the admin UI is authoritative
// (export the config to persist UI edits), mirroring model aliases.
type Profile struct {
	Name           string        `yaml:"name"`
	ProviderFilter ProfileFilter `yaml:"provider_filter"`
	ModelFilter    ProfileFilter `yaml:"model_filter"`
}

// Upstream describes one OpenAI-compatible API backend.
type Upstream struct {
	Name           string        `yaml:"name"`
	URL            *url.URL      `yaml:"url"`
	APIKey         string        `yaml:"-"` // resolved from APIKeyEnv
	APIKeyEnv      string        `yaml:"api_key_env"`
	Refresh        time.Duration `yaml:"refresh_interval"`
	AliasRules     []AliasRule   `yaml:"alias_rules"`
	Models         []ModelEntry  `yaml:"models"`
	Overlays       []Overlay     `yaml:"overlays"`
	DisableRefresh bool          `yaml:"disable_refresh"`

	// Position is the upstream's index in config order; used to resolve
	// cross-upstream alias collisions (first upstream wins).
	Position int
}

// Compile precompiles all alias rule and overlay regexes. Called by the
// loader after parsing; must be invoked before using Regexp() on rules.
func (u *Upstream) Compile() error {
	return compileRules(&u.AliasRules, &u.Overlays)
}

// Timeout bounds a single proxied request, including streaming.
func (u *Upstream) Timeout() time.Duration { return 10 * time.Minute }

// Config is the fully resolved process configuration.
type Config struct {
	Listen          string
	LogLevel        log.Level
	ShutdownTimeout time.Duration
	DataDir         string
	Upstreams       []*Upstream
	// Profiles are config-defined named provider/model filters. They seed the
	// store's profiles table at startup and are written back by the config
	// export, so the exported file round-trips them.
	Profiles []Profile
	// StorePrompts controls whether prompt/response bodies are persisted (to
	// the separate content database). Nil means "leave it to the store's
	// setting" (set from the admin UI); true/false seeds the setting at
	// startup and wins until the next restart.
	StorePrompts *bool

	fileLogLevel string
}

// UpstreamByName returns the upstream with the given name, or nil.
func (c *Config) UpstreamByName(name string) *Upstream {
	for _, u := range c.Upstreams {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// fileConfig mirrors the YAML surface.
type fileConfig struct {
	Listen       string         `yaml:"listen"`
	LogLevel     string         `yaml:"log_level"`
	DataDir      string         `yaml:"data_dir"`
	StorePrompts *bool          `yaml:"store_prompts"`
	Profiles     []Profile      `yaml:"profiles"`
	Upstreams    []yamlUpstream `yaml:"upstreams"`
}

type yamlUpstream struct {
	Name           string        `yaml:"name"`
	URL            string        `yaml:"url"`
	APIKeyEnv      string        `yaml:"api_key_env"`
	Refresh        time.Duration `yaml:"refresh_interval"`
	DisableRefresh bool          `yaml:"disable_refresh"`
	AliasRules     []AliasRule   `yaml:"alias_rules"`
	Models         []ModelEntry  `yaml:"models"`
	Overlays       []Overlay     `yaml:"overlays"`
}

// Load resolves configuration from (1) the config file located via the
// --config flag, the TOLL_CONFIG env var, or ./toll.yaml; (2) the TOLL_*
// environment; (3) CLI flags. It returns an error listing every problem at
// once so a misconfigured deployment fails fast.
func Load(ctx context.Context, flags Flags) (*Config, error) {
	cfg := &Config{
		Listen:          envOr("TOLL_LISTEN", ":8080"),
		ShutdownTimeout: 15 * time.Second,
		DataDir:         envOr("TOLL_DATA_DIR", "./data"),
		LogLevel:        log.InfoLevel,
	}

	path := configPath(flags)
	if path != "" {
		if err := loadFile(path, cfg); err != nil {
			return nil, err
		}
	}

	var errs []error
	switch lvlStr := envOr("TOLL_LOG_LEVEL", ""); {
	case lvlStr != "":
		if lvl, err := log.ParseLevel(lvlStr); err == nil {
			cfg.LogLevel = lvl
		} else {
			errs = append(errs, fmt.Errorf("TOLL_LOG_LEVEL: %w", err))
		}
	case cfg.fileLogLevel != "":
		if lvl, err := log.ParseLevel(cfg.fileLogLevel); err == nil {
			cfg.LogLevel = lvl
		}
	}

	// Prompt storage: the env var overrides the file; otherwise the file (or
	// the DB setting, when neither is present) decides.
	if v := strings.TrimSpace(os.Getenv("TOLL_STORE_PROMPTS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("TOLL_STORE_PROMPTS: %w", err))
		} else {
			cfg.StorePrompts = &b
		}
	}

	if flags.Listen != "" {
		cfg.Listen = flags.Listen
	}

	// Environment-based single upstream, when no file defines any.
	if len(cfg.Upstreams) == 0 {
		if u, err := upstreamFromEnv("default"); err != nil {
			errs = append(errs, err)
		} else if u != nil {
			cfg.Upstreams = append(cfg.Upstreams, u)
		}
	}

	// No upstreams is a valid state: the gateway starts anyway and providers
	// can be added at runtime through the admin UI.

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// DataDirOnly returns a minimal config with just the data dir resolved
// (env, falling back to the config file) — used by management subcommands
// that must run without configured upstreams.
func DataDirOnly() *Config {
	cfg := &Config{
		DataDir:         envOr("TOLL_DATA_DIR", "./data"),
		ShutdownTimeout: 15 * time.Second,
		LogLevel:        log.InfoLevel,
	}
	if path := configPath(Flags{}); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var fc fileConfig
			if yaml.Unmarshal(data, &fc) == nil && fc.DataDir != "" && os.Getenv("TOLL_DATA_DIR") == "" {
				cfg.DataDir = fc.DataDir
			}
		}
	}
	return cfg
}

func configPath(flags Flags) string {
	if flags.Config != "" {
		return flags.Config
	}
	if p := os.Getenv("TOLL_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("toll.yaml"); err == nil {
		return "toll.yaml"
	}
	return ""
}

func loadFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}

	var errs []error
	// Environment beats the file: only apply file values for keys the env
	// did not set.
	if fc.Listen != "" && os.Getenv("TOLL_LISTEN") == "" {
		cfg.Listen = fc.Listen
	}
	cfg.fileLogLevel = fc.LogLevel
	if fc.StorePrompts != nil {
		cfg.StorePrompts = fc.StorePrompts
	}
	if fc.DataDir != "" && os.Getenv("TOLL_DATA_DIR") == "" {
		cfg.DataDir = fc.DataDir
	}

	profiles, err := buildProfiles(fc.Profiles)
	if err != nil {
		errs = append(errs, err)
	} else {
		cfg.Profiles = profiles
	}

	for i, yu := range fc.Upstreams {
		u, err := buildUpstream(yu)
		if err != nil {
			errs = append(errs, fmt.Errorf("upstreams[%d] (%s): %w", i, yu.Name, err))
			continue
		}
		if err := u.Compile(); err != nil {
			errs = append(errs, fmt.Errorf("upstreams[%d] (%s): %w", i, u.Name, err))
			continue
		}
		u.Position = i
		cfg.Upstreams = append(cfg.Upstreams, &u)
	}
	return errors.Join(errs...)
}

// buildProfiles validates config-defined profiles, rejecting the reserved
// default name and duplicates up front so a misconfigured file fails at load
// rather than at seeding.
func buildProfiles(in []Profile) ([]Profile, error) {
	var errs []error
	seen := make(map[string]bool, len(in))
	out := make([]Profile, 0, len(in))
	for i, p := range in {
		p.Name = strings.TrimSpace(p.Name)
		label := fmt.Sprintf("profiles[%d]", i)
		switch {
		case p.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", label))
			continue
		case p.Name == DefaultProfileName:
			errs = append(errs, fmt.Errorf("%s: %q is reserved", label, p.Name))
			continue
		case seen[p.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate profile %q", label, p.Name))
			continue
		}
		seen[p.Name] = true
		label = fmt.Sprintf("profiles[%d] (%s)", i, p.Name)
		if err := validateProfileFilter(p.ProviderFilter); err != nil {
			errs = append(errs, fmt.Errorf("%s: provider_filter: %w", label, err))
		}
		if err := validateProfileFilter(p.ModelFilter); err != nil {
			errs = append(errs, fmt.Errorf("%s: model_filter: %w", label, err))
		}
		p.ProviderFilter = normalizeProfileFilter(p.ProviderFilter)
		p.ModelFilter = normalizeProfileFilter(p.ModelFilter)
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}

// validateProfileFilter mirrors the admin API's filter validation.
func validateProfileFilter(f ProfileFilter) error {
	switch f.Mode {
	case "", "none":
		return nil
	case "include", "exclude":
		if len(f.Values) == 0 {
			return errors.New("mode " + f.Mode + " requires at least one value")
		}
		return nil
	default:
		return errors.New("mode must be none, include or exclude")
	}
}

// normalizeProfileFilter gives a filter a concrete mode and non-nil values.
func normalizeProfileFilter(f ProfileFilter) ProfileFilter {
	switch f.Mode {
	case "include", "exclude":
	default:
		f.Mode = "none"
	}
	if f.Values == nil {
		f.Values = []string{}
	}
	if f.Mode == "none" {
		f.Values = []string{}
	}
	return f
}

func buildUpstream(yu yamlUpstream) (Upstream, error) {
	var errs []error
	u := Upstream{
		Name:           yu.Name,
		APIKeyEnv:      yu.APIKeyEnv,
		Refresh:        yu.Refresh,
		DisableRefresh: yu.DisableRefresh,
		AliasRules:     yu.AliasRules,
		Models:         yu.Models,
		Overlays:       yu.Overlays,
	}

	if yu.URL == "" {
		errs = append(errs, errors.New("url is required"))
	} else {
		parsed, err := url.Parse(yu.URL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("url: %w", err))
		case parsed.Scheme != "http" && parsed.Scheme != "https":
			errs = append(errs, fmt.Errorf("url: scheme must be http or https, got %q", parsed.Scheme))
		default:
			u.URL = parsed
		}
	}

	if u.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}

	if u.Refresh == 0 && !u.DisableRefresh {
		u.Refresh = 5 * time.Minute
	}

	if yu.APIKeyEnv == "" {
		errs = append(errs, errors.New("api_key_env is required (secrets are never read from the config file)"))
	} else {
		u.APIKey = os.Getenv(yu.APIKeyEnv)
		if u.APIKey == "" {
			errs = append(errs, fmt.Errorf("api_key_env: $%s is not set", yu.APIKeyEnv))
		}
	}

	return u, errors.Join(errs...)
}

func compileRules(rules *[]AliasRule, overlays *[]Overlay) error {
	var errs []error
	for i := range *rules {
		r := &(*rules)[i]
		re, err := regexp.Compile(r.Match)
		if err != nil {
			errs = append(errs, fmt.Errorf("alias_rules[%d]: %w", i, err))
			continue
		}
		r.compiled = re
	}
	for i := range *overlays {
		o := &(*overlays)[i]
		re, err := regexp.Compile(o.Match)
		if err != nil {
			errs = append(errs, fmt.Errorf("overlays[%d]: %w", i, err))
			continue
		}
		o.compiled = re
	}
	return errors.Join(errs...)
}

// upstreamFromEnv builds the legacy single-upstream config from
// TOLL_UPSTREAM_URL / TOLL_UPSTREAM_API_KEY. Returns (nil, nil) when unset.
func upstreamFromEnv(name string) (*Upstream, error) {
	rawURL := os.Getenv("TOLL_UPSTREAM_URL")
	apiKey := os.Getenv("TOLL_UPSTREAM_API_KEY")
	if rawURL == "" && apiKey == "" {
		return nil, nil
	}
	yu := yamlUpstream{Name: name, URL: rawURL, APIKeyEnv: "TOLL_UPSTREAM_API_KEY", Refresh: 5 * time.Minute}
	u, err := buildUpstream(yu)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
