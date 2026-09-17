package cost

import (
	"math"
	"testing"
)

const eps = 1e-15

func TestFromMetadata(t *testing.T) {
	r := FromMetadata([]byte(`{"id":"m","name":"m","pricing":{"input":0.16332,"output":0.5444,"cache_create":0,"cache_hit":0.0315752},"context_window":202000}`))
	if r == nil {
		t.Fatal("rates not found")
	}
	if r.Input != 0.16332 || r.Output != 0.5444 || r.CacheHit != 0.0315752 || r.CacheCreate != 0 {
		t.Errorf("rates = %+v", r)
	}

	if FromMetadata([]byte(`{"no":"pricing"}`)) != nil {
		t.Error("rates reported without pricing object")
	}
	if FromMetadata([]byte(`{"pricing":{"cache_hit":0.1}}`)) != nil {
		t.Error("rates reported without input/output")
	}
}

func TestComputeCachedSplit(t *testing.T) {
	// Findings-page rates for glm-5.3-flash.
	r := &Rates{Input: 0.16332, Output: 0.5444, CacheHit: 0.0315752}

	got := Compute(TokenCounts{PromptTokens: 6819, CompletionToken: 3, CachedTokens: 6656}, r)
	if got == nil {
		t.Fatal("cost unknown")
	}
	// uncached 163 * 0.16332 + 6656 * 0.0315752 + 3 * 0.5444, per Mtok
	want := (163*0.16332 + 6656*0.0315752 + 3*0.5444) / 1e6
	if math.Abs(*got-want) > eps {
		t.Errorf("cost = %v, want %v", *got, want)
	}

	// Full miss: everything at input rate.
	full := Compute(TokenCounts{PromptTokens: 1000, CompletionToken: 0, CachedTokens: 0}, r)
	wantFull := 1000 * 0.16332 / 1e6
	if math.Abs(*full-wantFull) > eps {
		t.Errorf("miss cost = %v, want %v", *full, wantFull)
	}

	// Cached exceeding prompt is clamped.
	clamped := Compute(TokenCounts{PromptTokens: 10, CompletionToken: 0, CachedTokens: 9999}, r)
	wantClamped := 10 * 0.0315752 / 1e6
	if math.Abs(*clamped-wantClamped) > eps {
		t.Errorf("clamped cost = %v, want %v", *clamped, wantClamped)
	}
}

func TestComputeUnknownRates(t *testing.T) {
	if got := Compute(TokenCounts{PromptTokens: 10}, nil); got != nil {
		t.Errorf("cost computed without rates: %v", *got)
	}
}

func TestDiverges(t *testing.T) {
	a, b := 1.0, 1.0
	if Diverges(&a, &b, 0.05) {
		t.Error("equal costs reported as divergent")
	}
	c, d := 1.0, 1.1
	if !Diverges(&c, &d, 0.05) {
		t.Error("11% divergence not reported")
	}
	e, f := 1.0, 1.02
	if Diverges(&e, &f, 0.05) {
		t.Error("2% divergence reported")
	}
	var nilFloat *float64
	if Diverges(nilFloat, &a, 0.05) || Diverges(&a, nilFloat, 0.05) {
		t.Error("unknown sides reported as divergent")
	}
}
