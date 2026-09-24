// The ConnectRPC surface of the admin API (MIGRATION.md). The connect handler
// is mounted alongside the legacy REST mux; each resource cuts over
// independently and everything not yet migrated answers CodeUnimplemented via
// the embedded UnimplementedAdminServiceHandler. Only the virtual-keys pilot
// is implemented here.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/trfdeer/toll/gen/toll/admin/v1"
	adminv1connect "github.com/trfdeer/toll/gen/toll/admin/v1/adminv1connect"
	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
)

// resolveProfile maps an optional profile name to the seeded "All" profile
// when the request omits one.
func (h *handlers) resolveProfile(ctx context.Context, name string) (store.Profile, error) {
	if strings.TrimSpace(name) == "" {
		name = store.DefaultProfileName
	}
	return h.store.ProfileByName(ctx, name)
}

// connectService implements toll.admin.v1.AdminService.
type connectService struct {
	adminv1connect.UnimplementedAdminServiceHandler
	*handlers
}

// connectError maps a failure onto a Connect error using the codes documented
// in admin.proto: store sentinels get their codes, anything else is logged and
// reduced to a generic internal message so store internals never reach the
// client. A nil err returns nil.
func (h *handlers) connectError(err error, fallback string) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrBadListParam):
		return connect.NewError(connect.CodeInvalidArgument, errors.New(err.Error()))
	case errors.Is(err, store.ErrUpstreamNotFound),
		errors.Is(err, store.ErrModelNotFound),
		errors.Is(err, store.ErrProfileNotFound),
		errors.Is(err, store.ErrKeyNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New(err.Error()))
	case errors.Is(err, store.ErrAliasConflict),
		errors.Is(err, store.ErrKeyNameExists):
		return connect.NewError(connect.CodeAlreadyExists, errors.New(err.Error()))
	case errors.Is(err, store.ErrProfileImmutable),
		errors.Is(err, store.ErrProfileInUse),
		errors.Is(err, store.ErrProfileInvalid):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(err.Error()))
	default:
		h.logger.Error("admin rpc failed", "err", err)
		return connect.NewError(connect.CodeInternal, errors.New(fallback))
	}
}

// virtualKeyProto maps a stored key onto its proto shape. The secret hash
// never leaves the store.
func virtualKeyProto(vk store.VirtualKey) *adminv1.VirtualKey {
	return &adminv1.VirtualKey{
		Name:    vk.Name,
		Profile: vk.ProfileName,
		Revoked: vk.Revoked,
		Paused:  vk.Paused,
	}
}

// ListKeys returns a page of keys, including revoked and paused ones.
func (s *connectService) ListKeys(ctx context.Context, req *connect.Request[adminv1.ListKeysRequest]) (*connect.Response[adminv1.ListKeysResponse], error) {
	p, err := listParamsFromProto(req.Msg.GetParams())
	if err != nil {
		return nil, s.connectError(err, "keys unavailable")
	}
	vks, total, err := s.store.ListVirtualKeysPaged(ctx, p)
	if err != nil {
		return nil, s.connectError(err, "keys unavailable")
	}
	out := make([]*adminv1.VirtualKey, 0, len(vks))
	for _, vk := range vks {
		out = append(out, virtualKeyProto(vk))
	}
	return connect.NewResponse(&adminv1.ListKeysResponse{Keys: out, Total: int32(total)}), nil
}

