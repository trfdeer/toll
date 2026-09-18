# Profiles

Design decisions for the **Profile** entity: a named, reusable provider/model
filter shared by virtual keys. This records what was decided (and why). Status:
core profile feature and composition (derived profiles) implemented.

## What a profile is

- A named pair of filters — `providerFilter` and `modelFilter` — identical in
  shape to the inline filters virtual keys used to carry. (This describes a
  **leaf** profile; a **derived** profile holds no filters of its own and unions
  its parents instead — see *Composition / derived profiles*.)
- Assigned to virtual keys: a key references **exactly one** profile, and any
  number of keys can share the same profile.
- **Dynamic**: the allowed set is resolved live against the registry on each
  request, so models a provider adds later are picked up automatically. It is
  not a frozen list of model IDs.
- A model is allowed when its provider passes the provider filter **and** its
  gateway ID passes the model filter (`keys.Allows`).

## Name

Chose **Profile** (`profiles`, `profile_id`, `/api/profiles`, "Profiles" page).

Names considered: Deck, Access Policy / Policy, Scope, Entitlement, Allowlist,
Model Set, Collection, Bundle. "Deck" was dropped as an unclear metaphor;
"Allowlist" as a misnomer because filters support `exclude`.

## Locked decisions

1. **Dynamic rules, not a frozen list.** A profile holds provider/model
   include-exclude rules; the set resolves live. This keeps
   "all of provider X except a few" true as the provider adds models.
2. **One profile per key.** Keys stop carrying inline filters; they name a
   profile. Default is the `All` profile.
3. **`All` is seeded, immutable.** `profiles.id = 1`, `is_default = 1`, both
   filters `none`. It cannot be edited or deleted: editing it would silently
   change every key that relies on it.
4. **Schema (migration v11).** `profiles` table + `virtual_keys.profile_id`;
   the old `provider_filter` / `model_filter` columns are dropped. No backfill
   — a fresh start, so pre-existing keys fall back to `All`.
5. **Delete policy.** Deleting a profile that keys still reference is refused
   (422 with the count), so a restrictive filter can never silently broaden a
   key's access. The `All` profile is refused unconditionally.
6. **API.** `GET/POST /api/profiles`, `PUT/DELETE /api/profiles/{name}`.
   Key create/update takes a `profile` name (omitted → `All`).
7. **Request path.** Key lookup joins the referenced profile and fills the
   existing provider/model filter fields, so `keys.Allows`, the proxy, and
   `/v1/models` gating keep working unchanged (for the non-composed case).
8. **UI.**
   - Profiles page: columns Name / Providers / Models / Based on /
     Allowed models / Keys. For a derived profile the Providers and Models
     cells read `—` and Based on lists the parent names.
   - The Providers and Models cells **summarize** (`only 3 models`,
     `all models except 2`) rather than enumerate values.
   - New/Edit uses a core `@carbon/react` **`Modal`**. (An early version used
     the `SidePanel` drawer from `@carbon/ibm-products`, but the extra
     dependency was not worth it for one component; core `@carbon/react` has
     no scrimmed SidePanel, and a modal fits the short form.)
   - Model selection and preview share one paginated table modal
     (`ModelTableModal`).
   - The model picker is **scoped by the provider filter**: only models whose
     provider passes the current provider filter are offered. Existing
     selections are preserved, never silently pruned.
   - The `All` row shows the same action icons as every other row, with Edit
     and Delete disabled and Preview enabled.
   - The old inline "N models allowed" text under the editor inputs was
     removed; the Preview action replaces it.
9. **Dependency cost.** No new dependencies: New/Edit, model selection and
   preview use core `@carbon/react` components only.
10. **Export / import round-trip.** The config export writes every non-default
    profile under `profiles:` (name plus `provider_filter` / `model_filter`, and
    `parents` for a derived profile), and startup seeds them back via
    `store.SeedProfiles`, mirroring model aliases: config wins at boot, then the
    UI is authoritative until the next restart. `All` is omitted (the migration
    recreates it) and rejected by config validation if redefined; a profile may
    still name `All` as a parent. Virtual keys are deliberately not exported —
    their key material is unrecoverable, so `toll.db` is their only home.

