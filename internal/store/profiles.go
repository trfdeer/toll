package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// reference, or that other profiles still inherit from. The caller must
	// reassign them first.
	ErrProfileInUse = errors.New("profile is in use")
	// ErrProfileInvalid is returned for a malformed definition: filters mixed
	// with parents, an unknown parent, or a parent cycle.
	ErrProfileInvalid = errors.New("invalid profile")
)

// DefaultProfileName is the seeded, read-only profile every key falls back to.
const DefaultProfileName = "All"

// Profile is a named, reusable model-access rule shared by virtual keys. A
// profile is exactly one of two kinds:
//
//   - leaf: carries its own provider/model filters and has no parents.
//   - derived: references one or more parents whose resolved sets are unioned,
//     and has no filters of its own.
//
// A model is allowed for a key when it passes any clause in the profile's
// resolved union; an ancestor that is the All profile permits everything.
type Profile struct {
	ID             int64
	Name           string
	ProviderFilter KeyFilter
	ModelFilter    KeyFilter
	// Parents are the names of the profiles this one unions. Empty for a leaf.
	Parents []string
	// IsDefault marks the seeded, read-only "All" profile.
	IsDefault bool
	// KeyCount is the number of virtual keys referencing this profile.
	KeyCount int
	// ChildCount is the number of profiles that list this one as a parent.
	ChildCount int
}

// KeyRule is one clause of a resolved profile: a model passes the rule when
// its provider passes ProviderFilter and its gateway ID passes ModelFilter.
// A resolved profile allows a model when any of its rules allows it.
type KeyRule struct {
	ProviderFilter KeyFilter
	ModelFilter    KeyFilter
}

