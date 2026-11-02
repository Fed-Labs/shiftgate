// The desktop client version, stamped from package.json at build time by the
// vite `define` in vite.config.ts. Reading it here (instead of hardcoding a
// string in the About card) keeps the UI on the same source the bundler uses.
declare const __APP_VERSION__: string;

export const APP_VERSION: string =
  typeof __APP_VERSION__ === "string" ? __APP_VERSION__ : "dev";
