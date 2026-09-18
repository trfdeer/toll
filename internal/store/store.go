// Package store provides SQLite persistence: migrations, the model registry,
// virtual keys, usage events and conversation transcripts.
//
// The database lives under the configured data dir (TOLL_DATA_DIR, default
// ./data) — the process itself stays stateless, per 12-factor backing
// services.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Store wraps the SQLite handles. Metadata (registry, keys, usage, transcript
// metadata, settings) lives in the main database; prompt/response bodies live
// in a separate content database so they can be excluded from backups or
// turned off entirely.
type Store struct {
	db      *sql.DB
	content *sql.DB
	// prompts caches the "store prompt content" setting so the proxy does not
	// hit the database on every request. SetSetting keeps it in sync.
	prompts atomic.Bool
}

// Open opens (creating if needed) the database at path plus its sibling
// content database (content.db in the same directory), and applies
// migrations to both. WAL mode + busy timeout make concurrent access from the
// proxy and admin UI safe.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if err := migrate(db, migrations); err != nil {
		db.Close()
		return nil, err
	}

	contentPath := filepath.Join(filepath.Dir(path), "content.db")
	content, err := openDB(contentPath)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(content, contentMigrations); err != nil {
		db.Close()
		content.Close()
		return nil, err
	}

	s := &Store{db: db, content: content}
	s.prompts.Store(true)
	if err := s.loadBoolSetting(context.Background(), SettingStorePrompts, true, &s.prompts); err != nil {
		db.Close()
		content.Close()
		return nil, err
	}
	return s, nil
}

// openDB opens one SQLite database with the gateway's pragmas.
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// A single writer is the SQLite reality; serialize writes instead of
	// surfacing SQLITE_BUSY to request handlers.
	db.SetMaxOpenConns(1)
	return db, nil
}

// Close closes both databases.
func (s *Store) Close() error {
	err := s.db.Close()
	if cerr := s.content.Close(); err == nil {
		err = cerr
	}
	return err
}

// DB exposes the raw main handle (admin helpers, tests).
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies all pending migrations in order, recording each in
// schema_migrations.
func migrate(db *sql.DB, migrations []string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for i, m := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.Exec(m); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: record version: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: commit: %w", version, err)
		}
	}
	return nil
}