// CreateKey stores a new key and returns it next to the plaintext secret,
// which is shown exactly once.
func (s *connectService) CreateKey(ctx context.Context, req *connect.Request[adminv1.CreateKeyRequest]) (*connect.Response[adminv1.CreateKeyResponse], error) {
	name := strings.TrimSpace(req.Msg.GetName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	prof, err := s.resolveProfile(ctx, req.Msg.GetProfile())
	if err != nil {
		return nil, s.connectError(err, "could not create key")
	}
	plaintext, hash, err := keys.Generate()
	if err != nil {
		return nil, s.connectError(err, "key generation failed")
	}
	if _, err := s.store.CreateVirtualKey(ctx, name, hash, prof.ID); err != nil {
		return nil, s.connectError(err, "could not create key")
	}
	return connect.NewResponse(&adminv1.CreateKeyResponse{
		Key:       &adminv1.VirtualKey{Name: name, Profile: prof.Name},
		Plaintext: plaintext,
	}), nil
}

// UpdateKey edits a key in place. Absent fields are left unchanged — the
// PATCH semantics that replace today's PUT (proto/README.md item 3).
func (s *connectService) UpdateKey(ctx context.Context, req *connect.Request[adminv1.UpdateKeyRequest]) (*connect.Response[adminv1.VirtualKey], error) {
	u := store.VirtualKeyUpdate{}
	if req.Msg.NewName != nil {
		name := strings.TrimSpace(*req.Msg.NewName)
		if name == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("new_name is required"))
		}
		u.Name = &name
	}
	if req.Msg.Profile != nil {
		prof, err := s.resolveProfile(ctx, *req.Msg.Profile)
		if err != nil {
			return nil, s.connectError(err, "could not update key")
		}
		u.ProfileID = &prof.ID
	}
	u.Paused = req.Msg.Paused

	if err := s.store.UpdateVirtualKey(ctx, req.Msg.GetName(), u); err != nil {
		return nil, s.connectError(err, "could not update key")
	}
	// Look the key back up under its (possibly new) name.
	lookup := req.Msg.GetName()
	if u.Name != nil {
		lookup = *u.Name
	}
	vk, err := s.store.VirtualKeyByName(ctx, lookup)
	if err != nil {
		return nil, s.connectError(err, "could not update key")
	}
	return connect.NewResponse(virtualKeyProto(*vk)), nil
}

// RotateKey replaces a key's secret; the new plaintext is shown exactly once
// and the old material stops working immediately.
func (s *connectService) RotateKey(ctx context.Context, req *connect.Request[adminv1.RotateKeyRequest]) (*connect.Response[adminv1.RotateKeyResponse], error) {
	plaintext, err := s.store.RotateVirtualKey(ctx, req.Msg.GetName())
	if err != nil {
		return nil, s.connectError(err, "could not rotate key")
	}
	vk, err := s.store.VirtualKeyByName(ctx, req.Msg.GetName())
	if err != nil {
		return nil, s.connectError(err, "could not rotate key")
	}
	return connect.NewResponse(&adminv1.RotateKeyResponse{
		Key:       virtualKeyProto(*vk),
		Plaintext: plaintext,
	}), nil
}

