import type { IncomingMessage, ServerResponse } from 'node:http';
import { defineConfig } from 'vite';
import type { Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import type {
  CreateProviderResponse,
  KeyFilter,
  Model,
  Profile,
  Provider,
  RequestDetail,
  RequestRow,
  UsageRow,
  VirtualKey,
} from './src/lib/types';

interface CreateProviderBody {
  name?: string;
  baseURL?: string;
  apiKey?: string;
}

interface CreateKeyBody {
  name?: string;
  profile?: string;
}

interface ProfileBody {
  name?: string;
  providerFilter?: KeyFilter;
  modelFilter?: KeyFilter;
  parents?: string[];
}

// In-memory mock of the toll admin API so the UI can be developed without a
// running toll. The real API lives in internal/admin (Go) and speaks the same
// shapes under /admin/api/*.
function mockApi(): Plugin {
  const none: KeyFilter = { mode: 'none', values: [] };
  const profiles: Profile[] = [
    { name: 'All', providerFilter: none, modelFilter: none, parents: [], isDefault: true, keyCount: 0, childCount: 0 },
    {
      name: 'hyper-chat',
      providerFilter: { mode: 'include', values: ['hyper'] },
      modelFilter: { mode: 'exclude', values: ['hyper/deepseek-v3'] },
      parents: [],
      isDefault: false,
      keyCount: 0,
      childCount: 1,
    },
    {
      name: 'hyper-strict',
      providerFilter: none,
      modelFilter: none,
      parents: ['hyper-chat'],
      isDefault: false,
      keyCount: 0,
      childCount: 0,
    },
  ];
  const keys: VirtualKey[] = [
    { name: 'web', profile: 'All', paused: false, revoked: false },
    { name: 'batch', profile: 'hyper-chat', paused: false, revoked: true },
  ];
  

  const json = (res: ServerResponse, status: number, body: unknown) => {
    res.statusCode = status;
    res.setHeader('Content-Type', 'application/json');
    res.end(body === null ? '' : JSON.stringify(body));
  };
  const readBody = (req: IncomingMessage): Promise<Record<string, unknown>> =>
    new Promise((resolve) => {
      let data = '';
      req.on('data', (c) => (data += c));
      req.on('end', () => {
        try {
          resolve(JSON.parse(data || '{}') as Record<string, unknown>);
        } catch {
          resolve({});
        }
      });
    });

  // ---- list params (mirrors the Go admin list endpoints) ----

  interface MockFilterSpec {
    op: string;
    values: string[];
  }
  interface MockList {
    /** undefined = endpoint default; 0 = all rows. */
    limit: number | undefined;
    offset: number;
    sort: string;
    dir: number | undefined;
    filters: Record<string, MockFilterSpec>;
  }

  const parseList = (params: URLSearchParams): MockList => {
    const raw = params.get('filter');
    let filters: Record<string, MockFilterSpec> = {};
    if (raw) {
      try {
        filters = JSON.parse(raw) as Record<string, MockFilterSpec>;
      } catch {
        filters = {};
      }
    }
    const limitRaw = params.get('limit');
    const dirRaw = params.get('dir');
    return {
      limit: limitRaw === null ? undefined : Number(limitRaw),
      offset: Number(params.get('offset') ?? 0),
      sort: params.get('sort') ?? '',
      dir: dirRaw === 'asc' ? 1 : dirRaw === 'desc' ? -1 : undefined,
      filters,
    };
  };

  // applyList applies the filter/sort/pagination of a MockList to rows. It
  // mirrors the Go list endpoints: an unknown sort/filter column is an error
  // (the real API answers 400), and an undefined limit falls back to the
  // endpoint default.
  const applyList = <T,>(
    rows: T[],
    p: MockList,
    sortAccessors: Record<string, (r: T) => unknown>,
    filterAccessors: Record<string, (r: T) => unknown>,
    defaultLimit: number,
    defaultCompare?: (a: T, b: T) => number,
  ): { rows: T[]; total: number; error?: string } => {
    let out = rows;
    for (const [col, spec] of Object.entries(p.filters)) {
      const acc = filterAccessors[col];
      if (!acc) {
        return { rows: [], total: 0, error: `unknown filter column "${col}"` };
      }
      out = out.filter((r) => {
        const raw = acc(r);
        const s = raw === null || raw === undefined ? '' : String(raw);
        const lv = s.toLowerCase();
        const vals = spec.values;
        switch (spec.op) {
          case 'in':
            if (vals.includes('') && s === '') return true;
            return vals.includes(s);
          case 'notContains':
            return !vals.some((v) => lv.includes(v.toLowerCase()));
          case 'equals':
            return vals.some((v) => lv === v.toLowerCase());
          case 'notEquals':
            return !vals.some((v) => lv === v.toLowerCase());
          case 'startsWith':
            return vals.some((v) => lv.startsWith(v.toLowerCase()));
          case 'endsWith':
            return vals.some((v) => lv.endsWith(v.toLowerCase()));
          case 'blank':
            return s === '';
          case 'notBlank':
            return s !== '';
          default:
            return vals.some((v) => lv.includes(v.toLowerCase()));
        }
      });
    }
    if (p.sort && !sortAccessors[p.sort]) {
      return { rows: [], total: 0, error: `unknown sort column "${p.sort}"` };
    }
    if (p.sort && sortAccessors[p.sort]) {
      const acc = sortAccessors[p.sort]!;
      const dir = p.dir ?? 1;
      out = out.slice().sort((a, b) => {
        const av = acc(a);
        const bv = acc(b);
        if (typeof av === 'number' && typeof bv === 'number') return (av - bv) * dir;
        return String(av).localeCompare(String(bv), undefined, { numeric: true }) * dir;
      });
    } else if (defaultCompare) {
      out = out.slice().sort(defaultCompare);
    }
    const total = out.length;
    const limit = p.limit === undefined ? defaultLimit : p.limit;
    const end = limit > 0 ? p.offset + limit : undefined;
    return { rows: out.slice(p.offset, end), total };
  };

  const modelStatus = (m: Model): string =>
    m.disabled ? 'disabled' : m.providerDisabled ? 'provider disabled' : !m.providerReachable ? 'unreachable' : 'active';
  const providerStatus = (p: Provider): string =>
    p.disabled ? 'disabled' : p.reachable ? 'active' : 'unreachable';
  const keyStatus = (k: VirtualKey): string =>
    k.revoked ? 'revoked' : k.paused ? 'paused' : 'active';

  // Metadata-derived model columns, mirroring the model_search view's
  // json_extract coalesce chains.
  const metaLookup = (m: Model, path: string): unknown => {
    let cur: unknown = m.metadata;
    for (const part of path.split('.')) {
      if (cur === null || typeof cur !== 'object') return undefined;
      cur = (cur as Record<string, unknown>)[part];
    }
    return cur;
  };
  const metaLimit = (m: Model, paths: string[]): number | string => {
    for (const path of paths) {
      const v = metaLookup(m, path);
      if (typeof v === 'number') return v;
      if (typeof v === 'string' && v.trim() !== '') return v;
    }
    return '';
  };
  const INPUT_LIMITS = ['max_input_tokens', 'context_window', 'max_model_len', 'context_length', 'max_context_length'];
  const OUTPUT_LIMITS = ['max_output_tokens', 'max_completion_tokens', 'max_tokens', 'top_provider.max_completion_tokens'];
  const inputPrice = (m: Model): number | string => {
    const v = metaLookup(m, 'pricing.input');
    return typeof v === 'number' ? v : '';
  };

  const providers: Provider[] = [
    {
      name: 'hyper', baseURL: 'http://zeph:9931/v1', modelCount: 3,
      disabled: false, reachable: true, lastError: '', lastSyncedAt: '2026-09-15T00:00:00.000Z',
    },
  ];
  let storePrompts = true;
  const models: Model[] = [
    { id: 1, upstream: 'hyper', upstreamModelId: 'glm-4.6', gatewayId: 'hyper/glm-4.6', displayName: 'GLM 4.6', alias: '', metadata: { max_model_len: 262144, max_output_tokens: 8192 }, disabled: false, providerDisabled: false, providerReachable: true },
    { id: 2, upstream: 'hyper', upstreamModelId: 'glm-4.5-air', gatewayId: 'hyper/glm-4.5-air', displayName: 'GLM 4.5 Air', alias: '', metadata: { context_window: 128000 }, disabled: false, providerDisabled: false, providerReachable: true },
    { id: 3, upstream: 'hyper', upstreamModelId: 'deepseek-v3', gatewayId: 'hyper/deepseek-v3', displayName: 'DeepSeek V3', alias: '', metadata: {}, disabled: true, providerDisabled: false, providerReachable: true },
  ];

  const requestDetails: Record<number, RequestDetail> = {
    1: {
      id: 1, conversationId: 'conv-001', gatewayModel: 'hyper/hyperbolic-70b',
      upstreamModel: 'glm-4.6', status: 200, createdAt: '2026-09-14T10:02:00.000Z',
      completedAt: '2026-09-14T10:02:02.100Z', durationMs: 2100, contentStored: true,
      messages: [
        { role: 'system', content: 'You are a concise assistant.' },
        { role: 'user', content: 'What is an LLM gateway?' },
        { role: 'assistant', reasoning: 'The user wants a one-line definition; keep it short.', content: 'An LLM gateway is a central layer that proxies requests to one or more model providers.', finishReason: 'stop' },
      ],
    },
    2: {
      id: 2, conversationId: 'conv-001', gatewayModel: 'hyper/hyperbolic-70b',
      upstreamModel: 'glm-4.6', status: 200, createdAt: '2026-09-14T10:03:00.000Z',
      completedAt: '2026-09-14T10:03:00.730Z', durationMs: 730, contentStored: true,
      messages: [
        { role: 'user', content: 'Summarize the previous answer in three words.' },
        { role: 'assistant', content: 'Central model proxy.' },
      ],
    },
    3: {
      id: 3, conversationId: 'conv-002', gatewayModel: 'hyper/hyperbolic-70b',
      upstreamModel: 'glm-4.6', status: 429, createdAt: '2026-09-14T11:47:00.000Z',
      completedAt: '2026-09-14T11:47:00.045Z', durationMs: 45, contentStored: false,
      messages: [
        { role: 'system', content: 'You can call tools when useful.' },
        { role: 'user', content: 'What is the weather in Berlin?' },
        { role: 'assistant', content: '', finishReason: 'tool_calls', toolCalls: [
          { id: 'call_1', name: 'get_weather', arguments: '{"city":"Berlin","units":"celsius"}' },
        ] },
        { role: 'tool', name: 'get_weather', toolCallId: 'call_1', content: '{"temp": 18, "sky": "cloudy"}' },
        { role: 'assistant', content: '', reasoning: 'I have the result; summarize it.', finishReason: 'length' },
      ],
    },
  };

  return {
    name: 'mock-admin-api',
    configureServer(server) {
      server.middlewares.use('/admin/api', async (req, res, next) => {
        const { method } = req;
        const [path = '', query] = (req.url ?? '').split('?');
        const params = new URLSearchParams(query ?? '');
        const seg = path.split('/').filter(Boolean); // e.g. ["keys","web","revoke"]

        // applyFilter mirrors the server's from/to/key constraints so the dev
        // mock responds to the same query params as the real API.
        const applyFilter = <T extends { createdAt: string; keyName?: string }>(rows: T[]): T[] => {
          const from = params.get('from');
          const to = params.get('to');
          // The reserved deleted-keys sentinel is ignored by the dev mock.
          const keyNames = params.getAll('key').filter((k) => k !== '__deleted__');
          return rows.filter(
            (r) =>
              (!from || r.createdAt >= from) &&
              (!to || r.createdAt <= to) &&
              (keyNames.length === 0 ||
                (r.keyName !== undefined && keyNames.includes(r.keyName)))
          );
        };

        if (method === 'GET' && path === '/providers') {
          const p = parseList(params);
          const r = applyList(
            providers,
            p,
            {
              name: (x) => x.name,
              baseURL: (x) => x.baseURL,
              modelCount: (x) => x.modelCount,
              status: providerStatus,
            },
            { name: (x) => x.name, baseURL: (x) => x.baseURL, status: providerStatus },
            50,
          );
          if (r.error) return json(res, 400, { error: r.error });
          return json(res, 200, { providers: r.rows, total: r.total });
        }
        if (method === 'POST' && path === '/providers') {
          const body = (await readBody(req)) as CreateProviderBody;
          if (!body.name || !body.baseURL || !body.apiKey) {
            return json(res, 422, { error: 'name, baseURL and apiKey are required' });
          }
          if (providers.some((p) => p.name === body.name)) {
            return json(res, 422, { error: 'provider already exists' });
          }
          providers.push({
            name: body.name, baseURL: body.baseURL, modelCount: 0,
            disabled: false, reachable: false, lastError: 'dev mock', lastSyncedAt: '',
          });
          const created: CreateProviderResponse = {
            name: body.name,
            baseURL: body.baseURL,
            modelCount: 0,
            warning: 'provider added, but its models could not be fetched (dev mock)',
          };
          return json(res, 200, created);
        }
        if (method === 'POST' && seg[0] === 'providers' && (seg[2] === 'disable' || seg[2] === 'enable')) {
          const p = providers.find((p) => p.name === decodeURIComponent(seg[1] ?? ''));
          if (!p) return json(res, 404, { error: 'provider not found' });
          p.disabled = seg[2] === 'disable';
          return json(res, 204, null);
        }
        if (method === 'DELETE' && seg[0] === 'providers' && seg[1]) {
          const name = decodeURIComponent(seg[1]);
          const i = providers.findIndex((p) => p.name === name);
          if (i < 0) return json(res, 404, { error: 'provider not found' });
          providers.splice(i, 1);
          for (let j = models.length - 1; j >= 0; j--) {
            if (models[j]?.upstream === name) models.splice(j, 1);
          }
          return json(res, 204, null);
        }
        if (method === 'GET' && path === '/models') {
          const p = parseList(params);
          const r = applyList(
            models,
            p,
            {
              gatewayId: (m) => m.gatewayId,
              upstream: (m) => m.upstream,
              displayName: (m) => m.displayName,
              alias: (m) => m.alias,
              status: modelStatus,
              inputLimit: (m) => metaLimit(m, INPUT_LIMITS),
              outputLimit: (m) => metaLimit(m, OUTPUT_LIMITS),
              cost: (m) => inputPrice(m),
            },
            {
              gatewayId: (m) => m.gatewayId,
              upstream: (m) => m.upstream,
              displayName: (m) => m.displayName,
              alias: (m) => m.alias,
              status: modelStatus,
              inputLimit: (m) => metaLimit(m, INPUT_LIMITS),
              outputLimit: (m) => metaLimit(m, OUTPUT_LIMITS),
              cost: (m) => inputPrice(m),
            },
            50,
            (a, b) => a.gatewayId.localeCompare(b.gatewayId),
          );
          if (r.error) return json(res, 400, { error: r.error });
          return json(res, 200, { models: r.rows, total: r.total });
        }
        if (method === 'POST' && path === '/models/refresh') {
          return json(res, 200, { providers: providers.length, models: models.length, warnings: [] });
        }
        if (method === 'DELETE' && seg[0] === 'models' && seg[1]) {
          const id = Number(seg[1]);
          const i = models.findIndex((m) => m.id === id);
          if (i < 0) return json(res, 404, { error: 'model not found' });
          models.splice(i, 1);
          return json(res, 204, null);
        }
        if (method === 'POST' && seg[0] === 'models' && (seg[2] === 'disable' || seg[2] === 'enable')) {
          const id = Number(seg[1]);
          const m = models.find((m) => m.id === id);
          if (!m) return json(res, 404, { error: 'model not found' });
          m.disabled = seg[2] === 'disable';
          return json(res, 204, null);
        }
        if (method === 'PUT' && seg[0] === 'models' && seg[2] === 'alias') {
          const id = Number(seg[1]);
          const m = models.find((m) => m.id === id);
          if (!m) return json(res, 404, { error: 'model not found' });
          const body = (await readBody(req)) as { alias?: string };
          const alias = (body.alias ?? '').trim();
          if (alias && models.some((x) => x.id !== id && x.gatewayId === alias)) {
            return json(res, 422, { error: 'alias is already another model gateway ID' });
          }
          m.alias = alias;
          m.gatewayId = alias || `${m.upstream}/${m.upstreamModelId}`;
          return json(res, 204, null);
        }
        if (method === 'GET' && path === '/settings') {
          return json(res, 200, { storePrompts });
        }
        if (method === 'PUT' && path === '/settings') {
          const body = (await readBody(req)) as { storePrompts?: boolean };
          if (typeof body.storePrompts === 'boolean') storePrompts = body.storePrompts;
          return json(res, 200, { storePrompts });
        }
        if (method === 'GET' && path === '/config') {
          res.statusCode = 200;
          res.setHeader('Content-Type', 'application/yaml');
          return res.end(
            'upstreams:\n' +
              '  - name: default\n' +
              '    url: http://zeph:9931/v1\n' +
              '    api_key_env: DEFAULT_API_KEY\n' +
              '    refresh_interval: 5m0s\n'
          );
        }
        if (method === 'GET' && path === '/usage') {
          const rows: Array<UsageRow & { createdAt: string }> = [
            {
              gatewayModel: 'hyper/hyperbolic-70b',
              requests: 2, promptTokens: 330, cachedTokens: 118, completionTokens: 115, costUSD: 0.0028,
              createdAt: '2026-09-14T10:02:00.000Z',
            },
          ];
          const kept = applyFilter(rows);
          const p = parseList(params);
          const sortAcc = {
            model: (x: UsageRow) => x.gatewayModel,
            requests: (x: UsageRow) => x.requests,
            prompt: (x: UsageRow) => x.promptTokens,
            cached: (x: UsageRow) => x.cachedTokens,
            completion: (x: UsageRow) => x.completionTokens,
            cost: (x: UsageRow) => x.costUSD,
          };
          // Totals are over the whole filtered set, not just the page.
          const all = applyList(
            kept,
            { ...p, limit: 0, offset: 0 },
            sortAcc,
            { model: (x) => x.gatewayModel },
            50,
            (a, b) => a.gatewayModel.localeCompare(b.gatewayModel),
          );
          if (all.error) return json(res, 400, { error: all.error });
          const filtered = all.rows;
          const limit = p.limit === undefined ? 50 : p.limit;
          const start = p.offset;
          const end = limit > 0 ? start + limit : undefined;
          return json(res, 200, {
            rows: filtered.slice(start, end),
            total: filtered.length,
            totalReqs: filtered.reduce((n, r) => n + r.requests, 0),
            totalCost: filtered.reduce((n, r) => n + r.costUSD, 0).toFixed(4) + ' USD',
            totals: {
              requests: filtered.reduce((n, r) => n + r.requests, 0),
              promptTokens: filtered.reduce((n, r) => n + r.promptTokens, 0),
              cachedTokens: filtered.reduce((n, r) => n + r.cachedTokens, 0),
              completionTokens: filtered.reduce((n, r) => n + r.completionTokens, 0),
              costUSD: filtered.reduce((n, r) => n + r.costUSD, 0),
            },
          });
        }
        if (method === 'GET' && path === '/keys') {
          const p = parseList(params);
          const r = applyList(
            keys,
            p,
            { name: (k) => k.name, profile: (k) => k.profile, status: keyStatus },
            { name: (k) => k.name, profile: (k) => k.profile, status: keyStatus },
            50,
          );
          if (r.error) return json(res, 400, { error: r.error });
          return json(res, 200, { keys: r.rows, total: r.total });
        }
        if (method === 'GET' && path === '/profiles') {
          const p = parseList(params);
          const withCounts = profiles.map((x) => ({
            ...x,
            // Live counts: key references plus inheritance children.
            keyCount: keys.filter((k) => k.profile === x.name).length,
            childCount: profiles.filter((y) => y.parents.includes(x.name)).length,
          }));
          const r = applyList(
            withCounts,
            p,
            { name: (x) => x.name, keys: (x) => x.keyCount },
            { name: (x) => x.name },
            50,
            (a, b) =>
              Number(b.isDefault) - Number(a.isDefault) ||
              a.name.localeCompare(b.name),
          );
          if (r.error) return json(res, 400, { error: r.error });
          return json(res, 200, { profiles: r.rows, total: r.total });
        }
        if (method === 'POST' && path === '/profiles') {
          const body = (await readBody(req)) as ProfileBody;
          if (!body.name) return json(res, 422, { error: 'name is required' });
          if (profiles.some((p) => p.name === body.name)) {
            return json(res, 422, { error: 'profile already exists' });
          }
          const parents = body.parents ?? [];
          profiles.push({
            name: body.name,
            providerFilter: body.providerFilter ?? none,
            modelFilter: body.modelFilter ?? none,
            parents,
            isDefault: false,
            keyCount: 0,
            childCount: 0,
          });
          return json(res, 204, null);
        }
        if (method === 'PUT' && seg[0] === 'profiles' && seg[1]) {
          const current = decodeURIComponent(seg[1]);
          const p = profiles.find((p) => p.name === current);
          if (!p) return json(res, 404, { error: 'profile not found' });
          if (p.isDefault) return json(res, 422, { error: 'the All profile is read-only' });
          const body = (await readBody(req)) as ProfileBody;
          if (!body.name) return json(res, 422, { error: 'name is required' });
          if (body.name !== current && profiles.some((x) => x.name === body.name)) {
            return json(res, 422, { error: 'profile already exists' });
          }
          const oldName = p.name;
          p.name = body.name;
          if (body.providerFilter) p.providerFilter = body.providerFilter;
          if (body.modelFilter) p.modelFilter = body.modelFilter;
          p.parents = body.parents ?? [];
          // Keys and derived profiles reference profiles by name, so keep them
          // pointing at the renamed profile.
          for (const k of keys) if (k.profile === oldName) k.profile = p.name;
          for (const x of profiles) {
            x.parents = x.parents.map((parent) => (parent === oldName ? p.name : parent));
          }
          return json(res, 204, null);
        }
        if (method === 'DELETE' && seg[0] === 'profiles' && seg[1]) {
          const name = decodeURIComponent(seg[1]);
          const i = profiles.findIndex((p) => p.name === name);
          if (i < 0) return json(res, 404, { error: 'profile not found' });
          if (profiles[i]?.isDefault) {
            return json(res, 422, { error: 'the All profile is read-only' });
          }
          const inUse = keys.filter((k) => k.profile === name).length;
          if (inUse > 0) {
            return json(res, 422, { error: `profile is in use by ${inUse} virtual key(s)` });
          }
          const children = profiles.filter((x) => x.parents.includes(name)).length;
          if (children > 0) {
            return json(res, 422, { error: `profile is in use as a parent by ${children} profile(s)` });
          }
          profiles.splice(i, 1);
          return json(res, 204, null);
        }
        if (method === 'POST' && path === '/keys') {
          const body = (await readBody(req)) as CreateKeyBody;
          if (!body.name) return json(res, 422, { error: 'name is required' });
          if (keys.some((k) => k.name === body.name)) {
            return json(res, 422, { error: 'key already exists' });
          }
          keys.push({
            name: body.name,
            profile: body.profile ?? 'All',
            paused: false,
            revoked: false,
          });
          return json(res, 200, { plaintext: 'gk-mock-' + Math.random().toString(36).slice(2, 12) });
        }
        if (method === 'PUT' && seg[0] === 'keys' && seg[1]) {
          const current = decodeURIComponent(seg[1]);
          const k = keys.find((k) => k.name === current);
          if (!k) return json(res, 404, { error: 'key not found' });
          const body = (await readBody(req)) as CreateKeyBody;
          if (!body.name) return json(res, 422, { error: 'name is required' });
          if (body.name !== current && keys.some((x) => x.name === body.name)) {
            return json(res, 422, { error: 'key already exists' });
          }
          k.name = body.name;
          if (body.profile) k.profile = body.profile;
          return json(res, 204, null);
        }
        if (method === 'POST' && seg[0] === 'keys' && seg[2] === 'revoke') {
          const k = keys.find((k) => k.name === decodeURIComponent(seg[1] ?? ''));
          if (k) k.revoked = true;
          return json(res, 204, null);
        }
        if (method === 'POST' && seg[0] === 'keys' && seg[2] === 'pause') {
          const k = keys.find((k) => k.name === decodeURIComponent(seg[1] ?? ''));
          if (k) k.paused = true;
          return json(res, 204, null);
        }
        if (method === 'POST' && seg[0] === 'keys' && seg[2] === 'resume') {
          const k = keys.find((k) => k.name === decodeURIComponent(seg[1] ?? ''));
          if (k) k.paused = false;
          return json(res, 204, null);
        }
        if (method === 'DELETE' && seg[0] === 'keys' && seg[1]) {
          const i = keys.findIndex((k) => k.name === decodeURIComponent(seg[1] ?? ''));
          if (i >= 0) keys.splice(i, 1);
          return json(res, 204, null);
        }
        if (method === 'GET' && path === '/requests') {
          const all: RequestRow[] = [
            {
              id: 1, conversationId: 'conv-001', keyName: 'web', gatewayModel: 'hyper/hyperbolic-70b',
              status: 200, promptTokens: 120, cachedTokens: 0, completionTokens: 84,
              costUSD: 0.0021, createdAt: '2026-09-14T10:02:00.000Z', durationMs: 2100,
            },
            {
              id: 2, conversationId: 'conv-001', keyName: 'web', gatewayModel: 'hyper/hyperbolic-70b',
              status: 200, promptTokens: 210, cachedTokens: 118, completionTokens: 31,
              costUSD: 0.0007, createdAt: '2026-09-14T10:03:00.000Z', durationMs: 730,
            },
            {
              id: 3, conversationId: 'conv-002', keyName: 'batch', gatewayModel: 'hyper/hyperbolic-70b',
              status: 429, promptTokens: 90, cachedTokens: 0, completionTokens: 0,
              costUSD: null, createdAt: '2026-09-14T11:47:00.000Z', durationMs: 45,
            },
          ].sort((a, b) => b.id - a.id);
          const kept = applyFilter(all);
          const p = parseList(params);
          const r = applyList(
            kept,
            p,
            {
              time: (x: RequestRow) => x.createdAt,
              key: (x: RequestRow) => x.keyName,
              model: (x: RequestRow) => x.gatewayModel,
              status: (x: RequestRow) => x.status,
              prompt: (x: RequestRow) => x.promptTokens,
              cached: (x: RequestRow) => x.cachedTokens,
              completion: (x: RequestRow) => x.completionTokens,
              cost: (x: RequestRow) => x.costUSD ?? -1,
              duration: (x: RequestRow) => x.durationMs ?? -1,
              id: (x: RequestRow) => x.id,
            },
            {
              key: (x: RequestRow) => x.keyName,
              model: (x: RequestRow) => x.gatewayModel,
              status: (x: RequestRow) => x.status,
              prompt: (x: RequestRow) => x.promptTokens,
              cached: (x: RequestRow) => x.cachedTokens,
              completion: (x: RequestRow) => x.completionTokens,
              cost: (x: RequestRow) => x.costUSD,
            },
            200,
            (a, b) => b.id - a.id,
          );
          if (r.error) return json(res, 400, { error: r.error });
          return json(res, 200, { requests: r.rows, total: r.total });
        }
        if (method === 'GET' && seg[0] === 'requests' && seg[1]) {
          const id = Number(seg[1]);
          const detail = requestDetails[id];
          if (!detail) return json(res, 404, { error: 'request not found' });
          return json(res, 200, detail);
        }
        next();
      });
    },
  };
}

// The SPA is served by the toll binary under /admin: Go handles /admin/api/*
// and serves the built assets from internal/admin/web/dist. The dev server
// serves the UI and mocks the API.
export default defineConfig({
  base: '/admin/',
  plugins: [react(), mockApi()],
  build: {
    outDir: '../internal/admin/web/dist',
    emptyOutDir: true,
  },
});