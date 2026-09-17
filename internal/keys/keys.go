// Package keys implements virtual API keys: secure token generation,
// hashing, provider/model filters and store-backed lookup.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Prefix makes gateway keys recognizable in client configs and lets leaked
// keys be identified in scans.
const Prefix = "gw_"

// FilterMode is how a key's provider or model filter treats its value list.
type FilterMode string

const (
	// FilterNone imposes no constraint.
	FilterNone FilterMode = "none"
	// FilterInclude allows only the listed values.
	FilterInclude FilterMode = "include"
	// FilterExclude allows everything except the listed values.
	FilterExclude FilterMode = "exclude"
)

// Filter constrains one dimension (provider or model). Mode none ignores
// Values entirely.
type Filter struct {
	Mode   FilterMode `json:"mode"`
	Values []string   `json:"values"`
}

// VirtualKey is a resolved virtual key from the store.
type VirtualKey struct {
	ID             int64
	Name           string
	ProviderFilter Filter
	ModelFilter    Filter
}

// Generate returns a new plaintext key. It is shown once at creation; only
// its SHA-256 is stored.
func Generate() (plaintext string, hash string, err error) {
	raw := make([]byte, 24)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	plaintext = Prefix + hex.EncodeToString(raw)
	return plaintext, Hash(plaintext), nil
}

// Hash derives the stored form of a plaintext key.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ParseBearer extracts the token from an Authorization header value.
func ParseBearer(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// Allows reports whether a model passes a key's provider and model filters.
//
// provider is where the model was discovered from — the name of the upstream
// that serves it, never parsed out of the model ID. Gating on the discovery
// source keeps the filter correct even when an alias rule rewrites a gateway
// ID so it no longer carries a provider prefix. modelID is the gateway-facing
// ID, matched against the model filter. A filter in mode none imposes no
// constraint.
func Allows(provider string, providerFilter, modelFilter Filter, modelID string) bool {
	return providerFilter.allows(provider) && modelFilter.allows(modelID)
}

// allows reports whether v passes the filter.
func (f Filter) allows(v string) bool {
	switch f.Mode {
	case FilterInclude:
		return contains(f.Values, v)
	case FilterExclude:
		return !contains(f.Values, v)
	default:
		return true
	}
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
