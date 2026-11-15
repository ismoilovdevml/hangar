/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** "true" serves the API from in-browser mocks (src/mocks) instead of the server. */
  readonly VITE_API_MOCKS?: string;
  /** "true" in the public demo build (`vite build --mode demo`): mocks plus the demo note. */
  readonly VITE_DEMO?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
