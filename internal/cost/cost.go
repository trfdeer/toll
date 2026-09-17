// Package cost computes per-request cost from the registry's merged pricing
// metadata.
//
// Convention: metadata may carry a "pricing" object with per-Mtok USD rates:
//
//	"pricing": {"input": 0.16332, "output": 0.5444,
//	            "cache_create": 0, "cache_hit": 0.0315752}
//
// Billed as: uncached prompt × input + cached prompt × cache_hit +
// completion × output. The cache_create rate is accepted but not billed —
// the gateway cannot tell whether a given request created cache entries;
// uncached prompt is billed at the input rate.
//
// If any needed rate is missing (no pricing object, or absent keys), the
// cost is reported as unknown (nil) rather than guessed.
package cost

import (
	"encoding/json"
	"math"
)

// Rates are per-Mtok USD rates from merged model metadata.
type Rates struct {
	Input       float64
	Output      float64
	CacheCreate float64
	CacheHit    float64
}

// FromMetadata extracts rates from a model's merged metadata JSON.
// Returns nil when there is no usable pricing object.
func FromMetadata(metadata []byte) *Rates {
	var meta struct {
		Pricing *struct {
			Input       *float64 `json:"input"`
			Output      *float64 `json:"output"`
			CacheCreate *float64 `json:"cache_create"`
			CacheHit    *float64 `json:"cache_hit"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil || meta.Pricing == nil {
		return nil
	}
	p := meta.Pricing
	if p.Input == nil || p.Output == nil {
		return nil
	}
	r := &Rates{Input: *p.Input, Output: *p.Output}
	if p.CacheCreate != nil {
		r.CacheCreate = *p.CacheCreate
	}
	if p.CacheHit != nil {
		r.CacheHit = *p.CacheHit
	}
	return r
}

// TokenCounts is the normalized usage the cost formula consumes.
type TokenCounts struct {
	PromptTokens    int
	CompletionToken int
	CachedTokens    int
}

// Compute returns the request cost in USD, or nil when rates are unknown.
func Compute(t TokenCounts, r *Rates) *float64 {
	if r == nil {
		return nil
	}
	cached := t.CachedTokens
	if cached > t.PromptTokens {
		cached = t.PromptTokens // never bill more cached than we saw
	}
	uncached := t.PromptTokens - cached
	usd := (float64(uncached)*r.Input + float64(cached)*r.CacheHit + float64(t.CompletionToken)*r.Output) / 1e6
	return &usd
}

// Diverges reports whether a computed cost and an upstream-reported cost
// disagree by more than relTol (relative to the larger). Used for the
// cross-check log line; two unknowns never diverge.
func Diverges(computed, upstream *float64, relTol float64) bool {
	if computed == nil || upstream == nil {
		return false
	}
	a, b := *computed, *upstream
	if a == b {
		return false
	}
	larger := math.Max(math.Abs(a), math.Abs(b))
	if larger == 0 {
		return false
	}
	return math.Abs(a-b)/larger > relTol
}
