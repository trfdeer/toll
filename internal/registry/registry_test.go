package registry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/trfdeer/toll/internal/config"
)

func mustCompile(t *testing.T, u *config.Upstream) *Engine {
	t.Helper()
	if err := u.Compile(); err != nil {
		t.Fatal(err)
	}
	return NewEngine(u)
}

func upstream(t *testing.T, mutate func(*config.Upstream)) *Engine {
	t.Helper()
	u := &config.Upstream{Name: "test"}
	if mutate != nil {
		mutate(u)
	}
	return mustCompile(t, u)
}

func TestAliasRuleRewritesIDAndName(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.AliasRules = []config.AliasRule{{Match: "^(.+)$", As: "hyper/$1", Name: "Hyper $1"}}
	})

	r, err := e.Resolve("glm-5.3-flash", []byte(`{"id":"glm-5.3-flash","pricing":{"input":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.GatewayID != "hyper/glm-5.3-flash" || r.DisplayName != "Hyper glm-5.3-flash" {
		t.Errorf("id=%q name=%q", r.GatewayID, r.DisplayName)
	}
}

func TestUpstreamNamePreservedOverGatewayID(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.AliasRules = []config.AliasRule{{Match: "^(.+)$", As: "x/$1"}} // no name template
	})

	r, err := e.Resolve("m1", []byte(`{"id":"m1","name":"Upstream Pretty Name"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.DisplayName != "Upstream Pretty Name" {
		t.Errorf("display name = %q, want upstream-provided", r.DisplayName)
	}

	// Without an upstream name, derive from the gateway ID.
	r2, err := e.Resolve("m2", []byte(`{"id":"m2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r2.DisplayName != "x/m2" {
		t.Errorf("display name = %q, want derived gateway ID", r2.DisplayName)
	}
}

func TestUnmatchedModelNamespacedByProvider(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.AliasRules = []config.AliasRule{{Match: "^gpt/", As: "wrapped/$1"}}
	})

	// "claude-x" matches no rule, so the default <provider>/<upstream id> applies.
	r, err := e.Resolve("claude-x", []byte(`{"id":"claude-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.GatewayID != "test/claude-x" {
		t.Errorf("gateway id = %q, want test/claude-x", r.GatewayID)
	}
}

func TestExplicitEntryOverridesRule(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.AliasRules = []config.AliasRule{{Match: "^(.+)$", As: "auto/$1", Name: "Auto $1"}}
		u.Models = []config.ModelEntry{{
			ID:    "glm-5.3-flash",
			Alias: "fast/model",
			Name:  "Fast Model",
		}}
	})

	r, err := e.Resolve("glm-5.3-flash", []byte(`{"id":"glm-5.3-flash"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.GatewayID != "fast/model" || r.DisplayName != "Fast Model" {
		t.Errorf("id=%q name=%q, want explicit entry to win", r.GatewayID, r.DisplayName)
	}
}

func TestOverlayDeepMerge(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.AliasRules = []config.AliasRule{{Match: "^(.+)$", As: "hyper/$1"}}
		u.Overlays = []config.Overlay{
			{Match: "^hyper/.*", Metadata: map[string]any{
				"pricing":        map[string]any{"cache_create": 0.0, "cache_hit": 0.03},
				"context_window": 202000,
			}},
			{Match: "^hyper/other/", Metadata: map[string]any{
				"pricing": map[string]any{"cache_hit": 9.9}, // must NOT apply (no id match)
			}},
		}
	})

	r, err := e.Resolve("glm-5.3-flash", []byte(`{"id":"glm-5.3-flash","pricing":{"input":0.16,"cache_hit":9.9}}`))
	if err != nil {
		t.Fatal(err)
	}

	var meta map[string]any
	if err := json.Unmarshal(r.Metadata, &meta); err != nil {
		t.Fatal(err)
	}

	pricing := meta["pricing"].(map[string]any)
	// Existing key preserved, missing keys injected, conflicting overlay key wins.
	if pricing["input"] != 0.16 {
		t.Errorf("pricing.input = %v, want upstream value preserved", pricing["input"])
	}
	if pricing["cache_create"] != 0.0 {
		t.Errorf("pricing.cache_create = %v, want injected", pricing["cache_create"])
	}
	if pricing["cache_hit"] != 0.03 {
		t.Errorf("pricing.cache_hit = %v, want overlay value", pricing["cache_hit"])
	}
	if meta["context_window"] != float64(202000) {
		t.Errorf("context_window = %v, want 202000", meta["context_window"])
	}
}

func TestOverlayMatchedOnGatewayID(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.Overlays = []config.Overlay{{Match: "^hyper/", Metadata: map[string]any{"tier": "free"}}}
	})

	r, err := e.Resolve("glm", []byte(`{"id":"glm"}`))
	if err != nil {
		t.Fatal(err)
	}
	// No alias rule → gateway ID = "test/glm", so the ^hyper/ overlay must not apply.
	if strings.Contains(string(r.Metadata), `"tier"`) {
		t.Errorf("overlay applied to wrong model: %s", r.Metadata)
	}
}

func TestEntryMetadataWinsOverOverlay(t *testing.T) {
	e := upstream(t, func(u *config.Upstream) {
		u.AliasRules = []config.AliasRule{{Match: "^(.+)$", As: "p/$1"}}
		u.Overlays = []config.Overlay{{Match: "^p/", Metadata: map[string]any{
			"priority": map[string]any{"a": 1, "b": 2},
		}}}
		u.Models = []config.ModelEntry{{
			ID:       "m1",
			Metadata: map[string]any{"priority": map[string]any{"b": 42}},
		}}
	})

	r, err := e.Resolve("m1", []byte(`{"id":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(r.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	prio := meta["priority"].(map[string]any)
	if prio["a"] != float64(1) || prio["b"] != float64(42) {
		t.Errorf("priority = %v, want entry metadata to win", prio)
	}
}

func TestInvalidUpstreamMetadataFails(t *testing.T) {
	e := upstream(t, nil)
	if _, err := e.Resolve("m1", []byte(`not-json`)); err == nil {
		t.Fatal("expected error for non-JSON upstream metadata")
	}
}

func TestEntryDisabledState(t *testing.T) {
	yes, no := true, false
	e := upstream(t, func(u *config.Upstream) {
		u.Models = []config.ModelEntry{
			{ID: "off", Disabled: &yes},
			{ID: "on", Disabled: &no},
			{ID: "unspecified"},
		}
	})

	cases := map[string]*bool{"off": &yes, "on": &no, "unspecified": nil}
	for id, want := range cases {
		r, err := e.Resolve(id, []byte(`{"id":"`+id+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case want == nil && r.Disabled != nil:
			t.Errorf("%s: Disabled = %v, want nil (unspecified)", id, *r.Disabled)
		case want != nil && (r.Disabled == nil || *r.Disabled != *want):
			t.Errorf("%s: Disabled = %v, want %v", id, r.Disabled, *want)
		}
	}
}
