package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
)

// SettingStorePrompts controls whether prompt/response bodies are written to
// the content database. The admin UI and the config file both set it; the DB
// value is authoritative while the process runs.
const SettingStorePrompts = "store_prompts"

// PromptsEnabled reports the cached prompt-storage setting.
func (s *Store) PromptsEnabled() bool { return s.prompts.Load() }

// Setting returns a stored setting's raw value; ok is false when unset.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get setting %q: %w", key, err)
	}
	return v, true, nil
}

// SetSetting upserts a setting and refreshes the in-memory caches.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	s.applySetting(key, value)
	return nil
}

// applySetting updates cached values after a write or at startup.
func (s *Store) applySetting(key, value string) {
	if key == SettingStorePrompts {
		if b, err := strconv.ParseBool(value); err == nil {
			s.prompts.Store(b)
		}
	}
}

// loadBoolSetting reads a boolean setting into dst, keeping def when unset.
func (s *Store) loadBoolSetting(ctx context.Context, key string, def bool, dst *atomic.Bool) error {
	v, ok, err := s.Setting(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		dst.Store(def)
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("setting %q: %w", key, err)
	}
	dst.Store(b)
	return nil
}