// migrations holds one SQL script per schema version, applied in order.
var migrations = []string{
	// 1: initial schema — upstreams, model registry, virtual keys,
	// usage events, conversations, transcripts.
	`
CREATE TABLE upstreams (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	base_url TEXT NOT NULL,
	api_key TEXT NOT NULL,
	refresh_seconds INTEGER NOT NULL DEFAULT 300
);

-- The model registry. Discovered models are stored with their upstream
-- metadata JSON verbatim; alias rules and overlays are applied at sync time
-- so this table always reflects what /v1/models serves.
CREATE TABLE models (
	id INTEGER PRIMARY KEY,
	upstream_id INTEGER NOT NULL REFERENCES upstreams(id) ON DELETE CASCADE,
	gateway_id TEXT NOT NULL,
	upstream_model_id TEXT NOT NULL,
	display_name TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '{}',   -- raw JSON from upstream + overlays
	discovered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	UNIQUE (upstream_id, gateway_id),
	UNIQUE (upstream_id, upstream_model_id)
);
CREATE INDEX idx_models_gateway_id ON models(gateway_id);

-- Virtual API keys. The plaintext key is shown once at creation; only the
-- hash is stored. "allowed" is a JSON array of glob patterns.
CREATE TABLE virtual_keys (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	key_hash TEXT NOT NULL UNIQUE,
	allowed TEXT NOT NULL DEFAULT '["*"]',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	revoked_at TEXT
);

-- One row per proxied LLM call that completed with usage.
CREATE TABLE usage_events (
	id INTEGER PRIMARY KEY,
	key_id INTEGER NOT NULL REFERENCES virtual_keys(id),
	upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
	gateway_model TEXT NOT NULL,
	upstream_model TEXT NOT NULL,
	prompt_tokens INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL,              -- computed from merged pricing
	upstream_cost_usd REAL,     -- upstream-reported cost, when present
	raw_usage TEXT,             -- verbatim usage JSON
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX idx_usage_key_time ON usage_events(key_id, created_at);
CREATE INDEX idx_usage_model_time ON usage_events(upstream_id, gateway_model, created_at);

-- Conversations group transcripts. The id is a UUID generated by the
-- gateway; clients can influence grouping via an optional header later.
CREATE TABLE conversations (
	id TEXT PRIMARY KEY,
	key_id INTEGER REFERENCES virtual_keys(id),
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- One row per request/response pair. Streamed responses are assembled into
-- response_json on completion.
CREATE TABLE transcripts (
	id INTEGER PRIMARY KEY,
	conversation_id TEXT NOT NULL REFERENCES conversations(id),
	gateway_model TEXT NOT NULL,
	upstream_model TEXT NOT NULL,
	request_json TEXT NOT NULL,
	response_json TEXT,         -- NULL until the response completes
	status INTEGER,
	prompt_tokens INTEGER,
	completion_tokens INTEGER,
	cached_tokens INTEGER,
	cost_usd REAL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	completed_at TEXT
);
CREATE INDEX idx_transcripts_conversation ON transcripts(conversation_id, created_at);
`,
	// 2: upstream position (config order) for cross-upstream alias
	// collision resolution — when two upstreams produce the same
	// gateway_id, the one earlier in config order wins.
	`
ALTER TABLE upstreams ADD COLUMN position INTEGER NOT NULL DEFAULT 0;
`,
	// 3: keys can be paused (temporarily disabled) as well as revoked, so
	// virtual_keys gains paused_at.
	`
ALTER TABLE virtual_keys ADD COLUMN paused_at TEXT;
`,
	// 4: allow deleting a virtual key while keeping its history. usage_events
	// must persist after the key is gone, so key_id becomes nullable and is
	// cleared (ON DELETE SET NULL) instead of blocking the delete. SQLite
	// cannot alter a NOT NULL/FK in place, so the table is rebuilt.
	`
CREATE TABLE usage_events_v4 (
	id INTEGER PRIMARY KEY,
	key_id INTEGER REFERENCES virtual_keys(id) ON DELETE SET NULL,
	upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
	gateway_model TEXT NOT NULL,
	upstream_model TEXT NOT NULL,
	prompt_tokens INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL,
	upstream_cost_usd REAL,
	raw_usage TEXT,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO usage_events_v4
	SELECT id, key_id, upstream_id, gateway_model, upstream_model,
	       prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens,
	       cost_usd, upstream_cost_usd, raw_usage, created_at
	FROM usage_events;
DROP TABLE usage_events;
ALTER TABLE usage_events_v4 RENAME TO usage_events;
CREATE INDEX idx_usage_key_time ON usage_events(key_id, created_at);
CREATE INDEX idx_usage_model_time ON usage_events(upstream_id, gateway_model, created_at);
`,
	// 5: replace the glob allowlist with provider and model filters. Each
	// key stores two {"mode":"none|include|exclude","values":[...]} objects.
	// Legacy globs migrate best-effort: ["*"]/[] mean no constraint, anything
	// else becomes a model-include list.
	`
ALTER TABLE virtual_keys ADD COLUMN provider_filter TEXT NOT NULL DEFAULT '{"mode":"none","values":[]}';
ALTER TABLE virtual_keys ADD COLUMN model_filter TEXT NOT NULL DEFAULT '{"mode":"none","values":[]}';
UPDATE virtual_keys
	SET model_filter = json_object('mode', 'include', 'values', json(allowed))
	WHERE allowed IS NOT NULL AND allowed NOT IN ('["*"]', '[]');
ALTER TABLE virtual_keys DROP COLUMN allowed;
`,
	// 6: models can be disabled without deleting them. A disabled model
	// stays in the registry (and in config exports) but is hidden from
	// /v1/models and refuses routing.
	`
ALTER TABLE models ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
`,
	// 7: providers gain their own disabled flag and reachability. A disabled
	// provider behaves like a disabled model for all of its models (hidden
	// from /v1/models, no routing). reachable is maintained by the discovery
	// sync: a failed fetch marks the provider unreachable while keeping its
	// last-known models, and the next success clears it. last_error/last_synced_at
	// are surfaced in the admin UI.
	`
ALTER TABLE upstreams ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstreams ADD COLUMN reachable INTEGER NOT NULL DEFAULT 1;
ALTER TABLE upstreams ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE upstreams ADD COLUMN last_synced_at TEXT;
`,
	// 8: prompt/response bodies move out of the main database into the
	// sibling content.db (transcript_content). transcripts keeps only the
	// metadata (timing, status, tokens, cost); the body columns are dropped.
	`
ALTER TABLE transcripts DROP COLUMN request_json;
ALTER TABLE transcripts DROP COLUMN response_json;
`,
	// 9: runtime settings editable from the admin UI (e.g. whether prompt
	// content is stored). A config value seeds the row at startup; the DB is
	// authoritative while the process runs.
	`
CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
	// 10: per-model gateway-ID aliases. A model either uses its computed
	// gateway ID (or the provider-namespaced default) or a custom alias.
	// base_gateway_id keeps the computed ID so clearing an alias can restore
	// it without another discovery pass.
	`
CREATE TABLE model_aliases (
	upstream_id INTEGER NOT NULL REFERENCES upstreams(id) ON DELETE CASCADE,
	upstream_model_id TEXT NOT NULL,
	alias TEXT NOT NULL,
	PRIMARY KEY (upstream_id, upstream_model_id)
);
ALTER TABLE models ADD COLUMN base_gateway_id TEXT NOT NULL DEFAULT '';
UPDATE models SET base_gateway_id = gateway_id WHERE base_gateway_id = '';
`,
	// 11: virtual keys gain reusable provider/model filters. A "profile" is a
	// named filter pair shared by any number of keys; every key references
	// exactly one, and a seeded read-only "All" profile (no constraint) is the
	// default. The old per-key filter columns are dropped: this install starts
	// fresh, so existing keys simply fall back to All.
	`
CREATE TABLE profiles (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	is_default INTEGER NOT NULL DEFAULT 0,
	provider_filter TEXT NOT NULL DEFAULT '{"mode":"none","values":[]}',
	model_filter TEXT NOT NULL DEFAULT '{"mode":"none","values":[]}',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO profiles (id, name, is_default) VALUES (1, 'All', 1);
ALTER TABLE virtual_keys ADD COLUMN profile_id INTEGER NOT NULL DEFAULT 1 REFERENCES profiles(id);
ALTER TABLE virtual_keys DROP COLUMN provider_filter;
ALTER TABLE virtual_keys DROP COLUMN model_filter;
`,
	// 12: derived profiles. A profile may union other profiles (its parents)
	// instead of carrying its own filters: a leaf has filters and no parents,
	// a derived profile has parents and neutral filters. Deletion of a profile
	// used as a parent is refused in code; the cascade is a safety net.
	`
CREATE TABLE profile_parents (
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	parent_id  INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	PRIMARY KEY (profile_id, parent_id),
	CHECK (profile_id <> parent_id)
);
`,
}

// contentMigrations are applied to the content database (content.db), which
// holds only prompt/response bodies. Keeping them separate lets the bodies be
// dropped, archived or disabled without touching the usage history.
var contentMigrations = []string{
	`
CREATE TABLE transcript_content (
	transcript_id INTEGER PRIMARY KEY,
	request_json TEXT NOT NULL DEFAULT '',
	response_json TEXT,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
`,
}

// FormatTime renders t in the canonical form the schema stores (ISO 8601
// UTC, ms precision). Lexicographic comparison of these strings is
// chronological, so they work as SQL range bounds too.
func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Time formats t the way the schema defaults do (ISO 8601 UTC, ms precision).
func Time(t time.Time) driver.Value {
	return FormatTime(t)
}
