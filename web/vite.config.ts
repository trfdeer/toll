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
          return json(res, 200, providers);
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
          return json(res, 200, models);
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
          return json(res, 200, {
            rows: kept,
            totalReqs: kept.reduce((n, r) => n + r.requests, 0),
            totalCost: kept.reduce((n, r) => n + r.costUSD, 0).toFixed(4) + ' USD',
          });
        }
        if (method === 'GET' && path === '/keys') {
          return json(res, 200, { keys });
        }
        if (method === 'GET' && path === '/profiles') {
          return json(res, 200, {
            profiles: profiles.map((p) => ({
              ...p,
              // Live counts: key references plus inheritance children.
              keyCount: keys.filter((k) => k.profile === p.name).length,
              childCount: profiles.filter((x) => x.parents.includes(p.name)).length,
            })),
          });
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
          const offset = Number(params.get('offset') ?? 0);
          const limit = Number(params.get('limit') ?? 200);
          return json(res, 200, {
            requests: kept.slice(offset, offset + limit),
            total: kept.length,
          });
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