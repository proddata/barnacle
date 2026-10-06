import { defineConfig } from 'vite';
import { resolve } from 'node:path';

export default defineConfig({
  server: {
    host: '127.0.0.1',
    proxy: {
      '/sql': { target: 'http://127.0.0.1:8080', changeOrigin: false },
      '/metrics': { target: 'http://127.0.0.1:9090', changeOrigin: false },
      '/v2': { target: 'ws://127.0.0.1:8080', ws: true, changeOrigin: false },
    },
  },
  build: {
    chunkSizeWarningLimit: 3200,
    rollupOptions: { input: { index: resolve(import.meta.dirname, 'index.html'), metrics: resolve(import.meta.dirname, 'metrics.html') } },
  },
});