## Composition / derived profiles

Goal example: *"allow all of provider B, plus a few specific models from
provider A."* This is a **union** — `(provider = B) OR (model ∈ {a1, a2})` —
and the current two-filter AND cannot express it.

Chosen model: **derived profiles (union of parents).** A profile is one of two
shapes, distinguished **structurally** by whether it has parents:

- **Leaf** — carries its own provider/model clause and no parents.
- **Derived** — lists one or more parents and has **no clause of its own**; its
  resolved set is purely the union of its parents' resolved sets.

There is no separate "kind" bit: a leaf whose filters are `none/none` allows
everything, which is the same value a "derived with zero parents" would have.
That is why a derived profile requires at least one parent (enforced by the
UI); the store cannot tell the two apart, and need not.

Rationale: reuse — a subset such as "a few A models" is defined once (as a leaf)
and inherited by several derived profiles.

A profile having **no clause of its own** is what makes the model unambiguous:
a derived profile contributes nothing beyond its parents, so there is no
question of how its own filter would combine with theirs.

Rejected alternatives:

- **Parents plus an own clause** — a profile could both inherit and add rules.
  Dropped: it makes the meaning of `none/none` on a child depend on context
  (everything vs. parents-only), and it blurs the leaf/derived distinction. A
  derived profile is strictly a union of parents.
- **Multiple clauses per profile** (allowed = OR of clauses) — direct and
  cycle-free, but no named reuse.
- **Key references multiple profiles** (union at the key) — simplest
  composition, but a combined set cannot itself be reused.

Semantics notes:

- Union is idempotent; overlapping rules are fine, not conflicts.
- No cross-clause negation. "Everything except X" stays inside a leaf's
  `exclude` mode; composition only ever adds.
- If any ancestor is the `All` profile, the union is everything.
- `none/none` means "everything" only on a **leaf**; a derived profile's own
  filters are empty *and* meaningless — its set is the parents' union.

Trade-off: adding "just a couple more models" to an inherited set now requires
a leaf profile to hold them rather than appending a clause to the derived
profile. More profiles, but consistent with the reuse rationale — the shared
leaf is defined once.

Implementation:

- `profile_parents(profile_id, parent_id)` join table (no `position` — union is
  commutative, so ordering carries no semantics); migration v12.
- Validation is a **XOR**: parents present ⇒ filters must be empty; filters
  present ⇒ no parents. A derived profile with zero parents is, structurally, a
  neutral leaf ("allow everything"); the UI refuses to save one, but the store
  treats it as a leaf. Enforced in the store, the admin API and
  config loading.
- Cycle detection on write (a profile cannot be its own ancestor); a
  `CHECK (profile_id <> parent_id)` is the cheap first line, plus store- and
  config-level graph checks.
- Deleting a profile used as a parent is refused, like the key-in-use guard.
- Resolution loads the profile graph once and walks it: `keys.Allows` takes an
  `allowAll` flag plus `[]Rule{provider, model}` instead of a single pair.
  `store.VirtualKey` and `keys.VirtualKey` carry `AllowAll` + `Rules`; `api.go`
  / `proxy/capture.go` pass them through.
- The web previews resolve the union client-side over the profile list
  (`profileAllowedModels`), with a `seen` set for cycle safety.
- The New/Edit UI branches on kind via a core `RadioButtonGroup`: leaf shows
  the provider/model filters and the provider-scoped `ModelTableModal` picker;
  derived shows a `MultiSelect` of parent profiles (self and descendants
  hidden). A derived profile's preview renders the union of its parents with no
  own row.
- Model selections that fall outside a changed provider filter are **preserved**,
  never silently pruned (same rule as the existing picker, decision 8).
- There is no preview-before-save for an unsaved composition: the table's
  Preview action is the only preview surface.
