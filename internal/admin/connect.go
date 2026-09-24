// The ConnectRPC surface of the admin API (MIGRATION.md). The connect handler
// is mounted alongside the legacy REST mux; each resource cuts over
// independently and everything not yet migrated answers CodeUnimplemented via
// the embedded UnimplementedAdminServiceHandler. Only the virtual-keys pilot
// is implemented here.
package admin

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

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
