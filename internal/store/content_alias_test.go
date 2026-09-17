package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptContentLivesInContentDB(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	upID, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := s.CreateVirtualKey(ctx, "app", "hash", KeyFilter{}, KeyFilter{})
	s.EnsureConversation(ctx, "conv", keyID)
	id, err := s.CreateTranscript(ctx, "conv", "m", "m-up", `{"messages":[{"role":"user","content":"s3cret"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTranscript(ctx, id, `{"choices":[{"message":{"content":"hi"}}]}`, 200,
		UsageEvent{KeyID: keyID, UpstreamID: upID}); err != nil {
		t.Fatal(err)
	}

	tr, err := s.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.RequestJSON, "s3cret") {
		t.Errorf("request body not merged back: %q", tr.RequestJSON)
	}
	if !strings.Contains(tr.ResponseJSON, `"hi"`) {
		t.Errorf("response body not merged back: %q", tr.ResponseJSON)
	}
	if tr.Status != 200 {
		t.Errorf("metadata status = %d, want 200", tr.Status)
	}

	// The main database no longer carries the body columns.
	for _, col := range []string{"request_json", "response_json"} {
		var n int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('transcripts') WHERE name = ?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("transcripts still has a %s column", col)
		}
	}

	// The body lives in the content database.
	var req string
	if err := s.content.QueryRow(
		`SELECT request_json FROM transcript_content WHERE transcript_id = ?`, id).Scan(&req); err != nil {
		t.Fatalf("content row missing: %v", err)
	}
	if !strings.Contains(req, "s3cret") {
		t.Errorf("content row = %q", req)
	}
}

func TestPromptsDisabledKeepsMetadata(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	if err := s.SetSetting(ctx, SettingStorePrompts, "false"); err != nil {
		t.Fatal(err)
	}
	if s.PromptsEnabled() {
		t.Fatal("PromptsEnabled still true after disabling")
	}

	upID, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := s.CreateVirtualKey(ctx, "app", "hash", KeyFilter{}, KeyFilter{})
	s.EnsureConversation(ctx, "conv", keyID)
	id, _ := s.CreateTranscript(ctx, "conv", "m", "m-up", `{"prompt":"hidden"}`)
	s.CompleteTranscript(ctx, id, `{"answer":"hidden"}`, 200, UsageEvent{KeyID: keyID, UpstreamID: upID})

	tr, err := s.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if tr.RequestJSON != "" || tr.ResponseJSON != "" {
		t.Errorf("content stored while disabled: req=%q resp=%q", tr.RequestJSON, tr.ResponseJSON)
	}
	if tr.Status != 200 || tr.CompletedAt == "" {
		t.Errorf("metadata lost: status=%d completed=%q", tr.Status, tr.CompletedAt)
	}
}

func TestModelAliasLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	upID, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	sync := func() {
		if _, err := s.ReplaceModels(ctx, upID, []DiscoveredModel{
			{UpstreamModelID: "glm", GatewayID: "hyper/glm", DisplayName: "GLM", Metadata: []byte(`{}`)},
			{UpstreamModelID: "other", GatewayID: "hyper/other", DisplayName: "Other", Metadata: []byte(`{}`)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	byModel := func() map[string]ModelRow {
		ms, err := s.ListModels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]ModelRow, len(ms))
		for _, m := range ms {
			out[m.UpstreamModelID] = m
		}
		return out
	}
	sync()

	glm := byModel()["glm"]
	if err := s.SetModelAlias(ctx, glm.ID, "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	got := byModel()["glm"]
	if got.GatewayID != "gpt-4o" || got.Alias != "gpt-4o" {
		t.Errorf("after set: gatewayId=%q alias=%q", got.GatewayID, got.Alias)
	}

	// A discovery re-sync must not clobber the alias.
	sync()
	if got := byModel()["glm"]; got.GatewayID != "gpt-4o" {
		t.Errorf("alias lost on re-sync: gatewayId=%q", got.GatewayID)
	}

	other := byModel()["other"]
	if err := s.SetModelAlias(ctx, other.ID, "gpt-4o"); !errors.Is(err, ErrAliasConflict) {
		t.Errorf("conflicting alias err = %v, want ErrAliasConflict", err)
	}

	if err := s.SetModelAlias(ctx, glm.ID, ""); err != nil {
		t.Fatal(err)
	}
	got = byModel()["glm"]
	if got.GatewayID != "hyper/glm" || got.Alias != "" {
		t.Errorf("after clear: gatewayId=%q alias=%q", got.GatewayID, got.Alias)
	}
}

func TestUsageDeletedKeyFilter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	upID, _ := s.UpsertUpstream(ctx, "hyper", "https://x/v1", "k", 300, 0)
	appID, _ := s.CreateVirtualKey(ctx, "app", "h1", KeyFilter{}, KeyFilter{})
	otherID, _ := s.CreateVirtualKey(ctx, "other", "h2", KeyFilter{}, KeyFilter{})
	s.RecordUsage(ctx, UsageEvent{KeyID: appID, UpstreamID: upID, GatewayModel: "m", UpstreamModel: "m"})
	s.RecordUsage(ctx, UsageEvent{KeyID: otherID, UpstreamID: upID, GatewayModel: "m", UpstreamModel: "m"})
	if err := s.DeleteVirtualKey(ctx, "app"); err != nil {
		t.Fatal(err)
	}

	count := func(f UsageFilter) int {
		rows, err := s.UsageSummary(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			n += r.Requests
		}
		return n
	}
	if n := count(UsageFilter{}); n != 2 {
		t.Errorf("no filter = %d, want 2", n)
	}
	if n := count(UsageFilter{Keys: []string{"other"}}); n != 1 {
		t.Errorf("other only = %d, want 1", n)
	}
	if n := count(UsageFilter{Keys: []string{"app"}}); n != 0 {
		t.Errorf("deleted key by name = %d, want 0", n)
	}
	if n := count(UsageFilter{IncludeDeleted: true}); n != 1 {
		t.Errorf("deleted only = %d, want 1", n)
	}
	if n := count(UsageFilter{Keys: []string{"other"}, IncludeDeleted: true}); n != 2 {
		t.Errorf("deleted + other = %d, want 2", n)
	}
}
