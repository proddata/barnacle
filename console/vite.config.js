import { defineConfig } from 'vite';

export default defineConfig({
  server: {
    host: '127.0.0.1',
    proxy: {
      '/sql': { target: 'http://127.0.0.1:8080', changeOrigin: false },
      '/v2': { target: 'ws://127.0.0.1:8080', ws: true, changeOrigin: false },
    },
  },
  build: { chunkSizeWarningLimit: 3200 },
});
