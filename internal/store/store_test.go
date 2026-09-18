package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationsRunAndAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toll.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.Close()

	// Reopen — migrations must be a no-op the second time.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	for _, table := range []string{
		"schema_migrations", "upstreams", "models", "virtual_keys", "profiles",
		"usage_events", "conversations", "transcripts",
	} {
		var name string
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}

	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("schema version = %d, want %d", version, len(migrations))
	}
}

func TestDeleteVirtualKeyKeepsUsage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := t.Context()
	upID, err := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := s.CreateVirtualKey(ctx, "app", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, UsageEvent{
		KeyID: keyID, UpstreamID: upID, GatewayModel: "m", UpstreamModel: "m",
		PromptTokens: 1, CompletionToken: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteVirtualKey(ctx, "app"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The key is gone, but its usage event survives with the reference cleared.
	if _, err := s.KeyByHash(ctx, "hash"); err != ErrKeyNotFound {
		t.Errorf("KeyByHash after delete = %v, want ErrKeyNotFound", err)
	}
	events, err := s.UsageEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].KeyID != 0 {
		t.Errorf("usage history not preserved: %+v", events)
	}

	// Summary still reports the orphaned usage, now keyed by model.
	sum, err := s.UsageSummary(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 1 || sum[0].GatewayModel != "m" || sum[0].Requests != 1 {
		t.Errorf("summary after delete = %+v", sum)
	}
}

func TestUpdateVirtualKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	hash := "hash-update"
	if _, err := s.CreateVirtualKey(t.Context(), "before", hash, 1); err != nil {
		t.Fatal(err)
	}

	provider := KeyFilter{Mode: "include", Values: []string{"zeph"}}
	model := KeyFilter{Mode: "exclude", Values: []string{"zeph/secret"}}
	profileID, err := s.CreateProfile(t.Context(), "restricted", provider, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateVirtualKey(t.Context(), "before", "after", profileID); err != nil {
		t.Fatal(err)
	}

	// The key material is unchanged: the same hash still resolves, and the
	// rules come from the profile it now points at.
	vk, err := s.KeyByHash(t.Context(), hash)
	if err != nil {
		t.Fatalf("key hash changed by update: %v", err)
	}
	if vk.Name != "after" || vk.ProfileName != "restricted" || vk.AllowAll ||
		len(vk.Rules) != 1 ||
		vk.Rules[0].ProviderFilter.Mode != "include" ||
		len(vk.Rules[0].ProviderFilter.Values) != 1 || vk.Rules[0].ProviderFilter.Values[0] != "zeph" ||
		vk.Rules[0].ModelFilter.Mode != "exclude" {
		t.Errorf("updated key = %+v", vk)
	}

	// Unknown key.
	if err := s.UpdateVirtualKey(t.Context(), "missing", "x", 1); err != ErrKeyNotFound {
		t.Errorf("update missing = %v, want ErrKeyNotFound", err)
	}
}

func TestPausedKeyInactive(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := t.Context()
	if _, err := s.CreateVirtualKey(ctx, "app", "hash", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseVirtualKey(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeyByHash(ctx, "hash"); err != ErrKeyNotFound {
		t.Errorf("KeyByHash while paused = %v, want ErrKeyNotFound", err)
	}
	if err := s.ResumeVirtualKey(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeyByHash(ctx, "hash"); err != nil {
		t.Errorf("KeyByHash after resume = %v, want nil", err)
	}
}

func TestUsageAndRequestFilters(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	upID, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := s.CreateVirtualKey(ctx, "app", "hash", 1)
	if err := s.RecordUsage(ctx, UsageEvent{
		KeyID: keyID, UpstreamID: upID, GatewayModel: "m", UpstreamModel: "m",
		PromptTokens: 1, CompletionToken: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s.EnsureConversation(ctx, "conv", keyID)
	tid, _ := s.CreateTranscript(ctx, "conv", "m", "m-upstream", `{"model":"m"}`)
	s.CompleteTranscript(ctx, tid, `{"ok":true}`, 200, UsageEvent{KeyID: keyID, UpstreamID: upID})

	// Key filter.
	if got, _ := s.UsageSummary(ctx, UsageFilter{Keys: []string{"app"}}); len(got) != 1 {
		t.Errorf("summary keyed by app = %d rows, want 1", len(got))
	}
	if got, _ := s.UsageSummary(ctx, UsageFilter{Keys: []string{"nope"}}); len(got) != 0 {
		t.Errorf("summary keyed by nope = %d rows, want 0", len(got))
	}

	// Time filter: a lower bound in the future matches nothing.
	future := FormatTime(time.Now().Add(time.Hour))
	if got, _ := s.UsageSummary(ctx, UsageFilter{From: future}); len(got) != 0 {
		t.Errorf("summary from future = %d rows, want 0", len(got))
	}
	past := FormatTime(time.Now().Add(-time.Hour))
	if got, _ := s.UsageSummary(ctx, UsageFilter{From: past}); len(got) != 1 {
		t.Errorf("summary from past = %d rows, want 1", len(got))
	}

	// Requests: filter, total and windowing.
	rows, total, err := s.Requests(ctx, RequestFilter{UsageFilter: UsageFilter{Keys: []string{"app"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || total != 1 {
		t.Errorf("requests = %d/%d, want 1/1", len(rows), total)
	}
	if len(rows) == 1 {
		if rows[0].DurationMS == nil {
			t.Error("durationMs is nil for a completed request")
		} else if *rows[0].DurationMS < 0 {
			t.Errorf("durationMs = %d, want >= 0", *rows[0].DurationMS)
		}
	}
	rows, total, err = s.Requests(ctx, RequestFilter{UsageFilter: UsageFilter{From: future}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || total != 0 {
		t.Errorf("requests from future = %d/%d, want 0/0", len(rows), total)
	}
	rows, total, err = s.Requests(ctx, RequestFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || total != 1 {
		t.Errorf("requests offset = %d/%d, want 0/1", len(rows), total)
	}

	// An in-flight transcript (no completed_at) has no duration.
	s.EnsureConversation(ctx, "conv-2", keyID)
	if _, err := s.CreateTranscript(ctx, "conv-2", "m", "m", `{"model":"m"}`); err != nil {
		t.Fatal(err)
	}
	rows, _, err = s.Requests(ctx, RequestFilter{UsageFilter: UsageFilter{Keys: []string{"app"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].DurationMS != nil {
		t.Errorf("in-flight duration = %v, want nil (rows=%d)", rows[0].DurationMS, len(rows))
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.db.Exec(
		`INSERT INTO usage_events (key_id, upstream_id, gateway_model, upstream_model, prompt_tokens, completion_tokens)
		 VALUES (1, 1, 'm', 'm', 1, 1)`); err == nil {
		t.Fatal("expected FK violation inserting usage without parents")
	}
}

func TestSeedProfiles(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	// An existing profile to be updated by the seed.
	if _, err := s.CreateProfile(ctx, "keep", KeyFilter{}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}

	seeds := []SeedProfile{
		{Name: "zeph", ProviderFilter: KeyFilter{Mode: "include", Values: []string{"zeph"}}},
		{Name: "keep", ModelFilter: KeyFilter{Mode: "exclude", Values: []string{"zeph/secret"}}},
	}
	if err := s.SeedProfiles(ctx, seeds); err != nil {
		t.Fatal(err)
	}

	zeph, err := s.ProfileByName(ctx, "zeph")
	if err != nil {
		t.Fatal(err)
	}
	if zeph.ProviderFilter.Mode != "include" || len(zeph.ProviderFilter.Values) != 1 ||
		zeph.ProviderFilter.Values[0] != "zeph" {
		t.Errorf("seeded profile = %+v", zeph)
	}
	keep, err := s.ProfileByName(ctx, "keep")
	if err != nil {
		t.Fatal(err)
	}
	if keep.ModelFilter.Mode != "exclude" || len(keep.ModelFilter.Values) != 1 {
		t.Errorf("existing profile not updated: %+v", keep.ModelFilter)
	}

	// The reserved default is never modified.
	if err := s.SeedProfiles(ctx, []SeedProfile{{Name: DefaultProfileName}}); err == nil {
		t.Error("seeding the default profile should fail")
	}

	// Profiles the config omits survive untouched.
	if err := s.SeedProfiles(ctx, []SeedProfile{{Name: "later"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfileByName(ctx, "keep"); err != nil {
		t.Errorf("unmentioned profile should survive: %v", err)
	}
}

func TestDerivedProfileResolution(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	// Two leaf profiles.
	if _, err := s.CreateProfile(ctx, "a-only",
		KeyFilter{Mode: "include", Values: []string{"a"}}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProfile(ctx, "b-only",
		KeyFilter{Mode: "include", Values: []string{"b"}}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}

	// A derived profile unions them and carries no filters of its own.
	cID, err := s.CreateProfile(ctx, "a-plus-b", KeyFilter{}, KeyFilter{}, []string{"a-only", "b-only"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ProfileByName(ctx, "a-plus-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Parents) != 2 || c.Parents[0] != "a-only" || c.Parents[1] != "b-only" {
		t.Errorf("derived parents = %v", c.Parents)
	}

	// A key on the derived profile resolves to both leaf rules.
	if _, err := s.CreateVirtualKey(ctx, "app", "hash", cID); err != nil {
		t.Fatal(err)
	}
	vk, err := s.KeyByHash(ctx, "hash")
	if err != nil {
		t.Fatal(err)
	}
	if vk.AllowAll || len(vk.Rules) != 2 {
		t.Fatalf("derived key resolution = allowAll=%v rules=%+v", vk.AllowAll, vk.Rules)
	}
	if vk.Rules[0].ProviderFilter.Values[0] != "a" || vk.Rules[1].ProviderFilter.Values[0] != "b" {
		t.Errorf("resolved rules = %+v", vk.Rules)
	}

	// A derived profile may also nest.
	dID, err := s.CreateProfile(ctx, "nested", KeyFilter{}, KeyFilter{}, []string{"a-plus-b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVirtualKey(ctx, "nested-key", "hash-2", dID); err != nil {
		t.Fatal(err)
	}
	nvk, err := s.KeyByHash(ctx, "hash-2")
	if err != nil {
		t.Fatal(err)
	}
	if nvk.AllowAll || len(nvk.Rules) != 2 {
		t.Fatalf("nested resolution = allowAll=%v rules=%+v", nvk.AllowAll, nvk.Rules)
	}

	// An All ancestor short-circuits to allowAll.
	_, err = s.ProfileByName(ctx, DefaultProfileName)
	if err != nil {
		t.Fatal(err)
	}
	eID, err := s.CreateProfile(ctx, "everything", KeyFilter{}, KeyFilter{}, []string{DefaultProfileName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVirtualKey(ctx, "all-key", "hash-3", eID); err != nil {
		t.Fatal(err)
	}
	avk, err := s.KeyByHash(ctx, "hash-3")
	if err != nil {
		t.Fatal(err)
	}
	if !avk.AllowAll {
		t.Errorf("All ancestor should set allowAll: %+v", avk)
	}
}

func TestDerivedProfileGuards(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	if _, err := s.CreateProfile(ctx, "a",
		KeyFilter{Mode: "include", Values: []string{"a"}}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProfile(ctx, "b",
		KeyFilter{Mode: "include", Values: []string{"b"}}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}

	// Mixed filters and parents is rejected.
	if _, err := s.CreateProfile(ctx, "bad", KeyFilter{Mode: "include", Values: []string{"x"}}, KeyFilter{}, []string{"a"}); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("mixed profile = %v, want ErrProfileInvalid", err)
	}
	// Unknown parent.
	if _, err := s.CreateProfile(ctx, "bad", KeyFilter{}, KeyFilter{}, []string{"nope"}); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("unknown parent = %v, want ErrProfileInvalid", err)
	}
	// Self-parent on update.
	if err := s.UpdateProfile(ctx, "a", "a", KeyFilter{Mode: "include", Values: []string{"a"}}, KeyFilter{}, []string{"a"}); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("self parent = %v, want ErrProfileInvalid", err)
	}
	// Cycle: a -> b then b -> a is refused.
	if err := s.UpdateProfile(ctx, "a", "a", KeyFilter{}, KeyFilter{}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProfile(ctx, "b", "b", KeyFilter{}, KeyFilter{}, []string{"a"}); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("cycle = %v, want ErrProfileInvalid", err)
	}
	// b is now a parent of a; deleting it is refused.
	if err := s.DeleteProfile(ctx, "b"); !errors.Is(err, ErrProfileInUse) {
		t.Errorf("delete parent = %v, want ErrProfileInUse", err)
	}
	// Clear the edge and it deletes.
	if err := s.UpdateProfile(ctx, "a", "a", KeyFilter{}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(ctx, "b"); err != nil {
		t.Errorf("delete after clearing parent = %v", err)
	}
}

func TestProfileRename(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	base, err := s.CreateProfile(ctx, "base",
		KeyFilter{Mode: "include", Values: []string{"a"}}, KeyFilter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProfile(ctx, "child", KeyFilter{}, KeyFilter{}, []string{"base"}); err != nil {
		t.Fatal(err)
	}

	// Renaming a parent keeps the edge (keyed by id) and the child sees the
	// new name.
	if err := s.UpdateProfile(ctx, "base", "base2",
		KeyFilter{Mode: "include", Values: []string{"a"}}, KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}
	child, err := s.ProfileByName(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Parents) != 1 || child.Parents[0] != "base2" {
		t.Errorf("child parents after rename = %v", child.Parents)
	}
	if resolved, err := s.ProfileByName(ctx, "base2"); err != nil || resolved.ID != base {
		t.Errorf("renamed parent = %+v err=%v", resolved, err)
	}

	// Renaming onto an existing name is a client error, not a raw UNIQUE.
	if err := s.UpdateProfile(ctx, "child", "base2", KeyFilter{}, KeyFilter{}, nil); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("rename collision = %v, want ErrProfileInvalid", err)
	}
	// The reserved default name is likewise refused.
	if err := s.UpdateProfile(ctx, "child", DefaultProfileName, KeyFilter{}, KeyFilter{}, nil); !errors.Is(err, ErrProfileInvalid) {
		t.Errorf("rename onto All = %v, want ErrProfileInvalid", err)
	}
}
