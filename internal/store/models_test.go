package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestUpsertUpstreamAndReplaceModels(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	id, err := s.UpsertUpstream(ctx, "hyper", "https://x.example/v1", "k", 300, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Re-upsert (config reload) must not create a second row.
	id2, err := s.UpsertUpstream(ctx, "hyper", "https://x.example/v1", "k2", 60, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id != id2 {
		t.Fatalf("upsert created second row: %d vs %d", id, id2)
	}

	sync := func(models ...DiscoveredModel) {
		t.Helper()
		if _, err := s.ReplaceModels(ctx, id, models); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		n, err := s.ModelCount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	sync(m("m1"), m("m2"), m("m3"))
	if got := count(); got != 3 {
		t.Fatalf("after first sync: %d models, want 3", got)
	}

	// Metadata update + removal in one pass.
	sync(m("m2", `{"id":"m2","context_window":200000}`), m("m4"))
	if got := count(); got != 2 {
		t.Fatalf("after second sync: %d models, want 2 (m1,m3 removed; m2,m4 kept)", got)
	}

	var meta string
	if err := s.db.QueryRow(`SELECT metadata FROM models WHERE upstream_model_id='m2'`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != `{"id":"m2","context_window":200000}` {
		t.Errorf("metadata = %s, stored verbatim expected", meta)
	}

	// Empty sync clears the upstream's registry.
	sync()
	if got := count(); got != 0 {
		t.Fatalf("after empty sync: %d models, want 0", got)
	}
}

func TestCrossUpstreamCollisionFirstWins(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	first, _ := s.UpsertUpstream(ctx, "first", "https://a.example/v1", "k", 300, 0)
	second, _ := s.UpsertUpstream(ctx, "second", "https://b.example/v1", "k", 300, 1)

	cm := func(upstream, id string) DiscoveredModel {
		return DiscoveredModel{
			UpstreamModelID: id + "-upstream",
			GatewayID:       id,
			DisplayName:     id,
			Metadata:        []byte(`{"id":"` + id + `"}`),
		}
	}

	// First upstream claims "shared/model".
	if _, err := s.ReplaceModels(ctx, first, []DiscoveredModel{cm("first", "shared/model")}); err != nil {
		t.Fatal(err)
	}
	// Second upstream collides — must be skipped.
	skipped, err := s.ReplaceModels(ctx, second, []DiscoveredModel{cm("second", "shared/model")})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}

	var owner string
	if err := s.db.QueryRow(
		`SELECT u.name FROM models m JOIN upstreams u ON u.id = m.upstream_id WHERE m.gateway_id = 'shared/model'`,
	).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "first" {
		t.Fatalf("gateway ID owned by %q, want %q (config order wins)", owner, "first")
	}

	// Now the FIRST upstream drops it — second must be able to claim it on
	// its next sync (delete-stale frees the ID).
	if _, err := s.ReplaceModels(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceModels(ctx, second, []DiscoveredModel{cm("second", "shared/model")}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(
		`SELECT u.name FROM models m JOIN upstreams u ON u.id = m.upstream_id WHERE m.gateway_id = 'shared/model'`,
	).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "second" {
		t.Fatalf("after first dropped it, owner = %q, want second", owner)
	}
}

func m(id string, meta ...string) DiscoveredModel {
	raw := `{"id":"` + id + `"}`
	if len(meta) > 0 {
		raw = meta[0]
	}
	return DiscoveredModel{
		UpstreamModelID: id,
		GatewayID:       id,
		DisplayName:     id,
		Metadata:        []byte(raw),
	}
}

func boolPtr(b bool) *bool { return &b }

// TestDisabledStateSemantics covers how the config-pinned disabled state and
// the admin/UI-toggled state interact across discovery syncs.
func TestDisabledStateSemantics(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	id, err := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	sync := func(models ...DiscoveredModel) {
		t.Helper()
		if _, err := s.ReplaceModels(ctx, id, models); err != nil {
			t.Fatal(err)
		}
	}
	disabled := func(model string) bool {
		t.Helper()
		var d bool
		if err := s.db.QueryRow(`SELECT disabled FROM models WHERE upstream_model_id = ?`, model).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// A config that does not pin the state leaves a new model enabled.
	sync(m("m1"))
	if disabled("m1") {
		t.Fatal("new model should default to enabled")
	}

	// A UI/admin disable survives a sync that does not pin the state.
	if err := s.SetModelDisabled(ctx, mustModelID(t, s, "m1"), true); err != nil {
		t.Fatal(err)
	}
	sync(m("m1"))
	if !disabled("m1") {
		t.Fatal("UI disable should survive a sync with no config pin")
	}

	// An explicit config disable always wins.
	d := m("m1")
	d.Disabled = boolPtr(true)
	sync(d)
	if !disabled("m1") {
		t.Fatal("config disabled=true should disable")
	}

	// An explicit config enable always wins.
	e := m("m1")
	e.Disabled = boolPtr(false)
	sync(e)
	if disabled("m1") {
		t.Fatal("config disabled=false should enable")
	}
}

func mustModelID(t *testing.T, s *Store, upstreamModelID string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM models WHERE upstream_model_id = ?`, upstreamModelID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDisabledModelNotRoutable(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	id, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	sync := func(m DiscoveredModel) {
		t.Helper()
		if _, err := s.ReplaceModels(ctx, id, []DiscoveredModel{m}); err != nil {
			t.Fatal(err)
		}
	}

	sync(m("m1"))
	if _, err := s.RouteForModel(ctx, "m1"); err != nil {
		t.Fatalf("enabled model should route: %v", err)
	}

	if err := s.SetModelDisabled(ctx, mustModelID(t, s, "m1"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RouteForModel(ctx, "m1"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("disabled model route = %v, want ErrModelNotFound", err)
	}

	// SetModelDisabled on an unknown id reports not found.
	if err := s.SetModelDisabled(ctx, 9999, true); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("SetModelDisabled(unknown) = %v, want ErrModelNotFound", err)
	}
}

func TestListModelsReportsDisabled(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	id, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	d := m("m1")
	d.Disabled = boolPtr(true)
	if _, err := s.ReplaceModels(ctx, id, []DiscoveredModel{d, m("m2")}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.GatewayID] = r.Disabled
	}
	if !got["m1"] || got["m2"] {
		t.Errorf("ListModels disabled flags = %v, want m1=true m2=false", got)
	}

	// Export carries the flag too.
	ex, err := s.ListModelsForExport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ex {
		if e.UpstreamModelID == "m1" && !e.Disabled {
			t.Errorf("export disabled flag = false for m1")
		}
	}
}
