import { fileURLToPath } from 'node:url';

import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import wails from '@wailsio/runtime/plugins/vite';
import { defineConfig } from 'vite';

// The desktop shell's page. `wails3 dev` proxies this server into the window
// (the port is the one it expects); `npm run dev` on its own serves the same
// page to a browser, where the connector is the mock (src/bridge/mock) and
// the scenario panel (src/dev) walks its states.
export default defineConfig({
    // The wails plugin injects the generated event types (frontend/bindings)
    // and refuses to build until bindings exist: `wails3 generate bindings`.
    plugins: [react(), tailwindcss(), wails('./bindings')],
    // `@/` is `src/`: the shadcn/ui components import `@/lib/utils`.
    resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
    server: {
        host: '127.0.0.1',
        port: Number(process.env.WAILS_VITE_PORT) || 9245,
        strictPort: true,
    },
    build: {
        target: 'es2022',
        sourcemap: false,
    },
});
