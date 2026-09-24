import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// The SPA is served by the toll binary under /admin: Go handles the
// ConnectRPC surface under /admin/api and serves the built assets from
// internal/admin/web/dist. The dev server serves the UI and mocks the entire
// admin API in the browser via the in-memory ConnectRPC transport in
// web/src/lib/mockAdmin.ts — no Go server and no server-side mocks needed.
export default defineConfig({
  base: '/admin/',
  plugins: [react()],
  build: {
    outDir: '../internal/admin/web/dist',
    emptyOutDir: true,
  },
});
