import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  // The viewer chunk is about 0.8 MB (c4-sdk bundles React Flow, Markdown and its dialog); the ELK layout
  // worker (about 2.1 MB) is a separate asset, not a chunk.
  build: { chunkSizeWarningLimit: 1600 },
  test: {
    include: ["tests/**/*.test.ts"],
    environment: "node",
  },
});
