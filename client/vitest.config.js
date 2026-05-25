import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// Standalone vitest config (not extending vite.config.js — kept simple so a
// vite plugin chain change doesn't accidentally affect the test environment).
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/__tests__/setup.js'],
    // Match component-co-located tests as well as a dedicated __tests__ dir.
    include: ['src/**/*.{test,spec}.{js,jsx}', 'src/**/__tests__/**/*.{js,jsx}'],
    // Exclude the smoke setup file itself from the test selector.
    exclude: ['src/__tests__/setup.js', 'node_modules/**', 'dist/**'],
  },
});