// rowsQuerier is the subset of *sql.DB / *sql.Tx these helpers need.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CreateProfile stores a profile. A leaf carries its own filters; a derived
// profile lists parents and must have neutral filters. Creates are rejected by
// the UNIQUE constraint when the name is taken.
func (s *Store) CreateProfile(ctx context.Context, name string, provider, model KeyFilter, parents []string) (int64, error) {
	if err := validateProfileShape(provider, model, parents); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("create profile %q: begin: %w", name, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO profiles (name, provider_filter, model_filter) VALUES (?, ?, ?)`,
		name, marshalJSON(provider.normalize()), marshalJSON(model.normalize()))
	if err != nil {
		return 0, fmt.Errorf("create profile %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create profile %q: %w", name, err)
	}
	if err := setParentsTx(ctx, tx, id, parents); err != nil {
		return 0, fmt.Errorf("create profile %q: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("create profile %q: %w", name, err)
	}
	return id, nil
}

// profileFilterCols and profileSortCols map the admin column ids to SQL
// expressions. Only the name is filterable; keyCount is an aggregate, valid in
// ORDER BY under GROUP BY. The provider/model/allowed-model columns are
// resolved in Go and are neither sortable nor filterable server-side.
var (
	profileFilterCols = map[string]string{
		"name": "p.name",
	}
	profileSortCols = map[string]string{
		"id":   "p.id",
		"name": "p.name",
		"keys": "COUNT(vk.id)",
	}
)

// ListProfilesPaged returns a page of profiles with their key/child counts,
// default first by default, plus the total before windowing.
func (s *Store) ListProfilesPaged(ctx context.Context, p ListParams) ([]Profile, int, error) {
	fconds, fargs, err := buildFilters(p.Filter, profileFilterCols)
	if err != nil {
		return nil, 0, err
	}
	where := ""
	if len(fconds) > 0 {
		where = "WHERE " + strings.Join(fconds, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profiles p `+where, fargs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := " ORDER BY p.is_default DESC, p.name"
	if p.Sort != "" {
		o, err := buildOrder(p, profileSortCols, "name", "asc", "p.name")
		if err != nil {
			return nil, 0, err
		}
		order = o
	}
	query := `
		SELECT p.id, p.name, p.provider_filter, p.model_filter, p.is_default,
		       COUNT(vk.id)
		FROM profiles p LEFT JOIN virtual_keys vk ON vk.profile_id = p.id
		` + where + `
		GROUP BY p.id` + order
	if limit := p.pageLimit(50, 1000); limit > 0 {
		query += " LIMIT ? OFFSET ?"
		fargs = append(fargs, limit, p.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, fargs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list profiles: %w", err)
	}
	var out []Profile
	for rows.Next() {
		pr, err := scanProfile(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	// Close before any further query: the pool holds a single connection.
	rows.Close()
	if err := attachParents(ctx, s.db, out); err != nil {
		return nil, 0, err
	}
	counts, err := childCounts(ctx, s.db)
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		out[i].ChildCount = counts[out[i].ID]
	}
	return out, total, nil
}

// ListProfiles returns every profile (no paging), for callers that need the
// full set.
func (s *Store) ListProfiles(ctx context.Context) ([]Profile, error) {
	profiles, _, err := s.ListProfilesPaged(ctx, ListParams{Limit: -1})
	return profiles, err
}

// ProfileByName resolves one profile, with its key/child counts and parents.
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
	parents, err := parentsByProfile(ctx, s.db)
	if err != nil {
		return Profile{}, err
	}
	p.Parents = parents[p.ID]
	counts, err := childCounts(ctx, s.db)
	if err != nil {
		return Profile{}, err
	}
	p.ChildCount = counts[p.ID]
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

// parentsByProfile maps each profile id to its parent names, alphabetical.
func parentsByProfile(ctx context.Context, q rowsQuerier) (map[int64][]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT pp.profile_id, p.name
		FROM profile_parents pp JOIN profiles p ON p.id = pp.parent_id
		ORDER BY p.name`)
	if err != nil {
		return nil, fmt.Errorf("list profile parents: %w", err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// attachParents fills Parents on the given profiles in place.
func attachParents(ctx context.Context, q rowsQuerier, profiles []Profile) error {
	parents, err := parentsByProfile(ctx, q)
	if err != nil {
		return err
	}
	for i := range profiles {
		profiles[i].Parents = parents[profiles[i].ID]
	}
	return nil
}

// childCounts maps each profile id to the number of profiles that inherit it.
func childCounts(ctx context.Context, q rowsQuerier) (map[int64]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT parent_id, COUNT(*) FROM profile_parents GROUP BY parent_id`)
	if err != nil {
		return nil, fmt.Errorf("count profile children: %w", err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// UpdateProfile edits a profile's name, filters and parents in place. The
// seeded default profile is read-only. A rename onto an existing name is
// rejected as invalid (not a raw UNIQUE failure).
func (s *Store) UpdateProfile(ctx context.Context, currentName, newName string, provider, model KeyFilter, parents []string) error {
	if err := validateProfileShape(provider, model, parents); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update profile %q: begin: %w", currentName, err)
	}
	defer tx.Rollback()

	var id int64
	var isDefault bool
	err = tx.QueryRowContext(ctx, `SELECT id, is_default FROM profiles WHERE name = ?`, currentName).Scan(&id, &isDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProfileNotFound
	}
	if err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	if isDefault {
		return ErrProfileImmutable
	}
	// Renaming onto an existing name is a client error (422), not a
	// server-side UNIQUE failure. Check inside the tx to keep it atomic.
	if newName != currentName {
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM profiles WHERE name = ?`, newName).Scan(&taken); err != nil {
			return fmt.Errorf("update profile %q: check name: %w", currentName, err)
		}
		if taken > 0 {
			return fmt.Errorf("%w: profile %q already exists", ErrProfileInvalid, newName)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE profiles SET name = ?, provider_filter = ?, model_filter = ? WHERE id = ?`,
		newName, marshalJSON(provider.normalize()), marshalJSON(model.normalize()), id); err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	if err := setParentsTx(ctx, tx, id, parents); err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update profile %q: %w", currentName, err)
	}
	return nil
}

// DeleteProfile removes a profile. The default is read-only, a profile still
// referenced by any virtual key is refused so a restrictive filter can never
// silently broaden a key's access, and a profile still used as a parent is
// refused so derived sets cannot silently change.
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
	var children int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profile_parents WHERE parent_id = ?`, id).Scan(&children); err != nil {
		return fmt.Errorf("delete profile %q: count children: %w", name, err)
	}
	if children > 0 {
		return fmt.Errorf("%w as a parent by %d profile(s)", ErrProfileInUse, children)
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
	Parents        []string
}

// SeedProfiles applies config-defined profiles at startup. Each is upserted by
// name; profiles the config does not mention are left untouched. Parents may
// reference a profile defined later in the same batch. Config is only the seed
// — once running, the DB (and the export that mirrors it) is authoritative.
// The reserved default profile is never modified.
func (s *Store) SeedProfiles(ctx context.Context, profiles []SeedProfile) error {
	if len(profiles) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed profiles: begin: %w", err)
	}
	defer tx.Rollback()

	// Upsert every profile first so parents defined later in the batch resolve.
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

	// Then wire parents, replacing the previous edges for each seeded profile.
	for _, p := range profiles {
		if err := validateProfileShape(p.ProviderFilter, p.ModelFilter, p.Parents); err != nil {
			return fmt.Errorf("seed profiles: %q: %w", p.Name, err)
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM profiles WHERE name = ?`, p.Name).Scan(&id); err != nil {
			return fmt.Errorf("seed profiles: resolve %q: %w", p.Name, err)
		}
		if err := setParentsTx(ctx, tx, id, p.Parents); err != nil {
			return fmt.Errorf("seed profiles: %q: %w", p.Name, err)
		}
	}
	return tx.Commit()
}

// validateProfileShape enforces the leaf/derived XOR: a profile either carries
// its own filters or references parents, never both.
func validateProfileShape(provider, model KeyFilter, parents []string) error {
	if len(parents) == 0 {
		return nil
	}
	if provider.normalize().Mode != "none" || model.normalize().Mode != "none" {
		return fmt.Errorf("%w: a derived profile has no filters of its own", ErrProfileInvalid)
	}
	return nil
}