// RevokeKey permanently disables a key (one-way; not an Update field).
func (s *connectService) RevokeKey(ctx context.Context, req *connect.Request[adminv1.RevokeKeyRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := s.store.RevokeVirtualKey(ctx, req.Msg.GetName()); err != nil {
		return nil, s.connectError(err, "revoke failed")
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// DeleteKey removes a key and detaches its usage history.
func (s *connectService) DeleteKey(ctx context.Context, req *connect.Request[adminv1.DeleteKeyRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := s.store.DeleteVirtualKey(ctx, req.Msg.GetName()); err != nil {
		return nil, s.connectError(err, "delete failed")
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// ---- providers & models ----

// providerProto maps an upstream row onto its proto shape.
func providerProto(u store.UpstreamRow) *adminv1.Provider {
	var synced *timestamppb.Timestamp
	if u.LastSyncedAt != "" {
		if t, err := time.Parse(time.RFC3339, u.LastSyncedAt); err == nil {
			synced = timestamppb.New(t)
		}
	}
	return &adminv1.Provider{
		Name:         u.Name,
		BaseUrl:      u.BaseURL,
		ModelCount:   int32(u.ModelCount),
		Disabled:     u.Disabled,
		Reachable:    u.Reachable,
		LastError:    u.LastError,
		LastSyncedAt: synced,
	}
}

// upstreamByName reads one upstream row back, for responses that must reflect
// the state after a mutation.
func (s *connectService) upstreamByName(ctx context.Context, name string) (store.UpstreamRow, error) {
	ups, err := s.store.ListUpstreams(ctx)
	if err != nil {
		return store.UpstreamRow{}, err
	}
	for _, u := range ups {
		if u.Name == name {
			return u, nil
		}
	}
	return store.UpstreamRow{}, store.ErrUpstreamNotFound
}

// modelProto maps a registry row onto its proto shape. The metadata JSON blob
// re-marshals through google.protobuf.Struct, so integers above 2^53 lose
// precision (documented in models.proto).
func modelProto(m store.ModelRow) (*adminv1.Model, error) {
	meta := &structpb.Struct{}
	if m.Metadata != "" {
		var v map[string]any
		if err := json.Unmarshal([]byte(m.Metadata), &v); err == nil {
			if s, err := structpb.NewStruct(v); err == nil {
				meta = s
			}
		}
	}
	return &adminv1.Model{
		Id:                m.ID,
		Upstream:          m.UpstreamName,
		UpstreamModelId:   m.UpstreamModelID,
		GatewayId:         m.GatewayID,
		DisplayName:       m.DisplayName,
		Alias:             m.Alias,
		Metadata:          meta,
		Disabled:          m.Disabled,
		ProviderDisabled:  m.UpstreamDisabled,
		ProviderReachable: m.UpstreamReachable,
	}, nil
}

// ListProviders returns a page of configured upstreams in registration order.
func (s *connectService) ListProviders(ctx context.Context, req *connect.Request[adminv1.ListProvidersRequest]) (*connect.Response[adminv1.ListProvidersResponse], error) {
	p, err := listParamsFromProto(req.Msg.GetParams())
	if err != nil {
		return nil, s.connectError(err, "registry unavailable")
	}
	ups, total, err := s.store.ListUpstreamsPaged(ctx, p)
	if err != nil {
		return nil, s.connectError(err, "registry unavailable")
	}
	out := make([]*adminv1.Provider, 0, len(ups))
	for _, u := range ups {
		out = append(out, providerProto(u))
	}
	return connect.NewResponse(&adminv1.ListProvidersResponse{Providers: out, Total: int32(total)}), nil
}

// CreateProvider registers an upstream and pulls its model catalog
// best-effort, so the returned provider already reflects the sync outcome
// (reachable/last_synced_at/model_count).
func (s *connectService) CreateProvider(ctx context.Context, req *connect.Request[adminv1.CreateProviderRequest]) (*connect.Response[adminv1.CreateProviderResponse], error) {
	name := strings.TrimSpace(req.Msg.GetName())
	baseURL := strings.TrimSpace(req.Msg.GetBaseUrl())
	if name == "" || baseURL == "" || req.Msg.GetApiKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name, baseURL and apiKey are required"))
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("baseURL must be an http(s) URL"))
	}
	refresh := int(req.Msg.GetRefreshInterval().GetSeconds())
	if refresh <= 0 {
		refresh = 300
	}
	position, err := s.store.NextUpstreamPosition(ctx)
	if err != nil {
		return nil, s.connectError(err, "could not add provider")
	}
	id, err := s.store.UpsertUpstream(ctx, name, baseURL, req.Msg.GetApiKey(), refresh, position)
	if err != nil {
		return nil, s.connectError(err, "could not add provider")
	}

	_, warning := s.syncProvider(ctx, name, baseURL, req.Msg.GetApiKey(), id)
	row, err := s.upstreamByName(ctx, name)
	if err != nil {
		return nil, s.connectError(err, "could not add provider")
	}
	return connect.NewResponse(&adminv1.CreateProviderResponse{
		Provider: providerProto(row),
		Warning:  warning,
	}), nil
}

// UpdateProvider changes a provider's disabled toggle; the base URL and key
// stay immutable (delete and re-create).
func (s *connectService) UpdateProvider(ctx context.Context, req *connect.Request[adminv1.UpdateProviderRequest]) (*connect.Response[adminv1.Provider], error) {
	if req.Msg.Disabled != nil {
		if err := s.store.SetUpstreamDisabled(ctx, req.Msg.GetName(), *req.Msg.Disabled); err != nil {
			return nil, s.connectError(err, "update failed")
		}
	}
	u, err := s.upstreamByName(ctx, req.Msg.GetName())
	if err != nil {
		return nil, s.connectError(err, "update failed")
	}
	return connect.NewResponse(providerProto(u)), nil
}

// DeleteProvider removes an upstream and its models.
func (s *connectService) DeleteProvider(ctx context.Context, req *connect.Request[adminv1.DeleteProviderRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := s.store.DeleteUpstream(ctx, req.Msg.GetName()); err != nil {
		return nil, s.connectError(err, "delete failed")
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// ListModels returns a page of registry entries with provider state.
func (s *connectService) ListModels(ctx context.Context, req *connect.Request[adminv1.ListModelsRequest]) (*connect.Response[adminv1.ListModelsResponse], error) {
	p, err := listParamsFromProto(req.Msg.GetParams())
	if err != nil {
		return nil, s.connectError(err, "models unavailable")
	}
	ms, total, err := s.store.ListModelsPaged(ctx, p)
	if err != nil {
		return nil, s.connectError(err, "models unavailable")
	}
	out := make([]*adminv1.Model, 0, len(ms))
	for _, m := range ms {
		pm, err := modelProto(m)
		if err != nil {
			return nil, s.connectError(err, "models unavailable")
		}
		out = append(out, pm)
	}
	return connect.NewResponse(&adminv1.ListModelsResponse{Models: out, Total: int32(total)}), nil
}

// RefreshModels re-pulls every provider's catalog synchronously, reporting
// per-provider failures without stopping the others.
func (s *connectService) RefreshModels(ctx context.Context, _ *connect.Request[adminv1.RefreshModelsRequest]) (*connect.Response[adminv1.RefreshModelsResponse], error) {
	ups, err := s.store.ListUpstreamsForSync(ctx)
	if err != nil {
		return nil, s.connectError(err, "refresh failed")
	}
	total := 0
	warnings := make([]*adminv1.ProviderWarning, 0)
	s.logger.Info("models refresh: starting", "providers", len(ups))
	for _, u := range ups {
		n, warning := s.syncProvider(ctx, u.Name, u.BaseURL, u.APIKey, u.ID)
		total += n
		if warning != "" {
			warnings = append(warnings, &adminv1.ProviderWarning{Name: u.Name, Error: warning})
		}
	}
	s.logger.Info("models refresh: done", "providers", len(ups), "models", total, "warnings", len(warnings))
	for _, w := range warnings {
		s.logger.Warn("models refresh: provider warning", "warning", w.Name+": "+w.Error)
	}
	return connect.NewResponse(&adminv1.RefreshModelsResponse{
		Providers: int32(len(ups)),
		Models:    int32(total),
		Warnings:  warnings,
	}), nil
}

// UpdateModel changes a registry entry's disabled toggle and custom alias;
// absent fields are left unchanged.
func (s *connectService) UpdateModel(ctx context.Context, req *connect.Request[adminv1.UpdateModelRequest]) (*connect.Response[adminv1.Model], error) {
	if req.Msg.Disabled != nil {
		if err := s.store.SetModelDisabled(ctx, req.Msg.GetId(), *req.Msg.Disabled); err != nil {
			return nil, s.connectError(err, "update failed")
		}
	}
	if req.Msg.Alias != nil {
		if err := s.store.SetModelAlias(ctx, req.Msg.GetId(), *req.Msg.Alias); err != nil {
			return nil, s.connectError(err, "update failed")
		}
	}
	m, err := s.store.ModelByID(ctx, req.Msg.GetId())
	if err != nil {
		return nil, s.connectError(err, "update failed")
	}
	pm, err := modelProto(m)
	if err != nil {
		return nil, s.connectError(err, "update failed")
	}
	return connect.NewResponse(pm), nil
}

// DeleteModel removes one registry entry; the next discovery refresh may
// re-add a model the upstream still reports.
func (s *connectService) DeleteModel(ctx context.Context, req *connect.Request[adminv1.DeleteModelRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := s.store.DeleteModel(ctx, req.Msg.GetId()); err != nil {
		return nil, s.connectError(err, "delete failed")
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// ExportConfig returns the current registry state as a toll.yaml document,
// secrets redacted to api_key_env references. The SPA builds its own download
// Blob; the endpoint no longer streams a Content-Disposition attachment
// (proto/README.md item 10).
func (s *connectService) ExportConfig(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[adminv1.ExportConfigResponse], error) {
	data, err := exportConfig(ctx, s.store)
	if err != nil {
		return nil, s.connectError(err, "config export unavailable")
	}
	return connect.NewResponse(&adminv1.ExportConfigResponse{Yaml: string(data)}), nil
}

// GetSettings returns the admin-editable runtime settings.
func (s *connectService) GetSettings(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[adminv1.Settings], error) {
	return connect.NewResponse(&adminv1.Settings{StorePrompts: s.store.PromptsEnabled()}), nil
}

// UpdateSettings persists the runtime settings; an absent field is left
// unchanged (today's *bool, as an optional proto field).
func (s *connectService) UpdateSettings(ctx context.Context, req *connect.Request[adminv1.UpdateSettingsRequest]) (*connect.Response[adminv1.Settings], error) {
	if req.Msg.StorePrompts != nil {
		if err := s.store.SetSetting(ctx, store.SettingStorePrompts,
			strconv.FormatBool(*req.Msg.StorePrompts)); err != nil {
			return nil, s.connectError(err, "could not save settings")
		}
	}
	return connect.NewResponse(&adminv1.Settings{StorePrompts: s.store.PromptsEnabled()}), nil
}
