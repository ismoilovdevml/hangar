/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** "true" serves the API from in-browser mocks (src/mocks) instead of the server. */
  readonly VITE_API_MOCKS?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
