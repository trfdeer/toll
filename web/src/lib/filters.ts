import type { KeyFilter, Model, Profile } from "./types";

// The neutral filter: no constraint on the dimension.
export const EMPTY_FILTER: KeyFilter = { mode: "none", values: [] };

// summarizeFilter renders a filter as a short table cell: "all models",
// "only 3 models" or "all models except 2". The full value list lives in the
// editor, so the table stays compact even when a profile names hundreds of
// models.
export function summarizeFilter(
  filter: KeyFilter,
  singular: string,
  plural: string,
): string {
  const n = filter?.values?.length ?? 0;
  if (!filter || filter.mode === "none" || n === 0) return `all ${plural}`;
  if (filter.mode === "include")
    return `only ${n} ${n === 1 ? singular : plural}`;
  return `all ${plural} except ${n}`;
}

// allowsValue mirrors internal/keys.allows for one dimension.
function allowsValue(filter: KeyFilter, value: string): boolean {
  if (!filter || filter.mode === "none") return true;
  const has = filter.values.includes(value);
  return filter.mode === "include" ? has : !has;
}

// allowsModel reports whether a model passes a provider/model filter pair.
// The provider is the discovery source (m.upstream), never parsed from the
// gateway ID, matching the server's gating.
export function allowsModel(
  m: Model,
  provider: KeyFilter,
  model: KeyFilter,
): boolean {
  return allowsValue(provider, m.upstream) && allowsValue(model, m.gatewayId);
}

// allowedModels resolves the set of models a profile permits. This is the
// same live resolution the gateway performs per request, so it previews
// exactly what a key using the profile would see (before global
// enabled/disabled state is applied).
export function allowedModels(
  models: Model[],
  provider: KeyFilter,
  model: KeyFilter,
): Model[] {
  return models.filter((m) => allowsModel(m, provider, model));
}

// profileAllowedModels resolves a profile's allowed set the way the gateway
// does: the All default permits everything; a leaf applies its own filters; a
// derived profile unions its parents. seen guards against parent cycles.
export function profileAllowedModels(
  profile: Profile,
  byName: Map<string, Profile>,
  models: Model[],
  seen: Set<string> = new Set(),
): Model[] {
  if (profile.isDefault) return models;
  if (seen.has(profile.name)) return [];
  seen.add(profile.name);
  if (profile.parents.length > 0) {
    const out = new Map<number, Model>();
    for (const parent of profile.parents) {
      const p = byName.get(parent);
      if (!p) continue;
      for (const m of profileAllowedModels(p, byName, models, seen)) {
        out.set(m.id, m);
      }
    }
    return [...out.values()];
  }
  return allowedModels(models, profile.providerFilter, profile.modelFilter);
}

// descendantsOf returns every profile that transitively inherits from root,
// so the parent picker can hide choices that would create a cycle.
export function descendantsOf(root: string, profiles: Profile[]): Set<string> {
  const children = new Map<string, string[]>();
  for (const p of profiles) {
    for (const parent of p.parents) {
      children.set(parent, [...(children.get(parent) ?? []), p.name]);
    }
  }
  const out = new Set<string>();
  const stack = [root];
  while (stack.length > 0) {
    const current = stack.pop() as string;
    for (const child of children.get(current) ?? []) {
      if (!out.has(child)) {
        out.add(child);
        stack.push(child);
      }
    }
  }
  return out;
}
