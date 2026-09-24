import type { IncomingMessage, ServerResponse } from 'node:http';
import { defineConfig } from 'vite';
import type { Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import type {
  KeyFilter,
  Profile,
} from './src/lib/types';

interface ProfileBody {
  name?: string;
  providerFilter?: KeyFilter;
  modelFilter?: KeyFilter;
  parents?: string[];
}

// In-memory mock of the toll admin API so the UI can be developed without a
// running toll. The real API lives in internal/admin (Go) and speaks the same
// shapes under /admin/api/*. Migrated resources (virtual keys) are not mocked
// here anymore: their ConnectRPC client runs against an in-memory transport
// (web/src/lib/mockKeys.ts), so only the REST surfaces below remain.
function mockApi(): Plugin {
  const none: KeyFilter = { mode: 'none', values: [] };
  const profiles: Profile[] = [
    // keyCount is static here: the keys themselves live in the SPA's
    // in-memory ConnectRPC mock, which this server-side process cannot see.
    { name: 'All', providerFilter: none, modelFilter: none, parents: [], isDefault: true, keyCount: 1, childCount: 1 },
    {
      name: 'hyper-chat',
      providerFilter: { mode: 'include', values: ['hyper'] },
      modelFilter: { mode: 'exclude', values: ['hyper/deepseek-v3'] },
      parents: [],
      isDefault: false,
      keyCount: 1,
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

  return {
    name: 'mock-admin-api',
    configureServer(server) {
      server.middlewares.use('/admin/api', async (req, res, next) => {
        const { method } = req;
        const [path = '', query] = (req.url ?? '').split('?');
        const params = new URLSearchParams(query ?? '');
        const seg = path.split('/').filter(Boolean); // e.g. ["keys","web","revoke"]

        if (method === 'GET' && path === '/profiles') {
          const p = parseList(params);
          const withCounts = profiles.map((x) => ({
            ...x,
            // Live child counts; keyCount is static (see the seed comment).
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
          // Derived profiles reference parents by name, so keep them pointing
          // at the renamed profile. (Keys are in the SPA's in-memory mock.)
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
          // The virtual keys themselves live in the SPA's in-memory mock, so
          // the "profile is in use by a key" check is only approximate in dev.
          const children = profiles.filter((x) => x.parents.includes(name)).length;
          if (children > 0) {
            return json(res, 422, { error: `profile is in use as a parent by ${children} profile(s)` });
          }
          profiles.splice(i, 1);
          return json(res, 204, null);
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