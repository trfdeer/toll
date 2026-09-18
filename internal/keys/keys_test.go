package keys

import "testing"

func TestAllows(t *testing.T) {
	none := Filter{Mode: FilterNone}
	inc := func(vals ...string) Filter { return Filter{Mode: FilterInclude, Values: vals} }
	exc := func(vals ...string) Filter { return Filter{Mode: FilterExclude, Values: vals} }

	cases := []struct {
		name        string
		provider    string
		providerFlt Filter
		modelFlt    Filter
		modelID     string
		want        bool
	}{
		{"none/none allows anything", "any", none, none, "any/thing", true},
		{"provider include", "hyper", inc("hyper"), none, "hyper/glm", true},
		{"provider include other", "other", inc("hyper"), none, "other/glm", false},
		{"provider exclude", "hyper", exc("hyper"), none, "hyper/glm", false},
		{"provider exclude other", "other", exc("hyper"), none, "other/glm", true},
		{"model include", "hyper", none, inc("hyper/glm"), "hyper/glm", true},
		{"model include miss", "hyper", none, inc("hyper/glm"), "hyper/other", false},
		{"model exclude", "hyper", none, exc("hyper/glm"), "hyper/other", true},
		{"model exclude hit", "hyper", none, exc("hyper/glm"), "hyper/glm", false},
		{"intersection allows", "hyper", inc("hyper"), exc("hyper/glm"), "hyper/other", true},
		{"intersection model wins", "hyper", inc("hyper"), exc("hyper/glm"), "hyper/glm", false},
		{"intersection provider wins", "other", inc("hyper"), exc("hyper/glm"), "other/other", false},
		// Provider is the discovery source, not the ID prefix: an alias rule
		// may rewrite the gateway ID so it carries no provider prefix at all.
		{"provider from source, not prefix", "zeph", inc("zeph"), none, "llamacpp/gemma", true},
		{"prefix does not grant provider", "zeph", inc("llamacpp"), none, "llamacpp/gemma", false},
		{"bare model id", "hyper", none, inc("glm"), "glm", true},
	}
	for _, tc := range cases {
		rules := []Rule{{ProviderFilter: tc.providerFlt, ModelFilter: tc.modelFlt}}
		if got := Allows(tc.provider, tc.modelID, false, rules); got != tc.want {
			t.Errorf("%s: Allows(%q, %+v, %+v, %q) = %v, want %v",
				tc.name, tc.provider, tc.providerFlt, tc.modelFlt, tc.modelID, got, tc.want)
		}
	}

	// allowAll (an All profile in the ancestry) permits everything.
	if !Allows("any", "any/thing", true, nil) {
		t.Error("allowAll should permit every model")
	}
	// Union: a model passes when any rule passes, even across providers.
	union := []Rule{
		{ProviderFilter: inc("b"), ModelFilter: none},
		{ProviderFilter: inc("a"), ModelFilter: inc("a/one", "a/two")},
	}
	if !Allows("b", "b/anything", false, union) {
		t.Error("second rule should allow provider b")
	}
	if !Allows("a", "a/one", false, union) {
		t.Error("first rule should allow a/one")
	}
	if Allows("a", "a/three", false, union) {
		t.Error("a/three is in neither rule")
	}
}

func TestGenerateHashRoundTrip(t *testing.T) {
	plaintext, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) != len(Prefix)+40 {
		t.Errorf("key length = %d", len(plaintext))
	}
	if Hash(plaintext) != hash {
		t.Error("hash mismatch")
	}
	// Keys must be unique.
	p2, h2, _ := Generate()
	if plaintext == p2 || hash == h2 {
		t.Error("key collision")
	}
}

func TestParseBearer(t *testing.T) {
	if _, ok := ParseBearer("Basic abc"); ok {
		t.Error("non-bearer accepted")
	}
	if _, ok := ParseBearer("Bearer "); ok {
		t.Error("empty token accepted")
	}
	if tok, ok := ParseBearer("Bearer sk-tl-abc"); !ok || tok != "sk-tl-abc" {
		t.Errorf("token = %q ok=%v", tok, ok)
	}
}
