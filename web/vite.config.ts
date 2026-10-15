import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vitest/config';
import vue from '@vitejs/plugin-vue';

export default defineConfig({
  plugins: [vue({ template: { transformAssetUrls: { includeAbsolute: false } } })],
  base: '/static/',
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  build: { target: 'es2022', sourcemap: false },
  server: {
    proxy: Object.fromEntries(['/projects', '/model-settings', '/settings', '/health'].map(path =>
      [path, { target: 'http://127.0.0.1:8000', changeOrigin: false }]))
  },
  test: { include: ['src/**/*.test.ts'], environment: 'node', restoreMocks: true }
});
