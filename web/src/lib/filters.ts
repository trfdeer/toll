import type { KeyFilter, Model } from "./types";

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
