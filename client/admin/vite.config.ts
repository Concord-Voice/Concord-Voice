import react from "@vitejs/plugin-react";
import { configDefaults, defineConfig } from "vitest/config";

export default defineConfig({
  base: "/admin/",
  plugins: [react()],
  preview: {
    headers: {
      "Referrer-Policy": "no-referrer",
    },
  },
  test: {
    environment: "jsdom",
    exclude: [...configDefaults.exclude, "e2e/**"],
    environmentOptions: {
      jsdom: {
        url: "http://localhost/admin/",
      },
    },
    // Node 26 defines both storage globals -- `localStorage` as an accessor returning
    // `undefined` without --localstorage-file (so .clear() throws), `sessionStorage` as a
    // working in-memory Storage. Vitest's populateGlobal skips keys already on globalThis,
    // so both shadow jsdom's real Storage. Node 24 (CI) defines neither -- no-op there.
    execArgv: ["--no-experimental-webstorage"],
    setupFiles: "./src/test/setup.ts",
    coverage: {
      provider: "istanbul",
      reporter: ["text", "lcov"],
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/**/*.test.{ts,tsx}", "src/test/**", "src/main.tsx"],
      thresholds: {
        statements: 80,
        branches: 80,
        functions: 80,
        lines: 80,
      },
    },
  },
});
