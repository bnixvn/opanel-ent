import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The build writes straight into the directory the Go binary embeds, so
// there is no copy step between `npm run build` and `go build` to forget.
//
// emptyOutDir is explicit because the target is outside this package and
// Vite refuses to clear such a directory without being told.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/httpapi/web',
    emptyOutDir: true,
    // One bundle rather than many small chunks: the panel is served from the
    // same process it manages, often over a single connection on a small
    // VPS, and a dozen round trips costs more than the bytes saved.
    chunkSizeWarningLimit: 1500,
  },
  server: {
    // Development against a real panel: the API stays where it is and only
    // the interface is served locally.
    proxy: {
      '/api': {
        target: process.env.OPANEL_API || 'https://127.0.0.1:2222',
        changeOrigin: true,
        secure: false,
      },
    },
  },
});