// setParentsTx replaces a profile's parent edges. It rejects unknown parents,
// duplicates, self-parenting and cycles.
func setParentsTx(ctx context.Context, tx *sql.Tx, profileID int64, parents []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM profile_parents WHERE profile_id = ?`, profileID); err != nil {
		return fmt.Errorf("clear parents: %w", err)
	}
	if len(parents) == 0 {
		return nil
	}
	adj, err := adjacencyTx(ctx, tx)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(parents))
	seen := make(map[string]bool, len(parents))
	for _, name := range parents {
		if seen[name] {
			return fmt.Errorf("%w: duplicate parent %q", ErrProfileInvalid, name)
		}
		seen[name] = true
		var pid int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM profiles WHERE name = ?`, name).Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: parent %q not found", ErrProfileInvalid, name)
		}
		if err != nil {
			return fmt.Errorf("resolve parent %q: %w", name, err)
		}
		if pid == profileID {
			return fmt.Errorf("%w: a profile cannot be its own parent", ErrProfileInvalid)
		}
		ids = append(ids, pid)
	}
	if reachesAny(adj, profileID, ids) {
		return fmt.Errorf("%w: parent cycle", ErrProfileInvalid)
	}
	for _, pid := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO profile_parents (profile_id, parent_id) VALUES (?, ?)`, profileID, pid); err != nil {
			return fmt.Errorf("insert parent: %w", err)
		}
	}
	return nil
}

// adjacencyTx loads profile_parents as profile_id -> parent_ids.
func adjacencyTx(ctx context.Context, tx *sql.Tx) (map[int64][]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT profile_id, parent_id FROM profile_parents`)
	if err != nil {
		return nil, fmt.Errorf("load parents: %w", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var id, parent int64
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		out[id] = append(out[id], parent)
	}
	return out, rows.Err()
}

// reachesAny reports whether target is reachable from any of the start nodes
// following the parent edges. An edge target -> start would then close a loop.
func reachesAny(adj map[int64][]int64, target int64, start []int64) bool {
	seen := map[int64]bool{}
	var reaches func(int64) bool
	reaches = func(n int64) bool {
		if n == target {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		for _, p := range adj[n] {
			if reaches(p) {
				return true
			}
		}
		return false
	}
	for _, n := range start {
		if reaches(n) {
			return true
		}
	}
	return false
}

// profileNode is one profile's raw rule in the in-memory graph.
type profileNode struct {
	isDefault bool
	provider  KeyFilter
	model     KeyFilter
	parents   []int64
}

// loadProfileGraph loads every profile and parent edge for resolution.
func (s *Store) loadProfileGraph(ctx context.Context) (map[int64]profileNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, is_default, provider_filter, model_filter FROM profiles`)
	if err != nil {
		return nil, fmt.Errorf("load profile graph: %w", err)
	}
	defer rows.Close()
	graph := map[int64]profileNode{}
	for rows.Next() {
		var id int64
		var isDefault bool
		var provider, model string
		if err := rows.Scan(&id, &isDefault, &provider, &model); err != nil {
			return nil, err
		}
		node := profileNode{isDefault: isDefault}
		_ = json.Unmarshal([]byte(provider), &node.provider)
		_ = json.Unmarshal([]byte(model), &node.model)
		node.provider = node.provider.normalize()
		node.model = node.model.normalize()
		graph[id] = node
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Close before the edges query: the pool holds a single connection.
	rows.Close()
	erows, err := s.db.QueryContext(ctx, `SELECT profile_id, parent_id FROM profile_parents ORDER BY parent_id`)
	if err != nil {
		return nil, fmt.Errorf("load profile graph edges: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var id, parent int64
		if err := erows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		node := graph[id]
		node.parents = append(node.parents, parent)
		graph[id] = node
	}
	return graph, erows.Err()
}

// resolveProfileRules walks a profile graph and returns its resolved rule set.
// An All profile anywhere in the ancestry sets allowAll. A leaf contributes one
// rule; a derived profile contributes only its parents' rules. The visited set
// makes it safe even if a cycle ever slipped past write-time validation.
func resolveProfileRules(graph map[int64]profileNode, id int64) (allowAll bool, rules []KeyRule) {
	visited := map[int64]bool{}
	var walk func(int64)
	walk = func(n int64) {
		if allowAll || visited[n] {
			return
		}
		visited[n] = true
		node, ok := graph[n]
		if !ok {
			return
		}
		if node.isDefault {
			allowAll = true
			return
		}
		if len(node.parents) == 0 {
			rules = append(rules, KeyRule{ProviderFilter: node.provider, ModelFilter: node.model})
			return
		}
		for _, p := range node.parents {
			walk(p)
		}
	}
	walk(id)
	return allowAll, rules
}
