// Package registry applies alias rules, explicit model entries and metadata
// overlays to discovered upstream models, producing the gateway-facing
// registry: gateway IDs, display names and merged metadata.
//
// Resolution order:
//
//	gateway ID:   explicit entry alias > first matching alias rule >
//	              {upstream name}/{upstream model ID}
//	display name: explicit entry name > alias rule name template >
//	              upstream-provided "name" field > gateway ID
//	metadata:     upstream JSON, deep-merged with regex-matched overlays
//	             (matched against the gateway ID), then explicit entry
//	              metadata (entry keys always win)
//
// The default ID is namespaced by the discovery source (the upstream's name)
// so IDs are consistent across providers and collision-free without rules.
package registry

import (
	"encoding/json"
	"fmt"

	"github.com/trfdeer/toll/internal/config"
)

// Engine resolves models for one upstream.
type Engine struct {
	upstream *config.Upstream
	entries  map[string]*config.ModelEntry // by upstream model ID
}

// NewEngine builds the resolution engine for an upstream.
func NewEngine(u *config.Upstream) *Engine {
	e := &Engine{upstream: u, entries: make(map[string]*config.ModelEntry, len(u.Models))}
	for i := range u.Models {
		e.entries[u.Models[i].ID] = &u.Models[i]
	}
	return e
}

// Resolved is a model ready for the registry.
type Resolved struct {
	GatewayID   string
	DisplayName string
	Metadata    []byte
	// Disabled is nil when the config does not pin the model's disabled
	// state (see config.ModelEntry.Disabled).
	Disabled *bool
}

// Resolve maps an upstream model (ID + raw metadata JSON) to its
// gateway-facing form.
func (e *Engine) Resolve(upstreamModelID string, rawMetadata []byte) (Resolved, error) {
	// Base metadata: parse upstream JSON so overlays can merge into it.
	meta := map[string]any{}
	if len(rawMetadata) > 0 {
		if err := json.Unmarshal(rawMetadata, &meta); err != nil {
			return Resolved{}, fmt.Errorf("model %q: upstream metadata is not a JSON object: %w", upstreamModelID, err)
		}
	}

	gatewayID, rule := e.resolveGatewayID(upstreamModelID)

	// Display name precedence: entry > rule template > upstream "name" >
	// gateway ID. A non-empty string "name" in upstream metadata counts.
	displayName := gatewayID
	if rule != nil && rule.Name != "" {
		displayName = rule.Regexp().ReplaceAllString(upstreamModelID, rule.Name)
	} else if s, ok := meta["name"].(string); ok && s != "" {
		displayName = s
	}

	// Regex overlays, matched against the gateway ID, deep-merged.
	for i := range e.upstream.Overlays {
		o := &e.upstream.Overlays[i]
		if re := o.Regexp(); re != nil && re.MatchString(gatewayID) {
			deepMerge(meta, o.Metadata)
		}
	}

	// Explicit entry: pins alias, name and metadata above everything else.
	var disabled *bool
	if entry, ok := e.entries[upstreamModelID]; ok {
		if entry.Alias != "" {
			gatewayID = entry.Alias
		}
		if entry.Name != "" {
			displayName = entry.Name
		}
		deepMerge(meta, entry.Metadata)
		disabled = entry.Disabled
	}

	// Keep the rendered name visible in the metadata itself.
	meta["name"] = displayName

	out, err := json.Marshal(meta)
	if err != nil {
		return Resolved{}, fmt.Errorf("model %q: marshal merged metadata: %w", upstreamModelID, err)
	}
	return Resolved{GatewayID: gatewayID, DisplayName: displayName, Metadata: out, Disabled: disabled}, nil
}

func (e *Engine) resolveGatewayID(upstreamModelID string) (string, *config.AliasRule) {
	if entry, ok := e.entries[upstreamModelID]; ok && entry.Alias != "" {
		return entry.Alias, nil
	}
	if rule := e.resolveRule(upstreamModelID); rule != nil && rule.As != "" {
		return rule.Regexp().ReplaceAllString(upstreamModelID, rule.As), rule
	}
	// Default: namespace the upstream's own model ID by the provider it came
	// from, so the ID is consistent across upstreams and cannot collide.
	if e.upstream.Name == "" {
		return upstreamModelID, nil
	}
	return e.upstream.Name + "/" + upstreamModelID, nil
}

func (e *Engine) resolveRule(upstreamModelID string) *config.AliasRule {
	for i := range e.upstream.AliasRules {
		r := &e.upstream.AliasRules[i]
		if re := r.Regexp(); re != nil && re.MatchString(upstreamModelID) {
			return r
		}
	}
	return nil
}

// deepMerge merges src into dst: maps merge recursively, any other value
// replaces. src is never mutated.
func deepMerge(dst map[string]any, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				deepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}
