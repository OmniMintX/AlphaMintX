import { defineConfig } from "vitest/config";

// Pure tests run under node (the default below); component/hook tests opt
// into jsdom per-file via a `// @vitest-environment jsdom` docblock. TSX is
// transformed by esbuild directly (tsconfig has jsx: preserve for Next, so
// the automatic runtime is forced here) — no vite react plugin needed.
export default defineConfig({
  esbuild: { jsx: "automatic" },
  test: {
    include: ["src/**/*.test.{ts,tsx}", "app/**/*.test.{ts,tsx}"],
    environment: "node",
    setupFiles: ["./vitest.setup.ts"],
  },
});
