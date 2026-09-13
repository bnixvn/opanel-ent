import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Dev-only convenience: proxy /api to the local backend so the frontend
// can be reached from any host/IP without baking a fixed API origin into
// the browser bundle (VITE_API_URL still overrides this when set).
export default defineConfig({
  plugins: [react()],
  server: {
    host: true,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8000',
        changeOrigin: true,
      },
    },
  },
});
