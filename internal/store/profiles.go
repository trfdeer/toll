package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Profile errors distinguish the reasons a profile edit or delete can fail.
var (
	// ErrProfileNotFound is returned when no profile matches a name.
	ErrProfileNotFound = errors.New("profile not found")
	// ErrProfileImmutable is returned for the seeded "All" profile, which
	// imposes no constraint and must stay that way: editing it would silently
	// change every key that relies on it.
	ErrProfileImmutable = errors.New("profile is read-only")
	// ErrProfileInUse is returned when deleting a profile that keys still
	// reference. The caller must reassign those keys first.
	ErrProfileInUse = errors.New("profile is in use")
)

// DefaultProfileName is the seeded, read-only profile every key falls back to.
const DefaultProfileName = "All"

// Profile is a named, reusable provider/model filter shared by virtual keys.
// A model is allowed for a key when its provider passes ProviderFilter and the
// model passes ModelFilter, exactly as the old inline key filters worked; the
// difference is that the definition lives on the profile and can be shared.
type Profile struct {
	ID             int64
	Name           string
	ProviderFilter KeyFilter
	ModelFilter    KeyFilter
	// IsDefault marks the seeded, read-only "All" profile.
	IsDefault bool
	// KeyCount is the number of virtual keys referencing this profile.
	KeyCount int
}

// CreateProfile stores a named filter pair. Creates are rejected by the UNIQUE
// constraint when the name is taken.
func (s *Store) CreateProfile(ctx context.Context, name string, provider, model KeyFilter) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO profiles (name, provider_filter, model_filter) VALUES (?, ?, ?)`,
		name, marshalJSON(provider.normalize()), marshalJSON(model.normalize()))
	if err != nil {
		return 0, fmt.Errorf("create profile %q: %w", name, err)
	}
	return res.LastInsertId()
}

// ListProfiles returns every profile with its key count, default first.
func (s *Store) ListProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.provider_filter, p.model_filter, p.is_default,
		       COUNT(vk.id)
		FROM profiles p LEFT JOIN virtual_keys vk ON vk.profile_id = p.id
		GROUP BY p.id
		ORDER BY p.is_default DESC, p.name`)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProfileByName resolves one profile, with its key count.
func (s *Store) ProfileByName(ctx context.Context, name string) (Profile, error) {
	p, err := scanProfile(s.db.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.provider_filter, p.model_filter, p.is_default,
		       (SELECT COUNT(*) FROM virtual_keys WHERE profile_id = p.id)
		FROM profiles p WHERE p.name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrProfileNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("profile %q: %w", name, err)
	}
	return p, nil
}

// scanProfile reads a row selecting id, name, provider_filter, model_filter,
// is_default, key_count.
func scanProfile(row interface{ Scan(...any) error }) (Profile, error) {
	var p Profile
	var provider, model string
	if err := row.Scan(&p.ID, &p.Name, &provider, &model, &p.IsDefault, &p.KeyCount); err != nil {
		return Profile{}, err
	}
	_ = json.Unmarshal([]byte(provider), &p.ProviderFilter)
	_ = json.Unmarshal([]byte(model), &p.ModelFilter)
	p.ProviderFilter = p.ProviderFilter.normalize()
	p.ModelFilter = p.ModelFilter.normalize()
	return p, nil
}

// UpdateProfile edits a profile's name and/or filters in place. The seeded
// default profile is read-only. A rename onto an existing name fails on the
// UNIQUE constraint.
func (s *Store) UpdateProfile(ctx context.Context, currentName, newName string, provider, model KeyFilter) error {
	var isDefault bool
	err := s.db.QueryRowContext(ctx, `SELECT is_default FROM profiles WHERE name = ?`, currentName).Scan(&isDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProfileNotFound
	}
	if err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	if isDefault {
		return ErrProfileImmutable
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE profiles SET name = ?, provider_filter = ?, model_filter = ? WHERE name = ?`,
		newName, marshalJSON(provider.normalize()), marshalJSON(model.normalize()), currentName)
	if err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	if n == 0 {
		return ErrProfileNotFound
	}
	return nil
}

// DeleteProfile removes a profile. The default is read-only, and a profile
// still referenced by any virtual key is refused so a restrictive filter can
// never silently broaden a key's access.
func (s *Store) DeleteProfile(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete profile %q: begin: %w", name, err)
	}
	defer tx.Rollback()

	var id int64
	var isDefault bool
	err = tx.QueryRowContext(ctx, `SELECT id, is_default FROM profiles WHERE name = ?`, name).Scan(&id, &isDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProfileNotFound
	}
	if err != nil {
		return fmt.Errorf("delete profile %q: lookup: %w", name, err)
	}
	if isDefault {
		return ErrProfileImmutable
	}

	var inUse int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM virtual_keys WHERE profile_id = ?`, id).Scan(&inUse); err != nil {
		return fmt.Errorf("delete profile %q: count keys: %w", name, err)
	}
	if inUse > 0 {
		return fmt.Errorf("%w by %d virtual key(s)", ErrProfileInUse, inUse)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM profiles WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete profile %q: %w", name, err)
	}
	return tx.Commit()
}

// SeedProfile is a config-defined profile applied at startup.
type SeedProfile struct {
	Name           string
	ProviderFilter KeyFilter
	ModelFilter    KeyFilter
}

// SeedProfiles applies config-defined profiles at startup. Each is upserted by
// name; profiles the config does not mention are left untouched. Config is only
// the seed — once running, the DB (and the export that mirrors it) is
// authoritative. The reserved default profile is never modified.
func (s *Store) SeedProfiles(ctx context.Context, profiles []SeedProfile) error {
	if len(profiles) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed profiles: begin: %w", err)
	}
	defer tx.Rollback()

	for _, p := range profiles {
		provider := marshalJSON(p.ProviderFilter.normalize())
		model := marshalJSON(p.ModelFilter.normalize())

		var isDefault bool
		err := tx.QueryRowContext(ctx, `SELECT is_default FROM profiles WHERE name = ?`, p.Name).Scan(&isDefault)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO profiles (name, provider_filter, model_filter) VALUES (?, ?, ?)`,
				p.Name, provider, model); err != nil {
				return fmt.Errorf("seed profiles: create %q: %w", p.Name, err)
			}
		case err != nil:
			return fmt.Errorf("seed profiles: lookup %q: %w", p.Name, err)
		case isDefault:
			return fmt.Errorf("seed profiles: %q is the read-only default", p.Name)
		default:
			if _, err := tx.ExecContext(ctx, `
				UPDATE profiles SET provider_filter = ?, model_filter = ? WHERE name = ?`,
				provider, model, p.Name); err != nil {
				return fmt.Errorf("seed profiles: update %q: %w", p.Name, err)
			}
		}
	}
	return tx.Commit()
}
